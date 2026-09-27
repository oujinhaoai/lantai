package events

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Source 由源库所有者提供；收录器不访问源业务表，也不共享 SQL 事务。
// Ack 必须经同一实例 Gate 进入源库写路径；重复确认应幂等。
type Source interface {
	Name() string
	Pull(context.Context, int) ([]commands.OutboxRecord, error)
	Ack(context.Context, []ids.ID, time.Time) error
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,39}$`)

// ErrDeferred 表示下一次重试时间尚未到；调用方不能热循环。
var ErrDeferred = errors.New("events: retry is deferred")

// ErrBlocked 表示 schema 不兼容或业务冲突，需显式处理并重试。
var ErrBlocked = errors.New("events: processing requires intervention")

func backoff(attempt int) time.Duration {
	if attempt > 9 {
		return 5 * time.Minute
	}
	if attempt < 1 {
		attempt = 1
	}
	return time.Second * time.Duration(1<<uint(attempt-1))
}

// Relay 执行一个来源的一批 pull→collect→ack。多源由组装方逐个调用，彼此不阻塞事务。
// 收录与确认之间退出后，新实例会重读源 outbox 并按 event_id 去重。
func (s *Store) Relay(ctx context.Context, source Source, limit int) (int, error) {
	if source == nil || !nameRE.MatchString(source.Name()) || limit < 1 || limit > 1000 {
		return 0, errors.New("events: invalid relay source or limit")
	}
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT retry_at FROM events_relay_state WHERE source=?`, source.Name()).Scan(&at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if at > clock.Millis(s.clock.Now()) {
		return 0, ErrDeferred
	}
	records, err := source.Pull(ctx, limit)
	if err != nil {
		return 0, errors.Join(err, s.recordRelay(ctx, source.Name(), false))
	}
	n, err := s.Collect(ctx, records)
	if err == nil && len(records) > 0 {
		idsToAck := make([]ids.ID, len(records))
		for i, r := range records {
			idsToAck[i] = r.EventID
		}
		err = source.Ack(ctx, idsToAck, s.clock.Now())
	}
	if err != nil {
		return n, errors.Join(err, s.recordRelay(ctx, source.Name(), false))
	}
	return n, s.recordRelay(ctx, source.Name(), true)
}

func (s *Store) recordRelay(ctx context.Context, name string, success bool) error {
	ctx, h, err := acquire(ctx, s.gate)
	if err != nil {
		return err
	}
	defer h.Release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if success {
		_, err = tx.ExecContext(ctx, `INSERT INTO events_relay_state(source) VALUES(?) ON CONFLICT(source) DO UPDATE SET attempts=0,retry_at=0,failure_kind=''`, name)
	} else {
		var attempts int
		err = tx.QueryRowContext(ctx, `SELECT attempts FROM events_relay_state WHERE source=?`, name).Scan(&attempts)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		attempts++
		_, err = tx.ExecContext(ctx, `INSERT INTO events_relay_state(source,attempts,retry_at,failure_kind) VALUES(?,?,?,'transient') ON CONFLICT(source) DO UPDATE SET attempts=excluded.attempts,retry_at=excluded.retry_at,failure_kind=excluded.failure_kind`, name, attempts, clock.Millis(s.clock.Now().Add(backoff(attempts))))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
