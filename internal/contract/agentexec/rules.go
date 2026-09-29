package agentexec

import (
	"encoding/json"
	"slices"
	"sort"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/tasks"
)

func invalid(reason string) error {
	return errcode.New(errcode.SchemaInvalid, "").WithDetails(errcode.Detail{Reason: reason})
}
func (s ActivationSnapshot) Validate() error {
	if err := tasks.ValidateShape("lantai.activation-snapshot/v1", s); err != nil {
		return err
	}
	if s.Activation.ExtensionID != s.Entry.ExtensionID || s.Activation.ExtensionVersion != s.Entry.ExtensionVersion || s.Activation.PackageDigest != s.Entry.PackageDigest {
		return invalid("activation_package_mismatch")
	}
	return nil
}
func (p ExecutionProfile) Validate() error {
	if err := tasks.ValidateShape("lantai.execution-profile/v1", p); err != nil {
		return err
	}
	if p.AdapterKind == "manual_cli" && p.ActivationSnapshot == (ActivationSnapshot{}) {
		return nil
	}
	return p.ActivationSnapshot.Validate()
}
func (p ExecutionProfile) Digest() (digest.Digest, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	b, err := canonjson.CanonicalizeValue(p)
	if err != nil {
		return "", err
	}
	return digest.Of(b), nil
}

// ExecutionKey 是稳定的 (TaskRun, tasks.Attempt) 启动键；同轮响应丢失先 lookup，
// 不能生成新键。新 Attempt 必须换键，但只有旧执行安全回收后才可启动。
func ExecutionKey(run, attempt ids.ID) (string, error) {
	if !run.Valid() || !attempt.Valid() {
		return "", invalid("execution_key_ref_invalid")
	}
	return "run:" + string(run) + ":attempt:" + string(attempt), nil
}
func hashWithout(v any, exclude ...string) (digest.Digest, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	decoded, err := canonjson.Decode(b)
	if err != nil {
		return "", err
	}
	m, ok := decoded.(map[string]any)
	if !ok {
		return "", invalid("object_required")
	}
	for _, k := range exclude {
		delete(m, k)
	}
	b, err = canonjson.CanonicalizeValue(m)
	if err != nil {
		return "", err
	}
	return digest.Of(b), nil
}

// RequestHash 与 ResultHash 都用 JCS。前者排除传输操作 ID 和摘要自身，后者
// 只排除 result_digest；封存时间、指定 execution 和全部产物仍受摘要保护。
func RequestHash(v any) (digest.Digest, error)   { return hashWithout(v, "operation_id", "request_hash") }
func ResultHash(v Result) (digest.Digest, error) { return hashWithout(v, "result_digest") }
func (r StartRequest) Validate(c Capabilities) error {
	if err := tasks.ValidateShape("lantai.agent-request/v1", r); err != nil {
		return err
	}
	if r.Action != execution.ActStart && r.Action != execution.ActResume {
		return invalid("start_action_required")
	}
	if err := tasks.ValidateShape("lantai.execution-capabilities/v1", c); err != nil {
		return err
	}
	if err := r.Input.Validate(); err != nil {
		return err
	}
	if err := r.Profile.Validate(); err != nil {
		return err
	}
	h, err := RequestHash(r)
	if err != nil {
		return err
	}
	if h != r.RequestHash {
		return invalid("request_hash_mismatch")
	}
	key, err := ExecutionKey(r.TaskRunID, r.Fence.AttemptID)
	if err != nil {
		return err
	}
	if key != r.ExecutionKey {
		return invalid("execution_key_mismatch")
	}
	if r.Profile.ActivationSnapshot == (ActivationSnapshot{}) && r.Profile.AdapterKind == "manual_cli" {
		if r.Activation != nil {
			return errcode.New(errcode.ExtensionActivationStale, "core manual adapter has no extension activation")
		}
	} else if r.Activation == nil || *r.Activation != r.Profile.ActivationSnapshot.Activation {
		return errcode.New(errcode.ExtensionActivationStale, "").WithDetails(errcode.Detail{Reason: "profile_activation_mismatch"})
	}
	if r.Profile.AdapterKind != c.AdapterKind {
		return errcode.New(errcode.UnsupportedCapability, "")
	}
	for _, need := range r.Profile.RequiredCapabilities {
		if !slices.Contains(c.Capabilities, need) {
			return errcode.New(errcode.UnsupportedCapability, "").WithDetails(errcode.Detail{Reason: "capability_missing"})
		}
	}
	if r.Profile.BudgetLimits.Tokens != nil && !slices.Contains(c.BudgetMeters, "tokens") {
		return errcode.New(errcode.UnsupportedCapability, "").WithDetails(errcode.Detail{Reason: "token_meter_unavailable"})
	}
	return nil
}

// CheckStartForRun 校验授权后的固定运行事实；actual lease/activation 状态随后交
// execution.Accept 检查，执行模块不能自己延长或创建 tasks 租约。
func CheckStartForRun(req StartRequest, run TaskRun, cap Capabilities, toolLog []ToolOperation) error {
	if err := req.Validate(cap); err != nil {
		return err
	}
	if err := tasks.ValidateShape("lantai.task-run/v1", run); err != nil {
		return err
	}
	if err := run.Input.Validate(); err != nil {
		return err
	}
	p, err := req.Profile.Digest()
	if err != nil {
		return err
	}
	if req.TaskRunID != run.ID || req.Fence.TaskID != run.TaskID || req.Fence.AttemptID != run.CurrentAttemptID || req.BudgetID != run.BudgetID || req.Input.Digest != run.Input.Digest || req.Profile.ID != run.Profile.ProfileID || req.Profile.Revision != run.Profile.Revision || p != run.Profile.Digest {
		return invalid("run_snapshot_mismatch")
	}
	switch req.Action {
	case execution.ActStart:
		if run.State != execution.RunPending && run.State != execution.RunStarting {
			return errcode.New(errcode.InvalidStateTransition, "")
		}
	case execution.ActResume:
		if run.State != execution.RunPaused {
			return errcode.New(errcode.InvalidStateTransition, "")
		}
		return CheckResume(req, cap, toolLog)
	default:
		return errcode.New(errcode.InvalidStateTransition, "")
	}
	return nil
}

// ReplayStart 只返回原稳定 execution ID，不启动进程。存储映射的持久化属于 M2。
func ReplayStart(old Admission, req StartRequest) (Admission, error) {
	if err := tasks.ValidateShape("lantai.agent-request/v1", req); err != nil {
		return Admission{}, err
	}
	h, err := RequestHash(req)
	if err != nil {
		return Admission{}, err
	}
	if h != req.RequestHash {
		return Admission{}, invalid("request_hash_mismatch")
	}
	if err := tasks.ValidateShape("lantai.agent-response/v1", old); err != nil {
		return Admission{}, err
	}
	if old.ExecutionKey != req.ExecutionKey {
		return Admission{}, errcode.New(errcode.NotFound, "")
	}
	if err := execution.CheckStartKey(old.AcceptedRequestHash, req.RequestHash); err != nil {
		return Admission{}, err
	}
	if old.TaskRunID != req.TaskRunID || old.AttemptID != req.Fence.AttemptID {
		return Admission{}, errcode.New(errcode.StartKeyConflict, "")
	}
	return old, nil
}
func (r Result) Validate() error {
	if err := tasks.ValidateShape("lantai.agent-result/v1", r); err != nil {
		return err
	}
	h, err := ResultHash(r)
	if err != nil {
		return err
	}
	if h != r.ResultDigest {
		return invalid("result_digest_mismatch")
	}
	return nil
}

// AcceptResult 先核对指定 execution 和封存摘要，再复用公共最终接受守卫。
// 失败结果可作隔离证据保存，但返回错误的结果不能影响 Task、审定或发布状态。
func AcceptResult(r Result, request StartRequest, admission Admission, accept execution.Acceptance) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := tasks.ValidateShape("lantai.agent-response/v1", admission); err != nil {
		return err
	}
	if err := tasks.ValidateShape("lantai.agent-request/v1", request); err != nil {
		return err
	}
	if err := request.Profile.Validate(); err != nil {
		return err
	}
	h, err := RequestHash(request)
	if err != nil {
		return err
	}
	if h != request.RequestHash || h != admission.AcceptedRequestHash || request.TaskRunID != admission.TaskRunID || request.Fence.AttemptID != admission.AttemptID {
		return invalid("admission_request_mismatch")
	}
	if r.ExecutionID != admission.ExecutionID || r.TaskRunID != admission.TaskRunID || r.AttemptID != admission.AttemptID || r.AcceptedRequestHash != admission.AcceptedRequestHash {
		return invalid("result_execution_mismatch")
	}
	if accept.Fence == nil || *accept.Fence != request.Fence {
		return errcode.New(errcode.LeaseStale, "")
	}
	if request.Profile.AdapterKind == "manual_cli" && request.Profile.ActivationSnapshot == (ActivationSnapshot{}) {
		if request.Activation != nil || accept.Activation != nil || accept.ActivationState != nil {
			return errcode.New(errcode.ExtensionActivationStale, "unexpected core adapter activation")
		}
		return execution.Accept(accept)
	}
	if accept.Activation == nil || accept.ActivationState == nil {
		return errcode.New(errcode.ExtensionActivationStale, "").WithDetails(errcode.Detail{Reason: "activation_missing"})
	}
	if request.Activation == nil || *accept.Activation != *request.Activation || *request.Activation != request.Profile.ActivationSnapshot.Activation {
		return errcode.New(errcode.ExtensionActivationStale, "").WithDetails(errcode.Detail{Reason: "profile_activation_mismatch"})
	}
	return execution.Accept(accept)
}

// CheckCancel 明确拒绝后端不能兑现的停止方式，不把 revoke_only 报成已杀进程。
func CheckCancel(req CancelRequest, cap Capabilities) error {
	if err := tasks.ValidateShape("lantai.agent-request/v1", req); err != nil {
		return err
	}
	if err := tasks.ValidateShape("lantai.execution-capabilities/v1", cap); err != nil {
		return err
	}
	if req.Action != execution.ActCancel {
		return invalid("cancel_action_required")
	}
	h, err := RequestHash(req)
	if err != nil {
		return err
	}
	if h != req.RequestHash {
		return invalid("request_hash_mismatch")
	}
	if req.RequestedCancelMode != cap.CancelMode {
		return errcode.New(errcode.UnsupportedCapability, "").WithDetails(errcode.Detail{Reason: "cancel_mode_unsupported"})
	}
	return nil
}

// CheckResume 只验证固定输入、profile、后端能力与副作用对账前提，不恢复授权。
// ops 必须是 owner 提供的完整工具日志（不能只提供 checkpoint 之后的片段）。
func CheckResume(req StartRequest, c Capabilities, ops []ToolOperation) error {
	if err := req.Validate(c); err != nil {
		return err
	}
	cp := req.ResumeFrom
	if req.Action != execution.ActResume || cp == nil {
		return invalid("checkpoint_required")
	}
	if err := tasks.ValidateShape("lantai.checkpoint/v1", cp); err != nil {
		return err
	}
	pd, err := req.Profile.Digest()
	if err != nil {
		return err
	}
	if cp.TaskRunID != req.TaskRunID || cp.AttemptID == req.Fence.AttemptID || cp.InputSnapshotDigest != req.Input.Digest || cp.ProfileDigest != pd {
		return invalid("checkpoint_snapshot_mismatch")
	}
	if !slices.Contains(c.ResumeClasses, cp.ResumeClass) || cp.ResumeClass == execution.ResumeManualOnly {
		return errcode.New(errcode.ResumeUnsupported, "")
	}
	if cp.ResumeClass == execution.ResumeBackendCheckpoint && (cp.BackendVersion != c.BackendVersion || !slices.Contains(c.CheckpointFormatVersions, cp.FormatVersion)) {
		return errcode.New(errcode.ResumeUnsupported, "").WithDetails(errcode.Detail{Reason: "checkpoint_format_or_backend_unsupported"})
	}
	seen := map[int64]bool{}
	maxSequence := int64(0)
	for _, op := range ops {
		if err := tasks.ValidateShape("lantai.tool-operation/v1", op); err != nil {
			return err
		}
		if op.TaskRunID != req.TaskRunID || seen[op.Sequence] {
			return invalid("tool_log_mismatch")
		}
		seen[op.Sequence] = true
		if op.Sequence > maxSequence {
			maxSequence = op.Sequence
		}
		decision := execution.Recover(op.EffectClass, op.State)
		if decision == execution.DecisionBlocked || decision == execution.DecisionReconcileFirst || (decision == execution.DecisionReuseKey && (op.State == execution.EffectDispatched || op.State == execution.EffectUnknown)) {
			return errcode.New(errcode.OperationNeedsReconciliation, "").WithDetails(errcode.Detail{Reason: "tool_effect_unresolved"})
		}
	}
	if int64(len(seen)) != maxSequence || cp.SideEffectWatermark > maxSequence {
		return invalid("tool_log_incomplete")
	}
	return nil
}

// EffectiveTools 是四个已授权集合的交集；humanOnly 来自 identity 的权威动作
// 注册表，必须一并排除。它只收窄集合，不创建权限，子 Agent 可再用此函数收窄。
func EffectiveTools(project, principal, profile, attempt, humanOnly []string) []string {
	out := []string{}
	for _, tool := range profile {
		if !slices.Contains(out, tool) && slices.Contains(project, tool) && slices.Contains(principal, tool) && slices.Contains(attempt, tool) && !slices.Contains(humanOnly, tool) {
			out = append(out, tool)
		}
	}
	sort.Strings(out)
	return out
}
func (a ArtifactCandidate) Validate() error {
	if err := tasks.ValidateShape("lantai.artifact-candidate/v1", a); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, f := range a.Files {
		if seen[f.Path] {
			return invalid("duplicate_artifact_path")
		}
		seen[f.Path] = true
	}
	return nil
}
func (a JobAttempt) Validate() error {
	if err := tasks.ValidateShape("lantai.job-attempt/v1", a); err != nil {
		return err
	}
	if a.Fence.JobAttemptID != a.ID || a.Fence.JobID != a.JobID {
		return invalid("job_fence_ref_mismatch")
	}
	if err := a.ActivationSnapshot.Validate(); err != nil {
		return err
	}
	return a.Input.Validate()
}
