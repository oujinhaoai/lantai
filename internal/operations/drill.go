package operations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

const drillFile = "recovery-drill.json"

// drillBoundary is intentionally separate from the restore receipt. It never
// asserts reconciliation, source shutdown, or knowledge after capture. Local
// administrators can replace files or binaries; this is not an OS sandbox.
type drillBoundary struct {
	Contract       string        `json:"contract"`
	Home           string        `json:"home"`
	InstanceID     ids.ID        `json:"instance_id"`
	RunID          ids.ID        `json:"restore_run_id"`
	BackupID       ids.ID        `json:"backup_id"`
	ManifestDigest digest.Digest `json:"manifest_digest"`
	RecoveryEpoch  int64         `json:"recovery_epoch"`
}

func (i *Instance) IsRecoveryDrill() bool { return i.drill }

// prepareDrillOpen runs before opening a lock, configuration, key, or database.
func (i *Instance) prepareDrillOpen(opts Options) error {
	_, err := os.Lstat(filepath.Join(i.layout.Home, drillFile))
	present := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if present && !opts.RecoveryDrill && !opts.DrillMaintenance {
		return errors.New("operations: recovery drill requires serve-drill or local administrator recovery; normal operation is forbidden")
	}
	if !present && !opts.RecoveryDrill {
		return nil
	}
	if err := ValidateDrillHome(i.layout.Home); err != nil {
		return err
	}
	i.drill = true
	i.drillServing = opts.RecoveryDrill
	return nil
}

// ValidateDrillHome requires a separate local tree with no aliases or shared
// regular-file inodes. Revalidate before serving. Concurrent hostile local
// filesystem mutation remains outside the application containment boundary.
func ValidateDrillHome(home string) error {
	abs, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	for p := abs; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("operations: drill home and ancestors must be real directories")
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	info, err := fsutil.Inspect(abs)
	if err != nil {
		return err
	}
	if !info.Known || info.Remote || info.FUSE {
		return errors.New("operations: recovery drill requires a verified local non-FUSE filesystem")
	}
	if err := drillMountBoundary(abs); err != nil {
		return err
	}
	root, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	return filepath.WalkDir(abs, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		if (!st.IsDir() && !st.Mode().IsRegular()) || !drillIndependentFile(root, st) {
			return errors.New("operations: drill tree must contain only independent regular files and directories on one local filesystem (no symlinks, hardlinks, or special files)")
		}
		return nil
	})
}

func (i *Instance) bindDrill(opts Options) error {
	if !i.drill {
		// A drill opener may have installed its boundary while this opener waited for the lock.
		if _, err := os.Lstat(filepath.Join(i.layout.Home, drillFile)); !errors.Is(err, fs.ErrNotExist) {
			return errors.New("operations: recovery drill boundary appeared; normal opening is forbidden")
		}
		return nil
	}
	// Repeat after taking the lock to catch ordinary changes during acquisition.
	if err := ValidateDrillHome(i.layout.Home); err != nil {
		return err
	}
	rel, err := filepath.Rel(i.layout.Home, i.cfg.SecretsDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("operations: recovery drill secrets must remain inside its home")
	}
	m := i.marker
	if m.Restore == nil || m.Restore.Stage != "reconciliation_required" || m.Migration != nil {
		return errors.New("operations: drill requires a restored copy with completed credential rotation and its reconciliation latch intact")
	}
	expected := drillBoundary{"lantai.recovery-drill/v1", i.layout.Home, m.InstanceID, m.Restore.RunID, m.Restore.BackupID, m.Restore.ManifestDigest, m.RecoveryEpoch}
	path := filepath.Join(i.layout.Home, drillFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if !opts.RecoveryDrill {
			return errors.New("operations: drill boundary disappeared")
		}
		raw, err = json.Marshal(expected)
		if err != nil {
			return err
		}
		return fsutil.CreateFileExclusive(path, append(raw, '\n'), 0600)
	}
	if err != nil {
		return err
	}
	if len(raw) > 4096 {
		return errors.New("operations: invalid drill boundary")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var actual drillBoundary
	if err := decoder.Decode(&actual); err != nil {
		return fmt.Errorf("operations: invalid drill boundary: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF || actual != expected {
		return errors.New("operations: drill boundary does not match home, restore run, or recovery epoch")
	}
	return nil
}
