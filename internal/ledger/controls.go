package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type VersionControl struct {
	VersionID          ids.ID `json:"version_id"`
	Revision           int64  `json:"revision"`
	ReviewState        string `json:"review_state"`
	Availability       string `json:"availability"`
	DisabledReason     string `json:"disabled_reason,omitempty"`
	Lifecycle          string `json:"lifecycle"`
	EffectiveReviewID  ids.ID `json:"effective_review_id,omitempty"`
	ReviewTargetID     ids.ID `json:"review_target_id,omitempty"`
	WithdrawalReason   string `json:"withdrawal_reason,omitempty"`
	PendingOperationID ids.ID `json:"pending_operation_id,omitempty"`
}
type AssetControl struct {
	AssetID             ids.ID `json:"asset_id"`
	Revision            int64  `json:"revision"`
	Lifecycle           string `json:"lifecycle"`
	PublicationState    string `json:"publication_state"`
	PublishedVersionID  ids.ID `json:"published_version_id,omitempty"`
	PublicationRevision int64  `json:"publication_revision"`
	PendingOperationID  ids.ID `json:"pending_operation_id,omitempty"`
}

func versionControl(ctx context.Context, q commands.DBTX, id ids.ID) (VersionControl, error) {
	var s VersionControl
	err := q.QueryRowContext(ctx, `SELECT version_id,revision,state,availability,disabled_reason,lifecycle,effective_review_id,review_target_id,withdrawal_reason,pending_operation_id FROM ledger_version_states WHERE version_id=?`, id).Scan(&s.VersionID, &s.Revision, &s.ReviewState, &s.Availability, &s.DisabledReason, &s.Lifecycle, &s.EffectiveReviewID, &s.ReviewTargetID, &s.WithdrawalReason, &s.PendingOperationID)
	return s, missing(err)
}
func assetControl(ctx context.Context, q commands.DBTX, id ids.ID) (AssetControl, error) {
	s := AssetControl{AssetID: id, Revision: 1, Lifecycle: "active", PublicationState: "unpublished"}
	err := q.QueryRowContext(ctx, `SELECT asset_id,revision,lifecycle,publication_state,published_version_id,publication_revision,pending_operation_id FROM ledger_asset_controls WHERE asset_id=?`, id).Scan(&s.AssetID, &s.Revision, &s.Lifecycle, &s.PublicationState, &s.PublishedVersionID, &s.PublicationRevision, &s.PendingOperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	return s, err
}
func (s *Service) VersionControl(ctx context.Context, id ids.ID) (VersionControl, error) {
	return versionControl(ctx, s.db, id)
}
func (s *Service) AssetControl(ctx context.Context, id ids.ID) (AssetControl, error) {
	if _, err := s.Asset(ctx, id); err != nil {
		return AssetControl{}, err
	}
	return assetControl(ctx, s.db, id)
}
func saveVersionControl(ctx context.Context, tx *sql.Tx, s VersionControl) error {
	_, err := tx.ExecContext(ctx, `UPDATE ledger_version_states SET revision=?,state=?,availability=?,disabled_reason=?,lifecycle=?,effective_review_id=?,review_target_id=?,withdrawal_reason=?,pending_operation_id=? WHERE version_id=?`, s.Revision, s.ReviewState, s.Availability, s.DisabledReason, s.Lifecycle, s.EffectiveReviewID, s.ReviewTargetID, s.WithdrawalReason, s.PendingOperationID, s.VersionID)
	return err
}
func saveAssetControl(ctx context.Context, tx *sql.Tx, s AssetControl) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ledger_asset_controls VALUES(?,?,?,?,?,?,?) ON CONFLICT(asset_id) DO UPDATE SET revision=excluded.revision,lifecycle=excluded.lifecycle,publication_state=excluded.publication_state,published_version_id=excluded.published_version_id,publication_revision=excluded.publication_revision,pending_operation_id=excluded.pending_operation_id`, s.AssetID, s.Revision, s.Lifecycle, s.PublicationState, s.PublishedVersionID, s.PublicationRevision, s.PendingOperationID)
	return err
}
func lifecycleReadable(lifecycle string, pending ids.ID) error {
	if lifecycle == "purged" {
		return errcode.New(errcode.AssetPurged, "")
	}
	if lifecycle == "trashed" || pending != "" {
		return errcode.New(errcode.NotFound, "")
	}
	return nil
}
func (s *Service) CheckVersionRead(ctx context.Context, asset, version ids.ID) error {
	v, err := s.Version(ctx, asset, version)
	if err != nil {
		return err
	}
	a, err := assetControl(ctx, s.db, asset)
	if err != nil {
		return err
	}
	if err = lifecycleReadable(a.Lifecycle, a.PendingOperationID); err != nil {
		return err
	}
	state, err := versionControl(ctx, s.db, v.VersionID)
	if err != nil {
		return err
	}
	if err = lifecycleReadable(state.Lifecycle, state.PendingOperationID); err != nil {
		return err
	}
	if state.Availability != "enabled" {
		return errcode.New(errcode.UseRestricted, "version is disabled")
	}
	return nil
}
func (s *Service) CheckAssetWrite(ctx context.Context, id ids.ID) error {
	a, err := s.Asset(ctx, id)
	if err != nil {
		return err
	}
	state, err := assetControl(ctx, s.db, id)
	if err != nil {
		return err
	}
	if state.Lifecycle != "active" || state.PendingOperationID != "" {
		return errcode.New(errcode.InvalidStateTransition, "asset is not writable")
	}
	project, err := s.Project(ctx, a.ProjectID)
	if err != nil {
		return err
	}
	if project.State != commit.ProjectActive {
		return errcode.New(errcode.InvalidStateTransition, "project is archived")
	}
	return s.checkPathLock(ctx, a.ProjectID, id, a.Slug)
}
func (s *Service) checkPathLock(ctx context.Context, project, asset ids.ID, path string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT scope,target FROM ledger_locks WHERE project_id=? AND active=1`, project)
	if err != nil {
		return err
	}
	defer rows.Close()
	key := pathrule.Key(path)
	for rows.Next() {
		var scope, target string
		if err = rows.Scan(&scope, &target); err != nil {
			return err
		}
		if scope == "asset" && target == string(asset) || scope == "path" && (target == key || strings.HasPrefix(key, target+"/")) {
			return errcode.New(errcode.AssetLocked, "")
		}
	}
	return rows.Err()
}

type Lock struct {
	ID        ids.ID `json:"lock_id"`
	ProjectID ids.ID `json:"project_id"`
	Scope     string `json:"scope"`
	AssetID   ids.ID `json:"asset_id,omitempty"`
	Path      string `json:"path,omitempty"`
	Reason    string `json:"reason"`
	CreatedBy ids.ID `json:"created_by"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at,omitempty"` // Informational: expiry never silently unlocks.
	Revision  int64  `json:"revision"`
	Active    bool   `json:"active"`
}
type LockRequest struct {
	ProjectID ids.ID `json:"project_id"`
	AssetID   ids.ID `json:"asset_id,omitempty"`
	Path      string `json:"path,omitempty"`
	Reason    string `json:"reason"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

func (s *Service) Lock(ctx context.Context, who authz.Context, key string, r LockRequest) (Lock, error) {
	var out Lock
	if !r.ProjectID.Valid() || (r.AssetID == "") == (r.Path == "") || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 4096 {
		return out, invalid("precise lock scope and reason required")
	}
	if r.AssetID != "" && !r.AssetID.Valid() {
		return out, invalid("invalid lock asset")
	}
	if r.Path != "" {
		if err := pathrule.CheckSlug(r.Path); err != nil {
			return out, err
		}
	}
	if r.ExpiresAt != "" {
		at, err := clock.Parse(r.ExpiresAt)
		if err != nil || !at.After(s.clock.Now()) {
			return out, invalid("lock expiry must be in the future")
		}
	}
	raw, err := canonjson.CanonicalizeValue(r)
	if err != nil {
		return out, err
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: "ledger.lock", ProjectID: r.ProjectID, Body: raw})
	if err != nil {
		return out, err
	}
	ctx, h, err := s.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(r.ProjectID)}})
	if err != nil {
		return out, err
	}
	defer h.Release()
	if err = s.authorize(ctx, who, "ledger.lock", r.ProjectID, "project", r.ProjectID); err != nil {
		return out, err
	}
	if _, err = s.Project(ctx, r.ProjectID); err != nil {
		return out, err
	}
	if r.AssetID != "" {
		a, err := s.Asset(ctx, r.AssetID)
		if err != nil {
			return out, err
		}
		if a.ProjectID != r.ProjectID {
			return out, errcode.New(errcode.RefMismatch, "")
		}
	}
	epoch, err := s.authority.RecoveryEpoch(ctx)
	if err != nil {
		return out, err
	}
	op, err := s.ids.New()
	if err != nil {
		return out, err
	}
	cmd := commands.Context{OperationID: op, IdempotencyKey: key, CommandType: "ledger.lock", ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: r.ProjectID, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: who.PolicyRevision}
	response, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		id, err := s.ids.New()
		if err != nil {
			return commands.Result{}, err
		}
		out = Lock{ID: id, ProjectID: r.ProjectID, AssetID: r.AssetID, Path: r.Path, Reason: r.Reason, CreatedBy: who.PrincipalID, CreatedAt: clock.Format(s.clock.Now()), ExpiresAt: r.ExpiresAt, Revision: 1, Active: true}
		target := string(r.AssetID)
		out.Scope = "asset"
		if r.Path != "" {
			out.Scope = "path"
			target = pathrule.Key(r.Path)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_locks VALUES(?,?,?,?,1,1,?)`, id, r.ProjectID, out.Scope, target, encoded(out)); err != nil {
			return commands.Result{}, err
		}
		e, err := s.event(cmd, "lock.created", "lock", id, 1, map[string]any{"scope": out.Scope, "asset_id": r.AssetID, "path": r.Path})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: out, Events: []event.Envelope{e}}, nil
	})
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(response.Receipt.ResponseSummary, &out)
	return out, err
}

// A reserved new asset has no public Asset record until its first commit.
func (s *Service) checkPreparedWrite(ctx context.Context, p commit.Prepared) error {
	project, err := s.Project(ctx, p.ProjectID)
	if err != nil {
		return err
	}
	if project.State != commit.ProjectActive {
		return errcode.New(errcode.InvalidStateTransition, "project is archived")
	}
	if p.AliasGeneration == 0 {
		if err := s.CheckAssetWrite(ctx, p.AssetID); err != nil {
			return err
		}
		var base ids.ID
		var revision int64
		if err := s.db.QueryRowContext(ctx, `SELECT base_version_id,base_control_revision FROM ledger_prepared WHERE operation_id=?`, p.OperationID).Scan(&base, &revision); err != nil {
			return err
		}
		if base != "" {
			if err := s.CheckVersionRead(ctx, p.AssetID, base); err != nil {
				return err
			}
			current, err := s.VersionControl(ctx, base)
			if err != nil {
				return err
			}
			if revision == 0 {
				revision = 1
			}
			if current.Revision != revision {
				return errcode.New(errcode.PreconditionFailed, "prepared base state changed; old work cannot resume after restore")
			}
		}
		return nil
	}
	var slug string
	if err := s.db.QueryRowContext(ctx, `SELECT slug FROM ledger_assets WHERE asset_id=? AND project_id=?`, p.AssetID, p.ProjectID).Scan(&slug); err != nil {
		return err
	}
	return s.checkPathLock(ctx, p.ProjectID, p.AssetID, slug)
}
func (s *Service) CheckVersionSearch(ctx context.Context, asset, version ids.ID, includeArchived bool) error {
	if err := s.CheckVersionRead(ctx, asset, version); err != nil {
		return err
	}
	if !includeArchived {
		assetRecord, err := s.Asset(ctx, asset)
		if err != nil {
			return err
		}
		project, err := s.Project(ctx, assetRecord.ProjectID)
		if err != nil {
			return err
		}
		if project.State == commit.ProjectArchived {
			return errcode.New(errcode.NotFound, "")
		}
		a, err := s.AssetControl(ctx, asset)
		if err != nil {
			return err
		}
		v, err := s.VersionControl(ctx, version)
		if err != nil {
			return err
		}
		if a.Lifecycle == "archived" || v.Lifecycle == "archived" {
			return errcode.New(errcode.NotFound, "")
		}
	}
	return nil
}

// Risk evidence remains appendable while locked/disabled, but not after a
// lifecycle intent has frozen the exact evidence list for movement/deletion.
func (s *Service) CheckEvidenceAppend(ctx context.Context, asset, version ids.ID) error {
	if _, err := s.Version(ctx, asset, version); err != nil {
		return err
	}
	a, err := assetControl(ctx, s.db, asset)
	if err != nil {
		return err
	}
	if err = lifecycleReadable(a.Lifecycle, a.PendingOperationID); err != nil {
		return err
	}
	v, err := versionControl(ctx, s.db, version)
	if err != nil {
		return err
	}
	return lifecycleReadable(v.Lifecycle, v.PendingOperationID)
}

func (s *Service) RetainsReferences(ctx context.Context, asset, version ids.ID) (bool, error) {
	if _, err := s.Version(ctx, asset, version); err != nil {
		return false, err
	}
	a, err := assetControl(ctx, s.db, asset)
	if err != nil {
		return false, err
	}
	v, err := versionControl(ctx, s.db, version)
	if err != nil {
		return false, err
	}
	live := func(state string) bool { return state == "active" || state == "archived" }
	return live(a.Lifecycle) && live(v.Lifecycle) && a.PendingOperationID == "" && v.PendingOperationID == "", nil
}

func (s *Service) VersionReadFence(ctx context.Context, asset, version ids.ID) (int64, error) {
	if _, err := s.Version(ctx, asset, version); err != nil {
		return 0, err
	}
	v, err := s.VersionControl(ctx, version)
	return v.Revision, err
}
