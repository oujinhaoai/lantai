//go:build windows

package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func assertPrivateDescriptor(t *testing.T, sd *windows.SECURITY_DESCRIPTOR, user *windows.SID) {
	t.Helper()
	owner, defaulted, err := sd.Owner()
	if err != nil || owner == nil || defaulted || !windows.EqualSid(owner, user) {
		t.Fatalf("private descriptor must explicitly own the current user: defaulted=%v err=%v", defaulted, err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("private DACL must be protected from inheritance: control=%v err=%v", control, err)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		t.Fatalf("private DACL must contain exactly one user ACE: err=%v", err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err = windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !windows.EqualSid((*windows.SID)(unsafe.Pointer(&ace.SidStart)), user) {
		t.Fatal("private DACL must grant only the current user")
	}
}

func TestWindowsPrivateFileExplicitOwner(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// Assert the descriptor itself, so this catches an omitted O: even on a
	// machine whose default TokenOwner happens to equal TokenUser.
	sd, err := privateSecurityDescriptor(user.User.Sid)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateDescriptor(t, sd, user.User.Sid)
	f, err := privateTemp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sd, err = windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateDescriptor(t, sd, user.User.Sid)
}

func TestWindowsPrivateWriteReadAndReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	for _, content := range []string{"first private value", "replacement private value"} {
		if err := WritePrivate(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
		got, err := ReadPrivate(path)
		if err != nil || string(got) != content {
			t.Fatalf("private round trip failed: %v", err)
		}
	}
}

func TestWindowsPrivateFileRejectsAnotherPrincipalACE(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WritePrivate(path, []byte("private")); err != nil {
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
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadPrivate(path); err == nil || !strings.Contains(err.Error(), "another principal") {
		t.Fatalf("broadened ACL was not rejected: %v", err)
	}
	if err = WritePrivate(path, []byte("replacement")); err == nil {
		t.Fatal("private replacement accepted a broadened ACL")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "private" {
		t.Fatalf("refused replacement changed existing data: %v", err)
	}
}
