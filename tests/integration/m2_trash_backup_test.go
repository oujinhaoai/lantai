package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Retained trash is authoritative content. A complete common backup must carry
// its exact version and record bytes, so an empty-directory restore can verify
// the copied ledger facts without relying on the source instance or its index.
func TestM2CommonBackupRetainsTrashFiles(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	app := reopenApplication(t, e)
	who := e.login().Context
	v := e.ingest(who, "backup-trash", []byte("synthetic retained trash bytes"), *rightsOwned())
	rightsEvidence(t, e, who, v, false)
	entry := trashForTest(t, e, app.Lifecycle, who, v.AssetID)
	key, err := masterkey.Load(app.Instance.Config().SecretsDir)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "archive")
	m, err := app.Backup(t.Context(), operations.BackupOptions{Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	prefix := "trash/" + string(entry.ID) + "/"
	var count int
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, prefix) {
			count++
			b, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(f.Path)))
			if err != nil || int64(len(b)) != f.Size || shaOf(b) != f.SHA256 {
				t.Fatal("retained trash bytes differ from the frozen manifest", f.Path, err)
			}
		}
	}
	if count < 4 {
		t.Fatalf("complete backup omitted retained trash: got %d members under %s", count, prefix)
	}
	if err = app.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "restored")
	if _, err = application.Restore(t.Context(), operations.RestoreOptions{Home: home, BackupDir: dest}, key, identity.Config{Password: fastPassword}); err != nil {
		t.Fatal(err)
	}
	restored, err := application.OpenOffline(t.Context(), application.Options{Instance: operations.Options{Home: home, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(context.Background())
	if _, err = restored.Lifecycle.ManifestRoots(t.Context()); err != nil {
		t.Fatal("restored ledger lost its retained trash root", err)
	}
	if report, err := restored.FSCK(t.Context(), true); err != nil || report.Err() != nil {
		t.Fatal("restored trash failed deep integrity verification", report.Findings, err)
	}
}
