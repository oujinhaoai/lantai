package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// OutboxRecord 是待搬运的源事件。Envelope 为规范化 JSON，relay 原样写入 events.db。
type OutboxRecord struct {
	Seq         int64
	EventID     ids.ID
	OperationID ids.ID
	Envelope    []byte
	OccurredAt  time.Time
}

// ReadUndelivered 按源库写入顺序读取尚未确认的事件，供 events 模块的 relay 使用。
// relay 必须先在 events.db 按 event_id 唯一收录，再调用 MarkDelivered；
// 两步之间崩溃只会导致重复收录尝试，不会产生第二个逻辑事件。
func ReadUndelivered(ctx context.Context, q DBTX, afterSeq int64, limit int) ([]OutboxRecord, error) {
	if limit <= 0 || limit > 10000 {
		return nil, fmt.Errorf("commands: outbox read limit %d out of range", limit)
	}
	rows, err := q.QueryContext(ctx, `SELECT seq, event_id, operation_id, envelope, occurred_at FROM outbox
		WHERE delivered_at IS NULL AND seq > ? ORDER BY seq LIMIT ?`, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRecord
	for rows.Next() {
		var r OutboxRecord
		var env string
		var occurred int64
		if err := rows.Scan(&r.Seq, &r.EventID, &r.OperationID, &env, &occurred); err != nil {
			return nil, err
		}
		r.Envelope, r.OccurredAt = []byte(env), clock.FromMillis(occurred)
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkDelivered 标记事件已被 events.db 收录；已标记的事件不受影响（幂等）。
// 返回本次新标记的数量。
func MarkDelivered(ctx context.Context, q DBTX, eventIDs []ids.ID, at time.Time) (int64, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}
	if len(eventIDs) > 1000 {
		return 0, errors.New("commands: mark at most 1000 events per call")
	}
	args := make([]any, 0, len(eventIDs)+1)
	args = append(args, clock.Millis(at))
	for _, id := range eventIDs {
		args = append(args, id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(eventIDs)), ",")
	res, err := q.ExecContext(ctx, `UPDATE outbox SET delivered_at = ? WHERE delivered_at IS NULL AND event_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// OutboxBacklog 返回未确认事件数与最老一条的发生时间，用于运维指标。
func OutboxBacklog(ctx context.Context, q DBTX) (count int64, oldest time.Time, err error) {
	var min sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT count(*), min(occurred_at) FROM outbox WHERE delivered_at IS NULL`).Scan(&count, &min); err != nil {
		return 0, time.Time{}, err
	}
	if min.Valid {
		oldest = clock.FromMillis(min.Int64)
	}
	return count, oldest, nil
}

func pendingEvents(ctx context.Context, q DBTX, opID ids.ID) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE operation_id = ? AND delivered_at IS NULL`, opID).Scan(&n)
	return n, err
}
