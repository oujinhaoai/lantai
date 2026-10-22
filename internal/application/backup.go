package application

import (
	"context"
	"errors"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"io"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
)

// Maintenance never opens a stopped application's write gate. Online callers
// retain the same shared barrier used by foreground writes and consumers.
func (a *App) Maintenance(ctx context.Context, run func(context.Context) error) error {
	if a.Instance.Gate().RequireMaintenance(ctx) == nil {
		return run(ctx)
	}
	if a.Instance.State() == operations.StateReady {
		m, err := a.Instance.Maintain(ctx, commands.ReasonMaintenance)
		if err != nil {
			return err
		}
		defer m.End()
		return run(m.Context())
	}
	return a.Instance.OfflineMaintenance(ctx, run)
}

// Backup checks owner integrity and freezes event/audit watermarks under the
// same barrier as all database and mutable-file snapshots. Confirmation is
// acknowledged only after verified COMPLETE; retries repeat the same watermark.
func (a *App) Backup(ctx context.Context, opts operations.BackupOptions) (manifest operations.BackupManifest, err error) {
	err = a.Maintenance(ctx, func(c context.Context) error {
		var e error
		manifest, e = a.backupComplete(c, opts)
		return e
	})
	return manifest, err
}

func (a *App) backupComplete(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
	keyID, err := a.Identity.RecoveryKeyID(ctx)
	if err != nil {
		return operations.BackupManifest{}, err
	}
	opts.KeyID = keyID
	opts.Check = func(c context.Context) error {
		if err := a.Identity.CheckStartup(c); err != nil {
			return err
		}
		rep, err := a.FSCK(c, true)
		if err != nil {
			return err
		}
		if err = rep.Err(); err != nil {
			return err
		}
		return a.sync(c, true)
	}
	opts.Watermarks = func(c context.Context) (map[string]int64, error) {
		m, err := a.Events.Metrics(c)
		if err != nil {
			return nil, err
		}
		return map[string]int64{"events": m.HighWater, "audit": m.AuditThrough, "pruned": m.PrunedThrough}, nil
	}
	manifest, err := a.Instance.Backup(ctx, opts)
	if err != nil {
		return manifest, err
	}
	info, err := writeCompleteBackup(ctx, a.Instance, manifest, keyID, func(w io.Writer) error { return a.Identity.WriteBackupRecoveryMaterial(ctx, w) }, opts.CheckDestination)
	if err != nil {
		return manifest, err
	}
	manifest.RecoveryPackage = &info
	through, ok := manifest.Watermarks["events"]
	if !ok {
		return manifest, errors.New("application: backup has no captured event watermark")
	}
	err = a.Maintenance(ctx, func(c context.Context) error { return a.Events.ConfirmBackup(c, through) })
	return manifest, err
}

// BackupBeforeUpgrade captures an offline, migration-pending instance without
// opening newer domain services. It has the same complete package contract.
func BackupBeforeUpgrade(ctx context.Context, inst *operations.Instance, opts operations.BackupOptions) (m operations.BackupManifest, err error) {
	err = inst.OfflineMaintenance(ctx, func(c context.Context) error {
		key, e := masterkey.Load(inst.Config().SecretsDir)
		if e != nil {
			return e
		}
		opts.KeyID = key.ID()
		m, e = inst.Backup(c, opts)
		if e != nil {
			return e
		}
		info, e := writeCompleteBackup(c, inst, m, key.ID(), key.WriteRecoveryMaterial, opts.CheckDestination)
		if e == nil {
			m.RecoveryPackage = &info
		}
		return e
	})
	return m, err
}
func writeCompleteBackup(ctx context.Context, inst *operations.Instance, m operations.BackupManifest, keyID ids.ID, writeKey func(io.Writer) error, check func(context.Context) error) (operations.RecoveryPackageInfo, error) {
	destination, err := inst.BackupDestination(ctx, m.BackupID)
	if err != nil {
		return operations.RecoveryPackageInfo{}, err
	}
	destination, err = filepath.EvalSymlinks(destination)
	if err != nil {
		return operations.RecoveryPackageInfo{}, err
	}
	return operations.WriteRecoveryPackage(ctx, destination, operations.RecoveryPackagePath(destination), keyID, writeKey, check)
}
