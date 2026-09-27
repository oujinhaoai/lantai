package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type FileEntry struct {
	Ref           string        `json:"ref"`
	Kind          string        `json:"kind"`
	Digest        digest.Digest `json:"digest"`
	Authoritative bool          `json:"authoritative"`
}
type FileReport struct {
	Checked  int               `json:"checked"`
	Files    []FileEntry       `json:"files"`
	Findings []storage.Finding `json:"findings"`
}

// CheckFiles checks immutable description history, derived snapshots, frozen
// intents and alias history. The metadata list is supplied by ledger, including
// historical committed revisions; an unaccepted file remains only a residual.
// Evidence files are checked by storage/provenance, their respective owners.
func (s *Service) CheckFiles(ctx context.Context, metadata []commit.CommittedMetadata) (FileReport, error) {
	var out FileReport
	add := func(path, reason string, op ids.ID, code errcode.Code) {
		out.Findings = append(out.Findings, storage.Finding{Code: code, Reason: reason, Ref: s.ref(path), OperationID: op})
	}
	expected := map[string]commit.CommittedMetadata{}
	latest := map[commit.MetadataTarget]commit.CommittedMetadata{}
	for _, m := range metadata {
		path := s.revisionPath(m.Target, m.Revision, m.OperationID)
		expected[path] = m
		if old := latest[m.Target]; m.Revision > old.Revision {
			latest[m.Target] = m
		}
		b, err := s.readMaintenanceFile(path)
		out.Checked++
		if err != nil {
			add(path, "revision_missing_or_unsafe", m.OperationID, errcode.OperationNeedsReconciliation)
			continue
		}
		if digest.Of(b) != m.ContentDigest {
			add(path, "revision_digest", m.OperationID, errcode.HashMismatch)
			continue
		}
		contract := AssetContract
		if m.Target.Kind == commit.TargetProject {
			contract = ProjectContract
		}
		if _, err = manifest.Decode(b, contract); err != nil {
			add(path, "revision_schema", m.OperationID, errcode.SchemaInvalid)
			continue
		}
		if m.Target.Kind == commit.TargetProject {
			var d ProjectDescription
			if parseInto(b, contract, &d) != nil || d.ProjectID != m.Target.ID || d.ProjectID != m.Target.ProjectID || d.Revision != m.Revision || d.UpdatedBy != m.CommittedBy {
				add(path, "revision_identity", m.OperationID, errcode.OperationNeedsReconciliation)
			}
		} else {
			var d AssetDescription
			if parseInto(b, contract, &d) != nil || d.AssetID != m.Target.ID || d.ProjectID != m.Target.ProjectID || d.Revision != m.Revision || d.UpdatedBy != m.CommittedBy {
				add(path, "revision_identity", m.OperationID, errcode.OperationNeedsReconciliation)
			}
		}
	}
	for target, m := range latest {
		path := s.snapshotPath(target)
		b, err := s.readMaintenanceFile(path)
		if err != nil || digest.Of(b) != m.ContentDigest {
			add(path, "snapshot_stale", m.OperationID, errcode.OperationNeedsReconciliation)
		}
	}
	for _, root := range []string{filepath.Join(s.home, "projects"), filepath.Join(s.home, "staging", "frozen")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			if err != nil {
				if path == root && errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				add(path, "unreadable", "", errcode.StorageUnavailable)
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
				add(path, "non_regular", "", errcode.OperationNeedsReconciliation)
				return nil
			}
			if d.IsDir() {
				return nil
			}
			kind := ""
			if strings.HasPrefix(path, filepath.Join(s.home, "staging", "frozen")+string(filepath.Separator)) {
				kind = "frozen"
			} else if filepath.Base(filepath.Dir(path)) == ".history" && (strings.HasPrefix(d.Name(), "asset.r") || strings.HasPrefix(d.Name(), "project.r")) {
				kind = "revision"
			}
			if kind == "" {
				return nil
			}
			b, e := s.readMaintenanceFile(path)
			if e != nil {
				add(path, "unreadable", "", errcode.StorageUnavailable)
				return nil
			}
			_, authority := expected[path]
			out.Files = append(out.Files, FileEntry{Ref: s.ref(path), Kind: kind, Digest: digest.Of(b), Authoritative: authority})
			if kind == "frozen" {
				id := ids.ID(filepath.Base(filepath.Dir(path)))
				var c manifest.Content
				e = json.Unmarshal(b, &c)
				md, de := c.Digest()
				if e != nil || de != nil || !id.Valid() || d.Name() != md.Hex()+".json" || digest.Of(b) != md {
					add(path, "frozen_digest_or_identity", id, errcode.HashMismatch)
				}
			}
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	for after := ids.ID(""); ; {
		versions, err := s.ledger.Versions(ctx, after, 500)
		if err != nil {
			return out, err
		}
		for _, v := range versions {
			if v.AliasGeneration == 0 {
				continue
			}
			a, err := s.ledger.Asset(ctx, v.AssetID)
			if err != nil {
				return out, err
			}
			path := s.aliasPath(a.ProjectID, a.Slug, a.Generation)
			out.Checked++
			if _, err = s.readMaintenanceFile(path); err != nil {
				add(path, "alias_missing_or_unsafe", a.OperationID, errcode.OperationNeedsReconciliation)
				continue
			}
			if _, err = s.aliasRecord(ctx, a.ProjectID, a.Slug, a.Generation); err != nil {
				add(path, "alias_mismatch", a.OperationID, errcode.OperationNeedsReconciliation)
			}
		}
		if len(versions) < 500 {
			break
		}
		after = versions[len(versions)-1].VersionID
	}
	return out, nil
}

// RepairSnapshots republishes only the currently committed, verified revision.
// A stale snapshot is derived data and never a source for repairing history.
func (s *Service) RepairSnapshots(ctx context.Context, metadata []commit.CommittedMetadata) (RepairReport, error) {
	var out RepairReport
	if err := s.gate.RequireMaintenance(ctx); err != nil {
		return out, err
	}
	seen := map[commit.MetadataTarget]bool{}
	for _, m := range metadata {
		if seen[m.Target] {
			continue
		}
		seen[m.Target] = true
		cur, err := s.ledger.CurrentMetadata(ctx, m.Target)
		if err != nil {
			return out, err
		}
		b, err := s.readMaintenanceFile(s.revisionPath(cur.Target, cur.Revision, cur.OperationID))
		if err != nil {
			return out, err
		}
		if digest.Of(b) != cur.ContentDigest {
			return out, errcode.New(errcode.HashMismatch, "current revision digest changed")
		}
		out.Checked++
		old, readErr := s.readMaintenanceFile(s.snapshotPath(cur.Target))
		if readErr == nil && digest.Of(old) == cur.ContentDigest {
			continue
		}
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			return out, readErr
		}
		lctx, release, err := s.write(ctx)
		if err != nil {
			return out, err
		}
		err = s.fs.ReplaceAtomic(s.snapshotPath(cur.Target), b, 0o644)
		release()
		if err != nil {
			return out, err
		}
		if err = lctx.Err(); err != nil {
			return out, err
		}
		out.Written++
	}
	return out, nil
}

// RecoverVersion resumes exactly the frozen reservation; final acceptance still
// verifies current permission, inputs, grant bindings and original recovery epoch.
func (s *Service) RecoverVersion(ctx context.Context, who authz.Context, p commit.Prepared) (commit.Committed, error) {
	if err := s.gate.RequireMaintenance(ctx); err != nil {
		return commit.Committed{}, err
	}
	if _, err := s.readMaintenanceFile(s.frozenPath(p.OperationID, p.ManifestDigest)); err != nil {
		return commit.Committed{}, errcode.New(errcode.OperationNeedsReconciliation, "frozen content is missing or unsafe")
	}
	c, err := s.thaw(p.OperationID, p.ManifestDigest)
	if err != nil {
		return commit.Committed{}, err
	}
	return s.installAndCommit(ctx, who, p, p.ManifestDigest, c)
}

// RecoverMetadata can only finish an already written, matching revision. Missing
// files cannot be recreated from the reservation's digest or a current snapshot.
func (s *Service) RecoverMetadata(ctx context.Context, who authz.Context, p commit.PreparedMetadata) (commit.CommittedMetadata, error) {
	if err := s.gate.RequireMaintenance(ctx); err != nil {
		return commit.CommittedMetadata{}, err
	}
	path := s.revisionPath(p.Target, p.Revision, p.OperationID)
	b, err := s.readMaintenanceFile(path)
	if err != nil {
		return commit.CommittedMetadata{}, errcode.New(errcode.OperationNeedsReconciliation, "the prepared revision is missing or unsafe")
	}
	if digest.Of(b) != p.ContentDigest {
		return commit.CommittedMetadata{}, errcode.New(errcode.HashMismatch, "the prepared revision digest changed")
	}
	proof := commit.RevisionProof{OperationID: p.OperationID, Target: p.Target, Revision: p.Revision, ContentDigest: p.ContentDigest, FileRef: s.ref(path), WrittenAt: s.now()}
	m, err := s.ledger.CommitMetadata(ctx, p.OperationID, who, proof)
	if err == nil {
		s.publishSnapshot(ctx, p.Target, m, b)
	}
	return m, err
}

func (s *Service) readMaintenanceFile(path string) ([]byte, error) {
	rel, err := filepath.Rel(s.home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errcode.New(errcode.OperationNeedsReconciliation, "path escapes data root")
	}
	for p := path; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if st.Mode()&os.ModeSymlink != 0 || (p == path && !st.Mode().IsRegular()) {
			return nil, errcode.New(errcode.OperationNeedsReconciliation, "unsafe maintenance file")
		}
		if p == s.home {
			break
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = 16 << 20
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(b) > limit {
		return nil, errcode.New(errcode.QuotaExceeded, "maintenance file exceeds size limit")
	}
	return b, err
}
