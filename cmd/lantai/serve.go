package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return serveMode(ctx, args, stdout, stderr, false)
}

func runServeDrill(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return serveMode(ctx, args, stdout, stderr, true)
}

func serveMode(ctx context.Context, args []string, stdout, stderr io.Writer, drill bool) int {
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
	a, err := application.Open(ctx, application.Options{Instance: operations.Options{Home: *home, RecoveryDrill: drill}, Diagnostics: stderr})
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
	// Backup storage is optional. Keep the administrator-facing controller
	// available to explain configuration errors without taking business service
	// offline when a removable backup destination is absent.
	a.ConfigureBackups(ctx, os.Getenv("LANTAI_BACKUP_ROOT"), os.Getenv("LANTAI_BACKUP_ROOT_ID"))
	s, err := a.StartHTTP(ctx, cfg, application.WithAccessLog(stderr))
	if err == nil {
		status := map[string]any{"ready": true, "instance_id": a.Instance.InstanceID(), "listeners": s.Addresses}
		if drill {
			status["recovery_drill"] = true
			status["production_ready"] = false
			status["unknown_after_capture"] = true
		}
		_ = json.NewEncoder(stdout).Encode(status)
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
