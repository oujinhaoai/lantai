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
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *home == "" || *setup == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: lantai start --home <data root> --setup-dir <private setup directory>")
		return exitUsage
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
