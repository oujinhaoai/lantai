// lantai 是兰台的统一入口。当前构建只包含已实现的子命令：version、schema，
// 以及只能在服务端本机运行的实例命令 init、migrate、doctor、recover-admin；
// 服务端、CLI 业务命令、MCP 与节点子命令在对应模块实现后再接入，这里不预留空命令。
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
	"syscall"
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
	fmt.Fprintln(w)
	fmt.Fprintln(w, "run 'lantai <command> -h' for command options")
}
