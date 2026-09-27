package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Pending 是已在消费事务接受的跨模块命令。OperationID 在首次执行前已持久化，重试保持不变。
type Pending struct {
	OperationID ids.ID
	CommandType string
	Payload     json.RawMessage
	Attempts    int
}

// Executor 调用目标领域接口。目标必须按 OperationID 幂等，并在结果不明时先查回执对账。
// 它在消费者事务/锁外运行，权限、revision 等由目标在最终接受时复验。
type Executor func(context.Context, Pending) (json.RawMessage, error)

// Dispatch 仅执行既有事件引出的有限待执行命令，不提供任务状态机、定时触发或插件执行。
// 每次目标调用前先持久化重试延迟；进程退出后不会立即无限热重试。
func (c *Consumer) Dispatch(ctx context.Context, limit int, execute Executor) (int, error) {
	if limit < 1 || limit > 100 || execute == nil {
		return 0, errors.New("events: invalid pending dispatch arguments")
	}
	n := 0
	for range limit {
		p, err := c.claim(ctx)
		if err != nil {
			return n, err
		}
		if p == nil {
			return n, nil
		}
		receipt, callErr := execute(ctx, *p)
		if callErr == nil {
			var err error
			receipt, err = canonjson.Canonicalize(receipt)
			if err != nil || len(receipt) > 64<<10 {
				callErr = &Failure{SchemaIncompatible, errors.New("invalid or oversized downstream receipt")}
			}
		}
		if err = c.finish(ctx, *p, receipt, callErr); err != nil {
			return n, errors.Join(callErr, err)
		}
		if callErr != nil {
			return n, callErr
		}
		n++
	}
	return n, nil
}

func (c *Consumer) claim(ctx context.Context) (*Pending, error) {
	ctx, h, err := acquire(ctx, c.gate)
	if err != nil {
		return nil, err
	}
	defer h.Release()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var p Pending
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT operation_id,command_type,payload,attempts FROM pending_commands WHERE consumer=? AND status='pending' AND retry_at<=? ORDER BY rowid LIMIT 1`, c.name, clock.Millis(c.clock.Now())).Scan(&p.OperationID, &p.CommandType, &raw, &p.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Payload = json.RawMessage(raw)
	p.Attempts++
	if _, err = tx.ExecContext(ctx, `UPDATE pending_commands SET attempts=?,retry_at=? WHERE operation_id=? AND consumer=?`, p.Attempts, clock.Millis(c.clock.Now().Add(backoff(p.Attempts))), p.OperationID, c.name); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Consumer) finish(ctx context.Context, p Pending, receipt json.RawMessage, callErr error) error {
	ctx, h, err := acquire(ctx, c.gate)
	if err != nil {
		return err
	}
	defer h.Release()
	if callErr == nil {
		_, err = c.db.ExecContext(ctx, `UPDATE pending_commands SET status='succeeded',receipt=?,failure_kind='',retry_at=0 WHERE consumer=? AND operation_id=? AND status<>'succeeded'`, string(receipt), c.name, p.OperationID)
		return err
	}
	kind := kindOf(callErr)
	status := "pending"
	retry := clock.Millis(c.clock.Now().Add(backoff(p.Attempts)))
	if permanent(kind) {
		status = "blocked"
		retry = 0
	}
	// 较旧的失败不能覆盖并发重试已写下的成功回执或较新的重试安排。
	_, err = c.db.ExecContext(ctx, `UPDATE pending_commands SET status=?,failure_kind=?,retry_at=? WHERE consumer=? AND operation_id=? AND status='pending' AND attempts=?`, status, kind, retry, c.name, p.OperationID, p.Attempts)
	return err
}

// RetryCommand 是人工/修复流程的显式重试入口，复用原 operation_id 和 payload。
func (c *Consumer) RetryCommand(ctx context.Context, operation ids.ID) error {
	ctx, h, err := acquire(ctx, c.gate)
	if err != nil {
		return err
	}
	defer h.Release()
	_, err = c.db.ExecContext(ctx, `UPDATE pending_commands SET status='pending',failure_kind='',retry_at=0 WHERE consumer=? AND operation_id=? AND status='blocked'`, c.name, operation)
	return err
}
