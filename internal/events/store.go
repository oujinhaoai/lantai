package events

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
)

// Store 只持有 events.db；源 outbox 通过 Source 接口交付。
type Store struct {
	db      *sql.DB
	clock   clock.Clock
	gate    *commands.Gate
	auditMu sync.Mutex
	// audit 记录本进程上次成功导出后已逐行核验的审计文件状态（受 auditMu
	// 保护）。只有整个文件的摘要仍与之相同才跳过逐行核验；进程重启、导出
	// 失败或文件被改动都回到完整核验。
	audit verifiedAudit
	// auditScans 统计完整逐行核验次数，供测试确认续写不随文件增长重扫。
	auditScans int
}

type verifiedAudit struct {
	path     string
	manifest AuditManifest
}

// New 创建收录器。写入必须传入实例共享 Gate；无 Gate 的实例只能读取。
func New(db *sql.DB, clk clock.Clock, gates ...*commands.Gate) *Store {
	if clk == nil {
		clk = clock.System{}
	}
	s := &Store{db: db, clock: clk}
	if len(gates) > 0 {
		s.gate = gates[0]
	}
	return s
}

func acquire(ctx context.Context, gate *commands.Gate) (context.Context, *commands.Held, error) {
	if gate == nil {
		return ctx, nil, errors.New("events: shared instance write gate is required")
	}
	return gate.Acquire(ctx, commands.Request{})
}

// Entry 的信封逐字节来自源 outbox。GlobalSeq 只表示收录顺序。
type Entry struct {
	GlobalSeq  int64
	RecordedAt time.Time
	Envelope   event.Envelope
	Canonical  []byte
}

// Page.HighWater 是已扫描的位置，即使调用者最终过滤掉全部条目也必须推进至此。
type Page struct {
	Entries   []Entry
	HighWater int64
}

// HighWater 返回持久化收录水位；热记录裁剪后也不会下降。
func (s *Store) HighWater(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(global_seq),0) FROM events_identities`).Scan(&n)
	return n, err
}

// Read 在一致的库快照内扫描有界的内部事件页。它不提供外部授权，不能直接暴露给客户端。
// 重同步须先捕获 S=HighWater，再全量替换权威可见快照，最后从 S 重放；不能沿用过期缓存。
func (s *Store) Read(ctx context.Context, after int64, limit int) (Page, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return Page{}, errors.New("events: invalid cursor or limit")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Page{}, err
	}
	defer tx.Rollback()
	var pruned, high int64
	if err = tx.QueryRowContext(ctx, `SELECT pruned_through FROM events_retention WHERE singleton=1`).Scan(&pruned); err != nil {
		return Page{}, err
	}
	if after < pruned {
		return Page{}, errcode.New(errcode.CursorExpired, "event cursor is outside the retained window; replace the visible snapshot and replay")
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(global_seq),0) FROM events_identities`).Scan(&high); err != nil {
		return Page{}, err
	}
	if after > high {
		return Page{}, errors.New("events: cursor is ahead of the log")
	}
	rows, err := tx.QueryContext(ctx, `SELECT global_seq,envelope,recorded_at FROM events_records WHERE global_seq>? ORDER BY global_seq LIMIT ?`, after, limit)
	if err != nil {
		return Page{}, err
	}
	page := Page{HighWater: after}
	for rows.Next() {
		var e Entry
		var raw string
		var recorded int64
		if err = rows.Scan(&e.GlobalSeq, &raw, &recorded); err != nil {
			rows.Close()
			return Page{}, err
		}
		e.Canonical = []byte(raw)
		e.RecordedAt = clock.FromMillis(recorded)
		if e.Envelope, err = event.Parse(e.Canonical); err != nil {
			rows.Close()
			return Page{}, err
		}
		page.Entries = append(page.Entries, e)
		page.HighWater = e.GlobalSeq
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Page{}, err
	}
	if len(page.Entries) < limit {
		page.HighWater = high
	}
	return page, tx.Commit()
}

// Collect 原子收录一批规范化源信封。重复 ID 的字节不同属于冲突，整批回滚；
// 裁剪后保留 ID/摘要去重事实，旧 outbox 重放不会复活为新逻辑事件。
func (s *Store) Collect(ctx context.Context, records []commands.OutboxRecord) (int, error) {
	if len(records) > 1000 {
		return 0, errors.New("events: collect at most 1000 records")
	}
	ctx, h, err := acquire(ctx, s.gate)
	if err != nil {
		return 0, err
	}
	defer h.Release()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, r := range records {
		e, err := event.Parse(r.Envelope)
		if err != nil {
			return 0, err
		}
		canonical, err := e.Canonical()
		if err != nil {
			return 0, err
		}
		if !bytes.Equal(canonical, r.Envelope) || e.EventID != r.EventID || e.OperationID != r.OperationID {
			return 0, errors.New("events: outbox envelope or identity mismatch")
		}
		d := digest.Of(r.Envelope)
		var previous string
		err = tx.QueryRowContext(ctx, `SELECT envelope_digest FROM events_identities WHERE event_id=?`, e.EventID).Scan(&previous)
		if err == nil {
			if previous != string(d) {
				return 0, fmt.Errorf("events: conflicting event identity %s", e.EventID)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		now := clock.Millis(s.clock.Now())
		result, err := tx.ExecContext(ctx, `INSERT INTO events_records(event_id,envelope,recorded_at) VALUES(?,?,?)`, e.EventID, string(r.Envelope), now)
		if err != nil {
			return 0, err
		}
		seq, err := result.LastInsertId()
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO events_identities(event_id,global_seq,envelope_digest,recorded_at) VALUES(?,?,?,?)`, e.EventID, seq, d, now); err != nil {
			return 0, err
		}
		n++
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}
