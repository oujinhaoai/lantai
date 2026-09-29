package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// DueTrash is a ledger inventory row offered to the scheduler.
type DueTrash struct {
	ProjectID ids.ID
	TrashID   ids.ID
	Revision  int64
	DueAt     time.Time
	Reminded  bool
}

// DueJob is the fixed request body of a scheduled ledger job. Its JSON field
// names equal ledger.DueTrashRequest, so both sides hash the same body.
type DueJob struct {
	ProjectID        ids.ID `json:"project_id"`
	TrashID          ids.ID `json:"trash_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// GCCandidate is a purged content hash accepted by T03 for collection.
type GCCandidate struct {
	SHA256      string
	OperationID ids.ID
	Since       time.Time
}

// LifecyclePorts adapt the owners. The scheduler never reads ledger or storage
// tables: T03 decides whether a purge is allowed, T02 performs byte actions
// after re-reading candidates, all authoritative roots and every pin source.
type LifecyclePorts interface {
	DueTrash(context.Context, time.Time, int) ([]DueTrash, error)
	RunDue(context.Context, commands.Context, DueJob) error
	GCCandidates(context.Context, string, int) ([]GCCandidate, error)
	GCState(context.Context, ids.ID) (string, error)
	CollectBlob(context.Context, string) (bool, error)
	PendingGC(context.Context) ([]string, error)
	RetryFileFailures(context.Context) (int, error)
}

// LifecycleScheduler runs due reminders, due purges and GC as recoverable
// server jobs. Every job is registered with a fixed operation, idempotency
// key, request digest and recovery epoch before its owner runs it; a retry
// after a crash reuses exactly that registration, never a new key.
type LifecycleScheduler struct {
	db       *sql.DB
	gate     *commands.Gate
	epochs   authz.EpochSource
	ports    LifecyclePorts
	clock    clock.Clock
	ids      *ids.Generator
	instance ids.ID
	batch    int
	mu       sync.Mutex
}

type SchedulerDeps struct {
	Runtime  *sql.DB
	Gate     *commands.Gate
	Epochs   authz.EpochSource
	Ports    LifecyclePorts
	Clock    clock.Clock
	IDs      *ids.Generator
	Instance ids.ID
	Batch    int
}

func NewLifecycleScheduler(d SchedulerDeps) (*LifecycleScheduler, error) {
	if d.Runtime == nil || d.Gate == nil || d.Epochs == nil || d.Ports == nil || d.IDs == nil || !d.Instance.Valid() {
		return nil, errors.New("operations: lifecycle scheduler needs runtime DB, gate, epochs, owner ports, IDs and instance")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.Batch < 1 || d.Batch > 1000 {
		d.Batch = 100
	}
	return &LifecycleScheduler{db: d.Runtime, gate: d.Gate, epochs: d.Epochs, ports: d.Ports, clock: d.Clock, ids: d.IDs, instance: d.Instance, batch: d.Batch}, nil
}

type lifecycleJob struct {
	Key                string        `json:"job_key"`
	Kind               string        `json:"kind"`
	OperationID        ids.ID        `json:"operation_id"`
	IdempotencyKey     string        `json:"idempotency_key"`
	RequestHash        digest.Digest `json:"request_hash,omitempty"`
	RecoveryEpoch      int64         `json:"recovery_epoch,omitempty"`
	ProjectID          ids.ID        `json:"project_id,omitempty"`
	TrashID            ids.ID        `json:"trash_id,omitempty"`
	ExpectedRevision   int64         `json:"expected_revision,omitempty"`
	SHA256             string        `json:"sha256,omitempty"`
	CandidateOperation ids.ID        `json:"candidate_operation_id,omitempty"`
	State              string        `json:"state"`
	Attempts           int           `json:"attempts"`
	NextAt             int64         `json:"next_at"`
	LastError          string        `json:"last_error,omitempty"`
	UpdatedAt          string        `json:"updated_at"`
}

// LifecycleReport counts one pass. Retained means a candidate is still
// referenced or pinned; Deferred means its 24-hour wait has not elapsed.
type LifecycleReport struct {
	Reminders  int      `json:"reminders"`
	Purges     int      `json:"purges"`
	Superseded int      `json:"superseded"`
	Collected  int      `json:"collected"`
	Retained   int      `json:"retained"`
	Deferred   int      `json:"deferred"`
	Resumed    int      `json:"resumed_file_actions"`
	Failed     int      `json:"failed"`
	Errors     []string `json:"errors"`
}

func (s *LifecycleScheduler) load(ctx context.Context, key string) (*lifecycleJob, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT record FROM operations_lifecycle_jobs WHERE job_key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j lifecycleJob
	return &j, json.Unmarshal([]byte(raw), &j)
}

func (s *LifecycleScheduler) save(ctx context.Context, j lifecycleJob) error {
	j.UpdatedAt = clock.Format(s.clock.Now())
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	lctx, h, err := s.gate.Acquire(ctx, commands.Request{})
	if err != nil {
		return err
	}
	defer h.Release()
	_, err = s.db.ExecContext(lctx, `INSERT INTO operations_lifecycle_jobs(job_key,kind,operation_id,state,attempts,next_at,record) VALUES(?,?,?,?,?,?,?) ON CONFLICT(job_key) DO UPDATE SET state=excluded.state,attempts=excluded.attempts,next_at=excluded.next_at,record=excluded.record`, j.Key, j.Kind, j.OperationID, j.State, j.Attempts, j.NextAt, string(b))
	return err
}

// ensure registers a job once; a later pass reuses the same operation and key.
func (s *LifecycleScheduler) ensure(ctx context.Context, j lifecycleJob) (lifecycleJob, error) {
	old, err := s.load(ctx, j.Key)
	if err != nil || old != nil {
		if old != nil {
			return *old, nil
		}
		return j, err
	}
	if j.OperationID, err = s.ids.New(); err != nil {
		return j, err
	}
	j.IdempotencyKey = "lifecycle-" + string(j.OperationID)
	j.State = "pending"
	return j, s.save(ctx, j)
}

// CheckLifecycleJob is the ledger's trusted scheduler authority: the command
// must be exactly a registered, unfinished server job for this trash entry,
// executed by the instance itself with no session or human grant.
func (s *LifecycleScheduler) CheckLifecycleJob(ctx context.Context, cmd commands.Context, kind string, trash ids.ID) error {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT record FROM operations_lifecycle_jobs WHERE operation_id=?`, cmd.OperationID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return errcode.New(errcode.Forbidden, "unregistered lifecycle job")
	}
	if err != nil {
		return err
	}
	var j lifecycleJob
	if err = json.Unmarshal([]byte(raw), &j); err != nil {
		return err
	}
	if j.Kind != kind || cmd.CommandType != kind || j.TrashID != trash || j.ProjectID != cmd.ProjectID || j.RequestHash != cmd.RequestHash || j.RecoveryEpoch != cmd.RecoveryEpoch || j.IdempotencyKey != cmd.IdempotencyKey || cmd.ActorID != s.instance || cmd.SessionID != "" || cmd.HumanGrantID != "" || j.State == "superseded" {
		return errcode.New(errcode.Forbidden, "lifecycle job does not match its registration")
	}
	return nil
}

// policyDeferred reports T03's refusal because project policy disables
// automatic purge; the entry may become eligible when policy changes.
func policyDeferred(err error) bool {
	e, ok := errcode.As(err)
	if !ok || e.Code != errcode.PreconditionFailed {
		return false
	}
	for _, d := range e.Details {
		if d.Reason == "auto_purge_disabled" {
			return true
		}
	}
	return false
}

func backoff(attempts int) time.Duration {
	d := time.Minute << min(attempts, 6)
	return min(d, time.Hour)
}

// RunOnce performs one bounded pass: finish interrupted GC deletions, retry
// failed purge file actions through T03, send due reminders, run due purges,
// then collect aged candidates. A maintenance barrier simply defers work.
func (s *LifecycleScheduler) RunOnce(ctx context.Context) (LifecycleReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := LifecycleReport{Errors: []string{}}
	note := func(err error) {
		if err != nil && len(r.Errors) < 20 {
			r.Errors = append(r.Errors, string(errcode.CodeOf(err))+": "+err.Error())
		}
	}
	pending, err := s.ports.PendingGC(ctx)
	if err != nil {
		return r, err
	}
	for _, sha := range pending {
		// Reuses the persisted deletion intent; roots and pins are rechecked.
		if _, err := s.ports.CollectBlob(ctx, sha); err != nil {
			r.Failed++
			note(err)
		}
	}
	n, err := s.ports.RetryFileFailures(ctx)
	r.Resumed = n
	note(err)
	if err = s.runDue(ctx, &r, note); err != nil {
		return r, err
	}
	return r, s.runGC(ctx, &r, note)
}

func (s *LifecycleScheduler) runDue(ctx context.Context, r *LifecycleReport, note func(error)) error {
	epoch, err := s.epochs.RecoveryEpoch(ctx)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	due, err := s.ports.DueTrash(ctx, now.Add(72*time.Hour), s.batch)
	if err != nil {
		return err
	}
	for _, e := range due {
		kinds := []string{}
		if !e.Reminded && !now.Before(e.DueAt.Add(-72*time.Hour)) {
			kinds = append(kinds, "ledger.purge_reminder")
		}
		if !now.Before(e.DueAt) {
			kinds = append(kinds, "ledger.purge_due")
		}
		for _, kind := range kinds {
			body := DueJob{ProjectID: e.ProjectID, TrashID: e.TrashID, ExpectedRevision: e.Revision}
			raw, err := canonjson.CanonicalizeValue(body)
			if err != nil {
				return err
			}
			hash, err := commands.RequestHash(commands.HashInput{CommandType: kind, ProjectID: e.ProjectID, Body: raw})
			if err != nil {
				return err
			}
			j, err := s.ensure(ctx, lifecycleJob{Key: fmt.Sprintf("%s:%s:%d:%d", kind, e.TrashID, e.Revision, epoch), Kind: kind, RequestHash: hash, RecoveryEpoch: epoch, ProjectID: e.ProjectID, TrashID: e.TrashID, ExpectedRevision: e.Revision})
			if err != nil {
				return err
			}
			if j.State == "done" || j.State == "superseded" || j.NextAt > clock.Millis(now) {
				continue
			}
			cmd := commands.Context{OperationID: j.OperationID, IdempotencyKey: j.IdempotencyKey, CommandType: kind, ActorID: s.instance, ProjectID: e.ProjectID, RequestHash: j.RequestHash, RecoveryEpoch: j.RecoveryEpoch}
			err = s.ports.RunDue(ctx, cmd, body)
			switch code := errcode.CodeOf(err); {
			case err == nil:
				j.State, j.LastError = "done", ""
				if kind == "ledger.purge_due" {
					r.Purges++
				} else {
					r.Reminders++
				}
			case code == errcode.NotDue || policyDeferred(err):
				// Not due by T03's clock, or the project disabled automatic purge:
				// keep the same registration and ask T03 again later.
				j.NextAt, j.LastError = clock.Millis(now.Add(time.Hour)), string(code)
				r.Deferred++
			case code == errcode.TrashHeld, code == errcode.InvalidStateTransition, code == errcode.PreconditionFailed, code == errcode.NotFound:
				// The entry changed or policy forbids it: T03 said no, do not retry.
				j.State, j.LastError = "superseded", string(code)
				r.Superseded++
			case code == errcode.MaintenanceMode:
				continue
			default:
				j.State, j.Attempts, j.NextAt, j.LastError = "failed", j.Attempts+1, clock.Millis(now.Add(backoff(j.Attempts))), string(code)
				r.Failed++
				note(err)
			}
			if err = s.save(ctx, j); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *LifecycleScheduler) runGC(ctx context.Context, r *LifecycleReport, note func(error)) error {
	now := s.clock.Now()
	for after := ""; ; {
		page, err := s.ports.GCCandidates(ctx, after, s.batch)
		if err != nil {
			return err
		}
		for _, c := range page {
			after = c.SHA256
			if now.Before(c.Since.Add(24 * time.Hour)) {
				r.Deferred++
				continue
			}
			state, err := s.ports.GCState(ctx, c.OperationID)
			if err != nil {
				return err
			}
			if state == "done" || state == "cancelled" {
				continue
			}
			j, err := s.ensure(ctx, lifecycleJob{Key: "storage.gc:" + c.SHA256 + ":" + string(c.OperationID), Kind: "storage.gc", SHA256: c.SHA256, CandidateOperation: c.OperationID})
			if err != nil {
				return err
			}
			if j.State == "done" || j.NextAt > clock.Millis(now) {
				continue
			}
			deleted, err := s.ports.CollectBlob(ctx, c.SHA256)
			switch {
			case err == nil && deleted:
				j.State, j.LastError = "done", ""
				r.Collected++
			case err == nil:
				// Still referenced or pinned: keep the candidate and look again later.
				j.State, j.NextAt = "pending", clock.Millis(now.Add(time.Hour))
				r.Retained++
			case errcode.CodeOf(err) == errcode.MaintenanceMode:
				continue
			case errcode.CodeOf(err) == errcode.NotDue:
				j.NextAt = clock.Millis(c.Since.Add(24 * time.Hour))
				r.Deferred++
			default:
				j.State, j.Attempts, j.NextAt, j.LastError = "failed", j.Attempts+1, clock.Millis(now.Add(backoff(j.Attempts))), string(errcode.CodeOf(err))
				r.Failed++
				note(err)
			}
			if err = s.save(ctx, j); err != nil {
				return err
			}
		}
		if len(page) < s.batch {
			return nil
		}
	}
}
