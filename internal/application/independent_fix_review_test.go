//go:build linux || darwin

package application

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
)

// A crash can leave the controller backing_up with no backup_id after the
// source record became complete. A different valid artifact must not satisfy it.
func TestIndependentFixReviewRejectDifferentCompletedPoint(t *testing.T) {
	reviewReplacedCompletedPoint(t, false)
}

func TestIndependentFixReviewRejectForeignCompletedPoint(t *testing.T) {
	reviewReplacedCompletedPoint(t, true)
}

func reviewReplacedCompletedPoint(t *testing.T, foreign bool) {
	t.Helper()
	a := openTestApp(t)
	b := testBackupControl(t, a)
	first, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first = waitBackup(t, b, string(first.ID))
	if first.State != "complete" {
		t.Fatal(first)
	}
	secondController := b
	if foreign {
		other := openTestApp(t)
		secondController = testBackupControl(t, other)
	}
	second, err := secondController.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	second = waitBackup(t, secondController, string(second.ID))
	if second.State != "complete" || first.BackupID == second.BackupID {
		t.Fatal(first, second)
	}
	// Model crash before the online final journal save, then replacement of the
	// first destination by a complete, integrity-valid backup of the same instance.
	expectedID := first.BackupID
	first.State, first.BackupID = "backing_up", ""
	if err = b.save(first); err != nil {
		t.Fatal(err)
	}
	root, home := b.root, a.Instance.Layout().Home
	firstDir, secondDir := filepath.Join(root, string(first.ID)), filepath.Join(secondController.root, string(second.ID))
	if err = a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(firstDir, firstDir+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(secondDir, firstDir); err != nil {
		t.Fatal(err)
	}
	m, err := operations.VerifyBackup(t.Context(), firstDir)
	if err != nil || m.BackupID != second.BackupID {
		t.Fatalf("replacement fixture invalid: %v %+v", err, m)
	}
	a2, err := Open(t.Context(), Options{Instance: operations.Options{Home: home}, Identity: identity.Config{Password: testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a2.Close(context.Background()) })
	b2, err := a2.EnableBackups(t.Context(), root, "synthetic-volume")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b2.Start(BackupRequest{Resume: first.ID}); err != nil {
		t.Fatal(err)
	}
	got := waitBackup(t, b2, string(first.ID))
	t.Logf("foreign=%v source backup=%s; substituted artifact=%s; resumed state=%s backup=%s error=%s", foreign, expectedID, second.BackupID, got.State, got.BackupID, got.Error)
	if got.State == "complete" {
		t.Fatal("wrong completed point was accepted for original online job")
	}
}

func TestIndependentFixReviewExactAtomicTempProtocol(t *testing.T) {
	// Pure recognition boundaries, independent of file-copy behavior.
	validID := "01M3RW3E8077NTBJ5SB1VP140R"
	for _, suffix := range []string{"0", "1", "4294967295"} {
		if !atomicBackupJournalTemp("." + validID + ".json.tmp-" + suffix) {
			t.Fatalf("valid suffix rejected: %q", suffix)
		}
	}
	for _, name := range []string{
		"." + validID + ".json.tmp-", "." + validID + ".json.tmp-00", "." + validID + ".json.tmp-4294967296",
		"." + validID + ".json.tmp--1", "." + validID + ".json.tmp-1x", "." + validID + ".json.tmp-1.bak",
		validID + ".json.tmp-1", ".bad.json.tmp-1", "." + validID + ".json.tmp-１",
	} {
		if atomicBackupJournalTemp(name) {
			t.Fatalf("invalid temp accepted: %q", name)
		}
	}
}
