package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// StressOptions 控制并发读写实测：每个库由多个写者和读者在同一进程内同时操作，
// 与兰台运行时一样走 Open 的打开参数和进程内写入排队。
type StressOptions struct {
	Duration  time.Duration // 运行时长，必须大于 0
	Databases int           // 库个数，默认 2
	Writers   int           // 每库写协程数，默认 8
	Readers   int           // 每库读协程数，默认 16
	Payload   int           // 每行字节数，默认 1500
	// Progress 非空时约每 30 秒收到一次计数快照（不含完整性结果）。
	Progress func(StressReport)
}

// StressReport 是并发读写实测的观测结果；是否满足兰台要求由 Passed 判定。
type StressReport struct {
	Duration         time.Duration     `json:"duration_ns"`
	Databases        int               `json:"databases"`
	Writers          int               `json:"writers_per_database"`
	Readers          int               `json:"readers_per_database"`
	Payload          int               `json:"payload_bytes"`
	Inserts          int64             `json:"inserts"`
	Updates          int64             `json:"updates"`
	Reads            int64             `json:"point_reads"`
	Scans            int64             `json:"range_scans"`
	DigestMismatches int64             `json:"digest_mismatches"`
	Errors           map[Class]int64   `json:"errors,omitempty"`
	ErrorSamples     []string          `json:"error_samples,omitempty"`
	Integrity        map[string]string `json:"integrity_check"`
	// KeptDir 是未通过时保留的测试库目录，供复查；通过时目录已删除。
	KeptDir string `json:"kept_dir,omitempty"`
}

// Passed 要求有实际读写、运行中没有任何错误与摘要不一致，且结束后每个库的
// integrity_check 都是 ok。只看运行中的错误不够：在不满足 SQLite 一致性要求的
// 文件系统上，库可能被静默写坏，只有完整性检查能发现。
func (r StressReport) Passed() bool {
	if r.Inserts == 0 || r.Reads+r.Scans == 0 || len(r.Errors) > 0 || r.DigestMismatches > 0 || len(r.Integrity) != r.Databases {
		return false
	}
	for _, v := range r.Integrity {
		if v != "ok" {
			return false
		}
	}
	return true
}

const stressSamplesPerClass = 3

type stressRun struct {
	o        StressOptions
	inserts  atomic.Int64
	updates  atomic.Int64
	reads    atomic.Int64
	scans    atomic.Int64
	mismatch atomic.Int64
	mu       sync.Mutex
	errs     map[Class]int64
	samples  map[Class][]string
}

func (s *stressRun) fail(db, op string, err error) {
	c := Classify(err)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[c]++
	if len(s.samples[c]) < stressSamplesPerClass {
		s.samples[c] = append(s.samples[c], fmt.Sprintf("%s %s %s: %v", time.Now().UTC().Format("15:04:05.000"), db, op, err))
	}
}

func (s *stressRun) snapshot() StressReport {
	r := StressReport{Duration: s.o.Duration, Databases: s.o.Databases, Writers: s.o.Writers, Readers: s.o.Readers, Payload: s.o.Payload,
		Inserts: s.inserts.Load(), Updates: s.updates.Load(), Reads: s.reads.Load(), Scans: s.scans.Load(), DigestMismatches: s.mismatch.Load()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) > 0 {
		r.Errors = make(map[Class]int64, len(s.errs))
		for c, n := range s.errs {
			r.Errors[c] = n
		}
		classes := make([]string, 0, len(s.samples))
		for c := range s.samples {
			classes = append(classes, string(c))
		}
		sort.Strings(classes)
		for _, c := range classes {
			r.ErrorSamples = append(r.ErrorSamples, s.samples[Class(c)]...)
		}
	}
	return r
}

// Stress 在 dir 下新建若干测试库，按 o 并发读写 o.Duration，再逐库用新的只读
// 连接做 integrity_check。读者逐行核对内容摘要；dir 应位于要验证的文件系统上。
// 未通过时保留测试库目录（见 KeptDir）。返回的错误只表示无法开始实测。
func Stress(ctx context.Context, dir string, o StressOptions) (StressReport, error) {
	if o.Duration <= 0 {
		return StressReport{}, errors.New("sqlite: stress duration must be positive")
	}
	o.Databases = defaultInt(o.Databases, 2)
	o.Writers = defaultInt(o.Writers, 8)
	o.Readers = defaultInt(o.Readers, 16)
	o.Payload = defaultInt(o.Payload, 1500)
	if o.Databases < 1 || o.Writers < 1 || o.Readers < 1 || o.Payload < 1 {
		return StressReport{}, errors.New("sqlite: stress counts must be positive")
	}
	work, err := os.MkdirTemp(dir, "lantai-sqlite-stress-")
	if err != nil {
		return StressReport{}, err
	}
	s := &stressRun{o: o, errs: map[Class]int64{}, samples: map[Class][]string{}}
	paths := make([]string, o.Databases)
	dbs := make([]*sql.DB, 0, o.Databases)
	closeAll := func() {
		for _, db := range dbs {
			_ = db.Close()
		}
	}
	for i := range paths {
		paths[i] = filepath.Join(work, fmt.Sprintf("stress-%d.db", i))
		db, err := Open(ctx, paths[i], Options{})
		if err == nil {
			_, err = db.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, k TEXT NOT NULL UNIQUE, v BLOB NOT NULL, h TEXT NOT NULL, n INTEGER NOT NULL DEFAULT 0) STRICT`)
			dbs = append(dbs, db)
		}
		if err != nil {
			closeAll()
			_ = os.RemoveAll(work)
			return StressReport{}, err
		}
	}

	run, cancel := context.WithTimeout(ctx, o.Duration)
	defer cancel()
	var wg sync.WaitGroup
	for i, db := range dbs {
		name := filepath.Base(paths[i])
		var maxID atomic.Int64
		for range o.Writers {
			wg.Go(func() { s.write(run, db, name, &maxID) })
		}
		for r := range o.Readers {
			// 每 8 个读者中有 1 个做较长的只读事务范围扫描，其余做单行读取。
			wg.Go(func() { s.read(run, db, name, &maxID, r%8 == 0) })
		}
	}
	stopProgress := make(chan struct{})
	if o.Progress != nil {
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					o.Progress(s.snapshot())
				case <-stopProgress:
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stopProgress)
	closeAll()
	if err := ctx.Err(); err != nil {
		_ = os.RemoveAll(work)
		return s.snapshot(), err
	}

	r := s.snapshot()
	r.Integrity = make(map[string]string, len(paths))
	for _, p := range paths {
		r.Integrity[filepath.Base(p)] = integrityOf(ctx, p)
	}
	if r.Passed() {
		_ = os.RemoveAll(work)
	} else {
		r.KeptDir = work
	}
	return r, nil
}

func defaultInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// write 在一个事务里插入一行随机内容及其摘要，四次中约有一次顺带更新一个旧行。
func (s *stressRun) write(ctx context.Context, db *sql.DB, name string, maxID *atomic.Int64) {
	buf := make([]byte, s.o.Payload)
	for ctx.Err() == nil {
		_, _ = rand.Read(buf)
		sum := sha256.Sum256(buf)
		h := hex.EncodeToString(sum[:])
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			if ctx.Err() == nil {
				s.fail(name, "begin", err)
			}
			continue
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO t (k, v, h) VALUES (?, ?, ?)`, h[:32], buf, h)
		updated := false
		if m := maxID.Load(); err == nil && m > 0 && mrand.IntN(4) == 0 {
			_, err = tx.ExecContext(ctx, `UPDATE t SET n = n + 1 WHERE id = ?`, 1+mrand.Int64N(m))
			updated = err == nil
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			if ctx.Err() == nil {
				s.fail(name, "write", err)
			}
			continue
		}
		if id, err := res.LastInsertId(); err == nil {
			for {
				cur := maxID.Load()
				if id <= cur || maxID.CompareAndSwap(cur, id) {
					break
				}
			}
		}
		s.inserts.Add(1)
		if updated {
			s.updates.Add(1)
		}
	}
}

// read 随机读取已提交的行并核对摘要；scan 为真时在只读事务内连续扫描一段并计数。
func (s *stressRun) read(ctx context.Context, db *sql.DB, name string, maxID *atomic.Int64, scan bool) {
	for ctx.Err() == nil {
		m := maxID.Load()
		if m == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		from := 1 + mrand.Int64N(m)
		if scan {
			if err := s.scan(ctx, db, from); err != nil {
				if ctx.Err() == nil {
					s.fail(name, "scan", err)
				}
				continue
			}
			s.scans.Add(1)
			continue
		}
		var v []byte
		var h string
		err := db.QueryRowContext(ctx, `SELECT v, h FROM t WHERE id = ?`, from).Scan(&v, &h)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			if ctx.Err() == nil {
				s.fail(name, "read", err)
			}
			continue
		}
		if err == nil {
			s.check(v, h)
		}
		s.reads.Add(1)
		// 读者偶尔停顿，让检查点有机会重置 WAL。
		time.Sleep(time.Duration(mrand.Int64N(int64(time.Millisecond))))
	}
}

func (s *stressRun) scan(ctx context.Context, db *sql.DB, from int64) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT v, h FROM t WHERE id >= ? ORDER BY id LIMIT 200`, from)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v []byte
		var h string
		if err = rows.Scan(&v, &h); err != nil {
			break
		}
		s.check(v, h)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var n int64
	return tx.QueryRowContext(ctx, `SELECT count(*) FROM t WHERE id <= ?`, from).Scan(&n)
}

func (s *stressRun) check(v []byte, h string) {
	sum := sha256.Sum256(v)
	if hex.EncodeToString(sum[:]) != h {
		s.mismatch.Add(1)
	}
}

// integrityOf 用新的只读连接检查一个库，返回 "ok" 或问题摘要（最多前 5 行）。
func integrityOf(ctx context.Context, path string) string {
	db, err := Open(ctx, path, Options{ReadOnly: true})
	if err != nil {
		return "open: " + err.Error()
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return "integrity_check: " + err.Error()
	}
	defer rows.Close()
	var lines []string
	for rows.Next() && len(lines) < 5 {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "integrity_check: " + err.Error()
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return "integrity_check: " + err.Error()
	}
	if len(lines) == 0 {
		return "integrity_check: no result"
	}
	return strings.Join(lines, "; ")
}
