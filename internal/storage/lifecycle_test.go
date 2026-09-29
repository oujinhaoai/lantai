package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/pin"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

type intentSource struct {
	plan FileIntent
	err  error
}

func (i *intentSource) AcceptedFileIntent(context.Context, ids.ID) (FileIntent, error) {
	return i.plan, i.err
}

type gcSource struct {
	candidate GCCandidate
	roots     []ManifestRoot
	err       error
}

func (g *gcSource) Candidate(context.Context, string) (GCCandidate, error) { return g.candidate, g.err }
func (g *gcSource) ManifestRoots(context.Context) ([]ManifestRoot, error)  { return g.roots, g.err }

type testPins struct {
	pins []pin.Pin
	err  error
}

func (p testPins) PinsFor(context.Context, string) ([]pin.Pin, error) { return p.pins, p.err }

func TestLifecycleMovesResumeAndPurgePreservesSharedBlob(t *testing.T) {
	faults := &fileop.Faults{}
	f := newFixture(t, testConfig(), faults)
	ctx := t.Context()
	body := []byte("shared blob")
	one := f.commitVersion(f.who, f.project, "", "one", "", map[string][]byte{"a.txt": body})
	two := f.commitVersion(f.who, f.project, "", "two", "", map[string][]byte{"a.txt": body})
	row, err := f.svc.installRow(ctx, f.svc.db, one.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := f.svc.proofOf(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	recordID := ids.New()
	recordBytes := []byte(`{"evidence":"synthetic"}`)
	recordDir := f.svc.layout.recordsDir(one.ProjectID, one.AssetID, one.VersionID)
	if err = os.MkdirAll(recordDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(recordDir, string(recordID)+".json"), recordBytes, 0600); err != nil {
		t.Fatal(err)
	}
	source := &intentSource{plan: FileIntent{OperationID: ids.New(), TrashID: ids.New(), Action: "trash", Versions: []install.Proof{proof}, Records: []LifecycleRecord{{VersionID: one.VersionID, RecordID: recordID, SHA256: shaOf(recordBytes), Size: int64(len(recordBytes))}}}}
	// Rename completed, parent sync failed: recovery must recognize the destination.
	fail := true
	faults.Sync = func(path string) error {
		if fail && strings.Contains(path, string(source.plan.TrashID)) && filepath.Base(path) == "versions" {
			if _, e := os.Stat(f.svc.trashVersion(source.plan.TrashID, one.VersionID)); e == nil {
				fail = false
				return errors.New("injected sync")
			}
		}
		return nil
	}
	if err = f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); err == nil {
		t.Fatal("expected fault")
	}
	f.restart()
	faults.Sync = nil
	if err = f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); err != nil {
		t.Fatal(err)
	}
	source.plan.OperationID = ids.New()
	source.plan.Action = "restore"
	if err = f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.VerifyDeep(ctx, one.OperationID); err != nil {
		t.Fatal(err)
	}
	source.plan.OperationID = ids.New()
	source.plan.Action = "trash"
	source.plan.TrashID = ids.New()
	if err = f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); err != nil {
		t.Fatal(err)
	}
	source.plan.OperationID = ids.New()
	source.plan.Action = "purge"
	if err = f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.VerifyDeep(ctx, two.OperationID); err != nil {
		t.Fatal("shared live version damaged", err)
	}
	if _, err = os.Stat(f.svc.layout.BlobPath(shaOf(body))); err != nil {
		t.Fatal("purge deleted CAS", err)
	}
	source.err = errcode.New(errcode.Forbidden, "not accepted")
	if err = f.svc.ApplyFileIntent(ctx, ids.New(), source); err == nil {
		t.Fatal("unaccepted plan allowed")
	}
}
func TestGCRequiresAgeRootsAndAllPins(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	ctx := t.Context()
	body := []byte("gc content")
	v := f.commitVersion(f.who, f.project, "", "one", "", map[string][]byte{"a.txt": body})
	row, _ := f.svc.installRow(ctx, f.svc.db, v.OperationID)
	proof, _ := f.svc.proofOf(ctx, row)
	a := &gcSource{candidate: GCCandidate{OperationID: ids.New(), Since: f.clk.Now()}}
	deps := GCDependencies{Authority: a, CommitPins: testPins{}, BackupPins: testPins{}}
	if _, e := f.svc.CollectBlob(ctx, shaOf(body), deps); errcode.CodeOf(e) != errcode.NotDue {
		t.Fatal(e)
	}
	f.clk.Advance(8 * 24 * time.Hour)
	a.roots = []ManifestRoot{{Proof: proof}}
	if deleted, e := f.svc.CollectBlob(ctx, shaOf(body), deps); deleted || e != nil {
		t.Fatal(deleted, e)
	}
	// A corrupt manifest must stop GC even when it would not reference the hash.
	path := filepath.Join(f.svc.layout.VersionDir(v.ProjectID, v.AssetID, v.VersionNumber), ManifestFile)
	if e := os.Chmod(path, 0600); e != nil {
		t.Fatal(e)
	}
	original, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	if deleted, e := f.svc.CollectBlob(ctx, shaOf(body), deps); deleted || e == nil {
		t.Fatal("corrupt root allowed collection")
	}
	if e = os.WriteFile(path, original, 0600); e != nil {
		t.Fatal(e)
	}
	// T03 confirms the version purged, no live roots remain. Backup pin still wins.
	source := &intentSource{plan: FileIntent{OperationID: ids.New(), TrashID: ids.New(), Action: "trash", Versions: []install.Proof{proof}}}
	if e = f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); e != nil {
		t.Fatal(e)
	}
	source.plan.Action = "purge"
	source.plan.OperationID = ids.New()
	if e = f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); e != nil {
		t.Fatal(e)
	}
	a.roots = nil
	deps.BackupPins = testPins{pins: []pin.Pin{{Blobs: []string{shaOf(body)}}}}
	if deleted, e := f.svc.CollectBlob(ctx, shaOf(body), deps); deleted || e != nil {
		t.Fatal(deleted, e)
	}
	deps.BackupPins = testPins{err: errors.New("backup state unavailable")}
	if deleted, e := f.svc.CollectBlob(ctx, shaOf(body), deps); deleted || e == nil {
		t.Fatal("uncertain pins allowed GC")
	}
	deps.BackupPins = testPins{}
	if deleted, e := f.svc.CollectBlob(ctx, shaOf(body), deps); !deleted || e != nil {
		t.Fatal(deleted, e)
	}
	if _, e = os.Stat(f.svc.layout.BlobPath(shaOf(body))); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if deleted, e := f.svc.CollectBlob(ctx, shaOf(body), deps); deleted || e != nil {
		t.Fatal(deleted, e)
	}
}
func TestLifecycleRejectsSymlinkAncestor(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	ctx := t.Context()
	body := []byte("safe")
	v := f.commitVersion(f.who, f.project, "", "one", "", map[string][]byte{"a.txt": body})
	row, _ := f.svc.installRow(ctx, f.svc.db, v.OperationID)
	proof, _ := f.svc.proofOf(ctx, row)
	outside := t.TempDir()
	trash := filepath.Join(f.svc.layout.Home, "trash")
	if err := os.Symlink(outside, trash); err != nil {
		t.Skip(err)
	}
	source := &intentSource{plan: FileIntent{OperationID: ids.New(), TrashID: ids.New(), Action: "trash", Versions: []install.Proof{proof}}}
	if err := f.svc.ApplyFileIntent(ctx, source.plan.OperationID, source); err == nil {
		t.Fatal("symlink accepted")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("escaped data root")
	}
}

func TestGCRejectsOmittedManifestAndCancelledOldDeletion(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	ctx := t.Context()
	body := []byte("keep")
	v := f.commitVersion(f.who, f.project, "", "one", "", map[string][]byte{"a.txt": body})
	f.clk.Advance(8 * 24 * time.Hour)
	a := &gcSource{candidate: GCCandidate{OperationID: ids.New(), Since: f.clk.Now().Add(-25 * time.Hour)}}
	deps := GCDependencies{Authority: a, CommitPins: testPins{}, BackupPins: testPins{}}
	if _, e := f.svc.CollectBlob(ctx, shaOf(body), deps); errcode.CodeOf(e) != errcode.OperationNeedsReconciliation {
		t.Fatal("omitted root not rejected", e)
	}
	row, _ := f.svc.installRow(ctx, f.svc.db, v.OperationID)
	proof, _ := f.svc.proofOf(ctx, row)
	src := &intentSource{plan: FileIntent{OperationID: ids.New(), TrashID: ids.New(), Action: "trash", Versions: []install.Proof{proof}}}
	if e := f.svc.ApplyFileIntent(ctx, src.plan.OperationID, src); e != nil {
		t.Fatal(e)
	}
	src.plan.OperationID = ids.New()
	src.plan.Action = "purge"
	if e := f.svc.ApplyFileIntent(ctx, src.plan.OperationID, src); e != nil {
		t.Fatal(e)
	}
	// Model a crash after deletion intent, then a verified same-hash upload.
	if _, e := f.svc.db.Exec(`INSERT INTO storage_gc_deletions VALUES (?,?,?,'deleting',NULL)`, a.candidate.OperationID, shaOf(body), a.candidate.Since.UnixMilli()); e != nil {
		t.Fatal(e)
	}
	temp := filepath.Join(t.TempDir(), "upload")
	if e := os.WriteFile(temp, body, 0600); e != nil {
		t.Fatal(e)
	}
	locked, release, e := f.svc.write(ctx, blobLock(shaOf(body)))
	if e != nil {
		t.Fatal(e)
	}
	_ = locked
	if _, e = f.svc.placeBlob(temp, shaOf(body), int64(len(body))); e != nil {
		t.Fatal(e)
	}
	release()
	if deleted, e := f.svc.CollectBlob(ctx, shaOf(body), deps); deleted || e != nil {
		t.Fatal("old intent deleted new content", deleted, e)
	}
	if _, e := os.Stat(f.svc.layout.BlobPath(shaOf(body))); e != nil {
		t.Fatal(e)
	}
}

type blockingGC struct {
	gcSource
	entered chan struct{}
	resume  chan struct{}
}

func (b *blockingGC) Candidate(context.Context, string) (GCCandidate, error) {
	close(b.entered)
	<-b.resume
	return b.candidate, nil
}
func TestGCSerializesNewSameHashUpload(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	ctx := t.Context()
	body := []byte("concurrent content")
	f.uploadAll(f.who, f.project, body)
	f.clk.Advance(8 * 24 * time.Hour)
	authority := &blockingGC{gcSource: gcSource{candidate: GCCandidate{OperationID: ids.New(), Since: f.clk.Now().Add(-25 * time.Hour)}}, entered: make(chan struct{}), resume: make(chan struct{})}
	gcDone := make(chan error, 1)
	go func() {
		_, e := f.svc.CollectBlob(ctx, shaOf(body), GCDependencies{Authority: authority, CommitPins: testPins{}, BackupPins: testPins{}})
		gcDone <- e
	}()
	<-authority.entered
	temp := filepath.Join(t.TempDir(), "incoming")
	if e := os.WriteFile(temp, body, 0600); e != nil {
		t.Fatal(e)
	}
	writerStarted := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		close(writerStarted)
		_, release, e := f.svc.write(ctx, blobLock(shaOf(body)))
		if e != nil {
			writerDone <- e
			return
		}
		defer release()
		_, e = f.svc.placeBlob(temp, shaOf(body), int64(len(body)))
		writerDone <- e
	}()
	<-writerStarted
	select {
	case e := <-writerDone:
		t.Fatal("upload bypassed GC hash lock", e)
	case <-time.After(20 * time.Millisecond):
	}
	close(authority.resume)
	if e := <-gcDone; e != nil {
		t.Fatal(e)
	}
	if e := <-writerDone; e != nil {
		t.Fatal(e)
	}
	if e := f.svc.VerifyBlob(ctx, shaOf(body)); e != nil {
		t.Fatal("new upload lost", e)
	}
}

func TestLifecycleRecordSnapshotIncludesOrphansAndRejectsUnknownFiles(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	ctx := t.Context()
	v := f.commitVersion(f.who, f.project, "", "snapshot", "", map[string][]byte{"a.txt": []byte("synthetic")})
	row, err := f.svc.installRow(ctx, f.svc.db, v.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := f.svc.proofOf(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := f.svc.SnapshotLifecycleRecords(ctx, []install.Proof{proof})
	if err != nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
	rec := Record{RecordID: ids.New(), ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, ManifestDigest: v.ManifestDigest, Kind: "rights_evidence", Payload: []byte(`{"note":"unaccepted synthetic evidence"}`), AuthorID: f.who.PrincipalID, CreatedAt: clock.Format(f.clk.Now())}
	installed, err := f.svc.AppendRecord(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.svc.SnapshotLifecycleRecords(ctx, []install.Proof{proof})
	if err != nil || len(records) != 1 || records[0].RecordID != rec.RecordID || records[0].SHA256 != installed.Digest.Hex() {
		t.Fatal(records, err)
	}
	rogue := filepath.Join(f.svc.layout.recordsDir(v.ProjectID, v.AssetID, v.VersionID), "not-a-record.txt")
	if err = os.WriteFile(rogue, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.SnapshotLifecycleRecords(ctx, []install.Proof{proof}); errcode.CodeOf(err) != errcode.OperationNeedsReconciliation {
		t.Fatal(err)
	}
}
