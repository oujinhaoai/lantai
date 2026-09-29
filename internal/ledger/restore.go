package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type RestoreRequest struct {
	ProjectID        ids.ID `json:"project_id"`
	TrashID          ids.ID `json:"trash_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	// Explicit alternative; empty means the original path, never an auto-rename.
	NewSlug string `json:"new_slug,omitempty"`
	Reason  string `json:"reason"`
}

func (l *Lifecycle) Restore(ctx context.Context, who authz.Context, key string, in RestoreRequest) (commands.Receipt, error) {
	if !in.ProjectID.Valid() || !in.TrashID.Valid() || in.ExpectedRevision < 1 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 {
		return commands.Receipt{}, invalid("restore target, revision and reason required")
	}
	if in.NewSlug != "" {
		if err := pathrule.CheckSlug(in.NewSlug); err != nil {
			return commands.Receipt{}, err
		}
	}
	s := l.ledger
	cmd, err := s.commandContext(ctx, who, key, "ledger.restore", in.ProjectID, in)
	if err != nil {
		return commands.Receipt{}, err
	}
	var receipt commands.Receipt
	err = func() error {
		ctx, h, err := s.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}})
		if err != nil {
			return err
		}
		defer h.Release()
		if err = s.authorize(ctx, who, "catalog.read", in.ProjectID, "project", in.ProjectID); err != nil {
			return err
		}
		if err = s.authorize(ctx, who, "ledger.restore", in.ProjectID, "trash", in.TrashID); err != nil {
			return err
		}
		entry, err := l.entry(ctx, in.TrashID)
		if err != nil {
			return err
		}
		if entry.ProjectID != in.ProjectID {
			return errcode.New(errcode.NotFound, "")
		}
		if entry.CreatedBy != who.PrincipalID {
			if err = s.authorize(ctx, who, "ledger.restore_others", in.ProjectID, "trash", entry.ID); err != nil {
				return err
			}
		}
		if prior, err := s.lookup(ctx, s.db, cmd); err != nil {
			return err
		} else if prior != nil {
			if commands.Decide(prior, cmd.RequestHash, s.clock.Now()) == commands.OutcomeExpired {
				return errcode.New(errcode.IdempotencyResultExpired, "")
			}
			receipt = *prior
			return nil
		}
		if entry.State == "purged" {
			return errcode.New(errcode.AssetPurged, "")
		}
		if entry.Revision != in.ExpectedRevision {
			return errcode.New(errcode.PreconditionFailed, "")
		}
		if entry.State != "trashed" || entry.PendingOperationID != "" {
			return errcode.New(errcode.InvalidStateTransition, "")
		}
		project, err := s.Project(ctx, in.ProjectID)
		if err != nil {
			return err
		}
		if project.State != commit.ProjectActive {
			return errcode.New(errcode.InvalidStateTransition, "project is archived")
		}
		asset, err := s.Asset(ctx, entry.AssetID)
		if err != nil {
			return err
		}
		if err = s.checkPathLock(ctx, in.ProjectID, entry.AssetID, asset.Slug); err != nil {
			return err
		}
		if entry.WholeAsset && l.aliases == nil {
			return errcode.New(errcode.OperationNeedsReconciliation, "restore requires the catalog alias writer")
		}
		if !entry.WholeAsset && in.NewSlug != "" {
			return invalid("version restore cannot rename its asset")
		}
		if !entry.WholeAsset {
			if err = s.CheckAssetWrite(ctx, entry.AssetID); err != nil {
				return err
			}
		}
		slug := entry.OriginalSlug
		if in.NewSlug != "" {
			slug = in.NewSlug
		}
		if entry.WholeAsset {
			if err = s.checkPathLock(ctx, in.ProjectID, entry.AssetID, slug); err != nil {
				return err
			}
		}
		for _, id := range entry.VersionIDs {
			v, err := s.Version(ctx, entry.AssetID, id)
			if err != nil {
				return err
			}
			d, err := l.rights.EvaluateRiskAccess(ctx, who, v.Ref(who.InstanceID))
			if err != nil {
				return err
			}
			if err = d.Err(); err != nil {
				return err
			}
		}
		plan, err := readJSON[storage.FileIntent](ctx, s.db, `SELECT plan FROM ledger_lifecycle_ops WHERE operation_id=? AND state='done'`, entry.OriginalOperationID)
		if err != nil {
			return err
		}
		plan.OperationID = cmd.OperationID
		plan.Action = "restore"
		result, err := s.store.Accept(ctx, s.db, cmd, commands.StagePrepared, []string{string(entry.AssetID)}, func(ctx context.Context, tx *sql.Tx) error {
			if entry.WholeAsset {
				old, err := readJSON[commit.Claim](ctx, tx, `SELECT record FROM ledger_namespace_claims WHERE project_id=? AND slug_key=?`, in.ProjectID, pathrule.Key(slug))
				if err != nil && errcode.CodeOf(err) != errcode.NotFound {
					return err
				}
				if err == nil && old.State != commit.ClaimReleased && !(old.State == commit.ClaimActive && old.AssetID == entry.AssetID) {
					return errcode.New(errcode.PathConflict, "restore path is occupied; choose an explicit new path")
				}
				generation := old.Generation + 1
				claim := commit.Claim{ProjectID: in.ProjectID, Slug: slug, Generation: generation, AssetID: entry.AssetID, OperationID: cmd.OperationID, State: commit.ClaimReserved, Revision: old.Revision + 1}
				var previous any
				if err == nil {
					previous = encoded(old)
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_namespace_claims(project_id,slug_key,record,previous_record) VALUES(?,?,?,?) ON CONFLICT(project_id,slug_key) DO UPDATE SET record=excluded.record,previous_record=excluded.previous_record`, in.ProjectID, pathrule.Key(slug), encoded(claim), previous); err != nil {
					return err
				}
				// Migration-compatible capture of the original immutable allocation.
				var n int
				if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM ledger_alias_allocations WHERE project_id=? AND slug_key=? AND generation=?`, asset.ProjectID, pathrule.Key(asset.Slug), asset.Generation).Scan(&n); err != nil {
					return err
				}
				if n == 0 {
					if err = saveAliasAllocation(ctx, tx, asset, "create"); err != nil {
						return err
					}
				}
				entry.RestoreSlug = slug
				entry.RestoreGeneration = generation
				a, err := assetControl(ctx, tx, entry.AssetID)
				if err != nil {
					return err
				}
				if a.PendingOperationID != "" || a.Lifecycle != "trashed" {
					return errcode.New(errcode.PreconditionFailed, "")
				}
				a.PendingOperationID = cmd.OperationID
				a.Revision++
				if err = saveAssetControl(ctx, tx, a); err != nil {
					return err
				}
			}
			for _, id := range entry.VersionIDs {
				v, err := versionControl(ctx, tx, id)
				if err != nil {
					return err
				}
				if v.Lifecycle != "trashed" || v.PendingOperationID != "" {
					return errcode.New(errcode.PreconditionFailed, "")
				}
				if state := entry.PriorVersionLifecycles[id]; state != "active" && state != "archived" {
					return errcode.New(errcode.OperationNeedsReconciliation, "original lifecycle is missing")
				}
				v.PendingOperationID = cmd.OperationID
				v.Revision++
				if err = saveVersionControl(ctx, tx, v); err != nil {
					return err
				}
			}
			entry.PendingOperationID = cmd.OperationID
			entry.Revision++
			if err = saveTrashEntry(ctx, tx, entry); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO ledger_lifecycle_ops VALUES(?,?,?,?,?,'accepted')`, cmd.OperationID, entry.ID, "restore", encoded(cmd), encoded(plan))
			return err
		})
		if err != nil {
			return err
		}
		receipt = *result.Receipt
		return nil
	}()
	if err != nil {
		return commands.Receipt{}, err
	}
	return l.Resume(ctx, receipt.OperationID)
}

func (l *Lifecycle) finishRestore(ctx context.Context, tx *sql.Tx, entry *TrashEntry, cmd commands.Context) error {
	s := l.ledger
	for _, id := range entry.VersionIDs {
		v, err := versionControl(ctx, tx, id)
		if err != nil {
			return err
		}
		if v.PendingOperationID != cmd.OperationID {
			return errcode.New(errcode.PreconditionFailed, "")
		}
		v.Lifecycle = entry.PriorVersionLifecycles[id]
		v.PendingOperationID = ""
		v.Revision++
		// Review history survives, but old targets/approvals cannot become effective.
		v.ReviewState = "withdrawn"
		v.ReviewTargetID = ""
		v.EffectiveReviewID = ""
		v.WithdrawalReason = "restored_requires_new_review"
		if err = saveVersionControl(ctx, tx, v); err != nil {
			return err
		}
	}
	if entry.WholeAsset {
		a, err := assetControl(ctx, tx, entry.AssetID)
		if err != nil {
			return err
		}
		if a.PendingOperationID != cmd.OperationID {
			return errcode.New(errcode.PreconditionFailed, "")
		}
		if entry.PriorAssetLifecycle != "active" && entry.PriorAssetLifecycle != "archived" {
			return errcode.New(errcode.OperationNeedsReconciliation, "original asset lifecycle is missing")
		}
		a.Lifecycle = entry.PriorAssetLifecycle
		a.PendingOperationID = ""
		a.Revision++
		if err = saveAssetControl(ctx, tx, a); err != nil {
			return err
		}
		asset, err := readJSON[commit.Asset](ctx, tx, `SELECT record FROM ledger_assets WHERE asset_id=?`, entry.AssetID)
		if err != nil {
			return err
		}
		asset.Slug = entry.RestoreSlug
		asset.Generation = entry.RestoreGeneration
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_assets SET slug=?,generation=?,record=? WHERE asset_id=?`, asset.Slug, asset.Generation, encoded(asset), asset.AssetID); err != nil {
			return err
		}
		allocation := asset
		allocation.CreatedAt = s.clock.Now()
		allocation.CreatedBy = cmd.ActorID
		allocation.OperationID = cmd.OperationID
		if err = saveAliasAllocation(ctx, tx, allocation, "restore"); err != nil {
			return err
		}
		claim, err := readJSON[commit.Claim](ctx, tx, `SELECT record FROM ledger_namespace_claims WHERE project_id=? AND slug_key=?`, entry.ProjectID, pathrule.Key(entry.RestoreSlug))
		if err != nil {
			return err
		}
		if claim.State != commit.ClaimReserved || claim.OperationID != cmd.OperationID || claim.AssetID != entry.AssetID || claim.Generation != entry.RestoreGeneration {
			return errcode.New(errcode.OperationNeedsReconciliation, "restore reservation changed")
		}
		claim.State = commit.ClaimActive
		claim.Revision++
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_namespace_claims SET record=?,previous_record=NULL WHERE project_id=? AND slug_key=?`, encoded(claim), entry.ProjectID, pathrule.Key(entry.RestoreSlug)); err != nil {
			return err
		}
	}
	entry.State = "restored"
	return nil
}

func (l *Lifecycle) repairRestoredAliases(ctx context.Context, r commands.Receipt) error {
	var entry TrashEntry
	if err := json.Unmarshal(r.ResponseSummary, &entry); err != nil {
		return err
	}
	if entry.State != "restored" || !entry.WholeAsset {
		return nil
	}
	if l.aliases == nil {
		return errcode.New(errcode.OperationNeedsReconciliation, "restore requires the catalog alias writer")
	}
	return l.aliases.RepairLifecycleAliases(ctx)
}
