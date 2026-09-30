// Package sqlite 封装兰台使用的 SQLite 驱动与连接基线。
//
// 驱动为纯 Go 的 modernc.org/sqlite（无需 CGo）。每个连接固定：WAL 日志、
// synchronous=FULL（已提交事务在断电后仍持久；macOS 另开 fullfsync）、
// busy_timeout、外键检查、
// defensive 模式，写事务用 BEGIN IMMEDIATE 避免读锁升级死锁；只读连接
// 额外设置 query_only。可写库的写事务与事务外写语句先在进程内按到达
// 顺序排队（见 gate.go），再向 SQLite 取锁。数据库文件必须位于服务端
// 本机文件系统，不支持网络盘。
// 五库的连接编排、迁移与维护屏障归 operations 模块（T08），本包只提供
// 经过验证的连接方式与能力探测。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// DriverName 是 database/sql 注册名。
const DriverName = "sqlite"

// Options 控制连接参数。
type Options struct {
	// ReadOnly 为 true 时只打开已存在的库，设置 query_only，不改日志模式、
	// 不用 BEGIN IMMEDIATE，也不会创建文件；用于核验快照与备份时不改动被核验的库。
	ReadOnly bool
	// BusyTimeout 是等待其他写者释放锁的时长，默认 5 秒。
	BusyTimeout time.Duration
	// WriteQueueTimeout 是在进程内写入队列中的最长等待，默认 15 秒；
	// 超时返回 ErrWriteQueueTimeout。只读模式不排队。
	WriteQueueTimeout time.Duration
}

// DSN 返回带兰台基线参数的连接串。path 不能包含 '?'。
func DSN(path string, opts Options) (string, error) {
	if path == "" || strings.ContainsRune(path, '?') {
		return "", fmt.Errorf("sqlite: invalid database path %q", path)
	}
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = 5 * time.Second
	}
	q := url.Values{}
	q.Set("_busy_timeout", strconv.FormatInt(busy.Milliseconds(), 10))
	q.Set("_foreign_keys", "1")
	q.Set("_defensive", "1")
	if opts.ReadOnly {
		q.Set("_query_only", "1")
		return path + "?" + q.Encode(), nil
	}
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "FULL")
	q.Set("_txlock", "immediate")
	if runtime.GOOS == "darwin" {
		// macOS 的 fsync 不把磁盘缓存刷到介质，需要 F_FULLFSYNC 才能保证
		// 已提交事务在断电后仍在；代价是提交变慢，实测数据见能力报告。
		q.Add("_pragma", "fullfsync(1)")
		q.Add("_pragma", "checkpoint_fullfsync(1)")
	}
	return path + "?" + q.Encode(), nil
}

// Open 打开数据库。可写模式必要时创建文件并确认基线参数已生效；只读模式
// 要求文件已存在。
func Open(ctx context.Context, path string, opts Options) (*sql.DB, error) {
	dsn, err := DSN(path, opts)
	if err != nil {
		return nil, err
	}
	if opts.ReadOnly {
		st, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("sqlite: read-only open: %w", err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("sqlite: read-only open: %s is not a regular file", path)
		}
	}
	var db *sql.DB
	if opts.ReadOnly {
		if db, err = sql.Open(DriverName, dsn); err != nil {
			return nil, err
		}
		err = db.PingContext(ctx)
	} else {
		db = sql.OpenDB(&gatedConnector{dsn: dsn, gate: newWriteGate(opts.WriteQueueTimeout)})
		err = verifyBaseline(ctx, db)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func verifyBaseline(ctx context.Context, db *sql.DB) error {
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("sqlite: read journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("sqlite: journal_mode is %q, want wal (network or read-only file systems are not supported)", mode)
	}
	var sync int
	if err := db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
		return fmt.Errorf("sqlite: read synchronous: %w", err)
	}
	if sync != 2 {
		return fmt.Errorf("sqlite: synchronous is %d, want 2 (FULL)", sync)
	}
	if runtime.GOOS == "darwin" {
		var full int
		if err := db.QueryRowContext(ctx, "PRAGMA fullfsync").Scan(&full); err != nil || full != 1 {
			return fmt.Errorf("sqlite: fullfsync not enabled on darwin (value %d, err %v)", full, err)
		}
	}
	return nil
}

// Result codes used by callers to classify failures.
const (
	codeBusy       = sqlite3.SQLITE_BUSY
	codeLocked     = sqlite3.SQLITE_LOCKED
	codeFull       = sqlite3.SQLITE_FULL
	codeReadOnly   = sqlite3.SQLITE_READONLY
	codeIOErr      = sqlite3.SQLITE_IOERR
	codeConstraint = sqlite3.SQLITE_CONSTRAINT
	codeCorrupt    = sqlite3.SQLITE_CORRUPT
	codeNotADB     = sqlite3.SQLITE_NOTADB
	codeInterrupt  = sqlite3.SQLITE_INTERRUPT
)

// Class 是驱动错误的粗分类，供上层映射为稳定的业务错误。
type Class string

const (
	ClassNone       Class = ""
	ClassBusy       Class = "busy"       // 锁等待或写入排队超时，可稍后重试
	ClassFull       Class = "full"       // 磁盘或数据库已满
	ClassReadOnly   Class = "read_only"  // 只读介质或 query_only 连接
	ClassIO         Class = "io"         // I/O 失败，结果不可假设
	ClassConstraint Class = "constraint" // 唯一键等约束冲突
	ClassCorrupt    Class = "corrupt"    // 文件损坏或不是数据库
	ClassCanceled   Class = "canceled"   // 上下文取消或中断
	ClassOther      Class = "other"
)

// Classify 按 SQLite 主结果码对错误分类。
func Classify(err error) Class {
	if err == nil {
		return ClassNone
	}
	if errors.Is(err, ErrWriteQueueTimeout) {
		return ClassBusy
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassCanceled
	}
	var se *msqlite.Error
	if !errors.As(err, &se) {
		return ClassOther
	}
	switch se.Code() & 0xFF { // 扩展结果码的低 8 位是主结果码
	case codeBusy, codeLocked:
		return ClassBusy
	case codeFull:
		return ClassFull
	case codeReadOnly:
		return ClassReadOnly
	case codeIOErr:
		return ClassIO
	case codeConstraint:
		return ClassConstraint
	case codeCorrupt, codeNotADB:
		return ClassCorrupt
	case codeInterrupt:
		return ClassCanceled
	default:
		return ClassOther
	}
}

// Structured 把错误转为对外结构化错误。领域已给出的错误码不变；落到
// INTERNAL 的锁竞争（BUSY/LOCKED 或写入排队超时）改为可重试的
// STORAGE_UNAVAILABLE，调用方用同一幂等键稍后重试即可，已持久的操作阶段
// 由幂等回放续上。其他非结构化错误仍为 INTERNAL。
func Structured(err error) *errcode.Error {
	e := errcode.From(err)
	if e == nil || e.Code != errcode.Internal || Classify(err) != ClassBusy {
		return e
	}
	return errcode.Wrap(errcode.StorageUnavailable, "the database is busy; retry the same request later", err).
		WithRetryAfter(time.Second).
		WithDetails(errcode.Detail{Reason: "database_busy"})
}

// IsUniqueViolation 报告错误是否为唯一约束冲突。
func IsUniqueViolation(err error) bool {
	var se *msqlite.Error
	return errors.As(err, &se) && (se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}
