package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	merged := fs.Bool("merged", false, "开发时合并 API 与传输监听")
	api := fs.String("api-listen", "", "覆盖内部 API 地址")
	xfer := fs.String("transfer-listen", "", "覆盖内部传输地址")
	ops := fs.String("operations-listen", "", "覆盖运维地址（仅数字 loopback）")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *home == "" || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: lantai serve -home <data root> [-merged]")
		return exitUsage
	}
	a, err := application.Open(ctx, application.Options{Instance: operations.Options{Home: *home}, Diagnostics: stderr})
	if err != nil {
		return reportStartup(stderr, "serve", err)
	}
	cfg := a.Instance.Config()
	if *merged {
		cfg.Listen.Merged = true
	}
	if *api != "" {
		cfg.Listen.API = *api
	}
	if *xfer != "" {
		cfg.Listen.Transfer = *xfer
	}
	if *ops != "" {
		cfg.Listen.Operations = *ops
	}
	s, err := a.StartHTTP(ctx, cfg, application.WithAccessLog(stderr))
	if err == nil {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"ready": true, "instance_id": a.Instance.InstanceID(), "listeners": s.Addresses})
		err = s.Wait(ctx)
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = errors.Join(err, s.Shutdown(shutdown))
		cancel()
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = errors.Join(err, a.Close(closeCtx))
	if err != nil {
		return reportStartup(stderr, "serve", err)
	}
	return exitOK
}
