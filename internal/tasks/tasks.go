// Package tasks 是 T05 的任务模块：runtime.db 中 Task/Seat/Attempt、任务租约
// 与 fence、签出、指派、阻塞/答复与交接的唯一写入者。业务结果、回执与 outbox
// 在同一 runtime 事务提交；审定、发布、资源锁归 T03，身份与授权归 T01，
// Agent 执行映射归 T06。本包不启动定时器、后台 goroutine 或外部进程。
package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
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
	"github.com/oujinhaoai/lantai/internal/ledger"
)

const Module = "tasks"

// 默认同一主体同时持有的有效 Attempt 上限；领取在最终接受时计数。
const DefaultMaxActive = 8

type Authority interface {
	authz.Authorizer
	authz.EpochSource
}

// Policies 读取项目生效策略（task.lease_minutes、task.max_attempts）。
type Policies interface {
	ResolvePolicies(context.Context, ids.ID) (map[string]identity.PolicyValue, error)
}

// Members 读取当前活跃项目成员与角色，用于角色池领取与收件箱收件人。
type Members interface {
	ProjectRecipients(context.Context, ids.ID) ([]identity.Member, error)
}

// Resources 是 T03 台账的只读权威。调用在本模块锁内、runtime 事务之外进行，
// 实现不得再取维护屏障或 security_guard。
type Resources interface {
	Asset(context.Context, ids.ID) (commit.Asset, error)
	VersionByID(context.Context, ids.ID) (commit.Committed, error)
	LatestVersion(context.Context, ids.ID) (commit.Committed, error)
	CheckVersionRead(context.Context, ids.ID, ids.ID) error
	CheckAssetWrite(context.Context, ids.ID) error
}

// Discussions 读取 T03 不可变讨论消息；阻塞、答复与交接只引用已写入的消息。
type Discussions interface {
	DiscussionMessage(context.Context, ids.ID) (ledger.Message, error)
}

// Authorities 提供任务完成/返工所需的 T03 权威事实：审定记录与检查/质检证据。
type Authorities interface {
	ReviewByOperation(context.Context, ids.ID) (ledger.ReviewFact, error)
	EvidenceByOperation(context.Context, authz.Context, ids.ID) (ledger.AcceptedEvidence, error)
}

// Contexts 固定任务开始时生效的项目上下文与决议（T02/T03）。
type Contexts interface {
	EffectiveContext(context.Context, authz.Context, ids.ID, manifest.AssetType) (catalog.ContextBundle, error)
}

// Steps 由 workflow 实现：流程创建的任务必须对应一个仍在推进的业务步骤。
type Steps interface {
	VerifyStepTask(ctx context.Context, project, flow, step ids.ID, spec digest.Digest) error
}

type Deps struct {
	DB          *sql.DB
	Gate        *commands.Gate
	Authority   Authority
	Policies    Policies
	Members     Members
	Resources   Resources
	Discussions Discussions
	Authorities Authorities
	Contexts    Contexts
	Clock       clock.Clock
	IDs         *ids.Generator
	InstanceID  ids.ID
	MaxActive   int
}

type Service struct {
	d      Deps
	store  *commands.Store
	wiring sync.RWMutex
	steps  Steps
	facts  Authorities
}

func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Gate == nil || d.Authority == nil || d.Policies == nil || d.Members == nil || d.Resources == nil || d.IDs == nil || !d.InstanceID.Valid() {
		return nil, errors.New("tasks: runtime, gate, identity, policy, membership, ledger and ids are required")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.MaxActive < 1 {
		d.MaxActive = DefaultMaxActive
	}
	st, err := commands.NewStore(Module, d.Clock)
	if err != nil {
		return nil, err
	}
	return &Service{d: d, store: st, facts: d.Authorities}, nil
}

// SetSteps 在组装后接入 workflow；未接入时拒绝流程创建的任务。
func (s *Service) SetSteps(v Steps) {
	s.wiring.Lock()
	defer s.wiring.Unlock()
	s.steps = v
}

// SetAuthorities 在组装后接入 T03 审定与证据读取（审定来源依赖 T05 执行桥）。
func (s *Service) SetAuthorities(v Authorities) {
	s.wiring.Lock()
	defer s.wiring.Unlock()
	s.facts = v
}

func (s *Service) wired() (Steps, Authorities) {
	s.wiring.RLock()
	defer s.wiring.RUnlock()
	return s.steps, s.facts
}

func (s *Service) now() time.Time { return s.d.Clock.Now() }

// lock 取得维护屏障共享、security_guard 读、项目锁与任务锁。项目锁与台账
// 版本提交串行，签出变化和追加版本的最终接受因此不会交错。
func (s *Service) lock(ctx context.Context, project, task ids.ID) (context.Context, func(), error) {
	req := commands.Request{Security: commands.ModeShared, Projects: []string{string(project)}}
	if task != "" {
		req.Tasks = []string{string(task)}
	}
	lctx, h, err := s.d.Gate.Acquire(ctx, req)
	if err != nil {
		return ctx, nil, err
	}
	return lctx, h.Release, nil
}

func (s *Service) authorize(ctx context.Context, who authz.Context, action authz.Action, project ids.ID, kind string, id ids.ID) error {
	d, err := s.d.Authority.Authorize(ctx, who, action, authz.Resource{ProjectID: project, Kind: kind, ID: id})
	if err != nil {
		return err
	}
	return d.Err()
}

func (s *Service) epoch(ctx context.Context) (int64, error) { return s.d.Authority.RecoveryEpoch(ctx) }

// command 构造规范化请求摘要的命令上下文。任务、Attempt 与 fence 只作绑定。
func (s *Service) command(ctx context.Context, who authz.Context, key, typ string, project ids.ID, body any) (commands.Context, error) {
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
	epoch, err := s.epoch(ctx)
	if err != nil {
		return commands.Context{}, err
	}
	id, err := s.d.IDs.New()
	if err != nil {
		return commands.Context{}, err
	}
	cmd := commands.Context{OperationID: id, IdempotencyKey: key, CommandType: typ, ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: project, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: who.PolicyRevision}
	if err = cmd.Validate(); err != nil {
		return commands.Context{}, invalid(err.Error())
	}
	return cmd, nil
}

// replay 返回同键同摘要的原结果；过期结果不重新执行。
func (s *Service) replay(ctx context.Context, cmd commands.Context) (*Result, error) {
	r, err := s.store.LookupReceipt(ctx, s.d.DB, cmd.Key())
	if err != nil || r == nil {
		return nil, err
	}
	switch commands.Decide(r, cmd.RequestHash, s.now()) {
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

// execute 在 runtime 事务中写业务状态、outbox 与回执。
func (s *Service) execute(ctx context.Context, cmd commands.Context, fn func(context.Context, *sql.Tx) (Result, []event.Envelope, error)) (Result, error) {
	var out Result
	resp, err := s.store.Execute(ctx, s.d.DB, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		r, events, err := fn(ctx, tx)
		if err != nil {
			return commands.Result{}, err
		}
		r.OperationID = cmd.OperationID
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: r, Events: events, ResultRefs: []commands.ResultRef{{Kind: "task", ID: r.TaskID, Revision: r.Revision}}}, nil
	})
	if err != nil {
		if errors.Is(err, commands.ErrReceiptRace) {
			if r, e := s.replay(ctx, cmd); e == nil && r != nil {
				return *r, nil
			}
		}
		return out, err
	}
	err = json.Unmarshal(resp.Receipt.ResponseSummary, &out)
	return out, err
}

func (s *Service) event(cmd commands.Context, typ string, t Task, payload any) (event.Envelope, error) {
	return event.New(s.d.IDs, s.d.Clock, event.Params{EventType: typ, SchemaVersion: 1, AggregateType: "task", AggregateID: t.ID, AggregateRevision: t.Revision, ActorID: cmd.ActorID, SessionID: cmd.SessionID, ProjectID: t.ProjectID, OperationID: cmd.OperationID, CorrelationID: cmd.Correlation(), Payload: payload})
}

func (s *Service) policies(ctx context.Context, project ids.ID) (lease time.Duration, maxAttempts int, err error) {
	p, err := s.d.Policies.ResolvePolicies(ctx, project)
	if err != nil {
		return 0, 0, err
	}
	minutes, err := policyInt(p, "task.lease_minutes", 30)
	if err != nil {
		return 0, 0, err
	}
	maxAttempts, err = policyInt(p, "task.max_attempts", 3)
	return time.Duration(minutes) * time.Minute, maxAttempts, err
}

func policyInt(p map[string]identity.PolicyValue, key string, def int) (int, error) {
	v, ok := p[key]
	if !ok {
		return def, nil
	}
	var n int
	if err := json.Unmarshal(v.Value, &n); err != nil {
		return 0, errcode.Wrap(errcode.SchemaInvalid, "invalid "+key+" policy", err)
	}
	return n, nil
}

// memberRoles 返回主体在项目中的当前角色；非成员返回空。
func (s *Service) memberRoles(ctx context.Context, project, principal ids.ID) ([]identity.Role, error) {
	members, err := s.d.Members.ProjectRecipients(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		if m.PrincipalID == principal {
			return m.Roles, nil
		}
	}
	return nil, nil
}

func hasAnyRole(have []identity.Role, want ...identity.Role) bool {
	return slices.ContainsFunc(have, func(r identity.Role) bool { return slices.Contains(want, r) })
}

func invalid(msg string) error { return errcode.New(errcode.SchemaInvalid, msg) }
func stateErr(reason string) error {
	return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: reason})
}
func stale(reason string) error {
	return errcode.New(errcode.LeaseStale, "").WithDetails(errcode.Detail{Reason: reason})
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
