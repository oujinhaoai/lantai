// Package jobs owns deterministic check dispatch and independent job fences.
// External execution and ledger evidence acceptance occur outside SQL transactions.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/commands"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

const Module = "jobs"

func ConfigDigest() digest.Digest {
	return digest.Of([]byte(`{"checks":["integrity","schema","license_evidence","purpose"],"purpose":"production","version":1}`))
}

type Authority interface {
	authz.Authorizer
	authz.EpochSource
}

// Host is the T09 processor port. Snapshot resolves the compiled builtin or a
// reviewed, enabled and probed package without side effects; Run admits and
// spawns one attempt; CheckSnapshot gates admitted result acceptance, including
// a bounded normal drain. CheckCurrentSnapshot requires current qualification
// when completed evidence is used for a new review or publication. Settle
// reports a reconciled stop for breaker bookkeeping.
type Host interface {
	Snapshot(context.Context, ids.ID, string, int64) (ae.ActivationSnapshot, storage.Producer, error)
	CheckSnapshot(context.Context, ids.ID, ae.ActivationSnapshot, ids.ID, int64) error
	CheckCurrentSnapshot(context.Context, ids.ID, ae.ActivationSnapshot, ids.ID, int64) error
	Run(context.Context, ids.ID, extensions.ProcessorInput, []byte) (extensions.InvocationResult, error)
	Settle(context.Context, ids.ID, execution.InvocationOutcome) error
}
type Files interface {
	ReadManifest(context.Context, commit.Committed) ([]byte, error)
	VerifyDeep(context.Context, ids.ID) error
}
type Evidence interface {
	AppendEvidence(context.Context, authz.Context, string, ledger.ReviewEvidenceInput) (ledger.AcceptedEvidence, error)
}
type Nodes interface {
	CheckWorker(context.Context, authz.Context, ids.ID, string) error
}
type Deps struct {
	DB         *sql.DB
	Gate       *commands.Gate
	Authority  Authority
	Tasks      *tasks.Service
	Catalog    *catalog.Service
	Ledger     *ledger.Service
	Files      Files
	Rights     rights.Evaluator
	Host       Host
	Nodes      Nodes
	Clock      clock.Clock
	IDs        *ids.Generator
	InstanceID ids.ID
}
type Service struct {
	d         Deps
	store     *commands.Store
	mu        sync.RWMutex
	evidence  Evidence
	execution ledger.ReviewExecution
	running   map[ids.ID]context.CancelFunc
}
type Job struct {
	Reconciliations []Reconciliation             `json:"reconciliations,omitempty"`
	ID              ids.ID                       `json:"job_id"`
	OperationID     ids.ID                       `json:"operation_id"`
	Request         workflow.JobRequest          `json:"request"`
	Revision        int64                        `json:"revision"`
	State           string                       `json:"state"`
	Epoch           int64                        `json:"recovery_epoch"`
	Attempt         *ae.JobAttempt               `json:"attempt,omitempty"`
	WorkerID        ids.ID                       `json:"worker_id,omitempty"`
	SessionID       ids.ID                       `json:"session_id,omitempty"`
	ExpiresAt       string                       `json:"expires_at,omitempty"`
	Attempts        int                          `json:"attempts"`
	Checks          []ledger.ReviewEvidenceInput `json:"checks"`
	Evidence        []workflow.JobEvidence       `json:"evidence"`
	Failure         string                       `json:"failure,omitempty"`
	CancelRequested bool                         `json:"cancel_requested"`
}

func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Gate == nil || d.Authority == nil || d.Tasks == nil || d.Catalog == nil || d.Ledger == nil || d.Files == nil || d.Rights == nil || d.Host == nil || d.IDs == nil || !d.InstanceID.Valid() {
		return nil, errors.New("jobs: all owner ports required")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	st, e := commands.NewStore(Module, d.Clock)
	return &Service{d: d, store: st, running: map[ids.ID]context.CancelFunc{}}, e
}
func (s *Service) SetEvidence(e Evidence) { s.mu.Lock(); defer s.mu.Unlock(); s.evidence = e }
func (s *Service) SetExecution(e ledger.ReviewExecution) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execution = e
}
func raw(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return string(b)
}
func (s *Service) load(ctx context.Context, id ids.ID) (Job, error) {
	var j Job
	var b string
	e := s.d.DB.QueryRowContext(ctx, `SELECT record FROM jobs_jobs WHERE job_id=?`, id).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return j, errcode.New(errcode.NotFound, "")
	}
	if e != nil {
		return j, e
	}
	e = json.Unmarshal([]byte(b), &j)
	return j, e
}
func (s *Service) auth(ctx context.Context, w authz.Context, act authz.Action, p ids.ID) error {
	d, e := s.d.Authority.Authorize(ctx, w, act, authz.Resource{ProjectID: p, Kind: "project", ID: p})
	if e != nil {
		return e
	}
	return d.Err()
}
func (s *Service) Read(ctx context.Context, w authz.Context, id ids.ID) (Job, error) {
	j, e := s.load(ctx, id)
	if e != nil {
		return j, e
	}
	if e = s.auth(ctx, w, identity.ActTasksRead, j.Request.ProjectID); e != nil {
		return Job{}, e
	}
	return j, nil
}
func (s *Service) List(ctx context.Context, w authz.Context, p, after ids.ID, limit int) ([]Job, error) {
	if !p.Valid() || limit < 1 || limit > 100 || after != "" && !after.Valid() {
		return nil, errcode.New(errcode.SchemaInvalid, "")
	}
	if e := s.auth(ctx, w, identity.ActTasksRead, p); e != nil {
		return nil, e
	}
	rows, e := s.d.DB.QueryContext(ctx, `SELECT record FROM jobs_jobs WHERE project_id=? AND job_id>? ORDER BY job_id LIMIT ?`, p, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var j Job
		var b string
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &j); e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Service) lock(ctx context.Context, j Job) (context.Context, func(), error) {
	c, h, e := s.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared, Projects: []string{string(j.Request.ProjectID)}, Tasks: []string{"jobs:dispatch"}})
	if e != nil {
		return ctx, nil, e
	}
	return c, h.Release, nil
}
func (s *Service) command(ctx context.Context, w authz.Context, key, typ string, j Job, body any) (commands.Context, error) {
	b, e := canonjson.CanonicalizeValue(body)
	if e != nil {
		return commands.Context{}, e
	}
	hash, e := commands.RequestHash(commands.HashInput{CommandType: typ, ProjectID: j.Request.ProjectID, Body: b})
	if e != nil {
		return commands.Context{}, e
	}
	epoch, e := s.d.Authority.RecoveryEpoch(ctx)
	if e != nil {
		return commands.Context{}, e
	}
	id, e := s.d.IDs.New()
	if e != nil {
		return commands.Context{}, e
	}
	c := commands.Context{OperationID: id, IdempotencyKey: key, CommandType: typ, ActorID: w.PrincipalID, SessionID: w.SessionID, ProjectID: j.Request.ProjectID, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: w.PolicyRevision}
	return c, c.Validate()
}
func (s *Service) replay(ctx context.Context, c commands.Context) (*Job, error) {
	r, e := s.store.LookupReceipt(ctx, s.d.DB, c.Key())
	if e != nil || r == nil {
		return nil, e
	}
	switch commands.Decide(r, c.RequestHash, s.d.Clock.Now()) {
	case commands.OutcomeConflict:
		return nil, errcode.New(errcode.IdempotencyConflict, "")
	case commands.OutcomeExpired:
		return nil, errcode.New(errcode.IdempotencyResultExpired, "")
	case commands.OutcomeInProgress:
		return nil, errcode.New(errcode.ResourceBusy, "")
	}
	var j Job
	e = json.Unmarshal(r.ResponseSummary, &j)
	return &j, e
}
func (s *Service) save(ctx context.Context, c commands.Context, j Job) (Job, error) {
	j.Revision++
	_, e := s.store.Execute(ctx, s.d.DB, c, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		_, e := tx.ExecContext(ctx, `INSERT INTO jobs_jobs(job_id,project_id,state,record) VALUES(?,?,?,?) ON CONFLICT(job_id) DO UPDATE SET state=excluded.state,record=excluded.record`, j.ID, j.Request.ProjectID, j.State, raw(j))
		if e != nil {
			return commands.Result{}, e
		}
		if j.Attempt != nil {
			if e = j.Attempt.Validate(); e != nil {
				return commands.Result{}, e
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO jobs_attempts(attempt_id,job_id,record) VALUES(?,?,?) ON CONFLICT(attempt_id) DO UPDATE SET record=excluded.record`, j.Attempt.ID, j.ID, raw(j.Attempt))
			if e != nil {
				return commands.Result{}, e
			}
		}
		ev, e := event.New(s.d.IDs, s.d.Clock, event.Params{EventType: "job.changed", SchemaVersion: 1, AggregateType: "job", AggregateID: j.ID, AggregateRevision: j.Revision, ActorID: c.ActorID, SessionID: c.SessionID, ProjectID: j.Request.ProjectID, OperationID: c.OperationID, CorrelationID: c.Correlation(), Payload: map[string]any{"state": j.State, "flow_id": j.Request.FlowID, "task_id": j.Request.Flow.TaskID}})
		if e != nil {
			return commands.Result{}, e
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: j, Events: []event.Envelope{ev}}, nil
	})
	return j, e
}
func (s *Service) currentTarget(ctx context.Context, w authz.Context, j Job, allowDone bool) (commit.Committed, error) {
	v, e := s.d.Catalog.ExecutionVersion(ctx, w, j.Request.Target.AssetID, j.Request.Target.VersionID)
	if e != nil {
		return commit.Committed{}, e
	}
	if j.Request.Target != v.Version.Ref(s.d.InstanceID) || v.Version.ProjectID != j.Request.ProjectID || v.Version.ManifestDigest.String() != j.Request.ManifestDigest {
		return commit.Committed{}, errcode.New(errcode.RefMismatch, "")
	}
	t, e := s.d.Tasks.Snapshot(ctx, j.Request.Flow.TaskID)
	if e != nil {
		return commit.Committed{}, e
	}
	f := j.Request.Flow
	if t.Task.State != "submitted" && !(allowDone && t.Task.State == "done") || t.Attempt == nil || t.Attempt.ID != f.AttemptID || t.Attempt.Fence.LeaseFence != f.Fence || int64(t.Meta.Round) != f.Round || !slices.Contains(t.Task.OutputRefs, j.Request.Target) {
		return commit.Committed{}, errcode.New(errcode.LeaseStale, "job production round is no longer current")
	}
	return v.Version, nil
}

func (s *Service) target(ctx context.Context, w authz.Context, j Job) (commit.Committed, error) {
	v, e := s.currentTarget(ctx, w, j, false)
	if e != nil {
		return commit.Committed{}, e
	}
	s.mu.RLock()
	x := s.execution
	s.mu.RUnlock()
	if x == nil {
		return commit.Committed{}, errcode.New(errcode.UnsupportedCapability, "production round authority required")
	}
	if e = x.Task(ctx, w, v, j.Request.Flow, "review"); e != nil {
		return commit.Committed{}, e
	}
	return v, nil
}
func (s *Service) StartJob(ctx context.Context, w authz.Context, in workflow.JobRequest) (workflow.JobRef, error) {
	if !in.OperationID.Valid() || !in.ProjectID.Valid() || in.Processor == "" {
		return workflow.JobRef{}, errcode.New(errcode.SchemaInvalid, "")
	}
	j := Job{Request: in}
	ctx, release, e := s.lock(ctx, j)
	if e != nil {
		return workflow.JobRef{}, e
	}
	defer release()
	if e = s.auth(ctx, w, identity.ActTasksWork, in.ProjectID); e != nil {
		return workflow.JobRef{}, e
	}
	c, e := s.command(ctx, w, string(in.OperationID), "jobs.start", j, in)
	if e != nil {
		return workflow.JobRef{}, e
	}
	if old, e := s.replay(ctx, c); old != nil || e != nil {
		if old != nil {
			return workflow.JobRef{JobID: old.ID}, e
		}
		return workflow.JobRef{}, e
	}
	if _, e = s.target(ctx, w, j); e != nil {
		return workflow.JobRef{}, e
	}
	// Fail early for a processor that is not builtin or not currently usable.
	if _, _, e = s.d.Host.Snapshot(ctx, in.ProjectID, in.Processor, c.RecoveryEpoch); e != nil {
		return workflow.JobRef{}, e
	}
	j.ID, e = s.d.IDs.New()
	if e != nil {
		return workflow.JobRef{}, e
	}
	j.OperationID = c.OperationID
	j.Epoch = c.RecoveryEpoch
	j.State = "queued"
	j.Checks = []ledger.ReviewEvidenceInput{}
	j.Evidence = []workflow.JobEvidence{}
	j, e = s.save(ctx, c, j)
	return workflow.JobRef{JobID: j.ID}, e
}

type Control struct {
	JobID            ids.ID `json:"job_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func (s *Service) Run(ctx context.Context, w authz.Context, key string, in Control) (Job, error) {
	j, e := s.Read(ctx, w, in.JobID)
	if e != nil {
		return Job{}, e
	}
	ctxLock, release, e := s.lock(ctx, j)
	if e != nil {
		return Job{}, e
	}
	j, e = s.load(ctxLock, in.JobID)
	if e != nil {
		release()
		return Job{}, e
	}
	if w.PrincipalKind != authz.Worker {
		release()
		return Job{}, errcode.New(errcode.Forbidden, "only an authorized worker may dispatch checks")
	}
	if e = s.auth(ctxLock, w, "ledger.append_check", j.Request.ProjectID); e != nil {
		release()
		return Job{}, e
	}
	if s.d.Nodes == nil {
		release()
		return Job{}, errcode.New(errcode.UnsupportedCapability, "worker capability authority required")
	}
	if e = s.d.Nodes.CheckWorker(ctxLock, w, j.Request.ProjectID, j.Request.Processor); e != nil {
		release()
		return Job{}, e
	}
	c, e := s.command(ctxLock, w, key, "jobs.run", j, in)
	if e != nil {
		release()
		return Job{}, e
	}
	if old, e := s.replay(ctxLock, c); old != nil || e != nil {
		release()
		if e != nil {
			return Job{}, e
		}
		current, e := s.Read(ctx, w, old.ID)
		if e != nil {
			return Job{}, e
		}
		if current.State == "accepting" {
			return s.acceptEvidence(ctx, w, current)
		}
		return current, nil
	}
	if j.Revision != in.ExpectedRevision {
		release()
		return Job{}, errcode.New(errcode.PreconditionFailed, "")
	}
	if j.State != "queued" {
		release()
		return Job{}, errcode.New(errcode.InvalidStateTransition, "")
	}
	var active int
	if e = s.d.DB.QueryRowContext(ctxLock, `SELECT count(*) FROM jobs_jobs WHERE state IN ('running','cancelling','needs_reconciliation','accepting')`).Scan(&active); e != nil {
		release()
		return Job{}, e
	}
	if active > 0 {
		release()
		return Job{}, errcode.New(errcode.ResourceBusy, "official checker concurrency is one; reconcile unresolved attempts first")
	}
	if j.Epoch != c.RecoveryEpoch {
		release()
		return Job{}, errcode.New(errcode.LeaseStale, "")
	}
	v, e := s.target(ctxLock, w, j)
	if e != nil {
		release()
		return Job{}, e
	}
	snapshot, producer, e := s.d.Host.Snapshot(ctxLock, j.Request.ProjectID, j.Request.Processor, c.RecoveryEpoch)
	if e != nil {
		release()
		return Job{}, e
	}
	data, e := s.d.Files.ReadManifest(ctxLock, v)
	if e != nil {
		release()
		return Job{}, e
	}
	aid, e := s.d.IDs.New()
	if e != nil {
		release()
		return Job{}, e
	}
	j.Attempts++
	input := tc.InputSnapshot{Refs: []ids.PermanentRef{j.Request.Target}}
	input.Digest, e = tc.SnapshotDigest(input.Refs)
	if e != nil {
		release()
		return Job{}, e
	}
	j.Attempt = &ae.JobAttempt{Contract: "lantai.job-attempt/v1", ID: aid, JobID: j.ID, OperationID: c.OperationID, Revision: 1, Protocol: execution.Processor, Fence: ae.JobFence{JobID: j.ID, JobAttemptID: aid, LeaseFence: int64(j.Attempts), RecoveryEpoch: c.RecoveryEpoch}, ActivationSnapshot: snapshot, Input: input, State: "running"}
	j.State = "running"
	j.WorkerID = w.PrincipalID
	j.SessionID = w.SessionID
	j.ExpiresAt = clock.Format(s.d.Clock.Now().Add(30 * time.Second))
	j, e = s.save(ctxLock, c, j)
	runCtx, cancelRun := context.WithCancel(ctx)
	if e == nil {
		s.mu.Lock()
		s.running[j.ID] = cancelRun
		s.mu.Unlock()
	}
	release()
	if e != nil {
		cancelRun()
		return Job{}, e
	}
	defer func() { cancelRun(); s.mu.Lock(); delete(s.running, j.ID); s.mu.Unlock() }()
	pi := extensions.ProcessorInput{Contract: "lantai.processor-input/v1", OperationID: c.OperationID, Fence: j.Attempt.Fence, InputRefs: input.Refs, InputDigest: v.ManifestDigest, Deadline: clock.Format(time.Now().Add(10 * time.Second)), Activation: snapshot, Producer: producer}
	result, hostErr := s.d.Host.Run(runCtx, j.Request.ProjectID, pi, data)
	// Observe the dispatched attempt even when its caller cancelled the request.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return s.finish(finishCtx, w, j, result, hostErr)
}
