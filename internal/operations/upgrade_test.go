package operations

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

func TestPublicUpgradeRequiresCompleteBoundBackupAndResumes(t *testing.T) {
	home := createActive(t, Options{migrations: oldBuild(1)})
	i := open(t, home, Options{beforeDB: func(db ownership.Database) error {
		if db == ownership.Runtime {
			return errBackupCrash
		}
		return nil
	}})
	if _, err := i.Migrate(t.Context()); err == nil {
		t.Fatal("upgrade without backup accepted")
	}
	if i.Marker().Migration != nil || len(appliedVersions(t, i.DB(ownership.Main))) != 1 {
		t.Fatal("refused upgrade modified data")
	}
	_, foreign, _ := completeTestBackup(t)
	if _, err := i.Migrate(t.Context(), foreign); err == nil {
		t.Fatal("foreign instance backup accepted")
	}
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	m, err := i.Backup(t.Context(), BackupOptions{Destination: first, KeyID: ids.New()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := i.Backup(t.Context(), BackupOptions{Destination: second, KeyID: m.KeyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Migrate(t.Context(), first); !errors.Is(err, errBackupCrash) {
		t.Fatalf("expected interrupted migration: %v", err)
	}
	marker := i.Marker()
	if marker.Migration == nil || marker.Migration.BackupID != m.BackupID || !marker.Migration.BackupDigest.Valid() {
		t.Fatalf("backup binding not durable: %+v", marker.Migration)
	}
	if len(appliedVersions(t, i.DB(ownership.Main))) != migrations.Latest(ownership.Main) || len(appliedVersions(t, i.DB(ownership.Runtime))) != 1 {
		t.Fatal("test did not stop between databases")
	}
	if _, err := i.Migrate(t.Context(), second); err == nil {
		t.Fatal("interrupted migration switched backup")
	}
	if err := i.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	i = open(t, home, Options{})
	if err := i.Start(t.Context()); err == nil {
		t.Fatal("incomplete migration started serving")
	}
	// Bound journal permits resumption while the verified backup medium is
	// offline. This does not authorize replacing the immutable backup binding.
	if err := os.Rename(first, first+".offline"); err != nil {
		t.Fatal(err)
	}
	report, err := i.Migrate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Applied[ownership.Main]) != 0 || len(report.Applied[ownership.Runtime]) == 0 || i.Marker().Migration != nil {
		t.Fatalf("bad migration resume: %+v", report)
	}
	for _, db := range migrations.Databases {
		if len(appliedVersions(t, i.DB(db))) != migrations.Latest(db) {
			t.Errorf("%s not current", db)
		}
	}
}

func TestPublicUpgradeRejectsIncompleteAndCorruptBackup(t *testing.T) {
	i := open(t, createActive(t, Options{migrations: oldBuild(1)}), Options{})
	dest := filepath.Join(t.TempDir(), "archive")
	m, err := i.Backup(t.Context(), BackupOptions{Destination: dest, KeyID: ids.New(), fault: func(s string) error {
		if s == "common_point_captured" {
			return errBackupCrash
		}
		return nil
	}})
	if !errors.Is(err, errBackupCrash) {
		t.Fatal(err)
	}
	if _, err := i.Migrate(t.Context(), dest); err == nil {
		t.Fatal("incomplete backup used for upgrade")
	}
	if _, err := i.Backup(t.Context(), BackupOptions{BackupID: m.BackupID}); err != nil {
		t.Fatal(err)
	}
	backupWrite(t, dest, "COMPLETE", []byte("bad"))
	if _, err := i.Migrate(t.Context(), dest); err == nil {
		t.Fatal("corrupt completion marker used for upgrade")
	}
	if i.Marker().Migration != nil || len(appliedVersions(t, i.DB(ownership.Main))) != 1 {
		t.Fatal("refused upgrade modified data")
	}
}
