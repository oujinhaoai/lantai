package application

import (
	"context"
	"errors"

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
func (a *App) Backup(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
	key, err := masterkey.Load(a.Instance.Config().SecretsDir)
	if err != nil {
		return operations.BackupManifest{}, err
	}
	opts.KeyID = key.ID()
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
	through, ok := manifest.Watermarks["events"]
	if !ok {
		return manifest, errors.New("application: backup has no captured event watermark")
	}
	err = a.Maintenance(ctx, func(c context.Context) error { return a.Events.ConfirmBackup(c, through) })
	return manifest, err
}
