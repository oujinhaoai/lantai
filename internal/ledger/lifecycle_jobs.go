package ledger

import (
	"context"
	"database/sql"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/storage"
)

// LifecycleJobs is the trusted T08 job authority, not an Agent or public auth
// context. Its implementation must verify a registered server job and its exact
// original command/epoch. Nil never enables scheduled deletion.
type LifecycleJobs interface {
	CheckLifecycleJob(context.Context, commands.Context, string, ids.ID) error
}
type DueTrashRequest struct {
	ProjectID        ids.ID `json:"project_id"`
	TrashID          ids.ID `json:"trash_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func (l *Lifecycle) RunDueJob(ctx context.Context, cmd commands.Context, in DueTrashRequest, authority LifecycleJobs) (commands.Receipt, error) {
	if authority == nil {
		return commands.Receipt{}, errcode.New(errcode.Forbidden, "server job authority required")
	}
	if cmd.CommandType != "ledger.purge_due" && cmd.CommandType != "ledger.purge_reminder" {
		return commands.Receipt{}, invalid("unsupported lifecycle job")
	}
	if !in.ProjectID.Valid() || !in.TrashID.Valid() || in.ExpectedRevision < 1 || cmd.ProjectID != in.ProjectID || cmd.HumanGrantID != "" {
		return commands.Receipt{}, invalid("fixed server job request required")
	}
	if err := cmd.Validate(); err != nil {
		return commands.Receipt{}, err
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return commands.Receipt{}, err
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: cmd.CommandType, ProjectID: in.ProjectID, Body: raw})
	if err != nil {
		return commands.Receipt{}, err
	}
	if hash != cmd.RequestHash {
		return commands.Receipt{}, errcode.New(errcode.IdempotencyConflict, "")
	}
	s := l.ledger
	var receipt commands.Receipt
	err = func() error {
		ctx, h, err := s.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}})
		if err != nil {
			return err
		}
		defer h.Release()
		if err = authority.CheckLifecycleJob(ctx, cmd, cmd.CommandType, in.TrashID); err != nil {
			return err
		}
		epoch, err := s.authority.RecoveryEpoch(ctx)
		if err != nil {
			return err
		}
		if cmd.RecoveryEpoch != epoch {
			return errcode.New(errcode.PreconditionFailed, "server job recovery epoch changed")
		}
		if prior, err := s.lookup(ctx, s.db, cmd); err != nil {
			return err
		} else if prior != nil {
			receipt = *prior
			return nil
		}
		entry, err := l.entry(ctx, in.TrashID)
		if err != nil {
			return err
		}
		if entry.ProjectID != in.ProjectID {
			return errcode.New(errcode.NotFound, "")
		}
		if entry.Revision != in.ExpectedRevision {
			return errcode.New(errcode.PreconditionFailed, "")
		}
		if entry.State != "trashed" || entry.PendingOperationID != "" {
			return errcode.New(errcode.InvalidStateTransition, "")
		}
		if cmd.CommandType == "ledger.purge_due" {
			receipt, err = l.acceptPurge(ctx, cmd, entry, false)
			return err
		}
		due, err := clock.Parse(entry.DueAt)
		if err != nil {
			return err
		}
		if s.clock.Now().Before(due.Add(-72 * time.Hour)) {
			return errcode.New(errcode.NotDue, "")
		}
		response, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
			result := commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: entry}
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ledger_trash_reminders WHERE trash_id=?`, entry.ID).Scan(&n); err != nil {
				return result, err
			}
			if n != 0 {
				return result, nil
			}
			e, err := s.event(cmd, "trash.purge_due", "trash", entry.ID, entry.Revision, map[string]any{"trash_id": entry.ID, "due_at": entry.DueAt, "hold": entry.Hold})
			if err != nil {
				return result, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_trash_reminders VALUES(?,?,?)`, entry.ID, cmd.OperationID, clock.Millis(s.clock.Now())); err != nil {
				return result, err
			}
			result.Events = []event.Envelope{e}
			return result, nil
		})
		if err != nil {
			return err
		}
		receipt = *response.Receipt
		return nil
	}()
	if err != nil {
		return commands.Receipt{}, err
	}
	if cmd.CommandType == "ledger.purge_due" {
		return l.Resume(ctx, receipt.OperationID)
	}
	return receipt, nil
}

type LifecycleFailure struct {
	OperationID ids.ID       `json:"operation_id"`
	TrashID     ids.ID       `json:"trash_id"`
	Code        errcode.Code `json:"code"`
	Attempts    int64        `json:"attempts"`
	LastAt      time.Time    `json:"last_at"`
}

func (l *Lifecycle) recordFileFailure(ctx context.Context, op ids.ID, cause error) error {
	ctx, h, err := l.ledger.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive})
	if err != nil {
		return err
	}
	defer h.Release()
	code := errcode.CodeOf(cause)
	if code == "" {
		code = errcode.OperationNeedsReconciliation
	}
	_, err = l.ledger.db.ExecContext(ctx, `INSERT INTO ledger_lifecycle_failures(operation_id,code,attempts,last_at) VALUES(?,?,1,?) ON CONFLICT(operation_id) DO UPDATE SET code=excluded.code,attempts=attempts+1,last_at=excluded.last_at`, op, code, clock.Millis(l.ledger.clock.Now()))
	return err
}

// Failures is a trusted maintenance inventory; public callers use authorized
// entry reads. A physical retry clears the failure only in the business commit.
func (l *Lifecycle) Failures(ctx context.Context) ([]LifecycleFailure, error) {
	rows, err := l.ledger.db.QueryContext(ctx, `SELECT f.operation_id,o.trash_id,f.code,f.attempts,f.last_at FROM ledger_lifecycle_failures f JOIN ledger_lifecycle_ops o USING(operation_id) ORDER BY f.last_at,f.operation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LifecycleFailure{}
	for rows.Next() {
		var f LifecycleFailure
		var at int64
		if err = rows.Scan(&f.OperationID, &f.TrashID, &f.Code, &f.Attempts, &at); err != nil {
			return nil, err
		}
		f.LastAt = clock.FromMillis(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// Candidate uses authoritative operations rather than a possibly rebuilding
// reference index. Unknown prepared work prevents GC; the file adapter then
// verifies the complete root set, every manifest and all retention pin sources.
func (l *Lifecycle) Candidate(ctx context.Context, sha string) (storage.GCCandidate, error) {
	open, err := l.ledger.OpenOperations(ctx)
	if err != nil {
		return storage.GCCandidate{}, err
	}
	if len(open) != 0 {
		return storage.GCCandidate{}, errcode.New(errcode.OperationNeedsReconciliation, "pending ledger operations prevent GC")
	}
	var out storage.GCCandidate
	var at int64
	err = l.ledger.db.QueryRowContext(ctx, `SELECT operation_id,since FROM ledger_gc_candidates WHERE sha256=?`, sha).Scan(&out.OperationID, &at)
	if err != nil {
		return out, missing(err)
	}
	out.Since = clock.FromMillis(at)
	return out, nil
}
func (l *Lifecycle) ManifestRoots(ctx context.Context) ([]storage.ManifestRoot, error) {
	s := l.ledger
	out := []storage.ManifestRoot{}
	for after := ids.ID(""); ; {
		page, err := s.Versions(ctx, after, 1000)
		if err != nil {
			return nil, err
		}
		for _, v := range page {
			location, err := s.VersionFileLocation(ctx, v.AssetID, v.VersionID)
			if err != nil {
				return nil, err
			}
			if location.Purged {
				continue
			}
			if location.PendingOperationID != "" {
				return nil, errcode.New(errcode.OperationNeedsReconciliation, "pending lifecycle prevents a stable GC root scan")
			}
			proof, err := readJSON[install.Proof](ctx, s.db, `SELECT proof FROM ledger_prepared WHERE operation_id=? AND proof IS NOT NULL`, v.OperationID)
			if err != nil {
				return nil, err
			}
			d, err := proof.Digest()
			if err != nil {
				return nil, err
			}
			if d != v.ProofDigest || proof.ManifestDigest != v.ManifestDigest || proof.VersionID != v.VersionID {
				return nil, errcode.New(errcode.HashMismatch, "")
			}
			out = append(out, storage.ManifestRoot{Proof: proof, TrashID: location.TrashID})
		}
		if len(page) < 1000 {
			break
		}
		after = page[len(page)-1].VersionID
	}
	return out, nil
}
