package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
)

// TrashMutation is the exact human-confirmed entry, inventory and revision.
// Hold does not silently extend retention; unhold keeps the original due time.
type TrashMutation struct {
	Action           authz.Action  `json:"action"`
	ProjectID        ids.ID        `json:"project_id"`
	TrashID          ids.ID        `json:"trash_id"`
	ExpectedRevision int64         `json:"expected_revision"`
	FilesDigest      digest.Digest `json:"files_digest"`
	Reason           string        `json:"reason"`
}

func (l *Lifecycle) MutationHumanAction(ctx context.Context, in TrashMutation) (identity.HumanAction, error) {
	var out identity.HumanAction
	if !slices.Contains([]authz.Action{identity.ActHold, identity.ActUnhold, identity.ActPurge}, in.Action) || !in.ProjectID.Valid() || !in.TrashID.Valid() || in.ExpectedRevision < 1 || !in.FilesDigest.Valid() || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 {
		return out, invalid("precise trash mutation required")
	}
	e, err := l.entry(ctx, in.TrashID)
	if err != nil {
		return out, err
	}
	if e.ProjectID != in.ProjectID {
		return out, errcode.New(errcode.NotFound, "")
	}
	if e.FilesDigest != in.FilesDigest {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return out, err
	}
	return identity.HumanAction{Action: in.Action, ProjectID: in.ProjectID, ResourceID: in.TrashID, ResourceRevision: in.ExpectedRevision, Request: raw}, nil
}
func (l *Lifecycle) MutateTrashHuman(ctx context.Context, who authz.Context, in TrashMutation, grant, child ids.ID, human HumanCommands) (commands.Receipt, error) {
	if human == nil {
		return commands.Receipt{}, errcode.New(errcode.HumanProofRequired, "")
	}
	action, err := l.MutationHumanAction(ctx, in)
	if err != nil {
		return commands.Receipt{}, err
	}
	e, err := l.entry(ctx, in.TrashID)
	if err != nil {
		return commands.Receipt{}, err
	}
	result, err := human.AcceptHumanItem(ctx, who, grant, child, action, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}, Assets: []string{string(e.AssetID)}}, &humanTrashMutation{l, in})
	if err != nil {
		return commands.Receipt{}, err
	}
	if in.Action == identity.ActPurge {
		return l.Resume(ctx, result.OperationID)
	}
	return result, nil
}

type humanTrashMutation struct {
	l  *Lifecycle
	in TrashMutation
}

func (h *humanTrashMutation) Receipt(ctx context.Context, op ids.ID) (commands.Receipt, error) {
	r, err := h.l.ledger.store.ReceiptByOperation(ctx, h.l.ledger.db, op)
	if err != nil {
		return commands.Receipt{}, err
	}
	return *r, nil
}
func (h *humanTrashMutation) Commit(ctx context.Context, cmd commands.Context, _ identity.HumanAction) (commands.Receipt, error) {
	l := h.l
	s := l.ledger
	in := h.in
	if prior, err := s.lookup(ctx, s.db, cmd); err != nil {
		return commands.Receipt{}, err
	} else if prior != nil {
		return *prior, nil
	}
	entry, err := l.entry(ctx, in.TrashID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if entry.ProjectID != in.ProjectID || entry.FilesDigest != in.FilesDigest {
		return commands.Receipt{}, errcode.New(errcode.RefMismatch, "")
	}
	if entry.State == "purged" {
		return commands.Receipt{}, errcode.New(errcode.AssetPurged, "")
	}
	if entry.Revision != in.ExpectedRevision {
		return commands.Receipt{}, errcode.New(errcode.PreconditionFailed, "")
	}
	if entry.State != "trashed" || entry.PendingOperationID != "" {
		return commands.Receipt{}, errcode.New(errcode.InvalidStateTransition, "")
	}
	if in.Action == identity.ActPurge {
		return l.acceptPurge(ctx, cmd, entry, true)
	}
	hold := in.Action == identity.ActHold
	if hold == entry.Hold {
		return commands.Receipt{}, errcode.New(errcode.InvalidStateTransition, "hold state already matches")
	}
	response, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		entry.Hold = hold
		entry.Revision++
		if err := saveTrashEntry(ctx, tx, entry); err != nil {
			return commands.Result{}, err
		}
		typ := "trash.unheld"
		if hold {
			typ = "trash.held"
		}
		e, err := s.event(cmd, typ, "trash", entry.ID, entry.Revision, map[string]any{"trash_id": entry.ID, "hold": hold, "reason": in.Reason, "human_grant_id": cmd.HumanGrantID})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: publicTrashEntry(entry), Events: []event.Envelope{e}}, nil
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *response.Receipt, nil
}

func (l *Lifecycle) acceptPurge(ctx context.Context, cmd commands.Context, entry TrashEntry, early bool) (commands.Receipt, error) {
	s := l.ledger
	if entry.Hold {
		return commands.Receipt{}, errcode.New(errcode.TrashHeld, "")
	}
	due, err := clock.Parse(entry.DueAt)
	if err != nil {
		return commands.Receipt{}, err
	}
	if !early && s.clock.Now().Before(due) {
		return commands.Receipt{}, errcode.New(errcode.NotDue, "")
	}
	asset, err := s.AssetControl(ctx, entry.AssetID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if asset.PendingOperationID != "" {
		return commands.Receipt{}, errcode.New(errcode.ResourceBusy, "asset lifecycle operation is pending")
	}
	if entry.WholeAsset {
		if err = l.checkHistoricalPurge(ctx, entry); err != nil {
			return commands.Receipt{}, err
		}
	}
	// The first manifest is the fallback for an interrupted initial description.
	// Do not destroy that only baseline before its metadata revision is accepted.
	if len(entry.VersionIDs) > 0 {
		first, err := readJSON[commit.Committed](ctx, s.db, `SELECT record FROM ledger_versions WHERE asset_id=? ORDER BY version_number LIMIT 1`, entry.AssetID)
		if err != nil {
			return commands.Receipt{}, err
		}
		if slices.Contains(entry.VersionIDs, first.VersionID) {
			if _, err = s.CurrentMetadata(ctx, commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: entry.ProjectID, ID: entry.AssetID}); err != nil {
				if errcode.CodeOf(err) == errcode.NotFound {
					return commands.Receipt{}, errcode.New(errcode.OperationNeedsReconciliation, "persist the asset description before purging its first manifest")
				}
				return commands.Receipt{}, err
			}
		}
	}
	plan, err := readJSON[storage.FileIntent](ctx, s.db, `SELECT plan FROM ledger_lifecycle_ops WHERE operation_id=? AND state='done'`, entry.OriginalOperationID)
	if err != nil {
		return commands.Receipt{}, err
	}
	plan.Action = "purge"
	plan.OperationID = cmd.OperationID
	result, err := s.store.Accept(ctx, s.db, cmd, commands.StagePrepared, []string{string(entry.AssetID)}, func(ctx context.Context, tx *sql.Tx) error {
		for _, id := range entry.VersionIDs {
			v, err := versionControl(ctx, tx, id)
			if err != nil {
				return err
			}
			if v.Lifecycle != "trashed" || v.PendingOperationID != "" {
				return errcode.New(errcode.PreconditionFailed, "")
			}
			v.PendingOperationID = cmd.OperationID
			v.Revision++
			if err = saveVersionControl(ctx, tx, v); err != nil {
				return err
			}
		}
		if entry.WholeAsset {
			a, err := assetControl(ctx, tx, entry.AssetID)
			if err != nil {
				return err
			}
			if a.Lifecycle != "trashed" || a.PendingOperationID != "" {
				return errcode.New(errcode.PreconditionFailed, "")
			}
			a.PendingOperationID = cmd.OperationID
			a.Revision++
			if err = saveAssetControl(ctx, tx, a); err != nil {
				return err
			}
		}
		entry.PendingOperationID = cmd.OperationID
		entry.Revision++
		if err := saveTrashEntry(ctx, tx, entry); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO ledger_lifecycle_ops VALUES(?,?,?,?,?,'accepted')`, cmd.OperationID, entry.ID, "purge", encoded(cmd), encoded(plan))
		return err
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *result.Receipt, nil
}

func (l *Lifecycle) finishPurge(ctx context.Context, tx *sql.Tx, entry *TrashEntry, cmd commands.Context, plan storage.FileIntent) error {
	for _, id := range entry.VersionIDs {
		v, err := versionControl(ctx, tx, id)
		if err != nil {
			return err
		}
		if v.PendingOperationID != cmd.OperationID {
			return errcode.New(errcode.PreconditionFailed, "")
		}
		v.Lifecycle = "purged"
		v.PendingOperationID = ""
		v.Revision++
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
		a.Lifecycle = "purged"
		a.PendingOperationID = ""
		a.Revision++
		if err = saveAssetControl(ctx, tx, a); err != nil {
			return err
		}
	}
	// Blobs remain in CAS. Only the independent >=24h, root/pin checked collector
	// may remove them; repeated hashes get a stable child operation per purge.
	seen := map[string]bool{}
	for _, v := range plan.Versions {
		for _, f := range v.Files {
			if seen[f.SHA256] {
				continue
			}
			seen[f.SHA256] = true
			op, err := ids.DeriveChild(cmd.OperationID, "gc:"+f.SHA256)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_gc_candidates(sha256,operation_id,since) VALUES(?,?,?) ON CONFLICT(sha256) DO UPDATE SET operation_id=excluded.operation_id,since=excluded.since`, f.SHA256, op, clock.Millis(l.ledger.clock.Now())); err != nil {
				return err
			}
		}
	}
	entry.State = "purged"
	return nil
}

// Entry exposes a tombstone only after current project and original-object risk
// checks. Purged content cannot be inspected, but the deleting actor/owner can
// still see its bounded ledger receipt and identifiers.
func (l *Lifecycle) Entry(ctx context.Context, who authz.Context, project, id ids.ID) (TrashEntry, error) {
	s := l.ledger
	ctx, h, err := s.gate.Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return TrashEntry{}, err
	}
	defer h.Release()
	return l.visibleEntry(ctx, who, project, id)
}

// visibleEntry inherits the caller security guard.
func (l *Lifecycle) visibleEntry(ctx context.Context, who authz.Context, project, id ids.ID) (TrashEntry, error) {
	s := l.ledger
	if err := s.authorize(ctx, who, "catalog.read", project, "project", project); err != nil {
		return TrashEntry{}, err
	}
	entry, err := l.entry(ctx, id)
	if err != nil {
		return TrashEntry{}, err
	}
	if entry.ProjectID != project {
		return TrashEntry{}, errcode.New(errcode.NotFound, "")
	}
	if entry.CreatedBy != who.PrincipalID {
		if err = s.authorize(ctx, who, "ledger.restore_others", project, "trash", id); err != nil {
			return TrashEntry{}, err
		}
	}
	if entry.State == "purged" || entry.PendingOperationID != "" || len(entry.VersionIDs) == 0 {
		// Files may already be unavailable: do not reuse historic access for
		// user text. Only the bounded receipt/identifiers survive this read.
		entry.OriginalSlug = ""
		entry.RestoreSlug = ""
		entry.Reason = ""
	} else {
		for _, id := range entry.VersionIDs {
			v, err := s.Version(ctx, entry.AssetID, id)
			if err != nil {
				return TrashEntry{}, err
			}
			d, err := l.rights.EvaluateRiskAccess(ctx, who, v.Ref(who.InstanceID))
			if err != nil {
				return TrashEntry{}, err
			}
			if err = d.Err(); err != nil {
				return TrashEntry{}, err
			}
		}
	}
	return entry, nil
}

func (l *Lifecycle) validateMutationTarget(ctx context.Context, a identity.HumanAction) error {
	var in TrashMutation
	if err := json.Unmarshal(a.Request, &in); err != nil {
		return err
	}
	expected, err := l.MutationHumanAction(ctx, in)
	if err != nil {
		return err
	}
	left, err := canonjson.CanonicalizeValue(a)
	if err != nil {
		return err
	}
	right, err := canonjson.CanonicalizeValue(expected)
	if err != nil {
		return err
	}
	if string(left) != string(right) {
		return errcode.New(errcode.HumanGrantMismatch, "")
	}
	return nil
}

// Old independent trash keeps its own retention and hold. Purging the asset's
// container would otherwise make those older versions impossible to restore.
func (l *Lifecycle) checkHistoricalPurge(ctx context.Context, entry TrashEntry) error {
	rows, err := l.ledger.db.QueryContext(ctx, `SELECT v.version_id,s.lifecycle,s.pending_operation_id FROM ledger_versions v JOIN ledger_version_states s ON s.version_id=v.version_id WHERE v.asset_id=?`, entry.AssetID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, pending ids.ID
		var state string
		if err = rows.Scan(&id, &state, &pending); err != nil {
			return err
		}
		if slices.Contains(entry.VersionIDs, id) {
			continue
		}
		if state != "purged" || pending != "" {
			return errcode.New(errcode.AssetInUse, "independent historical trash must finish retention or receive its own purge approval first")
		}
	}
	return rows.Err()
}
