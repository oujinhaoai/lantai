package tasks

import (
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type TaskState string
type SeatState string
type AttemptState string
type FlowState string
type StepState string

// 状态转移表只判断业务边；todo→done 必须再经 CheckTaskCompletion 限定 review/qa。
// 权限、输入/定义、修订、fence 和安全回收还需分别复验。
var taskEdges = map[TaskState][]TaskState{
	"waiting": {"todo", "cancelled"}, "todo": {"claimed", "done", "cancelled"}, "claimed": {"reconciling", "blocked", "submitted"},
	"reconciling": {"todo", "cancelled"}, "blocked": {"reconciling"}, "submitted": {"done", "rework", "cancelled"}, "rework": {"claimed", "cancelled"},
}
var seatEdges = map[SeatState][]SeatState{"open": {"claimed", "cancelled"}, "claimed": {"reconciling", "submitted"}, "reconciling": {"open", "cancelled"}, "submitted": {"done", "open", "cancelled"}}
var attemptEdges = map[AttemptState][]AttemptState{"active": {"reconciling", "submitted"}, "reconciling": {"released", "cancelled", "expired"}}
var flowEdges = map[FlowState][]FlowState{"running": {"waiting", "paused", "completed", "failed", "cancelled"}, "waiting": {"running", "paused", "failed", "cancelled"}, "paused": {"running", "cancelled"}, "failed": {"running", "cancelled"}}
var stepEdges = map[StepState][]StepState{"pending": {"ready", "cancelled"}, "ready": {"running", "cancelled"}, "running": {"waiting", "blocked", "completed", "failed", "cancelled"}, "waiting": {"running", "blocked", "completed", "failed", "cancelled"}, "blocked": {"ready", "cancelled"}}

func TaskTransition(from, to TaskState) bool       { return slices.Contains(taskEdges[from], to) }
func SeatTransition(from, to SeatState) bool       { return slices.Contains(seatEdges[from], to) }
func AttemptTransition(from, to AttemptState) bool { return slices.Contains(attemptEdges[from], to) }
func FlowTransition(from, to FlowState) bool       { return slices.Contains(flowEdges[from], to) }
func StepTransition(from, to StepState) bool       { return slices.Contains(stepEdges[from], to) }

func (a Attempt) Validate() error {
	if err := ValidateShape("lantai.attempt/v1", a); err != nil {
		return err
	}
	if a.Fence.AttemptID != a.ID || (a.Fence.TaskID != "" && a.Fence.TaskID != a.TaskID) {
		return invalid("attempt_fence_ref_mismatch")
	}
	issued, err := clock.Parse(a.IssuedAt)
	if err != nil {
		return err
	}
	expires, err := clock.Parse(a.ExpiresAt)
	if err != nil {
		return err
	}
	if !expires.After(issued) {
		return invalid("lease_interval_invalid")
	}
	return nil
}
func CheckRevision(expected, current int64) error {
	if expected < 1 || current < 1 {
		return invalid("revision_invalid")
	}
	if expected != current {
		return errcode.New(errcode.PreconditionFailed, "")
	}
	return nil
}

// SafeToReclaim 不从到期、撤权或 checkpoint 回滚推断外部进程已停止。
// 只有 owner 收集的确定性证据可提供此参数，不能信任调用方自报。
func SafeToReclaim(r Reconciliation) error {
	if r.UnresolvedEffects < 0 {
		return invalid("negative_unresolved_effects")
	}
	if !r.TerminationConfirmed || r.UnresolvedEffects != 0 {
		return errcode.New(errcode.OperationNeedsReconciliation, "").WithDetails(errcode.Detail{Reason: "prior_execution_unresolved"})
	}
	return nil
}

// CheckClaim 只核对当前可领取事实，不发号。真正的 tasks owner 必须原子条件
// 更新 seat.revision，并生成新 Attempt 与大于 last_lease_fence 的 fence。
func CheckClaim(t Task, s Seat, previous *Attempt, expected, epoch int64) error {
	if err := ValidateShape("lantai.task/v1", t); err != nil {
		return err
	}
	if err := ValidateShape("lantai.seat/v1", s); err != nil {
		return err
	}
	if err := t.Input.Validate(); err != nil {
		return err
	}
	if s.TaskID != t.ID || !slices.Contains(t.SeatIDs, s.ID) {
		return invalid("seat_task_mismatch")
	}
	if err := execution.CheckRecoveryEpoch(execution.SubjectAttempt, epoch, s.RecoveryEpoch); err != nil {
		return err
	}
	if err := CheckRevision(expected, s.Revision); err != nil {
		return err
	}
	if s.State == "reconciling" {
		return errcode.New(errcode.OperationNeedsReconciliation, "")
	}
	if s.State != "open" {
		return errcode.New(errcode.TaskAlreadyClaimed, "")
	}
	if t.Type == "review" {
		return errcode.New(errcode.InvalidStateTransition, "review tasks do not issue production leases")
	}
	if t.State != "todo" && t.State != "rework" && t.State != "claimed" {
		return errcode.New(errcode.InvalidStateTransition, "")
	}
	if s.LastLeaseFence > 0 && previous == nil {
		return errcode.New(errcode.OperationNeedsReconciliation, "")
	}
	if previous != nil {
		if err := previous.Validate(); err != nil {
			return err
		}
		if previous.SeatID != s.ID || previous.TaskID != t.ID || previous.ID != s.CurrentAttemptID || previous.Fence.LeaseFence != s.LastLeaseFence {
			return invalid("previous_attempt_mismatch")
		}
		if previous.State == "active" {
			return errcode.New(errcode.TaskAlreadyClaimed, "")
		}
		if err := SafeToReclaim(previous.Reconciliation); err != nil {
			return err
		}
	}
	return nil
}

// CheckNewAttempt 验证 tasks 的预设领取回执，恢复代次变化也不能复用旧 Attempt ID。
func CheckNewAttempt(s Seat, a Attempt) error {
	if err := ValidateShape("lantai.seat/v1", s); err != nil {
		return err
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if a.SeatID != s.ID || a.TaskID != s.TaskID || a.ID == s.CurrentAttemptID || a.Fence.LeaseFence <= s.LastLeaseFence || a.Fence.RecoveryEpoch != s.RecoveryEpoch || a.State != "active" {
		return invalid("new_attempt_mismatch")
	}
	return nil
}

// AcceptCommand 在最终条件更新时调用；绑定 Session、当前 Attempt、修订与 epoch。
// now 取服务端提交时间，调用者持 owner 协调锁并先重验当前权限。
func AcceptCommand(c Command, t Task, s Seat, a Attempt, now time.Time, currentEpoch int64) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := a.Validate(); err != nil {
		return err
	}
	if err := ValidateShape("lantai.task/v1", t); err != nil {
		return err
	}
	if err := ValidateShape("lantai.seat/v1", s); err != nil {
		return err
	}
	if err := t.Input.Validate(); err != nil {
		return err
	}
	if c.ProjectID != t.ProjectID || a.TaskID != t.ID || a.SeatID != s.ID || s.TaskID != t.ID || s.CurrentAttemptID != a.ID || c.TargetID != a.ID {
		return invalid("command_target_mismatch")
	}
	if err := execution.CheckRecoveryEpoch(execution.SubjectAttempt, c.RecoveryEpoch, currentEpoch); err != nil {
		return err
	}
	if s.RecoveryEpoch != currentEpoch || a.Fence.RecoveryEpoch != currentEpoch || a.Fence.LeaseFence != s.LastLeaseFence {
		return errcode.New(errcode.LeaseStale, "")
	}
	if err := CheckRevision(c.ExpectedRevision, a.Revision); err != nil {
		return err
	}
	if c.Fence == nil {
		return errcode.New(errcode.LeaseStale, "").WithDetails(errcode.Detail{Reason: "fence_missing"})
	}
	if c.Fence.TaskID != "" && c.Fence.TaskID != t.ID {
		return errcode.New(errcode.LeaseStale, "").WithDetails(errcode.Detail{Reason: "task_mismatch"})
	}
	if c.SessionID != a.SessionID {
		return errcode.New(errcode.LeaseStale, "").WithDetails(errcode.Detail{Reason: "session_mismatch"})
	}
	if (t.State != "claimed" && t.State != "blocked") || (c.Type == "tasks.submit" && t.State == "blocked") {
		return errcode.New(errcode.InvalidStateTransition, "")
	}
	expires, _ := clock.Parse(a.ExpiresAt)
	return execution.CheckFence(*c.Fence, execution.LeaseState{AttemptID: a.ID, LeaseFence: s.LastLeaseFence, RecoveryEpoch: currentEpoch, ExpiresAt: expires, Terminated: a.State != "active" || s.State != "claimed"}, now)
}

// AdvanceEvidence 来自领域权威读取；execution_succeeded 不能替代 ledger 的审定/发布事实。
type AdvanceEvidence struct {
	AuthorityOperationID                                 ids.ID
	ReviewApproved, Published                            bool
	TaskDone, JobCompleted, GateSatisfied, WaitSatisfied bool
}

func CheckStepCompletion(s StepRun, e AdvanceEvidence) error {
	if err := ValidateShape("lantai.step-run/v1", s); err != nil {
		return err
	}
	if !StepTransition(s.State, "completed") {
		return errcode.New(errcode.InvalidStateTransition, "")
	}
	if (s.Kind == "task" && !e.TaskDone) || (s.Kind == "job" && !e.JobCompleted) || (s.Kind == "gate" && !e.GateSatisfied) || (s.Kind == "wait" && !e.WaitSatisfied) {
		return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "step_authority_required"})
	}
	if s.Kind == "review" && (!e.ReviewApproved || !e.AuthorityOperationID.Valid()) {
		return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "review_authority_required"})
	}
	if s.Kind == "publish" && (!e.Published || !e.AuthorityOperationID.Valid()) {
		return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "publication_authority_required"})
	}
	return nil
}

// CompletionEvidence 由相应 owner 提供；审定拒绝也是一次已结束的 review 任务，
// 是否推进批准分支由 workflow 另按 ledger 结论裁决。
type CompletionEvidence struct {
	AuthorityOperationID                       ids.ID
	ReviewDecided, QAReported, OutputsAccepted bool
}

func CheckTaskCompletion(t Task, e CompletionEvidence) error {
	if err := ValidateShape("lantai.task/v1", t); err != nil {
		return err
	}
	allowed := t.State == "submitted" && e.OutputsAccepted
	if t.Type == "review" {
		allowed = t.State == "todo" && e.ReviewDecided
	}
	if t.Type == "qa" {
		allowed = (t.State == "todo" || t.State == "submitted") && e.QAReported
	}
	if !allowed || !e.AuthorityOperationID.Valid() {
		return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "task_completion_authority_required"})
	}
	return nil
}

// CheckStepBinding 保证一次 Flow 运行固定定义版本；更新定义只能新建 Flow。
func CheckStepBinding(f Flow, s StepRun) error {
	if err := ValidateShape("lantai.flow/v1", f); err != nil {
		return err
	}
	if err := ValidateShape("lantai.step-run/v1", s); err != nil {
		return err
	}
	if s.FlowID != f.ID || !slices.Contains(f.StepRunIDs, s.ID) || s.Definition != f.Definition {
		return invalid("flow_definition_mismatch")
	}
	return nil
}
