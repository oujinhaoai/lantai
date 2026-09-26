package execution

import (
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Fence 是调用方出示的任务执行权凭据（线上字段 task_fence）。
type Fence struct {
	TaskID        ids.ID
	AttemptID     ids.ID
	LeaseFence    int64
	RecoveryEpoch int64
}

// LeaseState 是 tasks 模块在最终接受边界读到的当前权威租约。
type LeaseState struct {
	AttemptID     ids.ID
	LeaseFence    int64
	RecoveryEpoch int64
	ExpiresAt     time.Time
	// Terminated 表示该 Attempt 已被取消、替代或超时回收。
	Terminated bool
}

// Fence 失效原因，写入错误 details.reason。
const (
	ReasonRecoveryEpoch   = "recovery_epoch_mismatch"
	ReasonAttemptReplaced = "attempt_superseded"
	ReasonFenceMismatch   = "fence_mismatch"
	ReasonAttemptEnded    = "attempt_terminated"
	ReasonLeaseExpired    = "lease_expired"
)

// errNoTime 表示调用方没有给出接受时间；按失败关闭处理，而不是放行。
func errNoTime() error {
	return errcode.New(errcode.Internal, "acceptance time is not set")
}

// CheckFence 在最终接受边界核对任务 fence；必须在与提交相同的协调锁内调用，
// now 取条件更新时的服务端时间（零值直接拒绝）。recovery_epoch 先于 fence
// 数值比较，恢复后重复出现的 fence 数值不能冒充当前执行权。
func CheckFence(presented Fence, current LeaseState, now time.Time) error {
	if now.IsZero() {
		return errNoTime()
	}
	reason := ""
	switch {
	case presented.RecoveryEpoch != current.RecoveryEpoch:
		reason = ReasonRecoveryEpoch
	case presented.AttemptID != current.AttemptID:
		reason = ReasonAttemptReplaced
	case presented.LeaseFence != current.LeaseFence:
		reason = ReasonFenceMismatch
	case current.Terminated:
		reason = ReasonAttemptEnded
	case !now.Before(current.ExpiresAt):
		reason = ReasonLeaseExpired
	default:
		return nil
	}
	return errcode.New(errcode.LeaseStale, "the presented attempt no longer holds the task lease").
		WithDetails(errcode.Detail{Reason: reason, Data: map[string]any{
			"presented_fence": presented.LeaseFence, "current_fence": current.LeaseFence,
		}})
}

// Activation 是调用绑定的扩展激活身份（线上字段 activation_ref）。
type Activation struct {
	ExtensionID      string
	ExtensionVersion string
	PackageDigest    digest.Digest
	Generation       int64
}

// Draining 是正常升级或停用后仍允许在期限内收尾的激活；停用时当前代次
// 也会被放进排空名单。
type Draining struct {
	Activation Activation
	Deadline   time.Time
}

// ActivationState 是 extensions 模块读到的当前权威激活状态。
type ActivationState struct {
	Current Activation
	// Enabled 为 false 表示停止接单；在途调用只能通过 Draining 收尾。
	Enabled bool
	// Revoked 表示安全撤权：立即失效，排空也不再接受结果。
	Revoked  bool
	Draining []Draining
}

// 激活失效原因。
const (
	ReasonRevoked         = "revoked"
	ReasonDisabled        = "disabled"
	ReasonPackageMismatch = "package_mismatch"
	ReasonGenerationStale = "generation_stale"
	ReasonDrainExpired    = "drain_expired"
)

// CheckActivation 核对扩展激活代次。撤权立即拒绝，排空中的调用也不例外；
// 当前代次在启用时接受；停用的当前代次与旧代次只有在排空名单内且未过期
// 才接受。包身份（ID、版本、摘要）必须与对应代次完全一致，同版本换摘要
// 一律拒绝。now 为零值时直接拒绝。
func CheckActivation(presented Activation, state ActivationState, now time.Time) error {
	if now.IsZero() {
		return errNoTime()
	}
	reason := ""
	switch {
	case state.Revoked:
		reason = ReasonRevoked
	case presented.Generation == state.Current.Generation && presented != state.Current:
		reason = ReasonPackageMismatch
	case presented.Generation == state.Current.Generation && state.Enabled:
		reason = ""
	default:
		reason = ReasonGenerationStale
		if presented.Generation == state.Current.Generation {
			reason = ReasonDisabled
		}
		for _, d := range state.Draining {
			if d.Activation.Generation != presented.Generation {
				continue
			}
			switch {
			case d.Activation != presented:
				reason = ReasonPackageMismatch
			case !now.Before(d.Deadline):
				reason = ReasonDrainExpired
			default:
				reason = ""
			}
		}
	}
	if reason == "" {
		return nil
	}
	return errcode.New(errcode.ExtensionActivationStale, "the extension activation is no longer current").
		WithDetails(errcode.Detail{Reason: reason, Data: map[string]any{
			"presented_generation": presented.Generation, "current_generation": state.Current.Generation,
		}})
}

// Acceptance 汇总接受一次执行或作业结果所需核对的事实。
type Acceptance struct {
	Now time.Time
	// Fence 与 Lease 必填：执行结果总是绑定任务或作业执行权。
	Fence *Fence
	Lease *LeaseState
	// Activation 与 ActivationState 在结果来自扩展时必填（M2 起联合检查）。
	Activation      *Activation
	ActivationState *ActivationState
}

// Accept 按固定顺序核对：任务 fence（含恢复代次）→ 扩展激活代次。两者是
// 不同类型的凭据，必须分别通过；任一失败，结果只能隔离保存为证据，不能
// 推进业务。
func Accept(a Acceptance) error {
	if a.Fence == nil || a.Lease == nil {
		return errcode.New(errcode.LeaseStale, "execution results must present a task fence").
			WithDetails(errcode.Detail{Reason: "fence_missing"})
	}
	if err := CheckFence(*a.Fence, *a.Lease, a.Now); err != nil {
		return err
	}
	if (a.Activation == nil) != (a.ActivationState == nil) {
		return errcode.New(errcode.ExtensionActivationStale, "activation and its current state must be checked together").
			WithDetails(errcode.Detail{Reason: "activation_missing"})
	}
	if a.Activation != nil {
		return CheckActivation(*a.Activation, *a.ActivationState, a.Now)
	}
	return nil
}

// Subject 是携带 recovery_epoch 的对象类别。
type Subject string

const (
	SubjectSession    Subject = "session"
	SubjectAttempt    Subject = "attempt"
	SubjectHumanGrant Subject = "human_grant"
	SubjectBlobGrant  Subject = "blob_grant"
	SubjectReadGrant  Subject = "read_grant"
	SubjectActivation Subject = "activation"
	SubjectOperation  Subject = "operation"
)

// epochCodes 是恢复代次不符时各类对象的统一错误映射。
var epochCodes = map[Subject]errcode.Code{
	SubjectSession:    errcode.TokenRevoked,
	SubjectAttempt:    errcode.LeaseStale,
	SubjectHumanGrant: errcode.HumanProofRequired,
	SubjectBlobGrant:  errcode.BlobGrantRequired,
	SubjectReadGrant:  errcode.Forbidden,
	SubjectActivation: errcode.ExtensionActivationStale,
	SubjectOperation:  errcode.OperationNeedsReconciliation,
}

// EpochCode 返回恢复代次不符时该类对象使用的错误码。
func EpochCode(s Subject) (errcode.Code, bool) {
	c, ok := epochCodes[s]
	return c, ok
}

// CheckRecoveryEpoch 核对对象的恢复代次。整馆恢复后旧会话、旧执行轮次、
// 旧授权都不能复活；未提交的旧操作需要对账，而不是直接继续。
func CheckRecoveryEpoch(s Subject, presented, current int64) error {
	if presented == current {
		return nil
	}
	code, ok := epochCodes[s]
	if !ok {
		code = errcode.Forbidden
	}
	return errcode.New(code, "issued before the current recovery epoch").
		WithDetails(errcode.Detail{Reason: ReasonRecoveryEpoch, Data: map[string]any{
			"subject": string(s), "presented_epoch": presented, "current_epoch": current,
		}})
}

// CheckOperation 核对重复到达的 operation：同一 operation_id 只对应一个请求摘要。
func CheckOperation(recorded, presented digest.Digest) error {
	if recorded == presented {
		return nil
	}
	return errcode.New(errcode.IdempotencyConflict, "operation already exists with a different request hash")
}

// CheckStartKey 核对 adapter 启动键：同一 execution_key 只对应一个启动请求摘要；
// 不同 Attempt 必须使用不同启动键。
func CheckStartKey(recorded, presented digest.Digest) error {
	if recorded == presented {
		return nil
	}
	return errcode.New(errcode.StartKeyConflict, "execution key already started a different request")
}
