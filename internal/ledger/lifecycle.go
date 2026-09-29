package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type LifecycleFiles interface {
	SnapshotLifecycleRecords(context.Context, []install.Proof) ([]storage.LifecycleRecord, error)
	ApplyFileIntent(context.Context, ids.ID, storage.FileIntentSource) error
}

// LifecycleUse is a fixed task/reference impact. The owning T05/provenance port
// supplies current revision/digest and must authorize disclosure to the caller.
// An empty list is a positive assertion that all relevant authority was scanned.
type LifecycleUse struct {
	Kind     string            `json:"kind"`
	ID       ids.ID            `json:"id"`
	Revision int64             `json:"revision"`
	Ref      *ids.PermanentRef `json:"ref,omitempty"`
	Digest   digest.Digest     `json:"digest"`
}
type LifecycleUses interface {
	CurrentUses(context.Context, authz.Context, ids.ID, []ids.PermanentRef) ([]LifecycleUse, error)
}
type Lifecycle struct {
	ledger   *Service
	files    LifecycleFiles
	uses     LifecycleUses
	rights   rights.RiskReader
	policies ReviewPolicies
	aliases  LifecycleAliases
}

type LifecycleAliases interface{ RepairLifecycleAliases(context.Context) error }

func (s *Service) NewLifecycle(files LifecycleFiles, uses LifecycleUses, rights rights.RiskReader, policies ReviewPolicies, aliases ...LifecycleAliases) (*Lifecycle, error) {
	if files == nil || uses == nil || rights == nil || policies == nil {
		return nil, errors.New("ledger: lifecycle requires file, use/task, rights and policy authorities")
	}
	if len(aliases) > 1 {
		return nil, errors.New("ledger: at most one alias writer")
	}
	out := &Lifecycle{ledger: s, files: files, uses: uses, rights: rights, policies: policies}
	if len(aliases) == 1 {
		out.aliases = aliases[0]
	}
	return out, nil
}

// TrashSelector chooses one fixed version or an entire asset. Directory requests
// must be expanded by the core into whole-asset selectors before confirmation;
// Directory remains bound in each request so grace can never bypass confirmation.
type TrashSelector struct {
	ProjectID  ids.ID `json:"project_id"`
	AssetID    ids.ID `json:"asset_id"`
	VersionID  ids.ID `json:"version_id,omitempty"`
	WholeAsset bool   `json:"whole_asset"`
	Directory  string `json:"directory,omitempty"`
	Reason     string `json:"reason"`
}
type TrashVersionTarget struct {
	VersionID      ids.ID        `json:"version_id"`
	Revision       int64         `json:"revision"`
	ManifestDigest digest.Digest `json:"manifest_digest"`
}

// TrashHistoryTarget binds a version already removed by a different lifecycle
// operation. It is not part of this request's physical move or retention clock.
type TrashHistoryTarget struct {
	TrashVersionTarget
	Lifecycle string `json:"lifecycle"`
	TrashID   ids.ID `json:"trash_id"`
}

// TrashRequest is generated from current authority, displayed to the human and
// included in the exact grant. Final acceptance reconstructs and compares it.
type TrashRequest struct {
	TrashSelector
	AssetRevision int64                `json:"asset_revision"`
	Targets       []TrashVersionTarget `json:"targets"`
	History       []TrashHistoryTarget `json:"history,omitempty"`
	FilesDigest   digest.Digest        `json:"files_digest"`
	Uses          []LifecycleUse       `json:"uses"`
}
type trashSnapshot struct {
	request TrashRequest
	proofs  []install.Proof
	records []storage.LifecycleRecord
	facts   trashFacts
	asset   commit.Asset
}

func (in TrashSelector) validate() error {
	if !in.ProjectID.Valid() || !in.AssetID.Valid() || in.WholeAsset == (in.VersionID != "") || in.VersionID != "" && !in.VersionID.Valid() || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 {
		return invalid("one version or whole asset, project and reason required")
	}
	return nil
}

// PreviewTrash inventories exact byte and impact sets under the same lock order
// used by acceptance. It returns no deletion authority and does not move files.
func (l *Lifecycle) PreviewTrash(ctx context.Context, who authz.Context, in TrashSelector) (TrashRequest, error) {
	if err := in.validate(); err != nil {
		return TrashRequest{}, err
	}
	ctx, held, err := l.ledger.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}, Assets: []string{string(in.AssetID)}})
	if err != nil {
		return TrashRequest{}, err
	}
	defer held.Release()
	snapshot, err := l.snapshot(ctx, who, in)
	return snapshot.request, err
}
func (l *Lifecycle) snapshot(ctx context.Context, who authz.Context, in TrashSelector) (trashSnapshot, error) {
	var out trashSnapshot
	s := l.ledger
	if err := in.validate(); err != nil {
		return out, err
	}
	if err := s.authorize(ctx, who, "catalog.read", in.ProjectID, "asset", in.AssetID); err != nil {
		return out, err
	}
	a, err := s.Asset(ctx, in.AssetID)
	if err != nil {
		return out, err
	}
	if a.ProjectID != in.ProjectID {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	if in.Directory != "" {
		if err := pathrule.CheckSlug(in.Directory); err != nil {
			return out, err
		}
		key, dir := pathrule.Key(a.Slug), pathrule.Key(in.Directory)
		if key != dir && !strings.HasPrefix(key, dir+"/") {
			return out, errcode.New(errcode.RefMismatch, "asset is outside confirmed directory")
		}
		if !in.WholeAsset {
			return out, invalid("directory deletion requires whole asset targets")
		}
	}
	out.asset = a
	control, err := s.AssetControl(ctx, in.AssetID)
	if err != nil {
		return out, err
	}
	if control.PendingOperationID != "" || control.Lifecycle == "trashed" || control.Lifecycle == "purged" {
		return out, errcode.New(errcode.InvalidStateTransition, "asset is not available for deletion")
	}
	if err = s.checkPathLock(ctx, a.ProjectID, a.AssetID, a.Slug); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM ledger_versions WHERE asset_id=? ORDER BY version_number`, a.AssetID)
	if err != nil {
		return out, err
	}
	versions := []commit.Committed{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return out, err
		}
		var v commit.Committed
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			rows.Close()
			return out, err
		}
		versions = append(versions, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(versions) == 0 {
		return out, errcode.New(errcode.NotFound, "")
	}
	policies, err := l.policies.ResolvePolicies(ctx, a.ProjectID)
	if err != nil {
		return out, err
	}
	var days int
	if err = json.Unmarshal(policies["trash.retention_days"].Value, &days); err != nil || days < 7 || days > 365 {
		return out, errcode.New(errcode.OperationNeedsReconciliation, "invalid trash retention policy")
	}
	out.facts = trashFacts{actor: who, latestCommitted: versions[len(versions)-1].CommittedAt, firstCommitted: versions[0].CommittedAt, wholeAsset: in.WholeAsset, directory: in.Directory != "", retentionDays: days, now: s.clock.Now()}
	out.request = TrashRequest{TrashSelector: in, AssetRevision: control.Revision, Targets: []TrashVersionTarget{}, Uses: []LifecycleUse{}}
	out.proofs = []install.Proof{}
	refs := []ids.PermanentRef{}
	for _, v := range versions {
		if !in.WholeAsset && v.VersionID != in.VersionID {
			continue
		}
		st, err := s.VersionControl(ctx, v.VersionID)
		if err != nil {
			return out, err
		}
		historical := st.Lifecycle == "trashed" || st.Lifecycle == "purged"
		if st.PendingOperationID != "" || (!historical && st.Lifecycle != "active" && st.Lifecycle != "archived") || historical && !in.WholeAsset {
			return out, errcode.New(errcode.InvalidStateTransition, "selected version is not available")
		}
		var reviewed, approved int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN json_extract(record,'$.verdict')='approve' THEN 1 ELSE 0 END),0) FROM ledger_reviews WHERE version_id=?`, v.VersionID).Scan(&reviewed, &approved); err != nil {
			return out, err
		}
		out.facts.versions = append(out.facts.versions, trashVersionFacts{id: v.VersionID, author: v.CommittedBy, committed: v.CommittedAt, everApproved: approved > 0 || st.ReviewState == "approved", humanReviewed: reviewed > 0 || st.EffectiveReviewID != "", lifecycle: st.Lifecycle, published: control.PublicationState == "published" && control.PublishedVersionID == v.VersionID})
		refs = append(refs, v.Ref(who.InstanceID))
		target := TrashVersionTarget{VersionID: v.VersionID, Revision: st.Revision, ManifestDigest: v.ManifestDigest}
		if historical {
			prior, err := l.versionTrashEntry(ctx, v.AssetID, v.VersionID, st.Lifecycle)
			if err != nil {
				return out, err
			}
			if st.Lifecycle == "purged" {
				if _, err = l.visibleEntry(ctx, who, v.ProjectID, prior.ID); err != nil {
					return out, err
				}
			}
			out.request.History = append(out.request.History, TrashHistoryTarget{TrashVersionTarget: target, Lifecycle: st.Lifecycle, TrashID: prior.ID})
		}
		if st.Lifecycle != "purged" {
			use, err := l.rights.EvaluateRiskAccess(ctx, who, v.Ref(who.InstanceID))
			if err != nil {
				return out, err
			}
			if err = use.Err(); err != nil {
				return out, err
			}
		}
		if historical {
			continue
		}
		proof, err := readJSON[install.Proof](ctx, s.db, `SELECT proof FROM ledger_prepared WHERE operation_id=? AND proof IS NOT NULL`, v.OperationID)
		if err != nil {
			return out, err
		}
		pd, err := proof.Digest()
		if err != nil {
			return out, err
		}
		if pd != v.ProofDigest || proof.VersionID != v.VersionID || proof.AssetID != v.AssetID || proof.ManifestDigest != v.ManifestDigest {
			return out, errcode.New(errcode.HashMismatch, "committed proof differs")
		}
		out.proofs = append(out.proofs, proof)
		out.request.Targets = append(out.request.Targets, target)
	}
	if len(refs) == 0 {
		return out, errcode.New(errcode.NotFound, "")
	}
	uses, err := l.uses.CurrentUses(ctx, who, a.ProjectID, refs)
	if err != nil {
		return out, err
	}
	for _, use := range uses {
		if !slices.Contains([]string{"task", "uses", "derived_from"}, use.Kind) || !use.ID.Valid() || use.Revision < 1 || !use.Digest.Valid() {
			return out, errcode.New(errcode.OperationNeedsReconciliation, "invalid current use snapshot")
		}
	}
	// Canonical ordering is part of the confirmation binding, independent of an
	// owning module's iteration order. Duplicate impacts are not silently removed.
	slices.SortFunc(uses, func(a, b LifecycleUse) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	for i := 1; i < len(uses); i++ {
		if uses[i].Kind == uses[i-1].Kind && uses[i].ID == uses[i-1].ID {
			return out, errcode.New(errcode.OperationNeedsReconciliation, "duplicate use snapshot")
		}
	}
	out.request.Uses = append(out.request.Uses, uses...)
	out.facts.inUse = len(uses) > 0
	out.records, err = l.files.SnapshotLifecycleRecords(ctx, out.proofs)
	if err != nil {
		return out, err
	}
	raw, err := canonjson.CanonicalizeValue(struct {
		Versions []install.Proof           `json:"versions"`
		Records  []storage.LifecycleRecord `json:"records"`
	}{out.proofs, out.records})
	if err != nil {
		return out, err
	}
	out.request.FilesDigest = digest.Of(raw)
	return out, nil
}

func (l *Lifecycle) versionTrashEntry(ctx context.Context, asset, version ids.ID, state string) (TrashEntry, error) {
	rows, err := l.ledger.db.QueryContext(ctx, `SELECT record FROM ledger_trash_entries WHERE asset_id=? AND state=? AND EXISTS(SELECT 1 FROM json_each(record,'$.version_ids') WHERE value=?)`, asset, state, version)
	if err != nil {
		return TrashEntry{}, err
	}
	defer rows.Close()
	var out TrashEntry
	count := 0
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return out, err
		}
		if err = json.Unmarshal([]byte(raw), &out); err != nil {
			return out, err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if count != 1 {
		return out, errcode.New(errcode.OperationNeedsReconciliation, "historical trash is not unique")
	}
	return out, nil
}
