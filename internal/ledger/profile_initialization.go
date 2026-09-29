package ledger

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// InitialProfile is an explicit owner-requested review of the configuration
// itself when the project has no active approved configuration. It still needs
// current T05/T06 evidence, nonwaivable checks and an exact human review grant.
// There is no pre-approved synthetic configuration or direct approval shortcut.
func (r *Reviews) initialProfile(ctx context.Context, who authz.Context, project ids.ID, ref ids.PermanentRef) (ProfileSnapshot, error) {
	var out ProfileSnapshot
	if err := r.ledger.authorize(ctx, who, "ledger.initialize_profile", project, "version", ref.VersionID); err != nil {
		return out, err
	}
	v, err := r.ledger.Version(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return out, err
	}
	if v.ProjectID != project {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	// Close SQL rows before consulting the owning immutable-file reader.
	rows, err := r.ledger.db.QueryContext(ctx, `SELECT v.version_id FROM ledger_versions v JOIN ledger_version_states s ON s.version_id=v.version_id LEFT JOIN ledger_asset_controls a ON a.asset_id=v.asset_id WHERE v.project_id=? AND s.state='approved' AND s.availability='enabled' AND s.lifecycle='active' AND s.pending_operation_id='' AND COALESCE(a.lifecycle,'active')='active' AND COALESCE(a.pending_operation_id,'')='' ORDER BY v.version_id`, project)
	if err != nil {
		return out, err
	}
	var versions []ids.ID
	for rows.Next() {
		var id ids.ID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		versions = append(versions, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, id := range versions {
		current, err := r.ledger.VersionByID(ctx, id)
		if err != nil {
			return out, err
		}
		kind, err := r.sources.AssetType(ctx, current)
		if err != nil {
			return out, err
		}
		if kind == manifest.TypeConfig {
			return out, errcode.New(errcode.InvalidStateTransition, "an active approved configuration exists; use the normal profile review path")
		}
	}
	return r.profileSnapshot(ctx, who, ref, false)
}
func (r *Reviews) profileForTarget(ctx context.Context, who authz.Context, t ReviewTarget) (ProfileSnapshot, error) {
	if !t.InitialProfile {
		return r.profile(ctx, who, t.Profile.Ref)
	}
	if t.Profile.Ref.AssetID != t.AssetID || t.Profile.Ref.VersionID != t.VersionID || t.Profile.ManifestDigest != t.ManifestDigest {
		return ProfileSnapshot{}, errcode.New(errcode.RefMismatch, "initial profile target changed")
	}
	state, err := r.ledger.VersionControl(ctx, t.VersionID)
	if err != nil {
		return ProfileSnapshot{}, err
	}
	// Once human approval committed, subsequent publication uses ordinary approved
	// profile authority; initialization is never repeated on the publication path.
	if state.ReviewState == "approved" {
		return r.profile(ctx, who, t.Profile.Ref)
	}
	return r.initialProfile(ctx, who, t.ProjectID, t.Profile.Ref)
}
