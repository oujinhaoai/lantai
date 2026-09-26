package sqlite

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeMeetsRequirements(t *testing.T) {
	r, err := Probe(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.MarshalIndent(r, "", "  ")
	t.Logf("sqlite capability report:\n%s", out)
	if missing := r.Required(); len(missing) > 0 {
		t.Fatalf("platform lacks required capabilities: %v (problems: %v)", missing, r.Problems)
	}
	if r.BusyWait < 150*time.Millisecond {
		t.Errorf("busy error returned after %v, before the configured timeout", r.BusyWait)
	}
	if r.CancelLatency > 5*time.Second {
		t.Errorf("cancellation took %v", r.CancelLatency)
	}
}

func TestDSNRejectsQueryInPath(t *testing.T) {
	if _, err := DSN("a?b.db", Options{}); err == nil {
		t.Fatal("path with '?' must be rejected")
	}
	if _, err := DSN("", Options{}); err == nil {
		t.Fatal("empty path must be rejected")
	}
	ro, err := DSN("x.db", Options{ReadOnly: true})
	if err != nil || !strings.Contains(ro, "_query_only=1") || strings.Contains(ro, "_txlock") || strings.Contains(ro, "_journal_mode") {
		t.Fatalf("read-only dsn = %s, %v", ro, err)
	}
	rw, err := DSN("x.db", Options{})
	if err != nil || !strings.Contains(rw, "_txlock=immediate") || !strings.Contains(rw, "_journal_mode=WAL") || !strings.Contains(rw, "_synchronous=FULL") {
		t.Fatalf("writable dsn = %s, %v", rw, err)
	}
}

// 只读打开用于核验快照与备份，不能改动或创建被核验的文件。
func TestReadOnlyOpenDoesNotModify(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if _, err := Open(ctx, missing, Options{ReadOnly: true}); err == nil {
		t.Fatal("read-only open of a missing file must fail")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("read-only open created the file")
	}

	// 准备一个回滚日志模式（非 WAL）的库，模拟 VACUUM INTO 快照或旧备份。
	path := filepath.Join(dir, "rollback.db")
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (v TEXT NOT NULL) STRICT; INSERT INTO t VALUES ('a');"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(path)

	ro, err := Open(ctx, path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ro.BeginTx(ctx, nil) // 只读连接上默认事务也能用于读取
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil || n != 1 {
		t.Fatalf("read: %d %v", n, err)
	}
	tx.Rollback()
	if _, err := ro.ExecContext(ctx, "INSERT INTO t VALUES ('b')"); err == nil {
		t.Fatal("write through a read-only connection succeeded")
	}
	ro.Close()
	after, _ := os.ReadFile(path)
	if len(before) < 20 || before[18] != after[18] || before[19] != after[19] {
		t.Fatalf("journal mode bytes changed: %v -> %v", before[18:20], after[18:20])
	}
	if string(before) != string(after) {
		t.Fatal("read-only open modified the database file")
	}
}

func TestClassify(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "c.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE u (k TEXT PRIMARY KEY) STRICT"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO u VALUES ('a')"); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO u VALUES ('a')")
	if Classify(err) != ClassConstraint || !IsUniqueViolation(err) {
		t.Fatalf("duplicate key: class %q unique=%v err=%v", Classify(err), IsUniqueViolation(err), err)
	}
	if Classify(context.Canceled) != ClassCanceled || Classify(errors.New("x")) != ClassOther || Classify(nil) != ClassNone {
		t.Fatal("generic classification")
	}
	ro, err := Open(ctx, filepath.Join(t.TempDir(), "c.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	ro.Close()
}

// 子进程在写事务中被强杀：已提交数据保留，未提交数据不出现，库完整。
func TestCrashDuringWriteTransaction(t *testing.T) {
	if os.Getenv(crashEnv) != "" {
		t.Skip("running as crash helper")
	}
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "crash.db")
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL) STRICT"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.v")
	cmd.Env = append(os.Environ(), crashEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "CRASH-HELPER-READY") {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatal("helper exited before signalling readiness")
		}
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("helper did not become ready")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	db, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity after crash: %q %v", integrity, err)
	}
	var committed, uncommitted int
	db.QueryRowContext(ctx, "SELECT count(*) FROM t WHERE v = 'committed'").Scan(&committed)
	db.QueryRowContext(ctx, "SELECT count(*) FROM t WHERE v = 'uncommitted'").Scan(&uncommitted)
	if committed != 100 || uncommitted != 0 {
		t.Fatalf("after crash: committed=%d uncommitted=%d", committed, uncommitted)
	}
}

const crashEnv = "LANTAI_SQLITE_CRASH_DB"

// TestCrashHelper 只在子进程中运行：提交 100 行，再开启不提交的写事务后等待被杀。
func TestCrashHelper(t *testing.T) {
	path := os.Getenv(crashEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	ctx := context.Background()
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if _, err := db.ExecContext(ctx, "INSERT INTO t(v) VALUES ('committed')"); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		if _, err := tx.ExecContext(ctx, "INSERT INTO t(v) VALUES ('uncommitted')"); err != nil {
			t.Fatal(err)
		}
	}
	os.Stdout.WriteString("CRASH-HELPER-READY\n")
	time.Sleep(10 * time.Minute)
}
