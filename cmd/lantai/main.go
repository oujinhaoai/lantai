// lantai 是兰台统一入口：本机实例命令、服务启动与经 REST 的薄 CLI。
// MCP、任务引擎与节点尚未启用，不注册占位命令。
//
// 退出码：0 成功；1 文档未通过校验，或实例状态不允许该操作；2 用法错误；
// 3 读写或内部错误。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/oujinhaoai/lantai/internal/cli"
)

const (
	exitOK       = 0
	exitInvalid  = 1
	exitUsage    = 2
	exitInternal = 3
)

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

var commands = []command{
	{"version", "显示版本、构建信息、公共契约与协议支持状态", runVersion},
	{"schema", "列出公共契约 schema，或校验 JSON/YAML 文档", runSchema},
	{"init", "本机初始化实例与首个管理员（口令、TOTP、恢复码），只能进行一次", runInit},
	{"migrate", "本机在维护屏障下应用待执行的数据库迁移", runMigrate},
	{"doctor", "只读诊断数据根：实例标记、五库版本、兼容矩阵与就绪原因", runDoctor},
	{"recover-admin", "本机离线重置单个管理员的口令与验证器（恢复码也丢失时）", runRecoverAdmin},
	{"backup", "共同备份、续传、取消和本机备份统计", runBackup},
	{"backup-verify", "只读校验完整备份清单、四库和文件摘要", runBackupVerify},
	{"restore", "向空目录恢复，轮换密钥并保持维护状态", runRestore},
	{"restore-complete", "核验凭据、索引和本机对账后完成恢复", runRestoreComplete},
	{"fsck", "本机只读核验领域文件、提交证明和暂存残留", runFSCK},
	{"recover", "维护屏障内按原操作和原授权分派恢复", runRecover},
	{"reindex", "从台账与领域文件重建派生查询索引", runReindex},
	{"serve", "启动真实 REST 与流式传输服务（内部监听；TLS 由外部网关负责）", runServe},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(ctx, args[1:], stdout, stderr)
		}
	}
	if slices.Contains(cli.Names(), args[0]) {
		return cli.Run(ctx, args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "lantai: unknown command %q\n\n", args[0])
	usage(stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: lantai <command> [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-14s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w, cli.Help())
	fmt.Fprintln(w)
	fmt.Fprintln(w, "run 'lantai <command> -h' for command options")
}
