package operations

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyBackupFileRejectsInternalDestinationParentLink(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	backupWrite(t, source, "payload", []byte("replacement"))
	backupWrite(t, destination, "real/target", []byte("preserve"))
	if err := os.Symlink("real", filepath.Join(destination, "alias")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	src, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := copyBackupFile(t.Context(), src, "payload", dst, "alias/target", nil); err == nil {
		t.Error("internal destination symlink accepted")
	}
	got, err := os.ReadFile(filepath.Join(destination, "real", "target"))
	if err != nil || string(got) != "preserve" {
		t.Fatalf("another destination entry was overwritten: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "real", "target.partial")); !os.IsNotExist(err) {
		t.Fatal("partial file left behind", err)
	}
}
