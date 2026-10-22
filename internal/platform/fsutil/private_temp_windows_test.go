//go:build windows

package fsutil

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateTempHasUserOnlyACL(t *testing.T) {
	f, err := CreatePrivateTemp(t.TempDir(), "private-")
	if err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	if _, err = f.Write([]byte("synthetic private bytes")); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = CheckPrivateFile(name); err != nil {
		t.Fatal(err)
	}
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
	if err = windows.SetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err = CheckPrivateFile(name); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("broadened ACL accepted: %v", err)
	}
	if b, err := os.ReadFile(name); err != nil || string(b) != "synthetic private bytes" {
		t.Fatalf("check changed content: %q %v", b, err)
	}
}
