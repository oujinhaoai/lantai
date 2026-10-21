package client

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPinnedOutputCreatesPrivateOwnerAndACL(t *testing.T) {
	root, err := os.OpenRoot(pinnedOutputTestRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, err := createPinnedOutputTemp(root, "private-output.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateDescriptor(t, sd, user.User.Sid)
	if err = checkPinnedOutputPrivate(f); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPinnedOutputRefusesBroadenedExistingACL(t *testing.T) {
	path := filepath.Join(pinnedOutputTestRoot(t), "report.json")
	o, err := OpenPinnedPrivateOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err = o.Write([]byte("retain\n")); err != nil {
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
	if err = o.Write([]byte("must not replace")); err == nil {
		t.Fatal("broadened output accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "retain\n" {
		t.Fatalf("refused output changed=%q %v", got, err)
	}
}
