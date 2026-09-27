package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func outputJSON(w io.Writer, v any) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	destination := fs.String("destination", "", "独立的空备份目录")
	resume := fs.String("resume", "", "继续同一 backup_id")
	cancel := fs.String("cancel", "", "取消已停止复制的 backup_id，保留中断文件")
	status := fs.Bool("status", false, "显示本机备份统计")
	upgrade := fs.Bool("before-upgrade", false, "为等待升级的旧 schema 捕获四库/文件共同点，不运行新 schema 领域检查")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	modes := 0
	for _, v := range []bool{*destination != "", *resume != "", *cancel != "", *status} {
		if v {
			modes++
		}
	}
	if *home == "" || fs.NArg() != 0 || modes != 1 {
		fmt.Fprintln(stderr, "usage: lantai backup -home <root> (-destination <empty directory> | -resume <backup_id> | -cancel <backup_id> | -status) [-before-upgrade]")
		return exitUsage
	}
	if *upgrade || *status || *cancel != "" {
		inst, err := operations.Open(ctx, operations.Options{Home: *home})
		if err != nil {
			return reportStartup(stderr, "backup", err)
		}
		defer inst.Close(context.Background())
		if *status {
			v, err := inst.BackupStats(ctx)
			if err != nil {
				return reportStartup(stderr, "backup", err)
			}
			if outputJSON(stdout, v) != nil {
				return exitInternal
			}
			return exitOK
		}
		if *cancel != "" {
			if err = inst.CancelBackup(ctx, ids.ID(*cancel)); err != nil {
				return reportStartup(stderr, "backup", err)
			}
			fmt.Fprintln(stdout, "cancelled; partial artifacts retained")
			return exitOK
		}
		pending := false
		for _, r := range inst.Readiness().Reasons {
			if r.Code == operations.CodeMigrationRequired {
				pending = true
			}
		}
		if !pending || inst.Marker().Migration != nil {
			fmt.Fprintln(stderr, "lantai backup: -before-upgrade requires a stopped instance awaiting a new migration")
			return exitInvalid
		}
		key, err := masterkey.Load(inst.Config().SecretsDir)
		if err != nil {
			return reportStartup(stderr, "backup", err)
		}
		m, err := inst.Backup(ctx, operations.BackupOptions{Destination: *destination, BackupID: ids.ID(*resume), KeyID: key.ID()})
		outputErr := outputJSON(stdout, m)
		if err != nil {
			return reportStartup(stderr, "backup", err)
		}
		if outputErr != nil {
			fmt.Fprintln(stderr, "lantai backup: state is durable but result output failed:", outputErr)
			return exitInternal
		}
		return exitOK
	}
	a, err := application.OpenOffline(ctx, application.Options{Instance: operations.Options{Home: *home}})
	if err != nil {
		return reportStartup(stderr, "backup", err)
	}
	defer a.Close(context.Background())
	m, err := a.Backup(ctx, operations.BackupOptions{Destination: *destination, BackupID: ids.ID(*resume)})
	outputErr := outputJSON(stdout, m)
	if err != nil {
		return reportStartup(stderr, "backup", err)
	}
	if outputErr != nil {
		fmt.Fprintln(stderr, "lantai backup: state is durable but result output failed:", outputErr)
		return exitInternal
	}
	return exitOK
}
func runBackupVerify(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup-verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("backup", "", "完整备份目录")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *dir == "" || fs.NArg() != 0 {
		return exitUsage
	}
	m, err := operations.VerifyBackup(ctx, *dir)
	if err != nil {
		return reportStartup(stderr, "backup-verify", err)
	}
	if outputJSON(stdout, m) != nil {
		return exitInternal
	}
	return exitOK
}
func runRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	dir := fs.String("backup", "", "完整备份目录")
	keyDir := fs.String("key-dir", "", "单独恢复的原主密钥目录")
	minimum := fs.Int64("minimum-epoch", 0, "已知旧实例的最高 recovery_epoch，新实例必须高于它")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *home == "" || *dir == "" || *keyDir == "" || *minimum < 0 || fs.NArg() != 0 {
		return exitUsage
	}
	key, err := masterkey.Load(*keyDir)
	if err != nil {
		return reportStartup(stderr, "restore", err)
	}
	marker, err := application.Restore(ctx, operations.RestoreOptions{Home: *home, BackupDir: *dir, MinimumEpoch: *minimum}, key, identity.Config{})
	outputErr := outputJSON(stdout, marker)
	if err != nil {
		return reportStartup(stderr, "restore", err)
	}
	if outputErr != nil {
		fmt.Fprintln(stderr, "lantai restore: state is durable but result output failed:", outputErr)
		return exitInternal
	}
	fmt.Fprintln(stderr, "Restored files and revoked old credentials. Service remains closed. Run recover-admin, reconcile newer revocations/deletions/open operations, then restore-complete with the bound review and its evidence.")
	return exitOK
}
func runRestoreComplete(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore-complete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	file := fs.String("review", "", "符合 lantai.recovery-review/v1 的本机对账 JSON")
	proof := fs.String("evidence", "", "review.evidence_digest 对应的对账报告文件")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *home == "" || *file == "" || *proof == "" || fs.NArg() != 0 {
		return exitUsage
	}
	raw, err := application.ReadRecoveryEvidence(*file)
	if err != nil {
		return reportStartup(stderr, "restore-complete", err)
	}
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return reportStartup(stderr, "restore-complete", err)
	}
	reg, err := schema.Default()
	if err != nil {
		return reportStartup(stderr, "restore-complete", err)
	}
	if err = reg.Validate("lantai.recovery-review/v1", doc); err != nil {
		return reportStartup(stderr, "restore-complete", err)
	}
	var review operations.RecoveryReview
	if err = json.Unmarshal(raw, &review); err != nil {
		return reportStartup(stderr, "restore-complete", err)
	}
	evidence, err := application.ReadRecoveryEvidence(*proof)
	if err != nil {
		return reportStartup(stderr, "restore-complete", err)
	}
	a, err := application.OpenOffline(ctx, application.Options{Instance: operations.Options{Home: *home}})
	if err != nil {
		return reportStartup(stderr, "restore-complete", err)
	}
	defer a.Close(context.Background())
	if err = a.CompleteRestore(ctx, review, evidence); err != nil {
		return reportStartup(stderr, "restore-complete", err)
	}
	fmt.Fprintln(stdout, "restore complete; the instance may now be started")
	return exitOK
}
func runFSCK(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runMaintenance(ctx, "fsck", args, stdout, stderr)
}
func runRecover(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runMaintenance(ctx, "recover", args, stdout, stderr)
}
func runReindex(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runMaintenance(ctx, "reindex", args, stdout, stderr)
}
func runMaintenance(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *home == "" || fs.NArg() != 0 {
		return exitUsage
	}
	a, err := application.OpenOffline(ctx, application.Options{Instance: operations.Options{Home: *home}})
	if err != nil {
		return reportStartup(stderr, name, err)
	}
	defer a.Close(context.Background())
	err = a.Maintenance(ctx, func(c context.Context) error {
		switch name {
		case "fsck":
			r, e := a.FSCK(c, true)
			if e != nil {
				return e
			}
			if e = outputJSON(stdout, r); e != nil {
				return e
			}
			return r.Err()
		case "recover":
			r, e := a.Recover(c)
			if w := outputJSON(stdout, r); w != nil {
				return w
			}
			return e
		case "reindex":
			r, e := a.Query.Rebuild(c)
			if e != nil {
				return e
			}
			return outputJSON(stdout, r)
		}
		return os.ErrInvalid
	})
	if err != nil {
		return reportStartup(stderr, name, err)
	}
	return exitOK
}
