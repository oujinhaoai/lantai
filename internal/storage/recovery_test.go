package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestInventoryKeepsUnreferencedBytesAndRejectsCorruption(t *testing.T) {
	f, req := installFixture(t, nil)
	if _, err := f.svc.Install(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	orphan := []byte("unreferenced verified synthetic bytes")
	sha := shaOf(orphan)
	path := f.svc.layout.BlobPath(sha)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, orphan, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := f.svc.Inventory(t.Context(), true)
	if err != nil || len(before.Blobs) != 3 || len(before.Installs) != 1 || len(before.Findings) != 0 {
		t.Fatalf("inventory: %+v %v", before, err)
	}
	// Neither an installed directory nor an unreferenced CAS object is promoted
	// into ledger authority by a read-only fsck.
	if _, err = f.ledger.Version(t.Context(), req.AssetID, req.VersionID); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("fsck manufactured authority: %v", err)
	}
	if err = os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.Inventory(t.Context(), true)
	if err != nil || len(after.Findings) != 1 || after.Findings[0].Code != errcode.HashMismatch {
		t.Fatalf("corruption: %+v %v", after, err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("fsck removed bytes", err)
	}
}

func TestInventoryDoesNotFollowSymlink(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	path := f.svc.layout.BlobPath(shaOf([]byte("private")))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	got, err := f.svc.Inventory(t.Context(), true)
	if err != nil || len(got.Blobs) != 0 || len(got.Findings) != 1 || got.Findings[0].Reason != "non_regular" {
		t.Fatalf("symlink: %+v %v", got, err)
	}
}

func TestPinReconciliationRequiresLiveMaintenanceAndPreservesStaging(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	b := []byte("verified blob")
	u := f.uploadAll(f.who, f.project, b)
	pending := []byte("pending blob")
	pendingU := f.createUpload(f.who, f.project, pending)
	f.putAll(f.who, pendingU, pending)
	stats, err := f.svc.Stats(t.Context())
	if err != nil || stats.OpenUploads != 2 || stats.UploadStagingBytes != int64(len(pending)) || stats.GCSupported {
		t.Fatalf("stats: %+v %v", stats, err)
	}
	if _, err = f.svc.ReconcilePins(t.Context()); err == nil {
		t.Fatal("reconcile without exclusive barrier")
	}
	if _, err = f.svc.db.Exec(`DELETE FROM storage_pins WHERE owner_id=?`, u.UploadID); err != nil {
		t.Fatal(err)
	}
	lctx, held, err := f.inst.Gate().Maintain(t.Context(), commands.ReasonRecovering)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { held.Release(); f.inst.Gate().Open() }()
	report, err := f.svc.ReconcilePins(lctx)
	if err != nil || report.Created != 1 || report.BlobsAdded != 1 {
		t.Fatalf("repair: %+v %v", report, err)
	}
	if _, err = os.Stat(f.svc.layout.uploadData(pendingU.UploadID, shaOf(pending))); err != nil {
		t.Fatalf("staging removed: %v", err)
	}
	pins, err := f.svc.PinsFor(t.Context(), shaOf(b))
	if err != nil || len(pins) != 1 || !pins[0].ActiveAt(f.clk.Now()) {
		t.Fatalf("pin: %+v %v", pins, err)
	}
	wantID := pins[0].PinID
	f.restart()
	repeat, err := f.svc.ReconcilePins(lctx)
	if err != nil || repeat.Created != 0 || repeat.BlobsAdded != 0 || repeat.Corrected != 0 {
		t.Fatalf("repeat: %+v %v", repeat, err)
	}
	// Explicit terminal owner state, rather than elapsed wall time, releases the
	// durable pin. Reconciliation never closes an open upload by itself.
	now := clock.Millis(f.clk.Now())
	if _, err = f.svc.db.Exec(`UPDATE storage_uploads SET state='completed',closed_at=? WHERE upload_id=?`, now, u.UploadID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.ReconcilePins(lctx); err != nil {
		t.Fatal(err)
	}
	pins, err = f.svc.PinsFor(t.Context(), shaOf(b))
	if err != nil || len(pins) != 1 || pins[0].PinID != wantID || pins[0].ActiveAt(f.clk.Now()) {
		t.Fatalf("terminal pin: %+v %v", pins, err)
	}
	if _, err = os.Stat(f.svc.layout.BlobPath(shaOf(b))); err != nil {
		t.Fatal("reconcile physically collected CAS", err)
	}
	held.Release()
	if _, err = f.svc.ReconcilePins(lctx); err == nil {
		t.Fatal("released maintenance context accepted")
	}
}

func TestInventoryReadsRecordIdentityWithoutAcceptingEvidence(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	v := f.commitVersion(f.who, f.project, "", "records/item", "", map[string][]byte{"a.txt": []byte("a")})
	r := Record{RecordID: ids.New(), ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, ManifestDigest: v.ManifestDigest, Kind: "note", Payload: []byte(`{"note":"synthetic"}`), AuthorID: f.who.PrincipalID, CreatedAt: clock.Format(f.clk.Now())}
	if _, err := f.svc.AppendRecord(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Inventory(t.Context(), true)
	if err != nil || len(got.Records) != 1 || got.Records[0].RecordID != r.RecordID || len(got.Findings) != 0 {
		t.Fatalf("records: %+v %v", got, err)
	}
}

func TestInventoryChecksReceivedPartsAndMalformedInstallFacts(t *testing.T) {
	f, req := installFixture(t, nil)
	if _, err := f.svc.Install(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	b := []byte("received but not yet verified")
	u := f.createUpload(f.who, f.project, b)
	f.putAll(f.who, u, b)
	path := f.svc.layout.uploadData(u.UploadID, shaOf(b))
	if err := os.WriteFile(path, []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.db.Exec(`UPDATE storage_version_files SET sha256='invalid' WHERE version_id=?`, req.VersionID); err != nil {
		t.Fatal(err)
	}
	inv, err := f.svc.Inventory(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	foundPart, foundInstall := false, false
	for _, v := range inv.Findings {
		if v.Reason == "upload_part_truncated" {
			foundPart = true
		}
		if v.Reason == "install_record_invalid" {
			foundInstall = true
		}
	}
	if !foundPart || !foundInstall {
		t.Fatalf("durable facts unchecked: %+v", inv.Findings)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("bad staging was deleted", err)
	}
}

func TestUnknownPinOwnerFailsClosedAndReconcileRollsBack(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	body := []byte("retained")
	u := f.uploadAll(f.who, f.project, body)
	if _, err := f.svc.db.Exec(`DELETE FROM storage_pins WHERE owner_id=?`, u.UploadID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.db.Exec(`CREATE TRIGGER synthetic_pin_failure BEFORE INSERT ON storage_pin_blobs BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	lctx, h, err := f.inst.Gate().Maintain(t.Context(), commands.ReasonRecovering)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { h.Release(); f.inst.Gate().Open() }()
	if _, err = f.svc.ReconcilePins(lctx); err == nil {
		t.Fatal("injected transaction failure ignored")
	}
	var count int
	if err = f.svc.db.QueryRow(`SELECT COUNT(*) FROM storage_pins WHERE owner_id=?`, u.UploadID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial repair committed: %d %v", count, err)
	}
	if _, err = f.svc.db.Exec(`DROP TRIGGER synthetic_pin_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.ReconcilePins(lctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.db.Exec(`DELETE FROM storage_uploads WHERE upload_id=?`, u.UploadID); err != nil {
		t.Fatal(err)
	}
	report, err := f.svc.ReconcilePins(lctx)
	if err != nil || report.UnknownOwners != 1 {
		t.Fatalf("unknown owner lost: %+v %v", report, err)
	}
	if _, err = f.svc.PinsFor(t.Context(), shaOf(body)); err == nil {
		t.Fatal("unknown owner treated as absent retention")
	}
}
