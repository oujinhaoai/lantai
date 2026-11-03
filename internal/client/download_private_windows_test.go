package client

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsDownloadRejectsBroadOriginalStateWithoutRepair(t *testing.T) {
	f := newWorksetFixture(t)
	f.shortOnce = true
	entry := f.entry("selected", map[string]string{"record.json": "synthetic private download bytes"})
	root := pinnedOutputTestRoot(t)
	input := WorksetInput{Schema: WorksetSchema, Entries: []WorksetEntry{entry}}
	if _, err := f.c.PullWorkset(t.Context(), input, root, "production"); err == nil {
		t.Fatal("short response accepted")
	}
	path := filepath.Join(root, "selected/record.json.lantai-download.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.PullWorkset(t.Context(), input, root, "production"); err == nil {
		t.Fatal("broad original metadata accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected metadata changed", err)
	}
	if _, err := ReadPrivate(path); err == nil {
		t.Fatal("rejection silently repaired original ACL")
	}
}
