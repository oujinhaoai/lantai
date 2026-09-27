// Package tasks 定义 T05 的领域契约与纯判定。它不签发租约、不运行任务或流程。
package tasks

import (
	"context"
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

type InputSnapshot struct {
	Refs   []ids.PermanentRef `json:"refs"`
	Digest digest.Digest      `json:"digest"`
}
type DefinitionRef struct {
	Ref    ids.PermanentRef `json:"ref"`
	Digest digest.Digest    `json:"definition_digest"`
}
type OutputRequirement struct {
	Slug           string `json:"slug"`
	AssetType      string `json:"asset_type"`
	CandidateCount int    `json:"candidate_count"`
}
type Reconciliation struct {
	TerminationConfirmed bool     `json:"termination_confirmed"`
	UnresolvedEffects    int      `json:"unresolved_effects"`
	EvidenceRefs         []ids.ID `json:"evidence_refs"`
}

type Task struct {
	Contract           string              `json:"contract"`
	ID                 ids.ID              `json:"id"`
	ProjectID          ids.ID              `json:"project_id"`
	Revision           int64               `json:"revision"`
	OperationID        ids.ID              `json:"operation_id"`
	Type               string              `json:"type"`
	Title              string              `json:"title"`
	State              TaskState           `json:"state"`
	Priority           string              `json:"priority"`
	Input              InputSnapshot       `json:"input_snapshot"`
	AcceptanceCriteria []string            `json:"acceptance_criteria"`
	ExpectedOutputs    []OutputRequirement `json:"expected_outputs"`
	SeatIDs            []ids.ID            `json:"seat_ids"`
	DependencyIDs      []ids.ID            `json:"dependency_ids"`
	OutputRefs         []ids.PermanentRef  `json:"output_refs"`
	FlowID             ids.ID              `json:"flow_id,omitempty"`
	StepRunID          ids.ID              `json:"step_run_id,omitempty"`
	DueAt              string              `json:"due_at,omitempty"`
}
type Seat struct {
	Contract         string    `json:"contract"`
	ID               ids.ID    `json:"id"`
	TaskID           ids.ID    `json:"task_id"`
	Revision         int64     `json:"revision"`
	Ordinal          int       `json:"ordinal"`
	State            SeatState `json:"state"`
	LastLeaseFence   int64     `json:"last_lease_fence"`
	RecoveryEpoch    int64     `json:"recovery_epoch"`
	CurrentAttemptID ids.ID    `json:"current_attempt_id,omitempty"`
}
type Attempt struct {
	Contract       string          `json:"contract"`
	ID             ids.ID          `json:"id"`
	TaskID         ids.ID          `json:"task_id"`
	SeatID         ids.ID          `json:"seat_id"`
	Revision       int64           `json:"revision"`
	OperationID    ids.ID          `json:"operation_id"`
	PrincipalID    ids.ID          `json:"principal_id"`
	SessionID      ids.ID          `json:"session_id"`
	State          AttemptState    `json:"state"`
	Fence          execution.Fence `json:"fence"`
	IssuedAt       string          `json:"issued_at"`
	ExpiresAt      string          `json:"expires_at"`
	Reconciliation Reconciliation  `json:"reconciliation"`
}
type Flow struct {
	Contract            string        `json:"contract"`
	ID                  ids.ID        `json:"id"`
	ProjectID           ids.ID        `json:"project_id"`
	Revision            int64         `json:"revision"`
	OperationID         ids.ID        `json:"operation_id"`
	State               FlowState     `json:"state"`
	Definition          DefinitionRef `json:"definition"`
	Input               InputSnapshot `json:"input_snapshot"`
	StepRunIDs          []ids.ID      `json:"step_run_ids"`
	PendingOperationIDs []ids.ID      `json:"pending_operation_ids"`
	LastEventID         ids.ID        `json:"last_event_id,omitempty"`
}

// StepRun 是业务流程的一轮步骤，不能用于表示 Agent 内部推理或工具步骤。
type StepRun struct {
	Contract             string             `json:"contract"`
	ID                   ids.ID             `json:"id"`
	FlowID               ids.ID             `json:"flow_id"`
	Revision             int64              `json:"revision"`
	OperationID          ids.ID             `json:"operation_id"`
	StepKey              string             `json:"step_key"`
	Round                int                `json:"round"`
	Kind                 string             `json:"kind"`
	State                StepState          `json:"state"`
	Definition           DefinitionRef      `json:"definition"`
	Input                InputSnapshot      `json:"input_snapshot"`
	TaskIDs              []ids.ID           `json:"task_ids"`
	JobIDs               []ids.ID           `json:"job_ids"`
	OutputRefs           []ids.PermanentRef `json:"output_refs"`
	AuthorityOperationID ids.ID             `json:"authority_operation_id,omitempty"`
}

// Command 的 target 是 Seat（claim/requeue）、Attempt（renew/release/submit/cancel）
// 或业务 StepRun（advance）。owner 在同一条件事务中更新对象、回执和 outbox。
// session_id 是已验证会话的绑定，不是授权材料；生产实现还须重新检查身份权限。
type Command struct {
	Contract             string             `json:"contract"`
	OperationID          ids.ID             `json:"operation_id"`
	RequestHash          digest.Digest      `json:"request_hash"`
	Type                 string             `json:"command_type"`
	ProjectID            ids.ID             `json:"project_id"`
	TargetID             ids.ID             `json:"target_id"`
	ExpectedRevision     int64              `json:"expected_revision"`
	RecoveryEpoch        int64              `json:"recovery_epoch"`
	SessionID            ids.ID             `json:"session_id"`
	AuthorityOperationID ids.ID             `json:"authority_operation_id,omitempty"`
	Fence                *execution.Fence   `json:"fence,omitempty"`
	OutputRefs           []ids.PermanentRef `json:"output_refs,omitempty"`
}

// MarshalJSON 保留 submit 的空输出数组（无文件的 question/qa 同样可以提交）。
func (c Command) MarshalJSON() ([]byte, error) {
	type plain Command
	b, err := json.Marshal(plain(c))
	if err != nil {
		return nil, err
	}
	if c.Type != "tasks.submit" {
		return b, nil
	}
	var v map[string]any
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if c.OutputRefs == nil {
		v["output_refs"] = []ids.PermanentRef{}
	} else {
		v["output_refs"] = c.OutputRefs
	}
	return json.Marshal(v)
}

type Receipt struct {
	Contract    string             `json:"contract"`
	OperationID ids.ID             `json:"operation_id"`
	RequestHash digest.Digest      `json:"request_hash"`
	TargetID    ids.ID             `json:"target_id"`
	Revision    int64              `json:"revision"`
	AttemptID   ids.ID             `json:"attempt_id,omitempty"`
	Fence       *execution.Fence   `json:"fence,omitempty"`
	OutputRefs  []ids.PermanentRef `json:"output_refs"`
}

// Reader 和 Commands 是领域端口。M1 只有契约；tasks/taskstest 提供预设事实桩。
// 生产实现必须在当前授权后返回数据；不能把接口桩接入服务入口。
type Reader interface {
	ReadTask(context.Context, ids.ID) (Task, error)
	ReadSeat(context.Context, ids.ID) (Seat, error)
	ReadAttempt(context.Context, ids.ID) (Attempt, error)
	ReadFlow(context.Context, ids.ID) (Flow, error)
	ReadStepRun(context.Context, ids.ID) (StepRun, error)
}
type Commands interface {
	Execute(context.Context, Command) (Receipt, error)
}

// ValidateShape 使用唯一的嵌入 schema，拒绝未知字段和非规范标识。
func ValidateShape(contract string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "invalid contract value", err)
	}
	r, err := schema.Default()
	if err != nil {
		return err
	}
	if err = r.ValidateJSON(contract, b); err != nil {
		if ve, ok := err.(*schema.ValidationError); ok {
			return ve.Err()
		}
		return err
	}
	return nil
}

// SnapshotDigest 固定输入数组的 JCS SHA-256；顺序是输入绑定的一部分。
func SnapshotDigest(refs []ids.PermanentRef) (digest.Digest, error) {
	if refs == nil {
		refs = []ids.PermanentRef{}
	}
	for _, r := range refs {
		if err := r.Validate(true); err != nil {
			return "", errcode.Wrap(errcode.SchemaInvalid, "input must use a permanent version reference", err)
		}
	}
	b, err := canonjson.CanonicalizeValue(refs)
	if err != nil {
		return "", err
	}
	return digest.Of(b), nil
}
func (s InputSnapshot) Validate() error {
	if err := ValidateShape("lantai.tasks-defs/v1#/$defs/input_snapshot", s); err != nil {
		return err
	}
	d, err := SnapshotDigest(s.Refs)
	if err != nil {
		return err
	}
	if d != s.Digest {
		return invalid("input_snapshot_digest_mismatch")
	}
	return nil
}

// HashCommand 不含 operation_id/request_hash；重试保持同一操作，不能换会话偷取租约。
func HashCommand(c Command) (digest.Digest, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	var v map[string]any
	if err = json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	delete(v, "operation_id")
	delete(v, "request_hash")
	b, err = canonjson.CanonicalizeValue(v)
	if err != nil {
		return "", err
	}
	return digest.Of(b), nil
}
func (c Command) Validate() error {
	if err := ValidateShape("lantai.task-command/v1", c); err != nil {
		return err
	}
	h, err := HashCommand(c)
	if err != nil {
		return err
	}
	if h != c.RequestHash {
		return invalid("request_hash_mismatch")
	}
	return nil
}
func invalid(reason string) error {
	return errcode.New(errcode.SchemaInvalid, "").WithDetails(errcode.Detail{Reason: reason})
}
