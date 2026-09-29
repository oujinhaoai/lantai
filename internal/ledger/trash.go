package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type TrashEntry struct {
	PriorAssetLifecycle    string               `json:"prior_asset_lifecycle"`
	PriorVersionLifecycles map[ids.ID]string    `json:"prior_version_lifecycles"`
	RestoreGeneration      int64                `json:"restore_generation,omitempty"`
	FilesDigest            digest.Digest        `json:"files_digest"`
	ID                     ids.ID               `json:"trash_id"`
	ProjectID              ids.ID               `json:"project_id"`
	AssetID                ids.ID               `json:"asset_id"`
	VersionIDs             []ids.ID             `json:"version_ids"`
	WholeAsset             bool                 `json:"whole_asset"`
	History                []TrashHistoryTarget `json:"history,omitempty"`
	OriginalSlug           string               `json:"original_slug"`
	OriginalGeneration     int64                `json:"original_generation"`
	CreatedBy              ids.ID               `json:"created_by"`
	CreatedAt              string               `json:"created_at"`
	DueAt                  string               `json:"due_at"`
	Grace                  bool                 `json:"grace"`
	RetentionDays          int                  `json:"retention_days"`
	State                  string               `json:"state"`
	Revision               int64                `json:"revision"`
	Hold                   bool                 `json:"hold"`
	Reason                 string               `json:"reason"`
	OriginalOperationID    ids.ID               `json:"original_operation_id"`
	PendingOperationID     ids.ID               `json:"pending_operation_id,omitempty"`
	RestoreSlug            string               `json:"restore_slug,omitempty"`
}

func (l *Lifecycle) entry(ctx context.Context, id ids.ID) (TrashEntry, error) {
	return readJSON[TrashEntry](ctx, l.ledger.db, `SELECT record FROM ledger_trash_entries WHERE trash_id=?`, id)
}
func saveTrashEntry(ctx context.Context, tx *sql.Tx, e TrashEntry) error {
	due, err := clock.Parse(e.DueAt)
	if err != nil {
		return err
	}
	hold := 0
	if e.Hold {
		hold = 1
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ledger_trash_entries VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(trash_id) DO UPDATE SET state=excluded.state,revision=excluded.revision,due_at=excluded.due_at,hold=excluded.hold,record=excluded.record`, e.ID, e.ProjectID, e.AssetID, e.State, e.Revision, clock.Millis(due), hold, encoded(e))
	return err
}

// TrashHumanAction binds each exact asset/version set and its current physical
// inventory. A force grant is distinct and can be issued only to human admins.
func (l *Lifecycle) TrashHumanAction(ctx context.Context, in TrashRequest, force bool) (identity.HumanAction, error) {
	var a identity.HumanAction
	if err := in.TrashSelector.validate(); err != nil {
		return a, err
	}
	if in.AssetRevision < 1 || len(in.Targets)+len(in.History) == 0 || len(in.Targets)+len(in.History) > 1000 || !in.FilesDigest.Valid() {
		return a, invalid("fixed trash revisions and inventory required")
	}
	asset, err := l.ledger.Asset(ctx, in.AssetID)
	if err != nil {
		return a, err
	}
	if asset.ProjectID != in.ProjectID {
		return a, errcode.New(errcode.RefMismatch, "")
	}
	seen := map[ids.ID]bool{}
	targets := append([]TrashVersionTarget{}, in.Targets...)
	for _, history := range in.History {
		if !in.WholeAsset || !slices.Contains([]string{"trashed", "purged"}, history.Lifecycle) || !history.TrashID.Valid() {
			return a, invalid("invalid historical trash target")
		}
		targets = append(targets, history.TrashVersionTarget)
	}
	for _, target := range targets {
		if !target.VersionID.Valid() || target.Revision < 1 || !target.ManifestDigest.Valid() || seen[target.VersionID] {
			return a, invalid("invalid or duplicate trash target")
		}
		seen[target.VersionID] = true
		v, err := l.ledger.Version(ctx, in.AssetID, target.VersionID)
		if err != nil {
			return a, err
		}
		if v.ManifestDigest != target.ManifestDigest {
			return a, errcode.New(errcode.RefMismatch, "")
		}
	}
	if !in.WholeAsset && (len(in.Targets) != 1 || in.Targets[0].VersionID != in.VersionID) {
		return a, invalid("single version deletion target differs")
	}
	action := authz.Action("ledger.trash")
	if force {
		action = "ledger.force_trash"
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return a, err
	}
	id, revision := in.AssetID, in.AssetRevision
	if !in.WholeAsset {
		id, revision = in.VersionID, in.Targets[0].Revision
		a.ManifestDigest = in.Targets[0].ManifestDigest
	}
	a.Action = action
	a.ProjectID = in.ProjectID
	a.ResourceID = id
	a.ResourceRevision = revision
	a.Request = raw
	return a, nil
}
func (l *Lifecycle) ValidateHumanTarget(ctx context.Context, who authz.Context, a identity.HumanAction) error {
	if a.Action == identity.ActReleaseName {
		return l.validateNameReleaseTarget(ctx, a)
	}
	if a.Action == identity.ActPurge || a.Action == identity.ActHold || a.Action == identity.ActUnhold {
		return l.validateMutationTarget(ctx, a)
	}
	if a.Action != "ledger.trash" && a.Action != "ledger.force_trash" {
		return l.ledger.ValidateHumanTarget(ctx, who, a)
	}
	var in TrashRequest
	if err := json.Unmarshal(a.Request, &in); err != nil {
		return err
	}
	expected, err := l.TrashHumanAction(ctx, in, a.Action == "ledger.force_trash")
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
func (l *Lifecycle) TrashHuman(ctx context.Context, who authz.Context, in TrashRequest, force bool, grant, child ids.ID, human HumanCommands) (commands.Receipt, error) {
	if human == nil {
		return commands.Receipt{}, errcode.New(errcode.HumanProofRequired, "")
	}
	a, err := l.TrashHumanAction(ctx, in, force)
	if err != nil {
		return commands.Receipt{}, err
	}
	receipt, err := human.AcceptHumanItem(ctx, who, grant, child, a, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}, Assets: []string{string(in.AssetID)}}, &humanTrash{l, who, in, force})
	if err != nil {
		return commands.Receipt{}, err
	}
	if receipt.Status == commands.ReceiptSucceeded {
		return receipt, nil
	}
	return l.Resume(ctx, receipt.OperationID)
}

type humanTrash struct {
	l     *Lifecycle
	who   authz.Context
	in    TrashRequest
	force bool
}

func (h *humanTrash) Receipt(ctx context.Context, id ids.ID) (commands.Receipt, error) {
	r, err := h.l.ledger.store.ReceiptByOperation(ctx, h.l.ledger.db, id)
	if err != nil {
		return commands.Receipt{}, err
	}
	return *r, nil
}
func (h *humanTrash) Commit(ctx context.Context, cmd commands.Context, _ identity.HumanAction) (commands.Receipt, error) {
	return h.l.acceptTrash(ctx, h.who, cmd, h.in, true, h.force)
}

// TrashOwn is limited to actual Agent principals. It cannot borrow a human's
// grant or bypass quotas by using another session/model of the same principal.
func (l *Lifecycle) TrashOwn(ctx context.Context, who authz.Context, key string, in TrashRequest) (commands.Receipt, error) {
	if who.PrincipalKind != authz.Agent {
		return commands.Receipt{}, errcode.New(errcode.Forbidden, "")
	}
	if _, err := l.TrashHumanAction(ctx, in, false); err != nil {
		return commands.Receipt{}, err
	}
	cmd, err := l.ledger.commandContext(ctx, who, key, "ledger.trash_own", in.ProjectID, in)
	if err != nil {
		return commands.Receipt{}, err
	}
	var receipt commands.Receipt
	err = func() error {
		ctx, h, err := l.ledger.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}, Assets: []string{string(in.AssetID)}})
		if err != nil {
			return err
		}
		defer h.Release()
		if err = l.ledger.authorize(ctx, who, "ledger.trash_own", in.ProjectID, "asset", in.AssetID); err != nil {
			return err
		}
		receipt, err = l.acceptTrash(ctx, who, cmd, in, false, false)
		return err
	}()
	if err != nil {
		return commands.Receipt{}, err
	}
	if receipt.Status == commands.ReceiptSucceeded {
		return receipt, nil
	}
	return l.Resume(ctx, receipt.OperationID)
}
func (l *Lifecycle) acceptTrash(ctx context.Context, who authz.Context, cmd commands.Context, in TrashRequest, human, force bool) (commands.Receipt, error) {
	s := l.ledger
	prior, err := s.lookup(ctx, s.db, cmd)
	if err != nil {
		return commands.Receipt{}, err
	}
	if prior != nil {
		if commands.Decide(prior, cmd.RequestHash, s.clock.Now()) == commands.OutcomeExpired {
			return commands.Receipt{}, errcode.New(errcode.IdempotencyResultExpired, "")
		}
		return *prior, nil
	}
	snapshot, err := l.snapshot(ctx, who, in.TrashSelector)
	if err != nil {
		return commands.Receipt{}, err
	}
	expected, err := canonjson.CanonicalizeValue(snapshot.request)
	if err != nil {
		return commands.Receipt{}, err
	}
	actual, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return commands.Receipt{}, err
	}
	if string(expected) != string(actual) {
		return commands.Receipt{}, errcode.New(errcode.PreconditionFailed, "trash targets, uses or file inventory changed")
	}
	snapshot.facts.humanAuthorized = human
	snapshot.facts.adminForceAuthorized = force
	id, err := s.ids.New()
	if err != nil {
		return commands.Receipt{}, err
	}
	response, err := s.store.Accept(ctx, s.db, cmd, commands.StagePrepared, []string{string(in.AssetID)}, func(ctx context.Context, tx *sql.Tx) error {
		f := snapshot.facts
		f.now = s.clock.Now()
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(CASE WHEN grace=0 THEN units ELSE 0 END),0),COALESCE(sum(CASE WHEN grace=1 THEN units ELSE 0 END),0) FROM ledger_trash_quota WHERE principal_id=? AND (state='reserved' OR (state='committed' AND created_at>?))`, who.PrincipalID, clock.Millis(f.now.Add(-time.Hour))).Scan(&f.normalReserved, &f.graceReserved); err != nil {
			return err
		}
		decision, err := decideTrash(f)
		if err != nil {
			return err
		}
		entry := TrashEntry{FilesDigest: in.FilesDigest, ID: id, ProjectID: in.ProjectID, AssetID: in.AssetID, VersionIDs: []ids.ID{}, WholeAsset: in.WholeAsset, History: in.History, OriginalSlug: snapshot.asset.Slug, OriginalGeneration: snapshot.asset.Generation, CreatedBy: who.PrincipalID, CreatedAt: clock.Format(f.now), DueAt: clock.Format(f.now.Add(time.Duration(decision.retentionDays) * 24 * time.Hour)), Grace: decision.grace, RetentionDays: decision.retentionDays, State: "pending", Revision: 1, Reason: in.Reason, OriginalOperationID: cmd.OperationID, PendingOperationID: cmd.OperationID}
		entry.PriorVersionLifecycles = map[ids.ID]string{}
		for _, target := range in.Targets {
			v, err := versionControl(ctx, tx, target.VersionID)
			if err != nil {
				return err
			}
			if v.Revision != target.Revision || v.PendingOperationID != "" {
				return errcode.New(errcode.PreconditionFailed, "")
			}
			entry.PriorVersionLifecycles[target.VersionID] = v.Lifecycle
			v.PendingOperationID = cmd.OperationID
			v.Revision++
			entry.VersionIDs = append(entry.VersionIDs, target.VersionID)
			if err = saveVersionControl(ctx, tx, v); err != nil {
				return err
			}
		}
		if in.WholeAsset {
			a, err := assetControl(ctx, tx, in.AssetID)
			if err != nil {
				return err
			}
			if a.Revision != in.AssetRevision || a.PendingOperationID != "" {
				return errcode.New(errcode.PreconditionFailed, "")
			}
			entry.PriorAssetLifecycle = a.Lifecycle
			a.PendingOperationID = cmd.OperationID
			a.Revision++
			if err = saveAssetControl(ctx, tx, a); err != nil {
				return err
			}
		}
		if err = saveTrashEntry(ctx, tx, entry); err != nil {
			return err
		}
		plan := storage.FileIntent{OperationID: cmd.OperationID, TrashID: id, Action: "trash", Versions: snapshot.proofs, Records: snapshot.records}
		if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_lifecycle_ops VALUES(?,?,?,?,?,'accepted')`, cmd.OperationID, id, "trash", encoded(cmd), encoded(plan)); err != nil {
			return err
		}
		if decision.quotaUnits > 0 {
			grace := 0
			if decision.grace {
				grace = 1
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_trash_quota VALUES(?,?,?,?,?,'reserved')`, cmd.OperationID, who.PrincipalID, clock.Millis(f.now), decision.quotaUnits, grace); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *response.Receipt, nil
}

// AcceptedFileIntent is T02's trusted port. It returns only a durable accepted
// plan whose entire target set is still behind that operation's write/read ban.
func (l *Lifecycle) AcceptedFileIntent(ctx context.Context, op ids.ID) (storage.FileIntent, error) {
	return l.acceptedFileIntent(ctx, op, true)
}

func (l *Lifecycle) acceptedFileIntent(ctx context.Context, op ids.ID, requireEpoch bool) (storage.FileIntent, error) {
	s := l.ledger
	var raw, state string
	if err := s.db.QueryRowContext(ctx, `SELECT plan,state FROM ledger_lifecycle_ops WHERE operation_id=?`, op).Scan(&raw, &state); err != nil {
		return storage.FileIntent{}, missing(err)
	}
	var plan storage.FileIntent
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return plan, err
	}
	if plan.OperationID != op {
		return plan, errcode.New(errcode.RefMismatch, "")
	}
	// Done plans cannot authorize another physical application after restore or
	// purge. Receipt replay is resolved by Resume without calling the file adapter.
	if state != "accepted" {
		return plan, errcode.New(errcode.InvalidStateTransition, "file intent is no longer active")
	}
	cmd, err := readJSON[commands.Context](ctx, s.db, `SELECT command FROM ledger_lifecycle_ops WHERE operation_id=?`, op)
	if err != nil {
		return plan, err
	}
	epoch, err := s.authority.RecoveryEpoch(ctx)
	if err != nil {
		return plan, err
	}
	if requireEpoch && cmd.RecoveryEpoch != epoch {
		return plan, errcode.New(errcode.OperationNeedsReconciliation, "accepted intent belongs to an earlier recovery epoch")
	}
	entry, err := l.entry(ctx, plan.TrashID)
	if err != nil {
		return plan, err
	}
	if entry.PendingOperationID != op {
		return plan, errcode.New(errcode.PreconditionFailed, "")
	}
	if entry.WholeAsset {
		asset, err := s.AssetControl(ctx, entry.AssetID)
		if err != nil {
			return plan, err
		}
		if asset.PendingOperationID != op {
			return plan, errcode.New(errcode.PreconditionFailed, "asset lifecycle write ban changed")
		}
	}
	if len(plan.Versions) != len(entry.VersionIDs) || len(plan.Versions) == 0 && (!entry.WholeAsset || len(entry.History) == 0 || len(plan.Records) != 0) {
		return plan, errcode.New(errcode.RefMismatch, "intent targets differ from trash entry")
	}
	rawDigest, err := canonjson.CanonicalizeValue(struct {
		Versions []install.Proof           `json:"versions"`
		Records  []storage.LifecycleRecord `json:"records"`
	}{plan.Versions, plan.Records})
	if err != nil {
		return plan, err
	}
	if digest.Of(rawDigest) != entry.FilesDigest {
		return plan, errcode.New(errcode.HashMismatch, "intent file inventory changed")
	}
	for _, proof := range plan.Versions {
		if proof.AssetID != entry.AssetID || proof.ProjectID != entry.ProjectID || !slices.Contains(entry.VersionIDs, proof.VersionID) {
			return plan, errcode.New(errcode.RefMismatch, "")
		}
		v, err := s.VersionControl(ctx, proof.VersionID)
		if err != nil {
			return plan, err
		}
		if v.PendingOperationID != op {
			return plan, errcode.New(errcode.PreconditionFailed, "lifecycle write ban changed")
		}
	}
	return plan, nil
}

// Resume completes an already accepted exact intent; this is a trusted recovery
// port, not a user permission-bearing endpoint. Authorization happens before the
// durable write ban. Recovery cannot add targets, alter a plan or accept a new
// deletion, and it never treats files already moved as proof of business commit.
func (l *Lifecycle) Resume(ctx context.Context, op ids.ID) (commands.Receipt, error) {
	outer := ctx
	s := l.ledger
	receipt, err := s.store.ReceiptByOperation(ctx, s.db, op)
	if err != nil {
		return commands.Receipt{}, err
	}
	if receipt.Status == commands.ReceiptFailed {
		return *receipt, nil
	}
	if receipt.Status == commands.ReceiptSucceeded {
		if err = l.repairRestoredAliases(ctx, *receipt); err != nil {
			return commands.Receipt{}, err
		}
		return *receipt, nil
	}
	plan, err := l.AcceptedFileIntent(ctx, op)
	if err != nil {
		return commands.Receipt{}, err
	}
	if len(plan.Versions) > 0 {
		if err = l.files.ApplyFileIntent(ctx, op, l); err != nil {
			failureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return commands.Receipt{}, errors.Join(err, l.recordFileFailure(failureCtx, op, err))
		}
	}
	entry, err := l.entry(ctx, plan.TrashID)
	if err != nil {
		return commands.Receipt{}, err
	}
	ctx, h, err := s.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(entry.ProjectID)}, Assets: []string{string(entry.AssetID)}})
	if err != nil {
		return commands.Receipt{}, err
	}
	defer h.Release()
	if _, err = l.AcceptedFileIntent(ctx, op); err != nil {
		return commands.Receipt{}, err
	}
	cmd, err := readJSON[commands.Context](ctx, s.db, `SELECT command FROM ledger_lifecycle_ops WHERE operation_id=?`, op)
	if err != nil {
		return commands.Receipt{}, err
	}
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		prior, err := s.store.ReceiptByOperation(ctx, tx, op)
		if err != nil {
			return err
		}
		if prior.Status == commands.ReceiptSucceeded {
			return nil
		}
		entry, err = readJSON[TrashEntry](ctx, tx, `SELECT record FROM ledger_trash_entries WHERE trash_id=?`, entry.ID)
		if err != nil {
			return err
		}
		if entry.PendingOperationID != op {
			return errcode.New(errcode.PreconditionFailed, "")
		}
		switch plan.Action {
		case "trash":
			err = l.finishTrash(ctx, tx, &entry, op)
		case "restore":
			err = l.finishRestore(ctx, tx, &entry, cmd)
		case "purge":
			err = l.finishPurge(ctx, tx, &entry, cmd, plan)
		default:
			err = errcode.New(errcode.InvalidStateTransition, "unsupported lifecycle completion")
		}
		if err != nil {
			return err
		}
		entry.Revision++
		entry.PendingOperationID = ""
		if err = saveTrashEntry(ctx, tx, entry); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_trash_quota SET state='committed',created_at=? WHERE operation_id=? AND state='reserved'`, clock.Millis(s.clock.Now()), op); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_lifecycle_ops SET state='done' WHERE operation_id=?`, op); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM ledger_lifecycle_failures WHERE operation_id=?`, op); err != nil {
			return err
		}
		e, err := s.event(cmd, map[string]string{"trash": "trash.created", "restore": "trash.restored", "purge": "trash.purged"}[plan.Action], "trash", entry.ID, entry.Revision, map[string]any{"trash_id": entry.ID, "asset_id": entry.AssetID, "version_ids": entry.VersionIDs, "due_at": entry.DueAt, "human_grant_id": cmd.HumanGrantID})
		if err != nil {
			return err
		}
		return s.store.Complete(ctx, tx, op, commands.StagePrepared, commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: publicTrashEntry(entry), Events: []event.Envelope{e}})
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	receipt, err = s.store.ReceiptByOperation(ctx, s.db, op)
	if err != nil {
		return commands.Receipt{}, err
	}
	h.Release()
	if err = l.repairRestoredAliases(outer, *receipt); err != nil {
		return commands.Receipt{}, err
	}
	return *receipt, nil
}

func (l *Lifecycle) finishTrash(ctx context.Context, tx *sql.Tx, entry *TrashEntry, op ids.ID) error {
	for _, id := range entry.VersionIDs {
		v, err := versionControl(ctx, tx, id)
		if err != nil {
			return err
		}
		if v.PendingOperationID != op {
			return errcode.New(errcode.PreconditionFailed, "")
		}
		v.Lifecycle = "trashed"
		v.PendingOperationID = ""
		v.Revision++
		v.ReviewState = "withdrawn"
		v.EffectiveReviewID = ""
		v.ReviewTargetID = ""
		v.WithdrawalReason = "trashed"
		if err = saveVersionControl(ctx, tx, v); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_publication_requests SET status='cancelled',failure_code='REVIEW_TARGET_STALE' WHERE version_id=? AND status IN ('pending','failed')`, id); err != nil {
			return err
		}
	}
	if entry.WholeAsset {
		a, err := assetControl(ctx, tx, entry.AssetID)
		if err != nil {
			return err
		}
		if a.PendingOperationID != op {
			return errcode.New(errcode.PreconditionFailed, "")
		}
		a.Lifecycle = "trashed"
		a.PendingOperationID = ""
		a.Revision++
		if err = saveAssetControl(ctx, tx, a); err != nil {
			return err
		}
		claim, err := readJSON[commit.Claim](ctx, tx, `SELECT record FROM ledger_namespace_claims WHERE project_id=? AND slug_key=?`, entry.ProjectID, pathrule.Key(entry.OriginalSlug))
		if err != nil {
			return err
		}
		if claim.AssetID != entry.AssetID || claim.Generation != entry.OriginalGeneration || claim.State != commit.ClaimActive {
			return errcode.New(errcode.OperationNeedsReconciliation, "namespace changed under lifecycle ban")
		}
		// Ordinary trash and purge keep occupying the name. Only the fixed
		// fresh-asset grace decision permits automatic release.
		if !entry.Grace {
			entry.State = "trashed"
			return nil
		}
		claim.State = commit.ClaimReleased
		claim.Revision++
		claim.OperationID = op
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_namespace_claims SET record=? WHERE project_id=? AND slug_key=?`, encoded(claim), entry.ProjectID, pathrule.Key(entry.OriginalSlug)); err != nil {
			return err
		}
	}
	entry.State = "trashed"
	return nil
}

// Logical-only containers retain private namespace data internally, but no
// surviving version can prove permission to reveal it in a public receipt.
func publicTrashEntry(entry TrashEntry) TrashEntry {
	if len(entry.VersionIDs) == 0 {
		entry.OriginalSlug = ""
		entry.RestoreSlug = ""
		entry.Reason = ""
	}
	return entry
}
