package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// BM-JOB-05: the destination can already contain a verified COMPLETE archive
// while the application's final retention acknowledgement fails. This fault
// targets only ConfirmBackup's persistent UPDATE, never FSCK or the copier.
func TestBackupManagementWatermarkConfirmationFailureAndSamePointResume(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleContributor, true)
	e.ingest(e.login().Context, "watermark/source", []byte("synthetic backup watermark confirmation bytes\n"), *rightsOwned())
	a := reopenApplication(t, e, storage.Config{MinFreeBytes: 1 << 20})
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	const rootID = "synthetic-watermark-confirmation-volume"
	if err := os.WriteFile(filepath.Join(root, application.BackupRootMarker), []byte(rootID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.ConfigureBackups(ctx, root, rootID)
	servers := backupHTTPServers(t, a)
	admin := backupLogin(t, e, "http://"+servers.Addresses.API, []identity.Scope{identity.ScopeAdmin, identity.ScopeRead})
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := a.Events.Metrics(ctx)
	if err != nil || before.HighWater <= before.BackupThrough {
		t.Fatalf("fixture needs an unconfirmed event watermark: %+v %v", before, err)
	}

	const fault = "synthetic ConfirmBackup persistent update failure"
	err = a.Maintenance(ctx, func(c context.Context) error {
		_, err := a.Instance.DB(ownership.Events).ExecContext(c, `CREATE TRIGGER reject_backup_confirmation BEFORE UPDATE OF backup_through ON events_retention BEGIN SELECT RAISE(ABORT,'synthetic ConfirmBackup persistent update failure'); END`)
		if err != nil {
			return err
		}
		// Verify explicitly that the injected trigger leaves the same early
		// integrity check and sync used by application.Backup operational.
		report, err := a.FSCK(c, true)
		if err != nil {
			return err
		}
		if err := report.Err(); err != nil {
			return err
		}
		return a.Sync(c)
	})
	if err != nil {
		t.Fatal("fault incorrectly blocked the early integrity phase", err)
	}
	command := identity.ApproveBackup{Kind: "start", DrainSeconds: 10, TimeoutSeconds: 60}
	grant := backupApproval(t, e, admin, string(identity.ActBackupStart), command)
	var accepted backupmgmt.Job
	admin.must(http.MethodPost, "admin/backups", "fail-watermark-confirmation", map[string]any{"grant_id": grant, "drain_seconds": 10, "timeout_seconds": 60}, &accepted, http.StatusAccepted)
	deadline := time.Now().Add(30 * time.Second)
	var failed backupmgmt.Job
	for {
		admin.must(http.MethodGet, "admin/backups/"+string(accepted.ID), "", nil, &failed, http.StatusOK)
		if failed.State == "complete" {
			t.Fatal("job reported success before its persistent backup watermark was confirmed")
		}
		if failed.State == "failed" {
			break
		}
		if failed.State == "cancelled" || failed.State == "interrupted" || time.Now().After(deadline) {
			t.Fatalf("unexpected confirmation fault result: %+v", failed)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if failed.Error != "backup_failed" || failed.Attempt != 1 || !failed.CaptureStarted || !failed.BackupID.Valid() {
		t.Fatalf("job did not preserve the completed capture as a failed operation: %+v", failed)
	}
	archive := filepath.Join(root, string(failed.ID))
	manifest, err := operations.VerifyBackup(ctx, archive)
	if err != nil || manifest.State != "complete" || manifest.BackupID != failed.BackupID {
		t.Fatalf("fault occurred before a real verified COMPLETE archive: %+v %v", manifest, err)
	}
	capturedThrough, ok := manifest.Watermarks["events"]
	if !ok || capturedThrough <= before.BackupThrough {
		t.Fatalf("backup did not capture a newer watermark: %+v", manifest.Watermarks)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(archive, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	completeBefore, err := os.ReadFile(filepath.Join(archive, "COMPLETE"))
	if err != nil {
		t.Fatal(err)
	}
	durableRaw, err := os.ReadFile(filepath.Join(a.Instance.Layout().Home, "backups", "online", string(failed.ID)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var durable backupmgmt.Job
	if err := json.Unmarshal(durableRaw, &durable); err != nil || durable.State != "failed" || durable.BackupID != manifest.BackupID {
		t.Fatalf("failure was not durable despite the completed archive: %+v %v", durable, err)
	}
	afterFailure, err := a.Events.Metrics(ctx)
	if err != nil || afterFailure.BackupThrough != before.BackupThrough || afterFailure.PrunedThrough != before.PrunedThrough {
		t.Fatalf("failed confirmation advanced retention: before=%+v after=%+v err=%v", before, afterFailure, err)
	}
	// This second direct call proves the fault is the exact persistent update,
	// with the trigger's distinct error, after the real worker has exited.
	err = a.Maintenance(ctx, func(c context.Context) error { return a.Events.ConfirmBackup(c, capturedThrough) })
	if err == nil || !strings.Contains(err.Error(), fault) {
		t.Fatalf("ConfirmBackup UPDATE fault was not active: %v", err)
	}
	if !a.Ready() {
		t.Fatal("confirmation failure left normal business readiness closed")
	}
	admin.must(http.MethodGet, "projects", "", nil, nil, http.StatusOK)
	// A successful business write followed by relay proves service resumed,
	// and creates newer events that a retry must never claim as backed up.
	admin.must(http.MethodPost, "projects", "after-watermark-failure", map[string]string{"key": "after-watermark-failure", "name": "After watermark failure"}, nil, http.StatusCreated)
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	later, err := a.Events.Metrics(ctx)
	if err != nil || later.HighWater <= capturedThrough || later.BackupThrough != before.BackupThrough {
		t.Fatalf("fixture failed to create later unconfirmed events: %+v %v", later, err)
	}
	t.Logf("FSCK/sync passed; real archive %s verified COMPLETE while durable job failed; targeted ConfirmBackup UPDATE aborted; backup_through remained %d; business reads/writes resumed", manifest.BackupID, before.BackupThrough)

	err = a.Maintenance(ctx, func(c context.Context) error {
		_, err := a.Instance.DB(ownership.Events).ExecContext(c, `DROP TRIGGER reject_backup_confirmation`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	resume := identity.ApproveBackup{Kind: "resume", JobID: failed.ID, DrainSeconds: 10, TimeoutSeconds: 60}
	resumeGrant := backupApproval(t, e, admin, string(identity.ActBackupResume), resume)
	var resumed backupmgmt.Job
	admin.must(http.MethodPost, "admin/backups/"+string(failed.ID)+"/resume", "resume-watermark-confirmation", map[string]any{"grant_id": resumeGrant, "drain_seconds": 10, "timeout_seconds": 60}, &resumed, http.StatusAccepted)
	succeeded := awaitBackup(t, admin, resumed.ID)
	if succeeded.ID != failed.ID || succeeded.BackupID != manifest.BackupID || succeeded.Attempt != 2 {
		t.Fatalf("confirmation retry replaced the original point: failed=%+v resumed=%+v", failed, succeeded)
	}
	verified, err := operations.VerifyBackup(ctx, archive)
	if err != nil || verified.BackupID != manifest.BackupID || verified.Watermarks["events"] != capturedThrough {
		t.Fatalf("resumed archive point changed: %+v %v", verified, err)
	}
	manifestAfter, err := os.ReadFile(filepath.Join(archive, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	completeAfter, err := os.ReadFile(filepath.Join(archive, "COMPLETE"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestBefore, manifestAfter) || !bytes.Equal(completeBefore, completeAfter) {
		t.Fatal("confirmation retry recaptured or rewrote the completed archive")
	}
	afterResume, err := a.Events.Metrics(ctx)
	if err != nil || afterResume.BackupThrough != capturedThrough || afterResume.HighWater <= capturedThrough || !a.Ready() {
		t.Fatalf("retry confirmed anything other than the frozen watermark or failed to reopen business: %+v %v", afterResume, err)
	}
	admin.must(http.MethodGet, "projects", "", nil, nil, http.StatusOK)
	t.Logf("new HumanGrant resumed same job/backup ID at attempt 2; archive bytes unchanged; backup_through confirmed exactly %d while newer high-water %d remained unconfirmed", capturedThrough, afterResume.HighWater)
}
