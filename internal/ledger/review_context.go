package ledger

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// ReviewDocuments is an optional immutable-file read port, used for the T02
// project context adapter. FileReviewSources supplies the production adapter.
type ReviewDocuments interface {
	ProjectDocument(context.Context, authz.Context, ids.PermanentRef) (catalog.ProjectDocument, error)
}

func (s *FileReviewSources) ProjectDocument(ctx context.Context, who authz.Context, ref ids.PermanentRef) (catalog.ProjectDocument, error) {
	var spec catalog.ProjectDocument
	if ref.InstanceID != who.InstanceID {
		return spec, errcode.New(errcode.RefMismatch, "")
	}
	v, err := s.ledger.Version(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return spec, err
	}
	if err = s.Use(ctx, who, v, authz.PurposeArchiveReview); err != nil {
		return spec, err
	}
	doc, err := s.read(ctx, v)
	if err != nil {
		return spec, err
	}
	if doc.Content.AssetType != manifest.TypeDoc {
		return spec, errcode.New(errcode.NotFound, "")
	}
	value, ok := doc.Content.Metadata["project_document"]
	if !ok {
		return spec, errcode.New(errcode.NotFound, "")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return spec, err
	}
	if err = json.Unmarshal(raw, &spec); err != nil {
		return spec, err
	}
	return spec, spec.Validate()
}

// ContextReview returns the current T03 receipt for a fixed document. Historical
// approved reviews cannot supply authority after revoke/disable/trash.
// A security read guard covers current state, authorization and evidence reads.
func (r *Reviews) ContextReview(ctx context.Context, who authz.Context, ref ids.PermanentRef) (catalog.ContextApproval, error) {
	ctx, held, err := r.ledger.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return catalog.ContextApproval{}, err
	}
	defer held.Release()
	return r.contextReview(ctx, who, ref)
}
func (r *Reviews) contextReview(ctx context.Context, who authz.Context, ref ids.PermanentRef) (catalog.ContextApproval, error) {
	var out catalog.ContextApproval
	s := r.ledger
	if ref.InstanceID != who.InstanceID {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	v, err := s.Version(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return out, err
	}
	if err = s.authorize(ctx, who, "catalog.read", v.ProjectID, "version", v.VersionID); err != nil {
		return out, err
	}
	if err = s.CheckVersionRead(ctx, v.AssetID, v.VersionID); err != nil {
		return out, err
	}
	if err = r.sources.Use(ctx, who, v, authz.PurposeArchiveReview); err != nil {
		return out, err
	}
	docs, ok := r.sources.(ReviewDocuments)
	if !ok {
		return out, errcode.New(errcode.InvalidStateTransition, "project document file adapter required")
	}
	if _, err = docs.ProjectDocument(ctx, who, ref); err != nil {
		return out, err
	}
	state, err := s.VersionControl(ctx, ref.VersionID)
	if err != nil {
		return out, err
	}
	if state.ReviewState != "approved" || state.EffectiveReviewID == "" {
		return out, errcode.New(errcode.NotFound, "")
	}
	review, err := readJSON[Review](ctx, s.db, `SELECT record FROM ledger_reviews WHERE review_id=?`, state.EffectiveReviewID)
	if err != nil {
		return out, err
	}
	if review.Verdict != "approve" || review.VersionID != v.VersionID || review.ManifestDigest != v.ManifestDigest {
		return out, errcode.New(errcode.ReviewTargetStale, "")
	}
	return catalog.ContextApproval{Ref: ref, ManifestDigest: v.ManifestDigest, ReviewID: review.ID, ApproverID: review.ActorID, EffectiveAt: review.CreatedAt, Revision: state.Revision}, nil
}

// EffectiveContext 在 security_guard 读保护下枚举当前生效的上下文审定。调用方
// 已持有该锁时（例如任务创建在最终接受边界复核）继承它，不再重复取锁。
func (r *Reviews) EffectiveContext(ctx context.Context, who authz.Context, project ids.ID, kind manifest.AssetType) ([]catalog.ContextApproval, error) {
	if !r.ledger.gate.HoldsSecurity(ctx) {
		guarded, held, err := r.ledger.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
		if err != nil {
			return nil, err
		}
		defer held.Release()
		ctx = guarded
	}
	if err := r.ledger.authorize(ctx, who, "catalog.read", project, "project", project); err != nil {
		return nil, err
	}
	docs, ok := r.sources.(ReviewDocuments)
	if !ok {
		return nil, errcode.New(errcode.InvalidStateTransition, "project document file adapter required")
	}
	// Enumeration uses ledger authority, not an eventually consistent index. Keep
	// the rows closed before file/permission callbacks (SQLite may use one handle).
	rows, err := r.ledger.db.QueryContext(ctx, `SELECT v.asset_id,v.version_id FROM ledger_versions v JOIN ledger_version_states s ON s.version_id=v.version_id WHERE s.state='approved' ORDER BY v.asset_id,v.version_number DESC`)
	if err != nil {
		return nil, err
	}
	refs := []ids.PermanentRef{}
	for rows.Next() {
		ref := ids.PermanentRef{InstanceID: who.InstanceID}
		if err = rows.Scan(&ref.AssetID, &ref.VersionID); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	selected := map[ids.ID]catalog.ContextApproval{}
	specs := map[ids.PermanentRef]catalog.ProjectDocument{}
	for _, ref := range refs {
		if _, ok := selected[ref.AssetID]; ok {
			continue
		}
		if err := r.ledger.CheckVersionSearch(ctx, ref.AssetID, ref.VersionID, false); err != nil {
			if contextHidden(err) {
				continue
			}
			return nil, err
		}
		approval, err := r.contextReview(ctx, who, ref)
		if err != nil {
			if contextHidden(err) {
				continue
			}
			return nil, err
		}
		spec, err := docs.ProjectDocument(ctx, who, ref)
		if err != nil {
			if contextHidden(err) {
				continue
			}
			return nil, err
		}
		v, err := r.ledger.Version(ctx, ref.AssetID, ref.VersionID)
		if err != nil {
			return nil, err
		}
		if spec.Scope.Kind != "global" && v.ProjectID != project {
			continue
		}
		if spec.Scope.Kind == "asset_type" && spec.Scope.AssetType != kind {
			continue
		}
		selected[ref.AssetID] = approval
		specs[ref] = spec
	}
	removed := map[ids.PermanentRef]bool{}
	for ref, spec := range specs {
		for _, old := range spec.Supersedes {
			prior, ok := specs[old]
			if !ok {
				continue
			}
			if old == ref || prior.Kind != spec.Kind || prior.Scope != spec.Scope {
				return nil, errcode.New(errcode.ReviewTargetStale, "context supersession crosses kind or scope")
			}
			removed[old] = true
		}
	}
	active, visited := map[ids.PermanentRef]bool{}, map[ids.PermanentRef]bool{}
	var visit func(ids.PermanentRef) error
	visit = func(ref ids.PermanentRef) error {
		if active[ref] {
			return errcode.New(errcode.DependencyCycle, "context supersession cycle")
		}
		if visited[ref] {
			return nil
		}
		active[ref] = true
		for _, old := range specs[ref].Supersedes {
			if _, ok := specs[old]; ok {
				if err := visit(old); err != nil {
					return err
				}
			}
		}
		active[ref] = false
		visited[ref] = true
		return nil
	}
	for ref := range specs {
		if err := visit(ref); err != nil {
			return nil, err
		}
	}
	out := []catalog.ContextApproval{}
	for _, a := range selected {
		if !removed[a.Ref] {
			out = append(out, a)
		}
	}
	// A cyclic replacement cannot silently make all approved instructions vanish.
	if len(selected) > 0 && len(out) == 0 {
		return nil, errcode.New(errcode.DependencyCycle, "context supersession cycle")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.AssetID < out[j].Ref.AssetID })
	return out, nil
}
func contextHidden(err error) bool {
	switch errcode.CodeOf(err) {
	case errcode.NotFound, errcode.Forbidden, errcode.UseRestricted, errcode.AssetPurged:
		return true
	}
	return false
}
