// sqlite 在指定目录所在的文件系统上实测 SQLite 能力，输出 JSON 报告，
// 作为平台验证证据（WAL、同步级别、busy/取消、回滚、检查点、VACUUM INTO 快照等）。
//
//	go run ./scripts/probe/sqlite -dir <被测文件系统上的目录>
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
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := sqlite.Probe(ctx, *dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(3)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		sqlite.Report
		Missing []string `json:"missing_required"`
	}{r, r.Required()}); err != nil {
		os.Exit(3)
	}
	if len(r.Required()) > 0 {
		os.Exit(1)
	}
}
