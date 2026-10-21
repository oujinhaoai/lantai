package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/oujinhaoai/lantai/internal/firststart"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func runStart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	setup := fs.String("setup-dir", "", "私有首次设置目录")
	template := fs.String("config-template", "", "新安装模板或旧只读配置 bind 的迁移输入；已有有效配置不覆盖")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *home == "" || *setup == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: lantai start --home <data root> --setup-dir <private setup directory> [--config-template <read-only template>]")
		return exitUsage
	}
	if err := firststart.SeedConfigTemplate(*home, *template); err != nil {
		return reportStartup(stderr, "start", err)
	}
	entry, err := firststart.OpenEntry(ctx, operations.Options{Home: *home}, *setup)
	if err != nil {
		return reportStartup(stderr, "start", err)
	}
	if entry != nil {
		fmt.Fprintln(stderr, entry.Description())
		err = errors.Join(entry.Serve(ctx), entry.Close())
		if err != nil {
			return reportStartup(stderr, "start", err)
		}
	}
	session, err := firststart.Open(ctx, operations.Options{Home: *home}, *setup)
	if err != nil {
		return reportStartup(stderr, "start", err)
	}
	if session != nil {
		h, e := firststart.NewHandler(session, session.Instance.Config().HTTP.AllowedOrigins)
		if e == nil {
			fmt.Fprintln(stderr, session.Description())
			e = h.Serve(ctx, session.Instance.Config().Listen.API)
		}
		err = errors.Join(e, session.Close())
		if err != nil {
			return reportStartup(stderr, "start", err)
		}
	}
	return runServe(ctx, []string{"--home", *home}, stdout, stderr)
}

func runPrepareVolumes(_ context.Context, args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("prepare-volumes", flag.ContinueOnError)
	fs.SetOutput(stderr)
	uid := fs.Int("uid", 65532, "服务 UID")
	gid := fs.Int("gid", 65532, "服务 GID")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 3 || *uid < 1 || *gid < 1 {
		fmt.Fprintln(stderr, "usage: lantai prepare-volumes [--uid 65532 --gid 65532] <data> <secrets> <setup>")
		return exitUsage
	}
	if err := firststart.PrepareVolumes(fs.Args(), *uid, *gid); err != nil {
		fmt.Fprintln(stderr, err)
		return exitInvalid
	}
	return exitOK
}

// runPrepareConfig is a one-shot offline preflight for the upgrade launcher.
// The service UID, lock and same mounts are retained; no listeners are opened.
func runPrepareConfig(_ context.Context, args []string, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("prepare-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	template := fs.String("config-template", "", "现有部署的只读配置输入")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *home == "" || *template == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: lantai prepare-config --home <data root> --config-template <existing read-only deployment config>")
		return exitUsage
	}
	if err := firststart.SeedConfigTemplate(*home, *template); err != nil {
		return reportStartup(stderr, "prepare-config", err)
	}
	fmt.Fprintln(stderr, "configuration ready; existing instance and keys retained")
	return exitOK
}
