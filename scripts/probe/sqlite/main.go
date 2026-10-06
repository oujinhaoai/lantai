// sqlite 在指定目录所在的文件系统上实测 SQLite 能力，输出 JSON 报告，
// 作为平台验证证据（WAL、同步级别、busy/取消、回滚、检查点、VACUUM INTO 快照等）。
//
//	go run ./scripts/probe/sqlite -dir <被测文件系统上的目录> [-stress 15m]
//
// -stress 另做多连接并发读写：逐行核对内容摘要，结束后用新连接做完整性检查。
// 能力项都是低并发检查，发现不了只在并发读写下出现的损坏；验证实例的数据位置时
// 必须加 -stress（见 docs/deployment.md）。未通过时保留测试库并在报告中给出目录。
//
// 报告只描述当前机器与文件系统；在其他平台或挂载方式上必须重新运行。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

func main() {
	dir := flag.String("dir", os.TempDir(), "在该目录下创建临时数据库（应位于被测文件系统）")
	stress := flag.Duration("stress", 0, "并发读写实测时长，0 为不做；验证数据位置时至少 15m")
	dbs := flag.Int("stress-dbs", 2, "并发实测的库个数")
	writers := flag.Int("stress-writers", 8, "并发实测每库写协程数")
	readers := flag.Int("stress-readers", 16, "并发实测每库读协程数")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := sqlite.Probe(ctx, *dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(3)
	}
	missing := r.Required()
	var sr *sqlite.StressReport
	if *stress > 0 {
		fmt.Fprintf(os.Stderr, "probe: concurrent read/write for %v\n", *stress)
		s, err := sqlite.Stress(context.Background(), *dir, sqlite.StressOptions{Duration: *stress, Databases: *dbs, Writers: *writers, Readers: *readers,
			Progress: func(p sqlite.StressReport) {
				fmt.Fprintf(os.Stderr, "probe: inserts=%d reads=%d scans=%d errors=%v digest_mismatches=%d\n", p.Inserts, p.Reads, p.Scans, p.Errors, p.DigestMismatches)
			}})
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe: stress:", err)
			os.Exit(3)
		}
		sr = &s
		if !s.Passed() {
			missing = append(missing, "concurrent read/write stress")
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		sqlite.Report
		Stress             *sqlite.StressReport `json:"stress,omitempty"`
		ConcurrencyChecked bool                 `json:"concurrency_checked"`
		Missing            []string             `json:"missing_required"`
	}{r, sr, sr != nil, missing}); err != nil {
		os.Exit(3)
	}
	if len(missing) > 0 {
		os.Exit(1)
	}
}
