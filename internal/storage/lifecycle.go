package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/pin"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// FileIntent is T03's durable, accepted physical plan. IDs and proofs, never
// caller-supplied host paths, determine the byte locations. Purge only unlinks
// this trash entry; shared CAS objects are handled separately by CollectBlob.
type FileIntent struct {
	OperationID ids.ID            `json:"operation_id"`
	TrashID     ids.ID            `json:"trash_id"`
	Action      string            `json:"action"`
	Versions    []install.Proof   `json:"versions"`
	Records     []LifecycleRecord `json:"records"`
}

// LifecycleRecord freezes the evidence files included in a lifecycle action.
// Empty records means no records are expected, not permission to delete unknown files.
type LifecycleRecord struct {
	VersionID ids.ID `json:"version_id"`
	RecordID  ids.ID `json:"record_id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// FileIntentSource must return only accepted intents with the write/read ban
// already durable. For pending intents the source rechecks the current lifecycle
// stage under the supplied asset/blob locks. No plugin implements this port.
type FileIntentSource interface {
	AcceptedFileIntent(context.Context, ids.ID) (FileIntent, error)
}

func (p FileIntent) validate() error {
	if !p.OperationID.Valid() || !p.TrashID.Valid() || !slices.Contains([]string{"trash", "restore", "purge"}, p.Action) || len(p.Versions) == 0 {
		return invalid("invalid accepted file plan")
	}
	seen := map[ids.ID]bool{}
	for _, v := range p.Versions {
		if _, err := v.Canonical(); err != nil {
			return err
		}
		if !digest.ValidHex(v.ManifestSHA256) || seen[v.VersionID] {
			return invalid("invalid or repeated version proof")
		}
		seen[v.VersionID] = true
	}
	recordIDs := map[ids.ID]bool{}
	for _, r := range p.Records {
		if !seen[r.VersionID] || !r.RecordID.Valid() || !digest.ValidHex(r.SHA256) || r.Size < 0 || recordIDs[r.RecordID] {
			return invalid("invalid or duplicate lifecycle record")
		}
		recordIDs[r.RecordID] = true
	}
	return nil
}
func intentDigest(p FileIntent) (digest.Digest, error) {
	b, e := canonjson.CanonicalizeValue(p)
	return digest.Of(b), e
}
func (s *Service) trashVersion(trash, version ids.ID) string {
	return filepath.Join(s.layout.Home, "trash", string(trash), "versions", string(version))
}
func (s *Service) trashRecords(trash, version ids.ID) string {
	return filepath.Join(s.layout.Home, "trash", string(trash), "records", string(version))
}

// ApplyFileIntent is an internal adapter, not a public permission-bearing API.
// Recovery calls the same method with the same operation. Each rename is synced
// on both parents; deletion resumes from a durable plan after partial unlink.
func (s *Service) ApplyFileIntent(ctx context.Context, op ids.ID, source FileIntentSource) error {
	if source == nil || !op.Valid() {
		return invalid("accepted intent source and operation required")
	}
	unlock := s.lockKey("lifecycle/" + string(op))
	defer unlock()
	plan, err := source.AcceptedFileIntent(ctx, op)
	if err != nil {
		return err
	}
	if plan.OperationID != op {
		return invalid("intent operation mismatch")
	}
	if err = plan.validate(); err != nil {
		return err
	}
	rd, err := intentDigest(plan)
	if err != nil {
		return err
	}
	req := commands.Request{}
	for _, v := range plan.Versions {
		req.Assets = append(req.Assets, string(v.AssetID))
		for _, f := range v.Files {
			req.Blobs = append(req.Blobs, f.SHA256)
		}
	}
	ctx, release, err := s.write(ctx, req)
	if err != nil {
		return err
	}
	defer release()
	fresh, err := source.AcceptedFileIntent(ctx, op)
	if err != nil {
		return err
	}
	got, err := intentDigest(fresh)
	if err != nil {
		return err
	}
	if got != rd {
		return errcode.New(errcode.IdempotencyConflict, "accepted intent changed")
	}
	var saved digest.Digest
	var state string
	err = s.db.QueryRowContext(ctx, `SELECT intent_digest,state FROM storage_file_actions WHERE operation_id=?`, op).Scan(&saved, &state)
	if err == nil {
		if saved != rd {
			return errcode.New(errcode.IdempotencyConflict, "file operation differs")
		}
		if state == "done" {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	} else {
		// Before any destructive action, establish that all exact targets are intact.
		for _, v := range plan.Versions {
			dir := s.layout.VersionDir(v.ProjectID, v.AssetID, v.VersionNumber)
			if plan.Action != "trash" {
				dir = s.trashVersion(plan.TrashID, v.VersionID)
			}
			if err = s.verifyLifecycleTree(ctx, dir, v); err != nil {
				return err
			}
			records := s.layout.recordsDir(v.ProjectID, v.AssetID, v.VersionID)
			if plan.Action != "trash" {
				records = s.trashRecords(plan.TrashID, v.VersionID)
			}
			if err = s.verifyLifecycleRecords(ctx, records, v.VersionID, plan.Records); err != nil {
				return err
			}
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO storage_file_actions VALUES (?,?,?,'ready',NULL)`, op, rd, jsonTextLifecycle(plan)); err != nil {
			return err
		}
	}
	for _, v := range plan.Versions {
		live := s.layout.VersionDir(v.ProjectID, v.AssetID, v.VersionNumber)
		trash := s.trashVersion(plan.TrashID, v.VersionID)
		records := s.layout.recordsDir(v.ProjectID, v.AssetID, v.VersionID)
		trashRecords := s.trashRecords(plan.TrashID, v.VersionID)
		switch plan.Action {
		case "trash":
			if err = s.moveLifecycleTree(ctx, live, trash, v, false, plan.Records); err == nil {
				err = s.moveLifecycleTree(ctx, records, trashRecords, v, true, plan.Records)
			}
		case "restore":
			if err = s.moveLifecycleTree(ctx, trash, live, v, false, plan.Records); err == nil {
				err = s.moveLifecycleTree(ctx, trashRecords, records, v, true, plan.Records)
			}
		case "purge":
			if err = s.removeLifecycleTree(trash); err == nil {
				err = s.removeLifecycleTree(trashRecords)
			}
		}
		if err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE storage_file_actions SET state='done',completed_at=? WHERE operation_id=?`, clock.Millis(s.now()), op)
	return err
}
func jsonTextLifecycle(v any) string { b, _ := json.Marshal(v); return string(b) }

func (s *Service) regularTree(root string, optional bool) error {
	rel, err := filepath.Rel(s.layout.Home, root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return invalid("lifecycle path escapes root")
	}
	p := s.layout.Home
	for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		p = filepath.Join(p, part)
		st, e := os.Lstat(p)
		if errors.Is(e, fs.ErrNotExist) && optional {
			return nil
		}
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errcode.New(errcode.OperationNeedsReconciliation, "lifecycle ancestor is not a directory")
		}
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
			return errcode.New(errcode.OperationNeedsReconciliation, "non-regular lifecycle tree")
		}
		return nil
	})
}
func (s *Service) verifyLifecycleTree(ctx context.Context, dir string, v install.Proof) error {
	if err := s.regularTree(dir, false); err != nil {
		return err
	}
	if _, err := s.checkTree(dir, v.Files, v.ManifestSHA256); err != nil {
		return err
	}
	expected := map[string]bool{ManifestFile: true, installMarker: true}
	for _, f := range v.Files {
		expected[filepath.Join(FilesDir, filepath.FromSlash(f.Path))] = true
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(dir, path)
		if e != nil {
			return e
		}
		if !expected[rel] {
			return errcode.New(errcode.OperationNeedsReconciliation, "unlisted file in lifecycle version")
		}
		return nil
	}); err != nil {
		return err
	}
	for _, f := range v.Files {
		h, n, e := hashMaintenanceFile(ctx, filepath.Join(dir, FilesDir, filepath.FromSlash(f.Path)))
		if e != nil {
			return e
		}
		if h != f.SHA256 || n != f.Size {
			return errcode.New(errcode.HashMismatch, "lifecycle target content differs")
		}
	}
	return nil
}
func (s *Service) moveLifecycleTree(ctx context.Context, src, dst string, v install.Proof, records bool, recordList []LifecycleRecord) error {
	if err := s.regularTree(src, true); err != nil {
		return err
	}
	if err := s.regularTree(dst, true); err != nil {
		return err
	}
	_, se := os.Lstat(src)
	_, de := os.Lstat(dst)
	if se == nil && de == nil {
		return errcode.New(errcode.OperationNeedsReconciliation, "both lifecycle locations exist")
	}
	if errors.Is(se, fs.ErrNotExist) {
		if errors.Is(de, fs.ErrNotExist) && records {
			return s.verifyLifecycleRecords(ctx, src, v.VersionID, recordList)
		}
		if de != nil {
			return errcode.New(errcode.OperationNeedsReconciliation, "lifecycle content is missing")
		}
		if !records {
			if e := s.verifyLifecycleTree(ctx, dst, v); e != nil {
				return e
			}
		} else if e := s.verifyLifecycleRecords(ctx, dst, v.VersionID, recordList); e != nil {
			return e
		}
	} else {
		if se != nil {
			return se
		}
		if !errors.Is(de, fs.ErrNotExist) {
			return de
		}
		if !records {
			if e := s.verifyLifecycleTree(ctx, src, v); e != nil {
				return e
			}
		} else if e := s.verifyLifecycleRecords(ctx, src, v.VersionID, recordList); e != nil {
			return e
		}
		if err := s.fs.MkdirAll(filepath.Dir(dst)); err != nil {
			return err
		}
		if err := s.fs.Rename(src, dst); err != nil {
			return err
		}
	}
	if err := s.fs.SyncDir(filepath.Dir(dst)); err != nil {
		return err
	}
	return s.fs.SyncDir(filepath.Dir(src))
}
func (s *Service) removeLifecycleTree(dir string) error {
	if err := s.regularTree(dir, true); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fileop.Wrap("purging accepted trash files", err)
	}
	if _, err := os.Stat(filepath.Dir(dir)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return s.fs.SyncDir(filepath.Dir(dir))
}

// ManifestRoot names an active, trash or unfinished-operation version. T03 owns
// the complete root set; index.db and filesystem link counts cannot supply it.
type ManifestRoot struct {
	Proof   install.Proof
	TrashID ids.ID
}
type GCCandidate struct {
	OperationID ids.ID
	Since       time.Time
}
type GCSource interface {
	// Candidate must fail while reference indexes are unverified/rebuilding, or
	// any unresolved operation/evidence reference makes collection uncertain.
	Candidate(context.Context, string) (GCCandidate, error)
	ManifestRoots(context.Context) ([]ManifestRoot, error)
}
type GCDependencies struct {
	Authority  GCSource
	CommitPins pin.Source
	BackupPins pin.Source
}

// ManifestReferences verifies each authoritative manifest byte digest and emits
// its frozen file references. This read-only scan can also feed T08 reconciliation.
func (s *Service) ManifestReferences(ctx context.Context, roots []ManifestRoot) (map[string]bool, error) {
	refs := map[string]bool{}
	known := map[string]bool{}
	for _, root := range roots {
		v := root.Proof
		if _, err := v.Canonical(); err != nil {
			return nil, err
		}
		dir := s.layout.VersionDir(v.ProjectID, v.AssetID, v.VersionNumber)
		if root.TrashID != "" {
			if !root.TrashID.Valid() {
				return nil, invalid("invalid trash root")
			}
			dir = s.trashVersion(root.TrashID, v.VersionID)
		}
		known[filepath.Clean(dir)] = true
		if err := s.regularTree(dir, false); err != nil {
			return nil, err
		}
		if _, err := s.checkTree(dir, v.Files, v.ManifestSHA256); err != nil {
			return nil, err
		}
		for _, f := range v.Files {
			refs[f.SHA256] = true
		}
	}
	// Fail closed on manifests/installation markers omitted from authority. This
	// catches orphaned installs and trash left by incomplete recovery, even when
	// a derived reference index reports zero references.
	for _, area := range []string{"projects", "trash", "quarantine", filepath.Join("staging", "install")} {
		root := filepath.Join(s.layout.Home, area)
		if err := s.regularTree(root, true); err != nil {
			return nil, err
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
			if errors.Is(e, fs.ErrNotExist) && path == root {
				return nil
			}
			if e != nil {
				return e
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !d.IsDir() && (d.Name() == ManifestFile || d.Name() == installMarker) && !known[filepath.Dir(path)] {
				return errcode.New(errcode.OperationNeedsReconciliation, "manifest outside the authoritative GC root set")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// CollectBlob is called by an explicitly authorized T08 scheduler after T03 has
// accepted a purge candidate. It holds the same hash lock as upload/install and
// pin creation. Nil/error sources, uncertain roots or a <24h candidate fail shut.
func (s *Service) CollectBlob(ctx context.Context, sha string, deps GCDependencies) (bool, error) {
	if !digest.ValidHex(sha) || deps.Authority == nil || deps.CommitPins == nil || deps.BackupPins == nil {
		return false, invalid("GC requires authority and all pin sources")
	}
	ctx, release, err := s.write(ctx, blobLock(sha))
	if err != nil {
		return false, err
	}
	defer release()
	var pending int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_file_actions WHERE state='ready'`).Scan(&pending); err != nil {
		return false, err
	}
	if pending != 0 {
		return false, errcode.New(errcode.OperationNeedsReconciliation, "unfinished file actions must be reconciled before GC")
	}
	c, err := deps.Authority.Candidate(ctx, sha)
	if err != nil {
		return false, err
	}
	if !c.OperationID.Valid() || c.Since.IsZero() || s.now().Before(c.Since.Add(24*time.Hour)) {
		return false, errcode.New(errcode.NotDue, "GC candidate must age for at least 24 hours")
	}
	roots, err := deps.Authority.ManifestRoots(ctx)
	if err != nil {
		return false, err
	}
	refs, err := s.ManifestReferences(ctx, roots)
	if err != nil {
		return false, err
	}
	if refs[sha] {
		return false, nil
	}
	held, err := pin.Held(ctx, sha, s.now(), s, deps.CommitPins, deps.BackupPins)
	if err != nil || held {
		return false, err
	}
	var stored string
	var at int64
	var state string
	err = s.db.QueryRowContext(ctx, `SELECT sha256,candidate_at,state FROM storage_gc_deletions WHERE operation_id=?`, c.OperationID).Scan(&stored, &at, &state)
	if err == nil {
		if stored != sha || at != clock.Millis(c.Since) {
			return false, errcode.New(errcode.IdempotencyConflict, "GC intent changed")
		}
		if state == "done" || state == "cancelled" {
			return false, nil
		} // A later upload of this hash is a new object lifetime.
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	} else {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO storage_gc_deletions VALUES (?,?,?,'deleting',NULL)`, c.OperationID, sha, clock.Millis(c.Since)); err != nil {
			return false, err
		}
	}
	path := s.layout.BlobPath(sha)
	if err = regularPath(s.layout.Home, path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fileop.Wrap("collecting unreferenced content", err)
	}
	if _, err = os.Stat(filepath.Dir(path)); err == nil {
		if err = s.fs.SyncDir(filepath.Dir(path)); err != nil {
			return false, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE storage_gc_deletions SET state='done',completed_at=? WHERE operation_id=?`, clock.Millis(s.now()), c.OperationID)
	return err == nil, err
}

// PendingFileActions exposes only storage-owned physical progress to T08. The
// caller must replay ApplyFileIntent through T03; this is not a business receipt.
func (s *Service) PendingFileActions(ctx context.Context) ([]FileIntent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT intent_json,intent_digest FROM storage_file_actions WHERE state='ready' ORDER BY operation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileIntent{}
	for rows.Next() {
		var raw string
		var expected digest.Digest
		if err = rows.Scan(&raw, &expected); err != nil {
			return nil, err
		}
		var p FileIntent
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		if err = p.validate(); err != nil {
			return nil, err
		}
		got, e := intentDigest(p)
		if e != nil {
			return nil, e
		}
		if got != expected {
			return nil, errcode.New(errcode.OperationNeedsReconciliation, "file plan digest changed")
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type PendingGC struct {
	SHA256    string
	Candidate GCCandidate
}

func (s *Service) PendingGC(ctx context.Context) ([]PendingGC, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id,sha256,candidate_at FROM storage_gc_deletions WHERE state='deleting' ORDER BY operation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingGC{}
	for rows.Next() {
		var p PendingGC
		var at int64
		if err = rows.Scan(&p.Candidate.OperationID, &p.SHA256, &at); err != nil {
			return nil, err
		}
		p.Candidate.Since = clock.FromMillis(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) verifyLifecycleRecords(ctx context.Context, root string, version ids.ID, records []LifecycleRecord) error {
	expected := map[string]LifecycleRecord{}
	for _, r := range records {
		if r.VersionID == version {
			expected[string(r.RecordID)+".json"] = r
		}
	}
	if err := s.regularTree(root, len(expected) == 0); err != nil {
		return err
	}
	found := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if errors.Is(e, fs.ErrNotExist) && path == root && len(expected) == 0 {
			return nil
		}
		if e != nil {
			return e
		}
		if d.IsDir() {
			if path != root {
				return errcode.New(errcode.OperationNeedsReconciliation, "unexpected evidence subdirectory")
			}
			return nil
		}
		r, ok := expected[d.Name()]
		if !ok {
			return errcode.New(errcode.OperationNeedsReconciliation, "evidence not in accepted lifecycle list")
		}
		hash, n, e := hashMaintenanceFile(ctx, path)
		if e != nil {
			return e
		}
		if hash != r.SHA256 || n != r.Size {
			return errcode.New(errcode.HashMismatch, "lifecycle evidence differs")
		}
		found++
		return nil
	})
	if err != nil {
		return err
	}
	if found != len(expected) {
		return errcode.New(errcode.OperationNeedsReconciliation, "accepted lifecycle evidence missing")
	}
	return nil
}

// SnapshotLifecycleRecords is an internal T03 planning port. The caller must
// hold the security writer guard and all target asset locks until its intent and
// write ban are committed. It inventories even unaccepted evidence files: they
// are physical bytes to retain, not authority to use them as business evidence.
func (s *Service) SnapshotLifecycleRecords(ctx context.Context, proofs []install.Proof) ([]LifecycleRecord, error) {
	out := []LifecycleRecord{}
	seen := map[ids.ID]bool{}
	for _, proof := range proofs {
		if seen[proof.VersionID] {
			return nil, invalid("repeated lifecycle version")
		}
		seen[proof.VersionID] = true
		if _, err := proof.Canonical(); err != nil {
			return nil, err
		}
		v, err := s.ledger.Version(ctx, proof.AssetID, proof.VersionID)
		if err != nil {
			return nil, err
		}
		pd, err := proof.Digest()
		if err != nil {
			return nil, err
		}
		if v.ProjectID != proof.ProjectID || v.ManifestDigest != proof.ManifestDigest || v.ProofDigest != pd {
			return nil, errcode.New(errcode.RefMismatch, "lifecycle proof is not the committed proof")
		}
		if err = s.verifyLifecycleTree(ctx, s.layout.VersionDir(proof.ProjectID, proof.AssetID, proof.VersionNumber), proof); err != nil {
			return nil, err
		}
		dir := s.layout.recordsDir(proof.ProjectID, proof.AssetID, proof.VersionID)
		if err = s.regularTree(dir, true); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fileop.Wrap("reading lifecycle records", err)
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				return nil, errcode.New(errcode.OperationNeedsReconciliation, "unknown file in version evidence directory")
			}
			id := ids.ID(strings.TrimSuffix(entry.Name(), ".json"))
			if !id.Valid() {
				return nil, errcode.New(errcode.OperationNeedsReconciliation, "invalid evidence filename")
			}
			raw, err := s.ReadRecord(ctx, proof.ProjectID, proof.AssetID, proof.VersionID, id)
			if err != nil {
				return nil, err
			}
			var record Record
			if err = json.Unmarshal(raw, &record); err != nil {
				return nil, err
			}
			if record.RecordID != id || record.VersionID != proof.VersionID || record.AssetID != proof.AssetID || record.ProjectID != proof.ProjectID || record.ManifestDigest != proof.ManifestDigest {
				return nil, errcode.New(errcode.RefMismatch, "evidence identity differs from its location")
			}
			out = append(out, LifecycleRecord{VersionID: proof.VersionID, RecordID: id, SHA256: digest.Of(raw).Hex(), Size: int64(len(raw))})
		}
	}
	slices.SortFunc(out, func(a, b LifecycleRecord) int {
		if a.VersionID != b.VersionID {
			return strings.Compare(string(a.VersionID), string(b.VersionID))
		}
		return strings.Compare(string(a.RecordID), string(b.RecordID))
	})
	return out, nil
}

// GCState reports storage's physical progress for one candidate operation:
// "" (never started), "deleting", "done" or "cancelled". T08 uses it only to
// stop rescheduling finished candidates; collection itself always rechecks.
func (s *Service) GCState(ctx context.Context, op ids.ID) (string, error) {
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT state FROM storage_gc_deletions WHERE operation_id=?`, op).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return state, err
}
