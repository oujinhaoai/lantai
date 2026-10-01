package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

// 许多写者同时写同一个库：事务与事务外写语句先在进程内排队，busy_timeout
// 很短也不会出现 SQLITE_BUSY。未排队的对照只记录，不作断言。
func TestWriteQueueAvoidsBusyUnderContention(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "q.db")
	opts := Options{BusyTimeout: 20 * time.Millisecond}
	db, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE w (id INTEGER PRIMARY KEY, v TEXT NOT NULL) STRICT"); err != nil {
		t.Fatal(err)
	}
	write := func(db *sql.DB) (writes, busy int, other []error) {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for g := range 24 {
			wg.Go(func() {
				for n := range 20 {
					var err error
					switch g % 6 {
					case 0:
						_, err = db.ExecContext(ctx, "INSERT INTO w(v) VALUES (?)", fmt.Sprint(g, n))
					case 1: // 事务外的 INSERT … RETURNING 经 QueryContext
						var id int64
						err = db.QueryRowContext(ctx, "INSERT INTO w(v) VALUES (?) RETURNING id", fmt.Sprint(g, n)).Scan(&id)
					case 2: // 事务外的预编译语句
						var st *sql.Stmt
						if st, err = db.PrepareContext(ctx, "INSERT INTO w(v) VALUES (?)"); err == nil {
							_, err = st.ExecContext(ctx, fmt.Sprint(g, n))
							st.Close()
						}
					default:
						var tx *sql.Tx
						if tx, err = db.BeginTx(ctx, nil); err == nil {
							if _, err = tx.ExecContext(ctx, "INSERT INTO w(v) VALUES (?)", fmt.Sprint(g, n)); err == nil {
								time.Sleep(time.Millisecond) // 持锁期间的其他工作
								err = tx.Commit()
							} else {
								tx.Rollback()
							}
						}
					}
					mu.Lock()
					switch {
					case err == nil:
						writes++
					case Classify(err) == ClassBusy:
						busy++
					default:
						other = append(other, err)
					}
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		return
	}
	writes, busy, other := write(db)
	if busy != 0 || len(other) != 0 || writes != 24*20 {
		t.Fatalf("queued writers: %d written, %d busy, other errors %v", writes, busy, other)
	}
	dsn, err := DSN(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open(DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	writes, busy, other = write(raw)
	t.Logf("without the queue: %d written, %d busy, %d other errors", writes, busy, len(other))
}

// 排队有上限且可取消，超时属于可重试的忙碌；只读事务与 VACUUM INTO 不排队；
// 提交、回滚或上下文取消后都释放写入资格。
func TestWriteQueueBoundsReleasesAndSkipsReads(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "b.db")
	db, err := Open(ctx, path, Options{WriteQueueTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE w (v TEXT NOT NULL) STRICT"); err != nil {
		t.Fatal(err)
	}
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = db.BeginTx(ctx, nil)
	if !errors.Is(err, ErrWriteQueueTimeout) || Classify(err) != ClassBusy || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("queued transaction: %v after %v", err, time.Since(start))
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO w VALUES ('x')"); !errors.Is(err, ErrWriteQueueTimeout) {
		t.Fatalf("queued statement: %v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	_, err = db.BeginTx(cctx, nil)
	cancel()
	if Classify(err) != ClassCanceled || sqliteCode(err) != errcode.Internal {
		t.Fatalf("cancelled wait: class %q, %v", Classify(err), err)
	}
	ro, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only transaction queued behind a writer: %v", err)
	}
	var n int
	if err := ro.QueryRowContext(ctx, "SELECT count(*) FROM w").Scan(&n); err != nil {
		t.Fatal(err)
	}
	ro.Rollback()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", filepath.Join(dir, "snapshot.db")); err != nil {
		t.Fatalf("VACUUM INTO queued behind a writer: %v", err)
	}
	// 另一个连接池有自己的队列，只能靠 SQLite 的 busy_timeout，得到的仍是可重试忙碌。
	other, err := Open(ctx, path, Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_, err = other.BeginTx(ctx, nil)
	if Classify(err) != ClassBusy || errors.Is(err, ErrWriteQueueTimeout) {
		t.Fatalf("driver busy: %v", err)
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("queue not released by commit: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	cctx, cancel = context.WithCancel(ctx)
	if _, err := db.BeginTx(cctx, nil); err != nil {
		t.Fatal(err)
	}
	cancel() // database/sql 在后台回滚并释放
	deadline := time.Now().Add(5 * time.Second)
	for {
		tx, err := db.BeginTx(ctx, nil)
		if err == nil {
			tx.Rollback()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue not released after context cancellation: %v", err)
		}
	}
}

func TestStructuredMapsOnlyUnmappedBusy(t *testing.T) {
	busy := fmt.Errorf("creating upload: %w", ErrWriteQueueTimeout)
	e := Structured(busy)
	if e.Code != errcode.StorageUnavailable || e.HTTPStatus() != 503 || !e.Retryable() || e.RetryAfter != time.Second ||
		len(e.Details) != 1 || e.Details[0].Reason != "database_busy" || !errors.Is(e, ErrWriteQueueTimeout) {
		t.Fatalf("busy mapping: %+v", e)
	}
	if e := Structured(errcode.Wrap(errcode.Internal, "", busy)); e.Code != errcode.StorageUnavailable {
		t.Fatalf("wrapped internal busy: %s", e.Code)
	}
	if e := Structured(errcode.Wrap(errcode.ResourceBusy, "", busy)); e.Code != errcode.ResourceBusy {
		t.Fatalf("domain code replaced: %s", e.Code)
	}
	if e := Structured(errors.New("disk exploded")); e.Code != errcode.Internal {
		t.Fatalf("plain error: %s", e.Code)
	}
	if Structured(nil) != nil {
		t.Fatal("nil error mapped")
	}
}

func sqliteCode(err error) errcode.Code { return Structured(err).Code }

// 事务外的写查询（INSERT … RETURNING）与预编译语句同样排队：有写者持锁时
// 它们在队列中等待并以队列超时结束，而不是先向 SQLite 抢锁；读查询不排队。
// 写结果集未关闭前持有写入资格。
func TestWriteQueueCoversQueriesAndPreparedStatements(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "p.db"), Options{BusyTimeout: 20 * time.Millisecond, WriteQueueTimeout: 120 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE w (id INTEGER PRIMARY KEY, v TEXT NOT NULL) STRICT"); err != nil {
		t.Fatal(err)
	}
	insert, err := db.PrepareContext(ctx, "INSERT INTO w(v) VALUES (?)")
	if err != nil {
		t.Fatal(err)
	}
	defer insert.Close()
	count, err := db.PrepareContext(ctx, "SELECT count(*) FROM w")
	if err != nil {
		t.Fatal(err)
	}
	defer count.Close()
	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	start := time.Now()
	err = db.QueryRowContext(ctx, "INSERT INTO w(v) VALUES ('r') RETURNING id").Scan(&id)
	if !errors.Is(err, ErrWriteQueueTimeout) || time.Since(start) < 120*time.Millisecond {
		t.Fatalf("INSERT RETURNING bypassed the queue: %v after %v", err, time.Since(start))
	}
	if _, err = insert.ExecContext(ctx, "p"); !errors.Is(err, ErrWriteQueueTimeout) {
		t.Fatalf("prepared statement bypassed the queue: %v", err)
	}
	var n int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM w").Scan(&n); err != nil {
		t.Fatalf("read query queued behind a writer: %v", err)
	}
	if err = count.QueryRowContext(ctx).Scan(&n); err != nil {
		t.Fatalf("prepared read queued behind a writer: %v", err)
	}
	if err = holder.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "INSERT INTO w(v) VALUES ('open') RETURNING id")
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = db.BeginTx(cctx, nil)
	cancel()
	if Classify(err) != ClassCanceled {
		t.Fatalf("an open write result set must keep the queue: %v", err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = insert.ExecContext(ctx, "after"); err != nil {
		t.Fatalf("queue not released after the result set closed: %v", err)
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM w").Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows written: %d %v", n, err)
	}
}

func TestModifiesRecognizesWriteStatements(t *testing.T) {
	for q, want := range map[string]bool{
		"INSERT INTO t VALUES (1) RETURNING id":              true,
		"  update t SET v = 1 RETURNING v":                   true,
		"-- note\nDELETE FROM t RETURNING id":                true,
		"/* note */ replace INTO t VALUES (1)":               true,
		"WITH a AS (SELECT 1) INSERT INTO t SELECT * FROM a": true,
		"SELECT * FROM t":                                    false,
		"WITH a AS (SELECT 1) SELECT * FROM a":               false,
		"PRAGMA journal_mode":                                false,
		"-- only a comment":                                  false,
		"":                                                   false,
	} {
		if modifies(q) != want {
			t.Errorf("modifies(%q) = %v", q, !want)
		}
	}
}
