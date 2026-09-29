package workflow

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

// 以下端口供 T03 审定、归档与 tasks 调用。它们继承调用者的锁，只读 runtime
// 与 T03 权威，不再取锁、不写数据。
var (
	_ ledger.ReviewExecution = (*ReviewExecution)(nil)
	_ ledger.Activity        = (*Service)(nil)
	_ ledger.ProjectActivity = (*Service)(nil)
	_ tasks.Steps            = (*Service)(nil)
)

// VerifyStepTask 核对流程创建的任务对应一个仍在推进的任务步骤，且规格一致。
func (w *Service) VerifyStepTask(ctx context.Context, project, flow, step ids.ID, spec digest.Digest) error {
	r, err := loadFlow(ctx, w.d.DB, flow)
	if err != nil {
		return err
	}
	s := r.step(step)
	if r.flow.ProjectID != project || s == nil || s.run.Kind != "task" {
		return errcode.New(errcode.FlowRequired, "").WithDetails(errcode.Detail{Reason: "step_not_found"})
	}
	if !activeFlow(r.flow.State) || !open(s.run.State) || s.run.State == "blocked" || s != r.latest(s.run.StepKey) {
		return errcode.New(errcode.FlowRequired, "").WithDetails(errcode.Detail{Reason: "step_not_active"})
	}
	if s.meta.TaskSpec != spec {
		return errcode.New(errcode.FlowRequired, "").WithDetails(errcode.Detail{Reason: "task_spec_mismatch"})
	}
	return nil
}

// RequireIdle 在归档等最终接受时核对资产没有未结束的任务、签出或流程。
func (w *Service) RequireIdle(ctx context.Context, project ids.ID, assets []ids.ID) error {
	uses, err := w.d.Tasks.OpenUses(ctx, assets)
	if err != nil {
		return err
	}
	if len(uses) > 0 {
		return errcode.New(errcode.AssetInUse, "").WithDetails(errcode.Detail{Reason: "open_task_or_checkout"})
	}
	for _, a := range assets {
		binds, err := bound(ctx, w.d.DB, "asset", a)
		if err != nil {
			return err
		}
		for _, b := range binds {
			var state string
			if err := w.d.DB.QueryRowContext(ctx, `SELECT state FROM workflow_flows WHERE flow_id=?`, b[0]).Scan(&state); err != nil {
				return missing(err)
			}
			if state != "completed" && state != "cancelled" {
				return errcode.New(errcode.AssetInUse, "").WithDetails(errcode.Detail{Reason: "open_flow"})
			}
		}
	}
	return nil
}

// RequireProjectIdle 核对项目内没有未结束的任务或流程（含未绑定资产的流程）。
func (w *Service) RequireProjectIdle(ctx context.Context, project ids.ID) error {
	n, err := w.d.Tasks.OpenInProject(ctx, project)
	if err != nil {
		return err
	}
	var flows int
	if err = w.d.DB.QueryRowContext(ctx, `SELECT count(*) FROM workflow_flows WHERE project_id=? AND state NOT IN ('completed','cancelled')`, project).Scan(&flows); err != nil {
		return err
	}
	if n > 0 || flows > 0 {
		return errcode.New(errcode.AssetInUse, "").WithDetails(errcode.Detail{Reason: "project_has_open_work"})
	}
	return nil
}

// ReviewExecution 是 T03 审定的 T05/T06 执行桥：任务身份、轮次与 fence 来自
// tasks 权威，流程状态来自 workflow；检查结果的执行核实委托 T06。
type ReviewExecution struct{ w *Service }

func (w *Service) ReviewExecution() *ReviewExecution { return &ReviewExecution{w} }

// Task 核对 submit/review/auto_publish 引用的是当前轮次的交付 Attempt。
// 返工后旧轮次、旧 Attempt 或旧 fence 的审定目标一律拒绝；流程暂停或结束
// 时拒绝审定与自动发布。
func (x *ReviewExecution) Task(ctx context.Context, who authz.Context, v commit.Committed, f ledger.ReviewFlow, action string) error {
	w := x.w
	view, err := w.d.Tasks.Snapshot(ctx, f.TaskID)
	if err != nil {
		return err
	}
	t := view.Task
	if t.ProjectID != v.ProjectID {
		return errcode.New(errcode.RefMismatch, "")
	}
	staleTarget := func(reason string) error {
		return errcode.New(errcode.ReviewTargetStale, "").WithDetails(errcode.Detail{Reason: reason})
	}
	a := view.Attempt
	if int64(view.Meta.Round) != f.Round || a == nil || a.ID != f.AttemptID || a.Fence.LeaseFence != f.Fence {
		return staleTarget("round_or_attempt_superseded")
	}
	if f.CandidateGroup != "" && (f.CandidateGroup != t.ID || !slices.ContainsFunc(t.ExpectedOutputs, func(o tc.OutputRequirement) bool { return o.CandidateCount > 1 })) {
		return staleTarget("candidate_group_not_authorized")
	}
	delivered := slices.ContainsFunc(t.OutputRefs, func(p ids.PermanentRef) bool { return p.VersionID == v.VersionID })
	if t.FlowID != "" {
		r, err := loadFlow(ctx, w.d.DB, t.FlowID)
		if err != nil {
			return err
		}
		if !activeFlow(r.flow.State) {
			return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "flow_" + string(r.flow.State)})
		}
	}
	switch action {
	case "submit":
		switch t.State {
		case "submitted":
			if !delivered {
				return staleTarget("version_not_delivered")
			}
			if a.PrincipalID == who.PrincipalID {
				return nil
			}
			return w.authorize(ctx, who, "workflow.operate", t.ProjectID, "task", t.ID)
		case "claimed":
			// 制作者在自己的有效轮次内提交本人提交的版本。
			if a.State != "active" || a.PrincipalID != who.PrincipalID || a.SessionID != who.SessionID || v.CommittedBy != who.PrincipalID {
				return errcode.New(errcode.LeaseStale, "")
			}
			return w.leaseValid(ctx, *a, view.Seat.LastLeaseFence)
		}
		return staleTarget("task_not_delivering")
	case "review":
		if t.State != "submitted" || !delivered {
			return staleTarget("task_not_awaiting_acceptance")
		}
		return nil
	case "auto_publish":
		if t.State != "submitted" && t.State != "done" || !delivered {
			return staleTarget("task_not_accepted")
		}
		return w.authorize(ctx, who, "workflow.operate", t.ProjectID, "task", t.ID)
	}
	return errcode.New(errcode.SchemaInvalid, "unsupported review execution action")
}

// VerifyEvidence 核对质检报告来自独立质检任务的当前执行者；检查结果委托
// T06 核实真实作业完成与处理器激活，未接入时拒绝。
func (x *ReviewExecution) VerifyEvidence(ctx context.Context, who authz.Context, v commit.Committed, actor ids.ID, in ledger.ReviewEvidenceInput, historical bool) error {
	w := x.w
	switch in.Kind {
	case "check_result":
		if w.d.Checks == nil {
			return errcode.New(errcode.UnsupportedCapability, "").WithDetails(errcode.Detail{Reason: "check_execution_authority_not_configured"})
		}
		return w.d.Checks.VerifyCheck(ctx, who, v, actor, in, historical)
	case "qa_report":
	default:
		return errcode.New(errcode.SchemaInvalid, "unsupported evidence kind")
	}
	list, err := w.d.Tasks.SubjectTasks(ctx, v.ProjectID, in.Flow.TaskID, "qa")
	if err != nil {
		return err
	}
	subject := tasks.Subject{Flow: in.Flow, Ref: in.Ref}
	for _, q := range list {
		if q.Meta.Subject == nil || *q.Meta.Subject != subject || slices.Contains(q.Meta.DistinctFrom, actor) {
			continue
		}
		if historical {
			// 已接受的历史证据只需证明作者曾是该质检任务的执行轮次主体。
			history, err := w.d.Tasks.AttemptHistory(ctx, q.Task.ID)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(history, func(a tasks.Attempt) bool { return a.PrincipalID == actor }) {
				return nil
			}
			continue
		}
		a := q.Attempt
		if a == nil || q.Task.State != "claimed" || a.State != "active" || a.PrincipalID != actor || actor != who.PrincipalID || a.SessionID != who.SessionID {
			continue
		}
		return w.leaseValid(ctx, *a, q.Seat.LastLeaseFence)
	}
	return errcode.New(errcode.ChecksNotSatisfied, "").WithDetails(errcode.Detail{Reason: "qa_task_attempt_required"})
}

// leaseValid 按当前恢复代次与服务端时间核对执行轮次仍持有有效租约。
func (w *Service) leaseValid(ctx context.Context, a tasks.Attempt, lastFence int64) error {
	epoch, err := w.d.Authority.RecoveryEpoch(ctx)
	if err != nil {
		return err
	}
	expires, err := clock.Parse(a.ExpiresAt)
	if err != nil {
		return err
	}
	return execution.CheckFence(a.Fence, execution.LeaseState{AttemptID: a.ID, LeaseFence: lastFence, RecoveryEpoch: epoch, ExpiresAt: expires}, w.now())
}

// FlowView 是按当前读取权限返回的流程完整状态。
type FlowView struct {
	Flow     Flow          `json:"flow"`
	Meta     FlowMeta      `json:"meta"`
	Steps    []StepView    `json:"steps"`
	Commands []CommandView `json:"commands"`
}
type StepView struct {
	StepRun StepRun  `json:"step_run"`
	Meta    StepMeta `json:"meta"`
}
type CommandView struct {
	OperationID ids.ID          `json:"operation_id"`
	StepRunID   ids.ID          `json:"step_run_id"`
	CommandType string          `json:"command_type"`
	Status      string          `json:"status"`
	ActorID     ids.ID          `json:"actor_id,omitempty"`
	Attempts    int             `json:"attempts"`
	FailureCode string          `json:"failure_code,omitempty"`
	Payload     json.RawMessage `json:"payload"`
}

func (w *Service) Flow(ctx context.Context, who authz.Context, id ids.ID) (FlowView, error) {
	r, err := loadFlow(ctx, w.d.DB, id)
	if err != nil {
		return FlowView{}, err
	}
	if err = w.authorize(ctx, who, "workflow.read", r.flow.ProjectID, "flow", id); err != nil {
		return FlowView{}, err
	}
	out := FlowView{Flow: r.flow, Meta: r.meta, Steps: []StepView{}, Commands: []CommandView{}}
	for _, s := range r.steps {
		out.Steps = append(out.Steps, StepView{StepRun: s.run, Meta: s.meta})
	}
	rows, err := w.d.DB.QueryContext(ctx, `SELECT operation_id,step_run_id,command_type,status,actor_id,attempts,failure_code,payload FROM workflow_commands WHERE flow_id=? ORDER BY created_at,operation_id`, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c CommandView
		var raw string
		if err = rows.Scan(&c.OperationID, &c.StepRunID, &c.CommandType, &c.Status, &c.ActorID, &c.Attempts, &c.FailureCode, &raw); err != nil {
			return out, err
		}
		c.Payload = json.RawMessage(raw)
		out.Commands = append(out.Commands, c)
	}
	return out, rows.Err()
}

// List 按流程 ID 递增分页返回项目流程。
func (w *Service) List(ctx context.Context, who authz.Context, project, after ids.ID, limit int) ([]Flow, error) {
	if !project.Valid() || limit < 1 || limit > 500 || after != "" && !after.Valid() {
		return nil, invalid("project and limit 1-500 required")
	}
	if err := w.authorize(ctx, who, "workflow.read", project, "project", project); err != nil {
		return nil, err
	}
	rows, err := w.d.DB.QueryContext(ctx, `SELECT record FROM workflow_flows WHERE project_id=? AND flow_id>? ORDER BY flow_id LIMIT ?`, project, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Flow{}
	for rows.Next() {
		var raw string
		var f Flow
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
