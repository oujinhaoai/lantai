package execution

import "slices"

// TaskRunState 是 TaskRun 的状态。它只描述执行是否完成交付，不表示任务
// 完成、候选被审定或资产已发布；这些分别由 tasks 与 ledger 裁决。
type TaskRunState string

const (
	RunPending             TaskRunState = "pending"
	RunStarting            TaskRunState = "starting"
	RunRunning             TaskRunState = "running"
	RunWaitingInput        TaskRunState = "waiting_input"
	RunPaused              TaskRunState = "paused"
	RunCancelling          TaskRunState = "cancelling"
	RunNeedsReconciliation TaskRunState = "needs_reconciliation"
	RunExecutionSucceeded  TaskRunState = "execution_succeeded"
	RunFailed              TaskRunState = "failed"
	RunCancelled           TaskRunState = "cancelled"
)

// TaskRunStates 是全部 TaskRun 状态。
var TaskRunStates = []TaskRunState{RunPending, RunStarting, RunRunning, RunWaitingInput, RunPaused,
	RunCancelling, RunNeedsReconciliation, RunExecutionSucceeded, RunFailed, RunCancelled}

// Valid 报告状态是否已定义。
func (s TaskRunState) Valid() bool { return slices.Contains(TaskRunStates, s) }

// Terminal 报告执行是否已结束。needs_reconciliation 不是终态：在对账前
// 既不能宣称已停止，也不能自动重启。
func (s TaskRunState) Terminal() bool {
	return s == RunExecutionSucceeded || s == RunFailed || s == RunCancelled
}

// ResolveCancel 决定取消请求之后的状态：只有确认停止且没有未决副作用才是
// cancelled；超过宽限期仍未确认或仍有未决副作用则进入 needs_reconciliation，
// 不谎报“已停止”。
func ResolveCancel(terminationConfirmed, graceExpired bool, unresolvedEffects int) TaskRunState {
	switch {
	case terminationConfirmed && unresolvedEffects == 0:
		return RunCancelled
	case terminationConfirmed, graceExpired:
		return RunNeedsReconciliation
	default:
		return RunCancelling
	}
}

// InstanceHealth 是常驻扩展实例的观察状态，与业务执行结果、人类授权无关。
type InstanceHealth string

const (
	HealthResolved    InstanceHealth = "resolved"
	HealthStarting    InstanceHealth = "starting"
	HealthReady       InstanceHealth = "ready"
	HealthDraining    InstanceHealth = "draining"
	HealthStopped     InstanceHealth = "stopped"
	HealthFailed      InstanceHealth = "failed"
	HealthQuarantined InstanceHealth = "quarantined"
)

// InstanceHealthStates 是全部宿主健康状态。
var InstanceHealthStates = []InstanceHealth{HealthResolved, HealthStarting, HealthReady, HealthDraining,
	HealthStopped, HealthFailed, HealthQuarantined}

// Valid 报告状态是否已定义。
func (h InstanceHealth) Valid() bool { return slices.Contains(InstanceHealthStates, h) }

// Dispatchable 报告实例能否接收新调用：只有 ready。ready 不代表任何业务任务完成。
func (h InstanceHealth) Dispatchable() bool { return h == HealthReady }

// ResumeClass 是 adapter 实测声明的恢复方式。
type ResumeClass string

const (
	ResumePortableArtifacts ResumeClass = "portable_artifacts"
	ResumeBackendCheckpoint ResumeClass = "backend_checkpoint"
	ResumeRestartSafe       ResumeClass = "restart_safe"
	ResumeManualOnly        ResumeClass = "manual_only"
)

// ResumeClasses 是全部恢复方式。
var ResumeClasses = []ResumeClass{ResumePortableArtifacts, ResumeBackendCheckpoint, ResumeRestartSafe, ResumeManualOnly}

// CancelMode 是 adapter 能兑现的取消方式。
type CancelMode string

const (
	// CancelRevokeOnly 只撤销兰台写入授权，停止需会话或操作者确认（manual-cli）。
	CancelRevokeOnly CancelMode = "revoke_only"
	// CancelCooperative 请求后端停止，可能延迟。
	CancelCooperative CancelMode = "cooperative"
	// CancelEnforced 由宿主终止进程树。
	CancelEnforced CancelMode = "enforced"
)

// CancelModes 是全部取消方式。
var CancelModes = []CancelMode{CancelRevokeOnly, CancelCooperative, CancelEnforced}

// EffectClass 是工具或外部动作的副作用类别。
type EffectClass string

const (
	EffectPureRead              EffectClass = "pure_read"
	EffectLocalReplaceable      EffectClass = "local_replaceable"
	EffectLantaiCommand         EffectClass = "lantai_command"
	EffectExternalIdempotent    EffectClass = "external_idempotent"
	EffectExternalQueryable     EffectClass = "external_queryable"
	EffectExternalNonIdempotent EffectClass = "external_non_idempotent"
)

// EffectClasses 是全部副作用类别。
var EffectClasses = []EffectClass{EffectPureRead, EffectLocalReplaceable, EffectLantaiCommand,
	EffectExternalIdempotent, EffectExternalQueryable, EffectExternalNonIdempotent}

// EffectState 是 ToolOperation 的副作用进度。
type EffectState string

const (
	// EffectIntended：intent 已持久化，尚未发送。
	EffectIntended EffectState = "intended"
	// EffectDispatched：已发送，结果未知。
	EffectDispatched EffectState = "dispatched"
	// EffectCompleted：已得到并记录成功回执。
	EffectCompleted EffectState = "completed"
	// EffectFailed：外部明确返回失败且无副作用。
	EffectFailed EffectState = "failed"
	// EffectNotExecuted：经对账确认没有执行。
	EffectNotExecuted EffectState = "not_executed"
	// EffectUnknown：结果不明，阻塞恢复。
	EffectUnknown EffectState = "effect_unknown"
)

// EffectStates 是全部副作用进度。
var EffectStates = []EffectState{EffectIntended, EffectDispatched, EffectCompleted, EffectFailed, EffectNotExecuted, EffectUnknown}

// RetryDecision 是恢复时对一个工具动作的处置。
type RetryDecision string

const (
	// DecisionDone：已完成，复用已记录的回执，不再执行。
	DecisionDone RetryDecision = "done"
	// DecisionRerun：可按输入摘要重新执行，结果重新校验。
	DecisionRerun RetryDecision = "rerun"
	// DecisionReuseKey：以同一幂等键重发并核对回执，禁止换键。
	DecisionReuseKey RetryDecision = "reuse_key"
	// DecisionReconcileFirst：先按外部请求 ID 查询对账，确认未执行后才能重试。
	DecisionReconcileFirst RetryDecision = "reconcile_first"
	// DecisionBlocked：结果不明且不可幂等或查询，进入 needs_reconciliation，
	// 由人或专门对账处理；禁止换键、换版本或换实例盲目重试。
	DecisionBlocked RetryDecision = "blocked"
)

// Recover 返回恢复时对工具动作的处置。checkpoint 回滚不等于撤销已发出的
// 外部动作；fence 只保护兰台写入。
func Recover(class EffectClass, state EffectState) RetryDecision {
	switch state {
	case EffectCompleted:
		return DecisionDone
	case EffectIntended, EffectNotExecuted, EffectFailed:
		// 确认没有产生副作用：可以（首次或再次）发送，可幂等的沿用原键。
		if class == EffectLantaiCommand || class == EffectExternalIdempotent {
			return DecisionReuseKey
		}
		return DecisionRerun
	}
	// dispatched 或 effect_unknown：结果不明。
	switch class {
	case EffectPureRead, EffectLocalReplaceable:
		return DecisionRerun
	case EffectLantaiCommand, EffectExternalIdempotent:
		return DecisionReuseKey
	case EffectExternalQueryable:
		return DecisionReconcileFirst
	default:
		return DecisionBlocked
	}
}

// InvocationOutcome 是一次性处理器或作业单次调用的运行结论，与检查的业务
// 结论（CheckVerdict）分开：合法的检查 fail 属于 completed，不是运行故障。
type InvocationOutcome string

const (
	// InvocationCompleted：退出码 0 且结果完整、schema 合法；业务结论可以是 pass、fail 或 unknown。
	InvocationCompleted InvocationOutcome = "completed"
	// InvocationNotDispatched：派发前拒绝（不支持的输入、权限或配置拒绝）或排队失败。
	InvocationNotDispatched InvocationOutcome = "not_dispatched"
	// InvocationCancelled：取消且已确认进程树结束。
	InvocationCancelled InvocationOutcome = "cancelled"
	// InvocationRuntimeFault：崩溃、非零退出、超时、结果缺失或畸形、可归因的启动故障。
	InvocationRuntimeFault InvocationOutcome = "runtime_fault"
	// InvocationUnresolved：已派发但停止状态未知，须先回收或对账。
	InvocationUnresolved InvocationOutcome = "unresolved"
)

// InvocationOutcomes 是全部运行结论。
var InvocationOutcomes = []InvocationOutcome{InvocationCompleted, InvocationNotDispatched, InvocationCancelled,
	InvocationRuntimeFault, InvocationUnresolved}

// Invocation 是宿主对一次调用的观察。
type Invocation struct {
	Dispatched    bool // 是否已启动进程
	StopConfirmed bool // 进程树已确认结束
	Cancelled     bool // 因取消而结束
	TimedOut      bool
	Exited        bool // 进程自行退出并给出退出码
	ExitCode      int
	ResultValid   bool // out/result.json 存在且符合结果 schema
}

// ClassifyInvocation 给出运行结论。退出码 0 只表示程序正常结束，还必须有完整
// 且合法的结果；非零退出属于运行故障，不能用来表达检查 fail。
func ClassifyInvocation(in Invocation) InvocationOutcome {
	switch {
	case !in.Dispatched:
		return InvocationNotDispatched
	case !in.StopConfirmed:
		return InvocationUnresolved
	case in.Cancelled:
		return InvocationCancelled
	case in.TimedOut, !in.Exited, in.ExitCode != 0, !in.ResultValid:
		return InvocationRuntimeFault
	default:
		return InvocationCompleted
	}
}

// CountsAsFault 报告该结论是否计入熔断窗口：只有运行故障计入；合法 fail、
// 派发前拒绝、取消与未决调用都不计。
func (o InvocationOutcome) CountsAsFault() bool { return o == InvocationRuntimeFault }

// CheckVerdict 是检查的业务结论。
type CheckVerdict string

const (
	VerdictPass    CheckVerdict = "pass"
	VerdictFail    CheckVerdict = "fail"
	VerdictUnknown CheckVerdict = "unknown"
)

// CheckVerdicts 是全部检查结论。
var CheckVerdicts = []CheckVerdict{VerdictPass, VerdictFail, VerdictUnknown}

// Verdict 返回可作为证据的检查结论：只有 completed 的调用能给出 pass 或 fail，
// 其余一律 unknown，永远不能把崩溃、超时或缺失结果当作 pass。
func Verdict(o InvocationOutcome, reported CheckVerdict) CheckVerdict {
	if o == InvocationCompleted && (reported == VerdictPass || reported == VerdictFail) {
		return reported
	}
	return VerdictUnknown
}
