//go:build linux || darwin

package application

import (
	"context"
	"errors"
	"github.com/oujinhaoai/lantai/internal/commands"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp/totptest"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func TestCompleteBackupRestoresWithoutSourceOrSeparateKey(t *testing.T) {
	a, clk, secret := openBackupHTTPApp(t)
	old, e := a.Identity.Login(t.Context(), identity.LoginRequest{Name: "ada", Password: "correct horse battery staple", Code: totptest.Fresh(t, clk, secret), Channel: identity.ChannelCLI, Scopes: []identity.Scope{identity.ScopeRead}})
	if e != nil {
		t.Fatal(e)
	}
	if _, err := a.Identity.RecoveryKeyID(t.Context()); err == nil {
		t.Fatal("key capability accessible without maintenance")
	}
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	backup := filepath.Join(dir, "snapshot")
	m, e := a.Backup(t.Context(), operations.BackupOptions{Destination: backup})
	if e != nil {
		t.Fatal(e)
	}
	if m.RecoveryPackage == nil {
		t.Fatal("successful backup lacks complete package")
	}
	oldID := m.KeyID
	home := a.Instance.Layout().Home
	if !strings.HasPrefix(a.Instance.Config().SecretsDir, home+string(filepath.Separator)) {
		t.Fatal("fixture secrets must be under temporary source")
	}
	if e = a.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home + ".unavailable") })
	if e = os.Rename(home, home+".unavailable"); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(home); !os.IsNotExist(e) {
		t.Fatal("source still reachable")
	}
	target := filepath.Join(t.TempDir(), "restored")
	marker, e := RestorePackage(t.Context(), operations.RecoveryPackagePath(backup), operations.RestoreOptions{Home: target}, identity.Config{Password: testPassword})
	if e != nil {
		t.Fatal(e)
	}
	if marker.Restore == nil || marker.Restore.Stage != "reconciliation_required" || marker.RecoveryEpoch <= m.RecoveryEpoch {
		t.Fatal("restore gate/epoch lost")
	}
	restored, e := OpenOffline(t.Context(), Options{Instance: operations.Options{Home: target}, Identity: identity.Config{Password: testPassword}})
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close(context.Background())
	key, e := masterkey.Load(restored.Instance.Config().SecretsDir)
	if e != nil || key.ID() == oldID {
		t.Fatal("old recovery key became runtime key", e)
	}
	if _, e = restored.Identity.AuthenticateToken(t.Context(), old.Token); e == nil {
		t.Fatal("old session survived restore")
	}
	var active int
	if e = restored.Instance.DB(ownership.Main).QueryRow("SELECT count(*) FROM identity_passwords").Scan(&active); e != nil || active != 0 {
		t.Fatal("old passwords survived", e)
	}
	if restored.Readiness().Ready {
		t.Fatal("restore opened business before administrator and reconciliation")
	}
}

// The original snapshot may already be complete when the containing recovery
// artifact is still being built. That must never reopen business or report a
// successful complete backup early.
func TestCompletePackageKeepsMaintenanceUntilExporterExits(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	entered, finish := make(chan struct{}), make(chan struct{})
	original := b.run
	b.run = func(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
		check := opts.CheckDestination
		held := false
		opts.CheckDestination = func(c context.Context) error {
			if check != nil {
				if err := check(c); err != nil {
					return err
				}
			}
			if !held {
				if _, err := os.Stat(filepath.Join(opts.Destination, "COMPLETE")); err == nil {
					held = true
					close(entered)
					<-finish
					return c.Err()
				}
			}
			return nil
		}
		return original(ctx, opts)
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("export did not enter post-snapshot phase")
	}
	if a.Ready() {
		t.Fatal("business reopened before complete package")
	}
	if _, lease, err := a.Instance.Gate().Acquire(t.Context(), commands.Request{}); err == nil {
		lease.Release()
		t.Fatal("writer entered package export")
	}
	if _, err = b.Start(BackupRequest{}); !errors.Is(err, ErrBackupBusy) {
		t.Fatal("concurrent export admitted", err)
	}
	if _, err = b.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if a.Ready() {
		t.Fatal("cancel reopened while exporter still alive")
	}
	close(finish)
	j = waitBackup(t, b, string(j.ID))
	if j.State != "cancelled" || j.Package != nil || !a.Ready() {
		t.Fatal("cancel did not safely restore service", j)
	}
	b.run = original
	j, err = b.Start(BackupRequest{Resume: j.ID})
	if err != nil {
		t.Fatal(err)
	}
	j = waitBackup(t, b, string(j.ID))
	if j.State != "complete" || j.Package == nil {
		t.Fatal("complete package retry failed", j)
	}
}
