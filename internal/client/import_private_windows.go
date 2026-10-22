//go:build windows

package client

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func makeImportPrivateDirectory(dir string) error {
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() {
			return errors.New("client: import state must be a directory")
		}
		// The caller checks the existing ACL. Never broaden or silently repair it.
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return errors.New("client: cannot create import state root")
	}
	if err := makeImportPrivateDirectory(parent); err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := privateSecurityDescriptor(user.User.Sid)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err = windows.CreateDirectory(name, &sa); err != nil && err != windows.ERROR_ALREADY_EXISTS {
		return err
	}
	return nil
}
