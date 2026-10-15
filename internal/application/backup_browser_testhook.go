//go:build lantai_test_hooks

package application

// This file is compiled only into explicitly tagged test binaries. The release
// build has no copy latch, external fault switch, or corresponding HTTP route.
import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/operations"
)

// InstallBackupCopyHookForTest observes the real persisted copying phase. It
// preserves the normal destination checks, snapshotter and consistency guard.
func (a *App) InstallBackupCopyHookForTest(hook func(context.Context)) error {
	b := a.backupControl
	if b == nil || hook == nil {
		return errors.New("test copy hook requires a configured backup controller")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != "" {
		return errors.New("test copy hook must be installed before a backup starts")
	}
	run := b.run
	b.run = func(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
		check := opts.CheckDestination
		observed := false
		opts.CheckDestination = func(c context.Context) error {
			if check != nil {
				if err := check(c); err != nil {
					return err
				}
			}
			if !observed {
				raw, err := os.ReadFile(filepath.Join(opts.Destination, "manifest.json"))
				var manifest operations.BackupManifest
				if err == nil && json.Unmarshal(raw, &manifest) == nil && manifest.State == "copying" {
					observed = true
					hook(c)
				}
			}
			return nil
		}
		return run(ctx, opts)
	}
	return nil
}
