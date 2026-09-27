package events

import (
	"context"
	"errors"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
)

// HotWindow 是默认热事件目标保留期，使用收录时间以保护延迟抵达的事件。
const HotWindow = 90 * 24 * time.Hour

// RegisterConsumer 固定关键消费者的保留依赖；首次注册水位为零。
// required 不能在重试中降级，移除关键依赖须另行完成停用协议。
func (s *Store) RegisterConsumer(ctx context.Context, name string, required bool) error {
	if !nameRE.MatchString(name) {
		return errors.New("events: invalid consumer name")
	}
	ctx, h, err := acquire(ctx, s.gate)
	if err != nil {
		return err
	}
	defer h.Release()
	_, err = s.db.ExecContext(ctx, `INSERT INTO events_consumers(consumer,required) VALUES(?,?) ON CONFLICT(consumer) DO UPDATE SET required=max(required,excluded.required)`, name, required)
	return err
}

// AcknowledgeConsumer 只接受已在消费者本库事务提交的水位；不得在事务前预报。
func (s *Store) AcknowledgeConsumer(ctx context.Context, name string, through int64) error {
	ctx, h, err := acquire(ctx, s.gate)
	if err != nil {
		return err
	}
	defer h.Release()
	hi, err := s.HighWater(ctx)
	if err != nil {
		return err
	}
	if through < 0 || through > hi {
		return errors.New("events: invalid consumer watermark")
	}
	r, err := s.db.ExecContext(ctx, `UPDATE events_consumers SET through_seq=max(through_seq,?) WHERE consumer=?`, through, name)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("events: consumer is not registered")
	}
	return err
}

// ConfirmBackup 固定与 T08 的对接：只有完整备份已验证后才报告其事件水位。
// 这里不执行备份；默认零水位会阻止任何裁剪，避免把尚未实现的备份视为成功。
func (s *Store) ConfirmBackup(ctx context.Context, through int64) error {
	ctx, h, err := acquire(ctx, s.gate)
	if err != nil {
		return err
	}
	defer h.Release()
	hi, err := s.HighWater(ctx)
	if err != nil {
		return err
	}
	if through < 0 || through > hi {
		return errors.New("events: invalid backup watermark")
	}
	_, err = s.db.ExecContext(ctx, `UPDATE events_retention SET backup_through=max(backup_through,?) WHERE singleton=1`, through)
	return err
}

// Prune 仅删除满足时间、审计、全部关键消费者和已确认备份水位的连续前缀。
// 身份摘要仍保留。返回新的裁剪水位，不返回删除数量。
func (s *Store) Prune(ctx context.Context) (int64, error) {
	ctx, h, err := acquire(ctx, s.gate)
	if err != nil {
		return 0, err
	}
	defer h.Release()
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var pruned, audit, backup int64
	if err = tx.QueryRowContext(ctx, `SELECT pruned_through,audit_through,backup_through FROM events_retention WHERE singleton=1`).Scan(&pruned, &audit, &backup); err != nil {
		return 0, err
	}
	cap := min(audit, backup)
	var consumer int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(min(through_seq),?) FROM events_consumers WHERE required=1`, cap).Scan(&consumer); err != nil {
		return 0, err
	}
	cap = min(cap, consumer)
	// 即使测试时钟回拨或批次时间不单调，也不跨过任何尚在热窗口的记录。
	var firstYoung int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(min(global_seq),?) FROM events_records WHERE recorded_at>=?`, cap+1, clock.Millis(s.clock.Now().Add(-HotWindow))).Scan(&firstYoung); err != nil {
		return 0, err
	}
	cap = min(cap, firstYoung-1)
	if cap < pruned {
		cap = pruned
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM events_records WHERE global_seq<=?`, cap); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE events_retention SET pruned_through=? WHERE singleton=1`, cap); err != nil {
		return 0, err
	}
	return cap, tx.Commit()
}

// Metrics 只供 T08 的本机运维读取，不含信封或凭据。
type Metrics struct {
	HighWater     int64
	PrunedThrough int64
	AuditThrough  int64
	BackupThrough int64
	HotRecords    int64
	RelayFailures int64
	ConsumerLag   int64
}

func (s *Store) Metrics(ctx context.Context) (Metrics, error) {
	var m Metrics
	var err error
	if m.HighWater, err = s.HighWater(ctx); err != nil {
		return m, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT pruned_through,audit_through,backup_through FROM events_retention WHERE singleton=1`).Scan(&m.PrunedThrough, &m.AuditThrough, &m.BackupThrough); err != nil {
		return m, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events_records`).Scan(&m.HotRecords); err != nil {
		return m, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events_relay_state WHERE failure_kind<>''`).Scan(&m.RelayFailures); err != nil {
		return m, err
	}
	var through int64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(min(through_seq),?) FROM events_consumers WHERE required=1`, m.HighWater).Scan(&through)
	m.ConsumerLag = m.HighWater - through
	return m, err
}
