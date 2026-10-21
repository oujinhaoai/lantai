//go:build windows

package client

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestImportWindowsPrivateDirectoryExplicitOwnerAndACL(t *testing.T) {
	dir, err := importAbsolute(filepath.Join(t.TempDir(), "nested", "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err = importPrivateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Dir(dir), dir} {
		sd, err := windows.GetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		assertPrivateDescriptor(t, sd, user.User.Sid)
	}
	if err = importPrivateDirectory(dir); err != nil {
		t.Fatal("same private directory not reusable", err)
	}
}

func TestImportWindowsExistingBroadDirectoryIsRejectedWithoutRepair(t *testing.T) {
	dir := importStateDir(t)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err = importPrivateDirectory(dir); err == nil {
		t.Fatal("broad existing state directory accepted or silently repaired")
	}
	if err = importPrivateDirectory(dir); err == nil {
		t.Fatal("broad ACL changed on rejection")
	}
}
