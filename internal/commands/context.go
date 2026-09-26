// Package commands 提供所有领域命令共用的上下文、请求摘要、幂等判定、
// 操作阶段、锁顺序，以及写入业务所属库的回执/operations/outbox 组件。
//
// 本包不拥有任何业务表，也没有全局回执库：每个模块在自己的数据库事务里
// 通过 Store 写基础设施表，业务结果、回执与 outbox 同事务提交。跨库后续
// 动作经 outbox 与幂等消费推进。
package commands

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Context 是一次领域命令的共同上下文。
//
// ActorID、SessionID、PolicyRevision、RecoveryEpoch 必须来自服务端认证结果，
// 不能取客户端自报值。任务、租约与人类授权字段按命令类型适用。
type Context struct {
	OperationID    ids.ID
	IdempotencyKey string
	RequestHash    digest.Digest
	CommandType    string
	ActorID        ids.ID
	SessionID      ids.ID
	// ProjectID 为空表示实例级命令（身份、实例运维）。
	ProjectID ids.ID
	// ExpectedRevisions 以目标 ID 为键；0 表示目标必须尚不存在。
	ExpectedRevisions map[string]int64
	PolicyRevision    int64
	RecoveryEpoch     int64
	TaskID            ids.ID
	AttemptID         ids.ID
	LeaseFence        int64
	HumanGrantID      ids.ID
	CorrelationID     ids.ID
}

var (
	idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	commandTypeRE    = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	moduleRE         = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
)

// ErrInvalidContext 表示命令上下文缺字段或格式错误；属于服务端编程错误。
var ErrInvalidContext = errors.New("commands: invalid command context")

// Validate 检查上下文完整性。
func (c Context) Validate() error {
	bad := func(field string) error { return fmt.Errorf("%w: %s", ErrInvalidContext, field) }
	switch {
	case !c.OperationID.Valid():
		return bad("operation_id")
	case !idempotencyKeyRE.MatchString(c.IdempotencyKey):
		return bad("idempotency_key")
	case !c.RequestHash.Valid():
		return bad("request_hash")
	case !commandTypeRE.MatchString(c.CommandType):
		return bad("command_type")
	case !c.ActorID.Valid():
		return bad("actor_id")
	case c.SessionID != "" && !c.SessionID.Valid():
		return bad("session_id")
	case c.ProjectID != "" && !c.ProjectID.Valid():
		return bad("project_id")
	case c.RecoveryEpoch < 1:
		return bad("recovery_epoch")
	case c.CorrelationID != "" && !c.CorrelationID.Valid():
		return bad("correlation_id")
	}
	if (c.AttemptID != "") != (c.LeaseFence > 0) {
		return bad("attempt_id and lease_fence must be given together")
	}
	if c.AttemptID != "" && !c.AttemptID.Valid() {
		return bad("attempt_id")
	}
	for k, v := range c.ExpectedRevisions {
		if v < 0 {
			return bad("expected_revisions[" + k + "]")
		}
	}
	return nil
}

// Key 返回幂等键的作用域：(actor, project, command_type, idempotency_key)。
func (c Context) Key() ReceiptKey {
	return ReceiptKey{ActorID: c.ActorID, ProjectID: c.ProjectID, CommandType: c.CommandType, IdempotencyKey: c.IdempotencyKey}
}

// Correlation 返回关联 ID；未指定时沿用 operation_id。
func (c Context) Correlation() ids.ID {
	if c.CorrelationID != "" {
		return c.CorrelationID
	}
	return c.OperationID
}
