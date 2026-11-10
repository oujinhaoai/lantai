//go:build linux || darwin

package operations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func drillTestHome(t *testing.T) string {
	t.Helper()
	_, archive, backup := completeTestBackup(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(parent, "restored")
	if _, err = RestoreTo(t.Context(), RestoreOptions{Home: home, BackupDir: archive, KeyID: backup.KeyID}); err != nil {
		t.Fatal(err)
	}
	i, err := Open(t.Context(), Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	err = i.OfflineMaintenance(t.Context(), func(c context.Context) error { return i.SetRestoreStage(c, "reconciliation_required") })
	if err != nil {
		t.Fatal(err)
	}
	if err = i.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestDrillBoundaryPersistsWithoutPromotion(t *testing.T) {
	home := drillTestHome(t)
	i, err := Open(t.Context(), Options{Home: home, RecoveryDrill: true})
	if err != nil {
		t.Fatal(err)
	}
	original := i.Marker()
	if !i.IsRecoveryDrill() {
		t.Fatal("missing drill mode")
	}
	if err = i.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if i.Marker().Restore == nil || i.Marker().RecoveryEpoch != original.RecoveryEpoch {
		t.Fatal("drill removed recovery truth")
	}
	if _, err = i.Backup(t.Context(), BackupOptions{Destination: t.TempDir()}); err == nil {
		t.Fatal("drill backup allowed")
	}
	if err = i.CompleteRestore(t.Context(), RecoveryReview{}, nil); err == nil {
		t.Fatal("drill promotion allowed")
	}
	if err = i.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if normal, err := Open(t.Context(), Options{Home: home}); err == nil {
		normal.Close(t.Context())
		t.Fatal("normal open allowed")
	}
	i, err = Open(t.Context(), Options{Home: home, DrillMaintenance: true})
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close(t.Context())
	if err = i.Start(t.Context()); err == nil {
		t.Fatal("maintenance started service")
	}
	if err = i.OfflineMaintenance(t.Context(), func(c context.Context) error { return i.CompleteRestore(c, RecoveryReview{}, nil) }); err == nil {
		t.Fatal("maintenance promoted drill")
	}
}

func TestDrillRejectsLiveAndForeignBinding(t *testing.T) {
	home, err := filepath.EvalSymlinks(createActive(t, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if i, err := Open(t.Context(), Options{Home: home, RecoveryDrill: true}); err == nil {
		i.Close(t.Context())
		t.Fatal("live opt-in")
	}
	if _, err := os.Lstat(filepath.Join(home, drillFile)); !os.IsNotExist(err) {
		t.Fatal("live home marked")
	}
	home = drillTestHome(t)
	i, err := Open(t.Context(), Options{Home: home, RecoveryDrill: true})
	if err != nil {
		t.Fatal(err)
	}
	i.Close(t.Context())
	raw, err := os.ReadFile(filepath.Join(home, drillFile))
	if err != nil {
		t.Fatal(err)
	}
	var boundary drillBoundary
	if err = json.Unmarshal(raw, &boundary); err != nil {
		t.Fatal(err)
	}
	boundary.RecoveryEpoch++
	raw, _ = json.Marshal(boundary)
	if err = os.WriteFile(filepath.Join(home, drillFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if i, err := Open(t.Context(), Options{Home: home, RecoveryDrill: true}); err == nil {
		i.Close(t.Context())
		t.Fatal("foreign binding allowed")
	}
}

func TestDrillRejectsAliasesBeforeWrites(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "external-secrets", "home-alias"} {
		t.Run(kind, func(t *testing.T) {
			home := drillTestHome(t)
			target := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink":
				if err := os.Symlink(target, filepath.Join(home, "alias")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(target, filepath.Join(home, "alias")); err != nil {
					t.Fatal(err)
				}
			case "external-secrets":
				if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nsecrets:\n  dir: "+filepath.Dir(target)+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "home-alias":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(home, alias); err != nil {
					t.Fatal(err)
				}
				home = alias
			}
			if i, err := Open(t.Context(), Options{Home: home, RecoveryDrill: true}); err == nil {
				i.Close(t.Context())
				t.Fatal("unsafe tree allowed")
			}
			if _, err := os.Lstat(filepath.Join(home, drillFile)); !os.IsNotExist(err) {
				t.Fatal("unsafe tree marked")
			}
			raw, err := os.ReadFile(target)
			if err != nil || string(raw) != "unchanged" {
				t.Fatal("outside file changed")
			}
		})
	}
}
