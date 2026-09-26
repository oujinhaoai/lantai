package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// View 是 lantai.operation/v1 的对外状态视图。返回前调用方必须按当前读取权限
// 过滤 ResultRefs，且不能附带令牌、签名 URL 或内部堆栈。
type View struct {
	OperationID       ids.ID
	ParentOperationID ids.ID
	OwnerModule       string
	CommandType       string
	Stage             Stage
	ProjectionPending *bool
	ResultRefs        []ResultRef
	Retryable         bool
	NextAction        errcode.RecoveryAction
	Reason            *Reason
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Reason 是终态或阻塞时的精简原因。
type Reason struct {
	Code    errcode.Code `json:"code"`
	Message string       `json:"message"`
}

// MarshalJSON 输出契约格式。
func (v View) MarshalJSON() ([]byte, error) {
	type wire struct {
		OperationID       ids.ID                 `json:"operation_id"`
		ParentOperationID ids.ID                 `json:"parent_operation_id,omitempty"`
		OwnerModule       string                 `json:"owner_module"`
		CommandType       string                 `json:"command_type"`
		Stage             Stage                  `json:"stage"`
		ProjectionPending *bool                  `json:"projection_pending,omitempty"`
		ResultRefs        []ResultRef            `json:"result_refs,omitempty"`
		Retryable         bool                   `json:"retryable"`
		NextAction        errcode.RecoveryAction `json:"next_action"`
		Reason            *Reason                `json:"reason,omitempty"`
		CreatedAt         string                 `json:"created_at"`
		UpdatedAt         string                 `json:"updated_at"`
	}
	return json.Marshal(wire{
		OperationID: v.OperationID, ParentOperationID: v.ParentOperationID, OwnerModule: v.OwnerModule,
		CommandType: v.CommandType, Stage: v.Stage, ProjectionPending: v.ProjectionPending,
		ResultRefs: v.ResultRefs, Retryable: v.Retryable, NextAction: v.NextAction, Reason: v.Reason,
		CreatedAt: clock.Format(v.CreatedAt), UpdatedAt: clock.Format(v.UpdatedAt),
	})
}

// View 组装操作视图：有 operations 记录时以其阶段为准，否则由回执推导
// （单库短命令只有回执）。committed 且事件全部收录时显示为 projected。
// 传入 *sql.DB 时在一个只读事务里读取，保证回执、操作与 outbox 来自同一快照。
func (s *Store) View(ctx context.Context, q DBTX, opID ids.ID) (*View, error) {
	if db, ok := q.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		q = tx
	}
	r, err := s.ReceiptByOperation(ctx, q, opID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	op, err := s.GetOperation(ctx, q, opID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if r == nil && op == nil {
		return nil, ErrNotFound
	}
	v := &View{OperationID: opID}
	var failure errcode.Code
	if op != nil {
		v.ParentOperationID, v.OwnerModule, v.CommandType = op.ParentOperationID, op.OwnerModule, op.CommandType
		v.Stage, v.CreatedAt, v.UpdatedAt = op.Stage, op.CreatedAt, op.UpdatedAt
		failure = op.FailureCode
	} else {
		v.OwnerModule, v.CommandType, v.CreatedAt = r.OwnerModule, r.Key.CommandType, r.CreatedAt
		v.UpdatedAt = r.CompletedAt
		switch r.Status {
		case ReceiptSucceeded:
			v.Stage = StageCommitted
		case ReceiptFailed:
			v.Stage = StageFailed
		default:
			v.Stage = StagePrepared
		}
		failure = r.FailureCode
	}
	if v.UpdatedAt.IsZero() {
		v.UpdatedAt = v.CreatedAt
	}
	if r != nil {
		v.ResultRefs = r.ResultRefs
		if failure == "" {
			failure = r.FailureCode
		}
	}
	switch v.Stage {
	case StageReceiving, StagePrepared, StageInstalled:
		v.Retryable, v.NextAction = true, errcode.ActionPollOperation
	case StageBlocked:
		v.NextAction = errcode.ActionHumanAction
	case StageQuarantined:
		v.NextAction = errcode.ActionReconcile
	case StageCommitted:
		v.NextAction = errcode.ActionNone
		n, err := pendingEvents(ctx, q, opID)
		if err != nil {
			return nil, err
		}
		pending := n > 0
		v.ProjectionPending = &pending
		if !pending {
			v.Stage = StageProjected
		}
	case StageFailed, StageCancelled:
		v.NextAction = errcode.ActionNone
		if spec, ok := errcode.Lookup(failure); ok {
			v.NextAction = spec.Recovery
		}
	}
	if spec, ok := errcode.Lookup(failure); ok {
		v.Reason = &Reason{Code: failure, Message: spec.Summary}
	}
	return v, nil
}
