package ledger

import (
	"context"
	"database/sql"
	"strings"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type CancelTrashRequest struct {
	ProjectID   ids.ID        `json:"project_id"`
	AssetID     ids.ID        `json:"asset_id"`
	OperationID ids.ID        `json:"operation_id"`
	RequestHash digest.Digest `json:"request_hash"`
	Reason      string        `json:"reason"`
}

type UnmovedTrashVerifier interface {
	VerifyUnmovedTrash(context.Context, storage.FileIntent) error
}

// CancelTrash reconciles an exact uncompleted deletion under current owner
// authority. File verification runs before the ledger transaction while holding
// all asset locks used by the file adapter. No already-moved deletion is undone.
func (l *Lifecycle) CancelTrash(ctx context.Context, who authz.Context, key string, in CancelTrashRequest) (commands.Receipt, error) {
	if !in.ProjectID.Valid() || !in.AssetID.Valid() || !in.OperationID.Valid() || !in.RequestHash.Valid() || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 {
		return commands.Receipt{}, invalid("exact deletion operation, request hash and reason required")
	}
	s := l.ledger
	cmd, err := s.commandContext(ctx, who, key, "ledger.cancel_trash", in.ProjectID, in)
	if err != nil {
		return commands.Receipt{}, err
	}
	ctx, held, err := s.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}, Assets: []string{string(in.AssetID)}})
	if err != nil {
		return commands.Receipt{}, err
	}
	defer held.Release()
	if err = s.authorize(ctx, who, "ledger.cancel_trash", in.ProjectID, "asset", in.AssetID); err != nil {
		return commands.Receipt{}, err
	}
	if prior, err := s.lookup(ctx, s.db, cmd); err != nil {
		return commands.Receipt{}, err
	} else if prior != nil {
		if commands.Decide(prior, cmd.RequestHash, s.clock.Now()) == commands.OutcomeExpired {
			return commands.Receipt{}, errcode.New(errcode.IdempotencyResultExpired, "")
		}
		return *prior, nil
	}
	original, err := s.store.ReceiptByOperation(ctx, s.db, in.OperationID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if original.Key.ProjectID != in.ProjectID {
		return commands.Receipt{}, errcode.New(errcode.NotFound, "")
	}
	if original.RequestHash != in.RequestHash {
		return commands.Receipt{}, errcode.New(errcode.PreconditionFailed, "deletion request differs")
	}
	if original.Status != commands.ReceiptInProgress {
		return commands.Receipt{}, errcode.New(errcode.InvalidStateTransition, "only uncompleted deletion can be cancelled")
	}
	// Cancellation confers no file authority, even after a recovery epoch change.
	// Its current permission and the physical reconciliation replace neither the
	// old deletion grant nor the epoch check on any actual file operation.
	plan, err := l.acceptedFileIntent(ctx, in.OperationID, false)
	if err != nil {
		return commands.Receipt{}, err
	}
	entry, err := l.entry(ctx, plan.TrashID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if entry.ProjectID != in.ProjectID || entry.AssetID != in.AssetID {
		return commands.Receipt{}, errcode.New(errcode.RefMismatch, "")
	}
	if plan.Action != "trash" || entry.State != "pending" || entry.OriginalOperationID != in.OperationID {
		return commands.Receipt{}, errcode.New(errcode.InvalidStateTransition, "only the initial deletion can be cancelled")
	}
	if len(plan.Versions) > 0 {
		verifier, ok := l.files.(UnmovedTrashVerifier)
		if !ok {
			return commands.Receipt{}, errcode.New(errcode.OperationNeedsReconciliation, "physical reconciliation adapter required")
		}
		if err = verifier.VerifyUnmovedTrash(ctx, plan); err != nil {
			return commands.Receipt{}, err
		}
	}
	result, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		if err := s.store.Cancel(ctx, tx, in.OperationID, commands.StagePrepared, errcode.InvalidStateTransition); err != nil {
			return commands.Result{}, err
		}
		for _, id := range entry.VersionIDs {
			v, err := versionControl(ctx, tx, id)
			if err != nil {
				return commands.Result{}, err
			}
			if v.PendingOperationID != in.OperationID || v.Lifecycle != entry.PriorVersionLifecycles[id] {
				return commands.Result{}, errcode.New(errcode.PreconditionFailed, "deletion ban changed")
			}
			v.PendingOperationID = ""
			v.Revision++
			if err = saveVersionControl(ctx, tx, v); err != nil {
				return commands.Result{}, err
			}
		}
		if entry.WholeAsset {
			a, err := assetControl(ctx, tx, entry.AssetID)
			if err != nil {
				return commands.Result{}, err
			}
			if a.PendingOperationID != in.OperationID || a.Lifecycle != entry.PriorAssetLifecycle {
				return commands.Result{}, errcode.New(errcode.PreconditionFailed, "asset deletion ban changed")
			}
			a.PendingOperationID = ""
			a.Revision++
			if err = saveAssetControl(ctx, tx, a); err != nil {
				return commands.Result{}, err
			}
		}
		entry.State = "cancelled"
		entry.PendingOperationID = ""
		entry.Revision++
		if err = saveTrashEntry(ctx, tx, entry); err != nil {
			return commands.Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_lifecycle_ops SET state='cancelled' WHERE operation_id=?`, in.OperationID); err != nil {
			return commands.Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_trash_quota SET state='released' WHERE operation_id=? AND state='reserved'`, in.OperationID); err != nil {
			return commands.Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM ledger_lifecycle_failures WHERE operation_id=?`, in.OperationID); err != nil {
			return commands.Result{}, err
		}
		e, err := s.event(cmd, "trash.cancelled", "trash", entry.ID, entry.Revision, map[string]any{"trash_id": entry.ID, "asset_id": entry.AssetID, "cancelled_operation_id": in.OperationID, "reason": in.Reason})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: map[string]any{"cancelled_operation_id": in.OperationID, "trash_id": entry.ID, "state": "cancelled"}, Events: []event.Envelope{e}}, nil
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *result.Receipt, nil
}
