package sqlite

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本机文件系统上短时并发读写应全部通过，通过后不留下测试库。
func TestStressPassesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	r, err := Stress(t.Context(), dir, StressOptions{Duration: 700 * time.Millisecond, Databases: 2, Writers: 2, Readers: 4})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(r)
	t.Logf("stress report: %s", out)
	if !r.Passed() {
		t.Fatalf("stress did not pass on the local filesystem: %s", out)
	}
	if r.Inserts == 0 || r.Reads+r.Scans == 0 || r.Integrity["stress-0.db"] != "ok" || r.Integrity["stress-1.db"] != "ok" {
		t.Fatalf("expected reads, writes and an integrity result per database: %s", out)
	}
	if r.KeptDir != "" {
		t.Fatalf("passing run kept %s", r.KeptDir)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "lantai-sqlite-stress-*")); len(left) > 0 {
		t.Fatalf("passing run left %v", left)
	}
}

// 未通过时保留测试库供复查：时长短到来不及读写时，结果不能算通过。
func TestStressKeepsDatabasesWhenNotPassed(t *testing.T) {
	r, err := Stress(t.Context(), t.TempDir(), StressOptions{Duration: time.Nanosecond, Databases: 1, Writers: 1, Readers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.Passed() {
		t.Fatalf("a run without reads or writes passed: %+v", r)
	}
	if r.KeptDir == "" {
		t.Fatal("failed run must keep its databases")
	}
	if _, err := os.Stat(filepath.Join(r.KeptDir, "stress-0.db")); err != nil {
		t.Fatal(err)
	}
}

func TestStressRejectsInvalidOptions(t *testing.T) {
	for _, o := range []StressOptions{{}, {Duration: time.Second, Writers: -1}} {
		if _, err := Stress(t.Context(), t.TempDir(), o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestStressPassedRequiresCleanRunAndIntact(t *testing.T) {
	good := StressReport{Databases: 1, Inserts: 10, Reads: 10, Integrity: map[string]string{"stress-0.db": "ok"}}
	if !good.Passed() {
		t.Fatal("clean run rejected")
	}
	bad := map[string]func(*StressReport){
		"no writes":       func(r *StressReport) { r.Inserts = 0 },
		"no reads":        func(r *StressReport) { r.Reads, r.Scans = 0, 0 },
		"runtime error":   func(r *StressReport) { r.Errors = map[Class]int64{ClassCorrupt: 1} },
		"digest mismatch": func(r *StressReport) { r.DigestMismatches = 1 },
		"silent corruption": func(r *StressReport) {
			r.Integrity = map[string]string{"stress-0.db": "Tree 2 page 7 cell 1: Rowid 9 out of order"}
		},
		"missing check": func(r *StressReport) { r.Integrity = map[string]string{} },
	}
	for name, mutate := range bad {
		r := good
		r.Integrity = map[string]string{"stress-0.db": "ok"}
		mutate(&r)
		if r.Passed() {
			t.Errorf("%s accepted", name)
		}
	}
}

// 完整性检查要能发现运行中没有报错、但已写坏的库。
func TestIntegrityOfReportsDamagedDatabase(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "damaged.db")
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL); CREATE INDEX t_v ON t(v)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO t (v) VALUES (?)`, strings.Repeat("x", 100+i%50)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := integrityOf(ctx, path); got != "ok" {
		db.Close()
		t.Fatalf("intact database reported %q", got)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	// 覆盖第 3 页起的若干页（表或索引的 B 树页），保留文件头使其仍可打开。
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, 4*4096)
	for i := range junk {
		junk[i] = byte(i*7 + 3)
	}
	if _, err = f.WriteAt(junk, 2*4096); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if got := integrityOf(ctx2, path); got == "ok" {
		t.Fatal("damaged database passed integrity_check")
	} else {
		t.Logf("damaged database: %s", got)
	}
}
