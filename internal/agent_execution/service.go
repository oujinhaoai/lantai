// Package agent_execution owns manual session registrations and their durable
// execution records. It never launches a model or creates a task lease.
package agent_execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/oujinhaoai/lantai/internal/commands"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

const Module = "agent_execution"

type Authority interface {
	authz.Authorizer
	authz.EpochSource
}
type Tasks interface {
	Task(context.Context, authz.Context, ids.ID) (tasks.View, error)
	CheckVersionWrite(context.Context, authz.Context, commands.Context, ids.ID, ids.ID) error
}

// Activations is an owner port: a caller cannot assert its own activation.
type Activations interface {
	CheckExecution(context.Context, ae.ActivationSnapshot) error
}
type Assets interface {
	CheckExecutionRef(context.Context, authz.Context, ids.PermanentRef) error
}
type Deps struct {
	DB          *sql.DB
	Gate        *commands.Gate
	Authority   Authority
	Tasks       Tasks
	Activations Activations
	Assets      Assets
	IDs         *ids.Generator
	Clock       clock.Clock
}
type Service struct {
	d     Deps
	store *commands.Store
}
type Record struct {
	Reconciliations []Reconciliation            `json:"reconciliations,omitempty"`
	TaskRound       int                         `json:"task_round"`
	Run             ae.TaskRun                  `json:"run"`
	ProjectID       ids.ID                      `json:"project_id"`
	Profile         ae.ExecutionProfile         `json:"profile"`
	Current         ae.Admission                `json:"current"`
	SessionID       ids.ID                      `json:"session_id"`
	Usage           ae.BudgetUsage              `json:"usage"`
	Steps           []ae.AgentStepRun           `json:"steps"`
	Tools           []ae.ToolOperation          `json:"tools"`
	Candidates      []ae.ArtifactCandidate      `json:"candidates"`
	Checkpoints     []ae.Checkpoint             `json:"checkpoints"`
	Inputs          []HumanInput                `json:"inputs"`
	CandidateRefs   map[ids.ID]ids.PermanentRef `json:"candidate_refs"`
	CancelRequested bool                        `json:"cancel_requested"`
	Result          *ae.Result                  `json:"result,omitempty"`
	UpdatedAt       string                      `json:"updated_at"`
}
type HumanInput struct {
	ID           ids.ID        `json:"id"`
	Kind         string        `json:"kind"`
	Question     string        `json:"question"`
	TargetDigest digest.Digest `json:"target_digest,omitempty"`
	ExpiresAt    string        `json:"expires_at"`
	Answer       string        `json:"answer,omitempty"`
	AnsweredBy   ids.ID        `json:"answered_by,omitempty"`
}
type Mutation struct {
	RunID            ids.ID          `json:"run_id"`
	ExpectedRevision int64           `json:"expected_revision"`
	Fence            execution.Fence `json:"fence"`
}

func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Gate == nil || d.Authority == nil || d.Tasks == nil || d.Assets == nil || d.IDs == nil {
		return nil, errors.New("agent_execution: all owner ports required")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	st, e := commands.NewStore(Module, d.Clock)
	if e != nil {
		return nil, e
	}
	return &Service{d, st}, nil
}
func Capabilities() ae.Capabilities {
	return ae.Capabilities{Contract: "lantai.execution-capabilities/v1", ProtocolVersions: []execution.Protocol{execution.AgentExecution}, AdapterKind: "manual_cli", BackendVersion: "1.0.0", SupportsIdempotentStart: true, SupportsLookupByKey: true, CheckpointFormatVersions: []string{"1"}, ResumeClasses: []execution.ResumeClass{execution.ResumePortableArtifacts, execution.ResumeRestartSafe}, CancelMode: execution.CancelRevokeOnly, ArtifactMode: "candidate_manifest", Capabilities: []string{"manual_session"}, BudgetMeters: []string{"wall_time", "model_calls", "tool_calls", "artifact_bytes"}}
}
func bad(s string) error   { return errcode.New(errcode.SchemaInvalid, s) }
func state(s string) error { return errcode.New(errcode.InvalidStateTransition, s) }
func encoded(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return string(b)
}
func (s *Service) load(ctx context.Context, id ids.ID) (Record, error) {
	var r Record
	var raw string
	e := s.d.DB.QueryRowContext(ctx, `SELECT record FROM agent_execution_runs WHERE run_id=?`, id).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return r, errcode.New(errcode.NotFound, "")
	}
	if e != nil {
		return r, e
	}
	e = json.Unmarshal([]byte(raw), &r)
	return r, e
}
func (s *Service) authorize(ctx context.Context, who authz.Context, action authz.Action, r Record) error {
	d, e := s.d.Authority.Authorize(ctx, who, action, authz.Resource{ProjectID: r.ProjectID, Kind: "task", ID: r.Run.TaskID})
	if e != nil {
		return e
	}
	return d.Err()
}
func (s *Service) Read(ctx context.Context, who authz.Context, id ids.ID) (Record, error) {
	r, e := s.load(ctx, id)
	if e != nil {
		return Record{}, e
	}
	if e = s.authorize(ctx, who, identity.ActTasksRead, r); e != nil {
		return Record{}, e
	}
	return r, nil
}
func (s *Service) List(ctx context.Context, who authz.Context, task, after ids.ID, limit int) ([]ae.TaskRun, error) {
	if limit < 1 || limit > 100 || !task.Valid() || after != "" && !after.Valid() {
		return nil, bad("task and bounded page required")
	}
	if _, e := s.d.Tasks.Task(ctx, who, task); e != nil {
		return nil, e
	}
	rows, e := s.d.DB.QueryContext(ctx, `SELECT record FROM agent_execution_runs WHERE task_id=? AND run_id>? ORDER BY run_id LIMIT ?`, task, after, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []ae.TaskRun{}
	for rows.Next() {
		var raw string
		var r Record
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &r); e != nil {
			return nil, e
		}
		out = append(out, r.Run)
	}
	return out, rows.Err()
}
func (s *Service) lock(ctx context.Context, r Record) (context.Context, func(), error) {
	c, h, e := s.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared, Projects: []string{string(r.ProjectID)}, Tasks: []string{string(r.Run.TaskID)}})
	if e != nil {
		return ctx, nil, e
	}
	return c, h.Release, nil
}
func (s *Service) fence(ctx context.Context, who authz.Context, r Record, f execution.Fence) error {
	if f.TaskID != r.Run.TaskID || f.AttemptID != r.Run.CurrentAttemptID || who.SessionID != r.SessionID {
		return errcode.New(errcode.LeaseStale, "")
	}
	if e := s.authorize(ctx, who, identity.ActTasksWork, r); e != nil {
		return e
	}
	cmd := commands.Context{TaskID: r.Run.TaskID, AttemptID: f.AttemptID, SessionID: who.SessionID, LeaseFence: f.LeaseFence, RecoveryEpoch: f.RecoveryEpoch}
	return s.d.Tasks.CheckVersionWrite(ctx, who, cmd, r.ProjectID, "")
}
func (s *Service) command(ctx context.Context, who authz.Context, key, typ string, r Record, body any) (commands.Context, error) {
	raw, e := canonjson.CanonicalizeValue(body)
	if e != nil {
		return commands.Context{}, e
	}
	if len(raw) > 256<<10 {
		return commands.Context{}, bad("execution request too large")
	}
	hash, e := commands.RequestHash(commands.HashInput{CommandType: typ, ProjectID: r.ProjectID, Body: raw})
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
	c := commands.Context{OperationID: id, IdempotencyKey: key, CommandType: typ, ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: r.ProjectID, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: who.PolicyRevision}
	return c, c.Validate()
}
func (s *Service) replay(ctx context.Context, c commands.Context) (*Record, error) {
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
	var ref struct {
		RunID    ids.ID `json:"run_id"`
		Revision int64  `json:"revision"`
	}
	if e = json.Unmarshal(r.ResponseSummary, &ref); e != nil {
		return nil, e
	}
	var raw string
	if e = s.d.DB.QueryRowContext(ctx, `SELECT record FROM agent_execution_history WHERE run_id=? AND revision=?`, ref.RunID, ref.Revision).Scan(&raw); e != nil {
		return nil, e
	}
	var out Record
	e = json.Unmarshal([]byte(raw), &out)
	return &out, e
}
func (s *Service) save(ctx context.Context, c commands.Context, r Record, insert func(context.Context, *sql.Tx) error) (Record, error) {
	r.Run.Revision++
	r.UpdatedAt = clock.Format(s.d.Clock.Now())
	if e := tc.ValidateShape("lantai.task-run/v1", r.Run); e != nil {
		return Record{}, e
	}
	raw := encoded(r)
	if len(raw) > 2<<20 {
		return Record{}, bad("execution record limit reached; checkpoint and hand off")
	}
	_, e := s.store.Execute(ctx, s.d.DB, c, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		if insert != nil {
			if e := insert(ctx, tx); e != nil {
				return commands.Result{}, e
			}
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO agent_execution_runs(run_id,task_id,project_id,revision,record) VALUES(?,?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET revision=excluded.revision,record=excluded.record`, r.Run.ID, r.Run.TaskID, r.ProjectID, r.Run.Revision, raw)
		if e != nil {
			return commands.Result{}, e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO agent_execution_history(run_id,revision,record) VALUES(?,?,?)`, r.Run.ID, r.Run.Revision, raw); e != nil {
			return commands.Result{}, e
		}
		ev, e := event.New(s.d.IDs, s.d.Clock, event.Params{EventType: "task_run.changed", SchemaVersion: 1, AggregateType: "task_run", AggregateID: r.Run.ID, AggregateRevision: r.Run.Revision, ActorID: c.ActorID, SessionID: c.SessionID, ProjectID: r.ProjectID, OperationID: c.OperationID, CorrelationID: c.Correlation(), Payload: map[string]any{"task_id": r.Run.TaskID, "state": r.Run.State}})
		if e != nil {
			return commands.Result{}, e
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: map[string]any{"run_id": r.Run.ID, "revision": r.Run.Revision}, Events: []event.Envelope{ev}}, nil
	})
	return r, e
}

// Start registers an existing authenticated session. No subprocess is launched.
func (s *Service) Start(ctx context.Context, who authz.Context, key string, in ae.StartRequest) (Record, error) {
	if e := in.Validate(Capabilities()); e != nil {
		return Record{}, e
	}
	v, e := s.d.Tasks.Task(ctx, who, in.Fence.TaskID)
	if e != nil {
		return Record{}, e
	}
	r := Record{ProjectID: v.Task.ProjectID, Run: ae.TaskRun{ID: in.TaskRunID, TaskID: v.Task.ID}}
	ctx, release, e := s.lock(ctx, r)
	if e != nil {
		return Record{}, e
	}
	defer release()
	if e = s.authorize(ctx, who, identity.ActTasksWork, r); e != nil {
		return Record{}, e
	}
	var oldSession, raw string
	var admittedRevision int64
	e = s.d.DB.QueryRowContext(ctx, `SELECT session_id,admission,run_revision FROM agent_execution_admissions WHERE execution_key=?`, in.ExecutionKey).Scan(&oldSession, &raw, &admittedRevision)
	if e == nil {
		if oldSession != string(who.SessionID) {
			return Record{}, errcode.New(errcode.Forbidden, "")
		}
		var ad ae.Admission
		if e = json.Unmarshal([]byte(raw), &ad); e != nil {
			return Record{}, e
		}
		if _, e = ae.ReplayStart(ad, in); e != nil {
			return Record{}, e
		}
		var snapshot string
		if e = s.d.DB.QueryRowContext(ctx, `SELECT record FROM agent_execution_history WHERE run_id=? AND revision=?`, in.TaskRunID, admittedRevision).Scan(&snapshot); e != nil {
			return Record{}, e
		}
		var admitted Record
		e = json.Unmarshal([]byte(snapshot), &admitted)
		return admitted, e
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return Record{}, e
	}
	v, e = s.d.Tasks.Task(ctx, who, in.Fence.TaskID)
	if e != nil {
		return Record{}, e
	}
	pd, e := in.Profile.Digest()
	if e != nil {
		return Record{}, e
	}
	previous, e := s.load(ctx, in.TaskRunID)
	if e == nil {
		if in.Action != execution.ActResume || previous.Run.State != execution.RunPaused || !previous.Run.Termination.Confirmed {
			return Record{}, state("existing run requires a stopped checkpoint and resume")
		}
		if previous.Result != nil {
			return Record{}, state("sealed run")
		}
		r = previous
		if in.ResumeFrom == nil || !slices.ContainsFunc(r.Checkpoints, func(cp ae.Checkpoint) bool { return encoded(cp) == encoded(in.ResumeFrom) }) {
			return Record{}, bad("checkpoint is not a persisted checkpoint")
		}
	} else if errcode.CodeOf(e) == errcode.NotFound {
		if in.Action != execution.ActStart {
			return Record{}, errcode.New(errcode.ResumeUnsupported, "")
		}
		// Changed inputs/profile or a new production round create a linked run.
		// An ordinary retry must resume its existing budget instead.
		var predecessor ids.ID
		e = s.d.DB.QueryRowContext(ctx, `SELECT run_id FROM agent_execution_runs WHERE task_id=? ORDER BY rowid DESC LIMIT 1`, v.Task.ID).Scan(&predecessor)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return Record{}, e
		}
		if predecessor != "" {
			old, e := s.load(ctx, predecessor)
			if e != nil {
				return Record{}, e
			}
			if !old.Run.Termination.Confirmed || unresolved(old) > 0 || old.Run.State == execution.RunRunning || old.Run.State == execution.RunWaitingInput || old.Run.CurrentAttemptID == in.Fence.AttemptID {
				return Record{}, state("predecessor must stop and release its task attempt")
			}
			if old.Run.Input.Digest == in.Input.Digest && old.Run.Profile.Digest == pd && old.TaskRound == v.Meta.Round {
				return Record{}, state("unchanged retry must resume the previous run and budget")
			}
		}
		r = Record{CandidateRefs: map[ids.ID]ids.PermanentRef{}, TaskRound: v.Meta.Round, ProjectID: v.Task.ProjectID, Profile: in.Profile, Run: ae.TaskRun{Contract: "lantai.task-run/v1", ID: in.TaskRunID, TaskID: v.Task.ID, SeatID: v.Task.SeatIDs[0], Revision: 1, OperationID: in.OperationID, State: execution.RunPending, SupersedesRunID: predecessor, Input: v.Task.Input, Profile: ae.ProfileRef{ProfileID: in.Profile.ID, Revision: in.Profile.Revision, Digest: pd}, BudgetID: in.BudgetID, CreatedBy: who.PrincipalID, FlowID: v.Task.FlowID, StepRunID: v.Task.StepRunID}, Steps: []ae.AgentStepRun{}, Tools: []ae.ToolOperation{}, Candidates: []ae.ArtifactCandidate{}, Checkpoints: []ae.Checkpoint{}, Inputs: []HumanInput{}}
	} else {
		return Record{}, e
	}
	r.Run.CurrentAttemptID = in.Fence.AttemptID
	r.SessionID = who.SessionID
	if e = ae.CheckStartForRun(in, r.Run, Capabilities(), r.Tools); e != nil {
		return Record{}, e
	}
	if e = s.fence(ctx, who, r, in.Fence); e != nil {
		return Record{}, e
	}
	if e = s.activation(ctx, in.Profile); e != nil {
		return Record{}, e
	}
	for _, ref := range append(slices.Clone(in.Input.Refs), in.Profile.PlaybookRefs...) {
		if e = s.d.Assets.CheckExecutionRef(ctx, who, ref); e != nil {
			return Record{}, e
		}
	}
	if e = checkBudget(r.Profile.BudgetLimits, r.Usage); e != nil {
		return Record{}, e
	}
	c, e := s.command(ctx, who, key, "agent_execution.start", r, in)
	if e != nil {
		return Record{}, e
	}
	if p, e := s.replay(ctx, c); p != nil || e != nil {
		if p != nil {
			return *p, e
		}
		return Record{}, e
	}
	id, e := s.d.IDs.New()
	if e != nil {
		return Record{}, e
	}
	r.Current = ae.Admission{Protocol: execution.AgentExecution, Action: in.Action, ExecutionID: id, ExecutionKey: in.ExecutionKey, TaskRunID: in.TaskRunID, AttemptID: in.Fence.AttemptID, AcceptedRequestHash: in.RequestHash, Revision: 1, State: execution.RunRunning}
	r.Run.State = execution.RunRunning
	r.Run.Termination = ae.Termination{}
	return s.save(ctx, c, r, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO agent_execution_admissions(execution_key,execution_id,run_id,attempt_id,session_id,request,admission,run_revision) VALUES(?,?,?,?,?,?,?,?)`, in.ExecutionKey, id, in.TaskRunID, in.Fence.AttemptID, who.SessionID, encoded(in), encoded(r.Current), r.Run.Revision+1)
		return e
	})
}
func checkBudget(l ae.BudgetLimits, u ae.BudgetUsage) error {
	if u.Tokens != nil || u.WallTimeMS < 0 || u.ModelCalls < 0 || u.ToolCalls < 0 || u.ArtifactBytes < 0 {
		return bad("manual usage requires nonnegative observable counters; token accounting unsupported")
	}
	if u.WallTimeMS > l.WallTimeMS || u.ModelCalls > l.ModelCalls || u.ToolCalls > l.ToolCalls || u.ArtifactBytes > l.ArtifactBytes {
		return errcode.New(errcode.BudgetExhausted, "")
	}
	return nil
}
func (s *Service) mutate(ctx context.Context, who authz.Context, key, typ string, in Mutation, body any, mode string, fn func(context.Context, *Record) error) (Record, error) {
	r, e := s.Read(ctx, who, in.RunID)
	if e != nil {
		return Record{}, e
	}
	ctx, release, e := s.lock(ctx, r)
	if e != nil {
		return Record{}, e
	}
	defer release()
	r, e = s.load(ctx, in.RunID)
	if e != nil {
		return Record{}, e
	}
	if e = s.authorize(ctx, who, identity.ActTasksRead, r); e != nil {
		return Record{}, e
	}
	c, e := s.command(ctx, who, key, typ, r, body)
	if e != nil {
		return Record{}, e
	}
	if p, e := s.replay(ctx, c); p != nil || e != nil {
		if p != nil {
			return *p, e
		}
		return Record{}, e
	}
	if in.ExpectedRevision != r.Run.Revision {
		return Record{}, errcode.New(errcode.PreconditionFailed, "")
	}
	if mode == "work" {
		if r.Run.State != execution.RunRunning {
			return Record{}, state("execution is not running")
		}
		if e = s.fence(ctx, who, r, in.Fence); e != nil {
			return Record{}, e
		}
		if e = s.activation(ctx, r.Profile); e != nil {
			return Record{}, e
		}
	} else {
		action := identity.ActTasksReconcile
		if mode == "answer" {
			action = identity.ActTasksAnswer
		}
		if mode == "cancel" {
			action = identity.ActTasksCancel
		}
		if who.SessionID != r.SessionID || mode == "answer" {
			if e = s.authorize(ctx, who, action, r); e != nil {
				return Record{}, e
			}
		} else if e = s.authorize(ctx, who, identity.ActTasksWork, r); e != nil {
			return Record{}, e
		}
	}
	if e = fn(ctx, &r); e != nil {
		return Record{}, e
	}
	return s.save(ctx, c, r, nil)
}

// CheckTaskWrite is called under the shared task/project acceptance locks. A
// cancelled or paused execution cannot keep writing through ordinary commits.
func (s *Service) CheckTaskWrite(ctx context.Context, task, attempt ids.ID) error {
	rows, e := s.d.DB.QueryContext(ctx, `SELECT record FROM agent_execution_runs WHERE task_id=?`, task)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var r Record
		if e = rows.Scan(&raw); e != nil {
			return e
		}
		if e = json.Unmarshal([]byte(raw), &r); e != nil {
			return e
		}
		if r.Run.CurrentAttemptID == attempt && r.Run.State != execution.RunRunning && r.Run.State != execution.RunExecutionSucceeded {
			return errcode.New(errcode.LeaseStale, "execution writes revoked")
		}
	}
	return rows.Err()
}

func (s *Service) activation(ctx context.Context, p ae.ExecutionProfile) error {
	if p.AdapterKind != "manual_cli" || p.AdapterVersion != "1.0.0" {
		return errcode.New(errcode.UnsupportedCapability, "only the built-in manual adapter is enabled")
	}
	if p.ActivationSnapshot == (ae.ActivationSnapshot{}) {
		return nil
	}
	if s.d.Activations == nil {
		return errcode.New(errcode.ExtensionActivationStale, "extension activation authority is unavailable")
	}
	return s.d.Activations.CheckExecution(ctx, p.ActivationSnapshot)
}
