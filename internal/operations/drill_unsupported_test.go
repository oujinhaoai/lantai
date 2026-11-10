//go:build !linux && !darwin

package operations

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDrillUnsupportedPlatformFailsClosed(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, "independent-file")
	if err := os.WriteFile(file, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if drillIndependentFile(root, entry) {
		t.Fatal("unsupported platform claimed independent file verification")
	}
	if err := ValidateDrillHome(home); err == nil {
		t.Fatal("unsupported platform accepted drill home")
	}
	if inst, err := Open(t.Context(), Options{Home: home, RecoveryDrill: true}); err == nil {
		inst.Close(t.Context())
		t.Fatal("unsupported platform opened drill")
	}
	for _, name := range []string{drillFile, "lantai.lock"} {
		if _, err := os.Lstat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("unsupported drill wrote %s: %v", name, err)
		}
	}
}
