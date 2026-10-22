//go:build windows

package fsutil

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CreatePrivateTemp creates an explicitly owned, inheritance-protected file.
// Unix permission bits do not restrict Windows access; use a user-only DACL.
func CreatePrivateTemp(dir, prefix string) (*os.File, error) {
	if strings.ContainsAny(prefix, `/\`) {
		return nil, errors.New("fsutil: temporary prefix must be one file name")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	for range 10 {
		name := filepath.Join(dir, prefix+rand.Text())
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ, &sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(h), name), nil
	}
	return nil, errors.New("fsutil: cannot allocate private temporary file")
}

// CheckPrivateFile verifies the native owner and DACL, never Unix mode bits.
func CheckPrivateFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return ErrInsecurePermissions
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !windows.EqualSid(owner, user.User.Sid) {
		return ErrInsecurePermissions
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return ErrInsecurePermissions
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 {
		return ErrInsecurePermissions
	}
	for n := uint32(0); n < uint32(acl.AceCount); n++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, n, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !windows.EqualSid((*windows.SID)(unsafe.Pointer(&ace.SidStart)), user.User.Sid) {
			return ErrInsecurePermissions
		}
	}
	return nil
}
