package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// FailureKind 将重试和人工干预分开；未分类的错误按 transient 处理。
type FailureKind string

const (
	Transient           FailureKind = "transient"
	SchemaIncompatible  FailureKind = "schema_incompatible"
	MissingPrerequisite FailureKind = "missing_prerequisite"
	BusinessConflict    FailureKind = "business_conflict"
)

type Failure struct {
	Kind  FailureKind
	Cause error
}

func (f *Failure) Error() string { return fmt.Sprintf("events: %s: %v", f.Kind, f.Cause) }
func (f *Failure) Unwrap() error { return f.Cause }
func kindOf(err error) FailureKind {
	var f *Failure
	if errors.As(err, &f) {
		switch f.Kind {
		case Transient, SchemaIncompatible, MissingPrerequisite, BusinessConflict:
			return f.Kind
		}
	}
	return Transient
}
func permanent(k FailureKind) bool { return k == SchemaIncompatible || k == BusinessConflict }

// Consumer 是单个模块在本库的事务消费者；Handler 仅能写所属业务状态。
type Consumer struct {
	db    *sql.DB
	name  string
	clock clock.Clock
	gate  *commands.Gate
	mu    sync.Mutex
}

func NewConsumer(db *sql.DB, name string, clk clock.Clock, gate *commands.Gate) (*Consumer, error) {
	if !nameRE.MatchString(name) {
		return nil, errors.New("events: invalid consumer name")
	}
	if clk == nil {
		clk = clock.System{}
	}
	return &Consumer{db: db, name: name, clock: clk, gate: gate}, nil
}

// Command 是单条跨模块后续动作。消费事务先保存该意图，提交后才通过领域接口调用。
type Command struct {
	StepKey     string
	CommandType string
	Payload     json.RawMessage
}
type Handler func(context.Context, *sql.Tx, Entry) ([]Command, error)

func (c *Consumer) Offset(ctx context.Context) (int64, error) {
	var n int64
	err := c.db.QueryRowContext(ctx, `SELECT through_seq FROM consumer_offsets WHERE consumer=?`, c.name).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Apply 把本地状态、待执行命令、processed_events 和水位放进同一事务。
// 返回 false 表示重复事件；出错后事务回滚、水位不前进，故障状态另行持久化。
func (c *Consumer) Apply(ctx context.Context, e Entry, handler Handler) (bool, error) {
	if e.GlobalSeq < 1 || !e.Envelope.EventID.Valid() || handler == nil {
		return false, errors.New("events: invalid consumer entry or handler")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, h, err := acquire(ctx, c.gate)
	if err != nil {
		return false, err
	}
	defer h.Release()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var prev int64
	err = tx.QueryRowContext(ctx, `SELECT global_seq FROM processed_events WHERE consumer=? AND event_id=?`, c.name, e.Envelope.EventID).Scan(&prev)
	if err == nil {
		if prev != e.GlobalSeq {
			return false, errors.New("events: duplicate event has a different sequence")
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var offset int64
	err = tx.QueryRowContext(ctx, `SELECT through_seq FROM consumer_offsets WHERE consumer=?`, c.name).Scan(&offset)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if e.GlobalSeq != offset+1 {
		return false, &Failure{MissingPrerequisite, errors.New("consumer must process the next sequence")}
	}
	var kind FailureKind
	var retry int64
	err = tx.QueryRowContext(ctx, `SELECT kind,retry_at FROM consumer_failures WHERE consumer=? AND event_id=?`, c.name, e.Envelope.EventID).Scan(&kind, &retry)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if permanent(kind) {
			return false, ErrBlocked
		}
		if retry > clock.Millis(c.clock.Now()) {
			return false, ErrDeferred
		}
	}
	cmds, err := handler(ctx, tx, e)
	if err == nil {
		err = c.enqueue(ctx, tx, e, cmds)
	}
	if err != nil {
		_ = tx.Rollback()
		return false, errors.Join(err, c.recordFailure(ctx, e, kindOf(err)))
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO processed_events(consumer,event_id,global_seq,processed_at) VALUES(?,?,?,?)`, c.name, e.Envelope.EventID, e.GlobalSeq, clock.Millis(c.clock.Now())); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO consumer_offsets(consumer,through_seq) VALUES(?,?) ON CONFLICT(consumer) DO UPDATE SET through_seq=excluded.through_seq`, c.name, e.GlobalSeq); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM consumer_failures WHERE consumer=? AND event_id=?`, c.name, e.Envelope.EventID); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Consumer) enqueue(ctx context.Context, tx *sql.Tx, e Entry, cmds []Command) error {
	invalid := func(err error) error { return &Failure{SchemaIncompatible, err} }
	if len(cmds) > 100 {
		return invalid(errors.New("events: too many follow-up commands"))
	}
	steps := make(map[string]bool, len(cmds))
	for _, cmd := range cmds {
		if cmd.StepKey == "" || steps[cmd.StepKey] {
			return invalid(errors.New("events: invalid or duplicate follow-up step or command type"))
		}
		steps[cmd.StepKey] = true
		child, err := ids.DeriveChild(e.Envelope.OperationID, "consumer:"+c.name+":event:"+string(e.Envelope.EventID)+":step:"+cmd.StepKey)
		if err != nil {
			return invalid(err)
		}
		raw, err := canonjson.Canonicalize(cmd.Payload)
		if err != nil {
			return invalid(err)
		}
		if len(raw) == 0 || raw[0] != '{' || len(raw) > 64<<10 {
			return invalid(errors.New("events: command payload must be an object of at most 64 KiB"))
		}
		// 复用共享命令语法和请求内容校验，不另定义一份命令协议。
		if _, err := commands.RequestHash(commands.HashInput{CommandType: cmd.CommandType, Body: raw}); err != nil {
			return invalid(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO pending_commands(consumer,event_id,step_key,operation_id,command_type,payload,status) VALUES(?,?,?,?,?,?,'pending')`, c.name, e.Envelope.EventID, cmd.StepKey, child, cmd.CommandType, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Consumer) recordFailure(ctx context.Context, e Entry, kind FailureKind) error {
	var attempts int
	err := c.db.QueryRowContext(ctx, `SELECT attempts FROM consumer_failures WHERE consumer=? AND event_id=?`, c.name, e.Envelope.EventID).Scan(&attempts)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	attempts++
	retry := int64(0)
	if !permanent(kind) {
		retry = clock.Millis(c.clock.Now().Add(backoff(attempts)))
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO consumer_failures(consumer,event_id,global_seq,kind,attempts,retry_at) VALUES(?,?,?,?,?,?) ON CONFLICT(consumer,event_id) DO UPDATE SET kind=excluded.kind,attempts=excluded.attempts,retry_at=excluded.retry_at`, c.name, e.Envelope.EventID, e.GlobalSeq, kind, attempts, retry)
	return err
}

// Retry 显式释放已处理的故障；保留 attempt 计数，避免错误处置后立即热循环。
func (c *Consumer) Retry(ctx context.Context, eventID ids.ID) error {
	ctx, h, err := acquire(ctx, c.gate)
	if err != nil {
		return err
	}
	defer h.Release()
	_, err = c.db.ExecContext(ctx, `UPDATE consumer_failures SET kind='transient',retry_at=0 WHERE consumer=? AND event_id=?`, c.name, eventID)
	return err
}

// Consume 处理一个有界页；第一条失败时停下，后面的水位绝不越过失败记录。
func (c *Consumer) Consume(ctx context.Context, s *Store, limit int, handler Handler) (int, error) {
	offset, err := c.Offset(ctx)
	if err != nil {
		return 0, err
	}
	page, err := s.Read(ctx, offset, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, entry := range page.Entries {
		ok, err := c.Apply(ctx, entry, handler)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// ConsumerMetrics 是消费者本库的运维状态，不包含业务 payload。
type ConsumerMetrics struct {
	Through         int64
	Failures        int64
	Pending         int64
	BlockedCommands int64
}

// ConsumerFailure is a local operations view, without event payloads or raw
// error strings. RetryAtMillis is zero for failures requiring explicit repair.
type ConsumerFailure struct {
	EventID       ids.ID      `json:"event_id"`
	GlobalSeq     int64       `json:"global_seq"`
	Kind          FailureKind `json:"kind"`
	Attempts      int         `json:"attempts"`
	RetryAtMillis int64       `json:"retry_at_ms"`
}

// Failures lists this consumer's durable failure queue under a live maintenance
// barrier. The sequence of the last returned item is the next page's cursor.
func (c *Consumer) Failures(ctx context.Context, after int64, limit int) ([]ConsumerFailure, error) {
	if c.gate == nil {
		return nil, errors.New("events: failure inventory requires maintenance")
	}
	if err := c.gate.RequireMaintenance(ctx); err != nil {
		return nil, err
	}
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("events: invalid failure inventory page")
	}
	rows, err := c.db.QueryContext(ctx, `SELECT event_id,global_seq,kind,attempts,retry_at FROM consumer_failures WHERE consumer=? AND global_seq>? ORDER BY global_seq LIMIT ?`, c.name, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConsumerFailure{}
	for rows.Next() {
		var f ConsumerFailure
		if err = rows.Scan(&f.EventID, &f.GlobalSeq, &f.Kind, &f.Attempts, &f.RetryAtMillis); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (c *Consumer) Metrics(ctx context.Context) (ConsumerMetrics, error) {
	var m ConsumerMetrics
	var err error
	if m.Through, err = c.Offset(ctx); err != nil {
		return m, err
	}
	if err = c.db.QueryRowContext(ctx, `SELECT count(*) FROM consumer_failures WHERE consumer=?`, c.name).Scan(&m.Failures); err != nil {
		return m, err
	}
	if err = c.db.QueryRowContext(ctx, `SELECT count(*) FROM pending_commands WHERE consumer=? AND status='pending'`, c.name).Scan(&m.Pending); err != nil {
		return m, err
	}
	err = c.db.QueryRowContext(ctx, `SELECT count(*) FROM pending_commands WHERE consumer=? AND status='blocked'`, c.name).Scan(&m.BlockedCommands)
	return m, err
}

// RebuildProjection replaces derived state and its consumer offset in one local
// transaction. It is deliberately maintenance-only, requires a caller-supplied
// authoritative snapshot, and refuses consumers with pending cross-module work.
// Business facts (review, publication, permissions) must never use this method.
// The caller captures through BEFORE enumerating authority, then replays after it.
func (c *Consumer) RebuildProjection(ctx context.Context, through int64, replace func(context.Context, *sql.Tx) error) error {
	if c.gate == nil || c.gate.RequireMaintenance(ctx) != nil || through < 0 || replace == nil {
		return errors.New("events: projection rebuild requires maintenance and a valid snapshot")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT through_seq FROM consumer_offsets WHERE consumer=?),0)`, c.name).Scan(&current); err != nil {
		return err
	}
	if through < current {
		return errors.New("events: projection rebuild cannot move the consumer backwards")
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM pending_commands WHERE consumer=? AND status<>'succeeded'`, c.name).Scan(&pending); err != nil {
		return err
	}
	if pending != 0 {
		return errors.New("events: pending business commands prevent projection replacement")
	}
	if err = replace(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO consumer_offsets(consumer,through_seq) VALUES(?,?) ON CONFLICT(consumer) DO UPDATE SET through_seq=excluded.through_seq`, c.name, through); err != nil {
		return err
	}
	// Only future entries will run again; retain older failure/deduplication facts
	// as diagnostics. Nothing in this method executes or completes business work.
	return tx.Commit()
}
