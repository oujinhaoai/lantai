package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Report 是在当前平台上实测的 SQLite 能力。字段只记录观测结果，
// 是否满足兰台要求由 Required 判定。
type Report struct {
	Driver         string        `json:"driver"`
	DriverVersion  string        `json:"driver_version"`
	SQLiteVersion  string        `json:"sqlite_version"`
	SQLiteSourceID string        `json:"sqlite_source_id"`
	GoVersion      string        `json:"go_version"`
	GOOS           string        `json:"goos"`
	GOARCH         string        `json:"goarch"`
	JournalMode    string        `json:"journal_mode"`
	Synchronous    int           `json:"synchronous"`
	FullFsync      bool          `json:"fullfsync"`
	CommitLatency  time.Duration `json:"single_row_commit_ns"`
	StrictTables   bool          `json:"strict_tables"`
	Returning      bool          `json:"returning_clause"`
	JSONFunctions  bool          `json:"json_functions"`
	FTS5           bool          `json:"fts5"`
	FTS5Error      string        `json:"fts5_error,omitempty"`
	BusyClassified bool          `json:"busy_error_classified"`
	BusyWait       time.Duration `json:"busy_wait_ns"`
	CancelWorks    bool          `json:"context_cancel_interrupts_query"`
	CancelLatency  time.Duration `json:"cancel_latency_ns"`
	RollbackWorks  bool          `json:"rollback_discards_writes"`
	CheckpointOK   bool          `json:"wal_checkpoint_truncate"`
	VacuumIntoOK   bool          `json:"vacuum_into_snapshot"`
	IntegrityOK    bool          `json:"integrity_check"`
	Problems       []string      `json:"problems,omitempty"`
}

// Required 返回兰台依赖但当前平台不满足的能力；FTS5 仅记录，不属必需项。
func (r Report) Required() []string {
	var missing []string
	check := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	check(strings.EqualFold(r.JournalMode, "wal"), "journal_mode=wal")
	check(r.Synchronous == 2, "synchronous=FULL")
	check(r.GOOS != "darwin" || r.FullFsync, "fullfsync on darwin")
	check(r.StrictTables, "STRICT tables")
	check(r.Returning, "RETURNING clause")
	check(r.JSONFunctions, "JSON functions")
	check(r.BusyClassified, "busy error classification")
	check(r.CancelWorks, "context cancellation")
	check(r.RollbackWorks, "transaction rollback")
	check(r.CheckpointOK, "wal_checkpoint(TRUNCATE)")
	check(r.VacuumIntoOK, "VACUUM INTO snapshot")
	check(r.IntegrityOK, "integrity_check")
	return missing
}

// Probe 在 dir 下新建临时数据库并实测能力；dir 应位于要验证的文件系统上。
func Probe(ctx context.Context, dir string) (Report, error) {
	r := Report{
		Driver:    "modernc.org/sqlite",
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "modernc.org/sqlite" {
				r.DriverVersion = d.Version
			}
		}
	}
	work, err := os.MkdirTemp(dir, "lantai-sqlite-probe-")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(work)
	path := filepath.Join(work, "probe.db")
	db, err := Open(ctx, path, Options{BusyTimeout: 200 * time.Millisecond})
	if err != nil {
		return r, err
	}
	defer db.Close()

	note := func(format string, args ...any) { r.Problems = append(r.Problems, fmt.Sprintf(format, args...)) }

	_ = db.QueryRowContext(ctx, "SELECT sqlite_version(), sqlite_source_id()").Scan(&r.SQLiteVersion, &r.SQLiteSourceID)
	_ = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&r.JournalMode)
	_ = db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&r.Synchronous)

	if _, err := db.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL) STRICT"); err != nil {
		note("strict: %v", err)
	} else {
		r.StrictTables = true
	}
	var full int
	_ = db.QueryRowContext(ctx, "PRAGMA fullfsync").Scan(&full)
	r.FullFsync = full == 1
	var id int64
	commitStart := time.Now()
	if err := db.QueryRowContext(ctx, "INSERT INTO t(v) VALUES ('a') RETURNING id").Scan(&id); err != nil {
		note("returning: %v", err)
	} else {
		r.Returning = id == 1
		r.CommitLatency = time.Since(commitStart)
	}
	var jv int
	if err := db.QueryRowContext(ctx, `SELECT json_extract('{"a":{"b":7}}', '$.a.b')`).Scan(&jv); err != nil {
		note("json: %v", err)
	} else {
		r.JSONFunctions = jv == 7
	}
	if _, err := db.ExecContext(ctx, "CREATE VIRTUAL TABLE f USING fts5(body)"); err != nil {
		r.FTS5Error = err.Error()
	} else {
		r.FTS5 = true
	}

	r.BusyClassified, r.BusyWait = probeBusy(ctx, path, db)
	r.CancelWorks, r.CancelLatency = probeCancel(ctx, db)
	r.RollbackWorks = probeRollback(ctx, db)

	var busy, logFrames, ckpt int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &ckpt); err != nil {
		note("checkpoint: %v", err)
	} else {
		r.CheckpointOK = busy == 0
	}

	snap := filepath.Join(work, "snapshot.db")
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", snap); err != nil {
		note("vacuum into: %v", err)
	} else if ok, err := snapshotMatches(ctx, snap); err != nil {
		note("snapshot: %v", err)
	} else {
		r.VacuumIntoOK = ok
	}

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		note("integrity: %v", err)
	} else {
		r.IntegrityOK = integrity == "ok"
	}
	return r, nil
}

// probeBusy 让第二个连接在第一个连接持有写锁时开启写事务，确认在 busy_timeout
// 之后得到可分类的 busy 错误，而不是无限等待或静默成功。
func probeBusy(ctx context.Context, path string, db *sql.DB) (bool, time.Duration) {
	holder, err := db.Conn(ctx)
	if err != nil {
		return false, 0
	}
	defer holder.Close()
	if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, 0
	}
	defer func() { _, _ = holder.ExecContext(ctx, "ROLLBACK") }() // 探测结束后释放锁

	other, err := Open(ctx, path, Options{BusyTimeout: 200 * time.Millisecond})
	if err != nil {
		return false, 0
	}
	defer other.Close()
	start := time.Now()
	tx, err := other.BeginTx(ctx, nil)
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO t(v) VALUES ('b')")
		tx.Rollback()
	}
	return Classify(err) == ClassBusy, time.Since(start)
}

// probeCancel 用超时上下文中断一个长查询。
func probeCancel(ctx context.Context, db *sql.DB) (bool, time.Duration) {
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	var n int64
	err := db.QueryRowContext(cctx, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT count(*) FROM c").Scan(&n)
	return err != nil && Classify(err) == ClassCanceled, time.Since(start)
}

func probeRollback(ctx context.Context, db *sql.DB) bool {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO t(v) VALUES ('rolled-back')"); err != nil {
		tx.Rollback()
		return false
	}
	if err := tx.Rollback(); err != nil {
		return false
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM t WHERE v = 'rolled-back'").Scan(&n); err != nil {
		return false
	}
	return n == 0
}

func snapshotMatches(ctx context.Context, path string) (bool, error) {
	db, err := Open(ctx, path, Options{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return false, err
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil {
		return false, err
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t(v) VALUES ('x')"); err == nil {
		return false, errors.New("query_only connection accepted a write")
	}
	return integrity == "ok" && n == 1, nil
}
