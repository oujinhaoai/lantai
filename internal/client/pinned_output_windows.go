package client

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func createPinnedOutputTemp(root *os.Root, name string) (*os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := privateSecurityDescriptor(user.User.Sid)
	if err != nil {
		return nil, err
	}
	parent, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.Fd()), ObjectName: objectName, Attributes: windows.OBJ_DONT_REPARSE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE, &oa, &windows.IO_STATUS_BLOCK{}, nil, windows.FILE_ATTRIBUTE_NORMAL, 0, windows.FILE_CREATE, windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_NON_DIRECTORY_FILE, 0, 0)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		return nil, os.ErrExist
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), name), nil
}
func checkPinnedOutputPrivate(f *os.File) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !windows.EqualSid(owner, user.User.Sid) {
		return errors.New("client: private output must be owned by the current user")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.New("client: private output must have a private ACL")
	}
	for n := uint32(0); n < uint32(acl.AceCount); n++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(acl, n, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("client: private output has an unsupported ACL entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !windows.EqualSid(sid, user.User.Sid) {
			return errors.New("client: private output ACL grants another principal access")
		}
	}
	return nil
}
func syncPinnedOutputDirectory(*os.Root) error { return nil }
