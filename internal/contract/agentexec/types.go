// Package agentexec 定义 T06 的执行对象与 adapter 端口。M1 不实现 adapter、
// runner、进程宿主或 Job 队列；共同的 fence、激活、取消和副作用规则复用 execution。
package agentexec

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/tasks"
)

type PackageEntry struct {
	ExtensionID      string        `json:"extension_id"`
	ExtensionVersion string        `json:"extension_version"`
	PackageDigest    digest.Digest `json:"package_digest"`
	Target           string        `json:"target"`
	Entry            string        `json:"entry"`
	EntryDigest      digest.Digest `json:"entry_digest"`
}

// ActivationSnapshot 由 extensions 提供，只引用、不创建激活代次；不是 tasks 的租约。
type ActivationSnapshot struct {
	Contract                 string               `json:"contract"`
	Activation               execution.Activation `json:"activation"`
	Entry                    PackageEntry         `json:"entry"`
	EffectiveConfigRevision  int64                `json:"effective_config_revision"`
	EnablementPolicyRevision int64                `json:"enablement_policy_revision"`
}
type BudgetLimits struct {
	WallTimeMS    int64  `json:"wall_time_ms"`
	ModelCalls    int64  `json:"model_calls"`
	ToolCalls     int64  `json:"tool_calls"`
	Tokens        *int64 `json:"tokens,omitempty"`
	SubagentDepth int64  `json:"subagent_depth"`
	SubagentCount int64  `json:"subagent_count"`
	Concurrency   int64  `json:"concurrency"`
	ArtifactBytes int64  `json:"artifact_bytes"`
}
type BudgetUsage struct {
	WallTimeMS    int64  `json:"wall_time_ms"`
	ModelCalls    int64  `json:"model_calls"`
	ToolCalls     int64  `json:"tool_calls"`
	Tokens        *int64 `json:"tokens,omitempty"`
	ArtifactBytes int64  `json:"artifact_bytes"`
}
type ProfileRef struct {
	ProfileID ids.ID        `json:"profile_id"`
	Revision  int64         `json:"revision"`
	Digest    digest.Digest `json:"profile_digest"`
}
type ExecutionProfile struct {
	Contract             string             `json:"contract"`
	ID                   ids.ID             `json:"id"`
	Revision             int64              `json:"revision"`
	AdapterKind          string             `json:"adapter_kind"`
	AdapterVersion       string             `json:"adapter_version"`
	ActivationSnapshot   ActivationSnapshot `json:"activation_snapshot,omitzero"`
	PlaybookRefs         []ids.PermanentRef `json:"playbook_refs"`
	SkillDigests         []digest.Digest    `json:"skill_digests"`
	RequiredCapabilities []string           `json:"required_capabilities"`
	AllowedTools         []string           `json:"allowed_tools"`
	NetworkPolicy        string             `json:"network_policy"`
	NetworkAllowlist     []string           `json:"network_allowlist"`
	BudgetLimits         BudgetLimits       `json:"budget_limits"`
	ResultSchema         string             `json:"result_schema"`
}
type Termination struct {
	Confirmed         bool `json:"termination_confirmed"`
	UnresolvedEffects int  `json:"unresolved_effects"`
}
type TaskRun struct {
	Contract         string                 `json:"contract"`
	ID               ids.ID                 `json:"id"`
	TaskID           ids.ID                 `json:"task_id"`
	SeatID           ids.ID                 `json:"seat_id"`
	Revision         int64                  `json:"revision"`
	OperationID      ids.ID                 `json:"operation_id"`
	State            execution.TaskRunState `json:"state"`
	Input            tasks.InputSnapshot    `json:"input_snapshot"`
	Profile          ProfileRef             `json:"profile"`
	BudgetID         ids.ID                 `json:"budget_id"`
	CreatedBy        ids.ID                 `json:"created_by"`
	CurrentAttemptID ids.ID                 `json:"current_attempt_id,omitempty"`
	FlowID           ids.ID                 `json:"flow_id,omitempty"`
	StepRunID        ids.ID                 `json:"step_run_id,omitempty"`
	SupersedesRunID  ids.ID                 `json:"supersedes_run_id,omitempty"`
	Termination      Termination            `json:"termination"`
}
type AgentStepRun struct {
	Contract         string             `json:"contract"`
	ID               ids.ID             `json:"id"`
	TaskRunID        ids.ID             `json:"task_run_id"`
	AttemptID        ids.ID             `json:"attempt_id"`
	Revision         int64              `json:"revision"`
	PlanRevision     int64              `json:"plan_revision"`
	ParentID         ids.ID             `json:"parent_id,omitempty"`
	Kind             string             `json:"kind"`
	State            string             `json:"state"`
	InputRefs        []ids.PermanentRef `json:"input_refs"`
	ToolOperationIDs []ids.ID           `json:"tool_operation_ids"`
	OutputRefs       []ids.PermanentRef `json:"output_refs"`
}
type Checkpoint struct {
	Contract             string                `json:"contract"`
	ID                   ids.ID                `json:"id"`
	TaskRunID            ids.ID                `json:"task_run_id"`
	AttemptID            ids.ID                `json:"attempt_id"`
	Sequence             int64                 `json:"sequence"`
	InputSnapshotDigest  digest.Digest         `json:"input_snapshot_digest"`
	ProfileDigest        digest.Digest         `json:"profile_digest"`
	BackendVersion       string                `json:"backend_version"`
	FormatVersion        string                `json:"format_version"`
	ResumeClass          execution.ResumeClass `json:"resume_class"`
	ArtifactRefs         []ids.PermanentRef    `json:"artifact_refs"`
	CompletedStepIDs     []ids.ID              `json:"completed_step_ids"`
	SideEffectWatermark  int64                 `json:"side_effect_watermark"`
	BackendCheckpointRef string                `json:"backend_checkpoint_ref,omitempty"`
	CreatedAt            string                `json:"created_at"`
}
type ArtifactFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Producer struct {
	CoreReleaseDigest digest.Digest `json:"core_release_digest,omitempty"`
	ExtensionID       string        `json:"extension_id"`
	ExtensionVersion  string        `json:"extension_version"`
	PackageDigest     digest.Digest `json:"package_digest"`
	Source            string        `json:"source"`
	ContributionID    string        `json:"contribution_id,omitempty"`
}
type ArtifactCandidate struct {
	Contract            string         `json:"contract"`
	ID                  ids.ID         `json:"id"`
	TaskRunID           ids.ID         `json:"task_run_id"`
	AttemptID           ids.ID         `json:"attempt_id"`
	ManifestDigest      digest.Digest  `json:"manifest_digest"`
	Files               []ArtifactFile `json:"files"`
	Purpose             string         `json:"purpose"`
	ProvenanceRefs      []ids.ID       `json:"provenance_refs"`
	LicenseEvidenceRefs []ids.ID       `json:"license_evidence_refs"`
	ValidationState     string         `json:"validation_state"`
	Producer            Producer       `json:"producer,omitzero"`
}
type ToolOperation struct {
	Contract                   string                `json:"contract"`
	ID                         ids.ID                `json:"id"`
	TaskRunID                  ids.ID                `json:"task_run_id"`
	AgentStepRunID             ids.ID                `json:"agent_step_run_id"`
	AttemptID                  ids.ID                `json:"attempt_id"`
	Sequence                   int64                 `json:"sequence"`
	Tool                       string                `json:"tool"`
	ToolVersion                string                `json:"tool_version"`
	RequestHash                digest.Digest         `json:"request_hash"`
	EffectClass                execution.EffectClass `json:"effect_class"`
	State                      execution.EffectState `json:"state"`
	IdempotencyKey             string                `json:"idempotency_key,omitempty"`
	ExternalRequestRef         string                `json:"external_request_ref,omitempty"`
	ReceiptDigest              digest.Digest         `json:"receipt_digest,omitempty"`
	ReconciliationEvidenceRefs []ids.ID              `json:"reconciliation_evidence_refs"`
}

// JobFence 与 execution.Fence 在类型和字段上均不同；只有 jobs 签发它。
type JobFence struct {
	JobID         ids.ID `json:"job_id"`
	JobAttemptID  ids.ID `json:"job_attempt_id"`
	LeaseFence    int64  `json:"lease_fence"`
	RecoveryEpoch int64  `json:"recovery_epoch"`
}
type JobAttempt struct {
	Contract           string                      `json:"contract"`
	ID                 ids.ID                      `json:"id"`
	JobID              ids.ID                      `json:"job_id"`
	OperationID        ids.ID                      `json:"operation_id"`
	Revision           int64                       `json:"revision"`
	Protocol           execution.Protocol          `json:"protocol"`
	Fence              JobFence                    `json:"job_fence"`
	ActivationSnapshot ActivationSnapshot          `json:"activation_snapshot"`
	Input              tasks.InputSnapshot         `json:"input_snapshot"`
	State              string                      `json:"state"`
	Termination        Termination                 `json:"termination"`
	Outcome            execution.InvocationOutcome `json:"outcome,omitempty"`
	Verdict            execution.CheckVerdict      `json:"verdict,omitempty"`
	ResultDigest       digest.Digest               `json:"result_digest,omitempty"`
}
type Capabilities struct {
	CheckpointFormatVersions []string                `json:"checkpoint_format_versions"`
	Contract                 string                  `json:"contract"`
	ProtocolVersions         []execution.Protocol    `json:"protocol_versions"`
	AdapterKind              string                  `json:"adapter_kind"`
	BackendVersion           string                  `json:"backend_version"`
	SupportsIdempotentStart  bool                    `json:"supports_idempotent_start"`
	SupportsLookupByKey      bool                    `json:"supports_lookup_by_key"`
	ResumeClasses            []execution.ResumeClass `json:"resume_classes"`
	CancelMode               execution.CancelMode    `json:"cancel_mode"`
	ArtifactMode             string                  `json:"artifact_mode"`
	Capabilities             []string                `json:"capabilities"`
	BudgetMeters             []string                `json:"budget_meters"`
}

// Header 复用公共 execution_header。来自 profile 的激活身份和 task fence 分别复验。
type Header struct {
	Protocol    execution.Protocol        `json:"protocol"`
	Action      execution.ExecutionAction `json:"action"`
	OperationID ids.ID                    `json:"operation_id"`
	RequestHash digest.Digest             `json:"request_hash"`
	Fence       execution.Fence           `json:"fence"`
	Activation  *execution.Activation     `json:"activation,omitempty"`
}
type StartRequest struct {
	Header
	ExecutionKey string              `json:"execution_key"`
	TaskRunID    ids.ID              `json:"task_run_id"`
	Input        tasks.InputSnapshot `json:"input_snapshot"`
	Profile      ExecutionProfile    `json:"profile"`
	BudgetID     ids.ID              `json:"budget_id"`
	ResumeFrom   *Checkpoint         `json:"resume_from,omitempty"`
}
type LookupRequest struct {
	Protocol     execution.Protocol        `json:"protocol"`
	Action       execution.ExecutionAction `json:"action"`
	ExecutionKey string                    `json:"execution_key"`
}
type QueryRequest struct {
	Protocol    execution.Protocol        `json:"protocol"`
	Action      execution.ExecutionAction `json:"action"`
	ExecutionID ids.ID                    `json:"execution_id"`
}
type CancelRequest struct {
	Header
	ExecutionID         ids.ID               `json:"execution_id"`
	Reason              string               `json:"reason"`
	RequestedCancelMode execution.CancelMode `json:"requested_cancel_mode"`
}
type Admission struct {
	Protocol            execution.Protocol        `json:"protocol"`
	Action              execution.ExecutionAction `json:"action"`
	ExecutionID         ids.ID                    `json:"execution_id"`
	ExecutionKey        string                    `json:"execution_key"`
	TaskRunID           ids.ID                    `json:"task_run_id"`
	AttemptID           ids.ID                    `json:"attempt_id"`
	AcceptedRequestHash digest.Digest             `json:"accepted_request_hash"`
	Revision            int64                     `json:"revision"`
	State               execution.TaskRunState    `json:"state"`
	BackendRunRef       string                    `json:"backend_run_ref,omitempty"`
}
type Status struct {
	Protocol     execution.Protocol        `json:"protocol"`
	Action       execution.ExecutionAction `json:"action"`
	ExecutionID  ids.ID                    `json:"execution_id"`
	Revision     int64                     `json:"revision"`
	State        execution.TaskRunState    `json:"state"`
	ObservedAt   string                    `json:"observed_at"`
	HeartbeatAt  string                    `json:"heartbeat_at,omitempty"`
	BudgetUsage  BudgetUsage               `json:"budget_usage"`
	Termination  Termination               `json:"termination"`
	CheckpointID ids.ID                    `json:"checkpoint_id,omitempty"`
}
type CancelResponse struct {
	Protocol    execution.Protocol        `json:"protocol"`
	Action      execution.ExecutionAction `json:"action"`
	ExecutionID ids.ID                    `json:"execution_id"`
	State       execution.TaskRunState    `json:"state"`
	CancelMode  execution.CancelMode      `json:"cancel_mode"`
	Termination Termination               `json:"termination"`
}

// Result 只对应指定 execution/Attempt 的封存结果。它从不表示人审、发布或任务 done。
type Result struct {
	Contract            string                 `json:"contract"`
	ExecutionID         ids.ID                 `json:"execution_id"`
	TaskRunID           ids.ID                 `json:"task_run_id"`
	AttemptID           ids.ID                 `json:"attempt_id"`
	AcceptedRequestHash digest.Digest          `json:"accepted_request_hash"`
	ResultDigest        digest.Digest          `json:"result_digest"`
	Outcome             execution.TaskRunState `json:"outcome"`
	CandidateIDs        []ids.ID               `json:"candidate_ids"`
	EvidenceRefs        []ids.ID               `json:"evidence_refs"`
	BudgetUsage         BudgetUsage            `json:"budget_usage"`
	Limitations         []string               `json:"limitations"`
	SealedAt            string                 `json:"sealed_at"`
	Termination         Termination            `json:"termination"`
}

// Adapter 是未来 M2/M3 实现端口；M1 无实现。身份凭据由受信调用上下文提供，
// 绝不写进请求、checkpoint、结果或 CLI 参数。Result 返回结构化 RESULT_NOT_READY。
type Adapter interface {
	Describe(context.Context) (Capabilities, error)
	Start(context.Context, StartRequest) (Admission, error)
	Lookup(context.Context, LookupRequest) (Admission, error)
	Status(context.Context, QueryRequest) (Status, error)
	Resume(context.Context, StartRequest) (Admission, error)
	Cancel(context.Context, CancelRequest) (CancelResponse, error)
	Result(context.Context, QueryRequest) (Result, error)
}
