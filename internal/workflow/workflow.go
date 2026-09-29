// Package workflow 是 T05 的业务流程模块：runtime.db 中 Flow、业务 StepRun、
// 输入锁定、步骤转移去重与待发命令的唯一写入者。外层流程只执行已固定版本的
// 定义（M2 为固定顺序：制作 →（检查）→（质检）→ 审定 → 发布），按领域事件
// 与权威事实推进；不调用模型、不运行任意图、不启动定时器或后台 goroutine。
// 任务与租约归 tasks，审定与发布归 T03，作业归 T06。
package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

const (
	Module       = "workflow"
	consumerName = "workflow.flows"
)

type Authority interface {
	authz.Authorizer
	authz.EpochSource
}

// Ledger 是 T03 的只读权威。调用在事务之外进行，不取锁。
type Ledger interface {
	VersionByID(context.Context, ids.ID) (commit.Committed, error)
	LatestVersion(context.Context, ids.ID) (commit.Committed, error)
	Asset(context.Context, ids.ID) (commit.Asset, error)
	CheckVersionRead(context.Context, ids.ID, ids.ID) error
	VersionControl(context.Context, ids.ID) (ledger.VersionControl, error)
	AssetControl(context.Context, ids.ID) (ledger.AssetControl, error)
	EvidenceIDByOperation(context.Context, ids.ID) (ids.ID, error)
}

// Reviews 是流程调用的 T03 审定/发布接口；目标在最终接受时复验全部条件。
type Reviews interface {
	Submit(context.Context, authz.Context, string, ledger.SubmitReview) (ledger.ReviewTarget, error)
	RunPublication(context.Context, authz.Context, ids.ID) (ledger.Publication, error)
	PublicationRequests(context.Context, authz.Context, ids.ID) ([]ledger.PublicationRequest, error)
	ReviewByID(context.Context, ids.ID) (ledger.ReviewFact, error)
	TargetFact(context.Context, ids.ID) (ledger.ReviewTarget, error)
}

// Manifests 读取已提交版本的冻结清单，用于解析流程定义。
type Manifests interface {
	ReadManifest(context.Context, commit.Committed) ([]byte, error)
}

// Jobs 是 T06 的作业端口。未接入时检查步骤进入阻塞，不伪造检查结果。
type Jobs interface {
	StartJob(context.Context, authz.Context, JobRequest) (JobRef, error)
	JobResult(context.Context, ids.ID) (JobResult, error)
}

type JobRequest struct {
	OperationID    ids.ID            `json:"operation_id"`
	ProjectID      ids.ID            `json:"project_id"`
	FlowID         ids.ID            `json:"flow_id"`
	StepRunID      ids.ID            `json:"step_run_id"`
	Processor      string            `json:"processor"`
	Target         ids.PermanentRef  `json:"target"`
	Flow           ledger.ReviewFlow `json:"flow"`
	ManifestDigest string            `json:"manifest_digest"`
}
type JobRef struct {
	JobID ids.ID `json:"job_id"`
}

// JobEvidence 是作业接受的一条检查证据及其接受操作。
type JobEvidence struct {
	EvidenceID  ids.ID `json:"evidence_id"`
	OperationID ids.ID `json:"operation_id"`
	Verdict     string `json:"verdict"`
}

// JobResult 来自 T06 权威：running、succeeded（证据已接受）或 failed（宿主故障）。
type JobResult struct {
	State    string        `json:"state"`
	Evidence []JobEvidence `json:"evidence"`
	Failure  string        `json:"failure,omitempty"`
}

// CheckExecutions 由 T06 实现：核实检查证据来自真实完成的作业与当前激活代次。
type CheckExecutions interface {
	VerifyCheck(context.Context, authz.Context, commit.Committed, ids.ID, ledger.ReviewEvidenceInput, bool) error
}

type Deps struct {
	DB         *sql.DB
	Gate       *commands.Gate
	Authority  Authority
	Tasks      *tasks.Service
	Ledger     Ledger
	Reviews    Reviews
	Manifests  Manifests
	Events     *events.Store
	Jobs       Jobs
	Checks     CheckExecutions
	Clock      clock.Clock
	IDs        *ids.Generator
	InstanceID ids.ID
}

type Service struct {
	d        Deps
	store    *commands.Store
	consumer *events.Consumer
	wiring   sync.RWMutex
	reviews  Reviews
}

func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Gate == nil || d.Authority == nil || d.Tasks == nil || d.Ledger == nil || d.Manifests == nil || d.Events == nil || d.IDs == nil || !d.InstanceID.Valid() {
		return nil, errors.New("workflow: runtime, gate, identity, tasks, ledger, manifests, events and ids are required")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	st, err := commands.NewStore(Module, d.Clock)
	if err != nil {
		return nil, err
	}
	c, err := events.NewConsumer(d.DB, consumerName, d.Clock, d.Gate)
	if err != nil {
		return nil, err
	}
	w := &Service{d: d, store: st, consumer: c, reviews: d.Reviews}
	d.Tasks.SetSteps(w)
	return w, nil
}

// SetReviews 在组装后接入 T03 审定服务：审定来源又依赖本模块的执行桥。
func (w *Service) SetReviews(r Reviews) {
	w.wiring.Lock()
	defer w.wiring.Unlock()
	w.reviews = r
}

func (w *Service) reviewService() (Reviews, error) {
	w.wiring.RLock()
	defer w.wiring.RUnlock()
	if w.reviews == nil {
		return nil, errcode.New(errcode.InvalidStateTransition, "T03 review authority is not configured")
	}
	return w.reviews, nil
}

func (w *Service) now() time.Time { return w.d.Clock.Now() }

func (w *Service) authorize(ctx context.Context, who authz.Context, action authz.Action, project ids.ID, kind string, id ids.ID) error {
	d, err := w.d.Authority.Authorize(ctx, who, action, authz.Resource{ProjectID: project, Kind: kind, ID: id})
	if err != nil {
		return err
	}
	return d.Err()
}

func (w *Service) lock(ctx context.Context, project ids.ID) (context.Context, func(), error) {
	lctx, h, err := w.d.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared, Projects: []string{string(project)}})
	if err != nil {
		return ctx, nil, err
	}
	return lctx, h.Release, nil
}

func (w *Service) command(ctx context.Context, who authz.Context, key, typ string, project ids.ID, body any) (commands.Context, error) {
	raw, err := canonjson.CanonicalizeValue(body)
	if err != nil {
		return commands.Context{}, err
	}
	if len(raw) > 48<<10 {
		return commands.Context{}, invalid("command request too large")
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: typ, ProjectID: project, Body: raw})
	if err != nil {
		return commands.Context{}, err
	}
	epoch, err := w.d.Authority.RecoveryEpoch(ctx)
	if err != nil {
		return commands.Context{}, err
	}
	id, err := w.d.IDs.New()
	if err != nil {
		return commands.Context{}, err
	}
	cmd := commands.Context{OperationID: id, IdempotencyKey: key, CommandType: typ, ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: project, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: who.PolicyRevision}
	if err = cmd.Validate(); err != nil {
		return commands.Context{}, invalid(err.Error())
	}
	return cmd, nil
}

// Result 是流程命令回执中的紧凑结果。
type Result struct {
	OperationID ids.ID `json:"operation_id"`
	FlowID      ids.ID `json:"flow_id"`
	Revision    int64  `json:"revision"`
	State       string `json:"state"`
}

func (w *Service) replay(ctx context.Context, cmd commands.Context) (*Result, error) {
	r, err := w.store.LookupReceipt(ctx, w.d.DB, cmd.Key())
	if err != nil || r == nil {
		return nil, err
	}
	switch commands.Decide(r, cmd.RequestHash, w.now()) {
	case commands.OutcomeConflict:
		return nil, errcode.New(errcode.IdempotencyConflict, "").WithOperation(string(r.OperationID))
	case commands.OutcomeExpired:
		return nil, errcode.New(errcode.IdempotencyResultExpired, "").WithOperation(string(r.OperationID))
	case commands.OutcomeInProgress:
		return nil, errcode.New(errcode.ResourceBusy, "").WithOperation(string(r.OperationID))
	}
	var out Result
	if err = json.Unmarshal(r.ResponseSummary, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (w *Service) execute(ctx context.Context, cmd commands.Context, fn func(context.Context, *sql.Tx) (Result, []event.Envelope, error)) (Result, error) {
	var out Result
	resp, err := w.store.Execute(ctx, w.d.DB, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		r, evs, err := fn(ctx, tx)
		if err != nil {
			return commands.Result{}, err
		}
		r.OperationID = cmd.OperationID
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: r, Events: evs, ResultRefs: []commands.ResultRef{{Kind: "flow", ID: r.FlowID, Revision: r.Revision}}}, nil
	})
	if err != nil {
		if errors.Is(err, commands.ErrReceiptRace) {
			if r, e := w.replay(ctx, cmd); e == nil && r != nil {
				return *r, nil
			}
		}
		return out, err
	}
	err = json.Unmarshal(resp.Receipt.ResponseSummary, &out)
	return out, err
}

// flowEvent 构造流程事件；流程推进由消费者与派发器产生时，actor 为发起命令者。
func (w *Service) flowEvent(actor, session, op ids.ID, typ string, f Flow, payload any) (event.Envelope, error) {
	return event.New(w.d.IDs, w.d.Clock, event.Params{EventType: typ, SchemaVersion: 1, AggregateType: "flow", AggregateID: f.ID, AggregateRevision: f.Revision, ActorID: actor, SessionID: session, ProjectID: f.ProjectID, OperationID: op, Payload: payload})
}

func invalid(msg string) error { return errcode.New(errcode.SchemaInvalid, msg) }
func stateErr(reason string) error {
	return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: reason})
}
func missing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errcode.New(errcode.NotFound, "")
	}
	return err
}
func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
