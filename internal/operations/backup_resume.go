package operations

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// ResolveBackupID identifies an online job's existing point from the source
// journal. The destination manifest can be absent during a durable capture
// reset, so it is not the authority for finding the record. This method never
// creates a backup, adopts an orphan manifest, or changes any recorded point.
func (i *Instance) ResolveBackupID(ctx context.Context, id ids.ID, destination string) (ids.ID, error) {
	release, err := i.lockBackup(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	target, err := independentTarget(i.layout.Home, destination)
	if err != nil {
		return "", err
	}
	if id != "" {
		if !id.Valid() {
			return "", errors.New("operations: invalid backup ID")
		}
		_, err := i.readBoundBackupRecord(id, target)
		return id, err
	}
	entries, err := os.ReadDir(filepath.Join(i.layout.Home, "backups", "records"))
	if err != nil {
		return "", err
	}
	var found ids.ID
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		candidate := ids.ID(strings.TrimSuffix(entry.Name(), ".json"))
		if !candidate.Valid() {
			return "", errors.New("operations: invalid backup record filename")
		}
		var rec backupRecord
		if err = readBackupJSON(i.backupRecordPath(candidate), &rec); err != nil {
			return "", err
		}
		if rec.Destination != target {
			continue
		}
		if _, err = i.readBoundBackupRecord(candidate, target); err != nil {
			return "", err
		}
		if found != "" {
			return "", errors.New("operations: multiple backup records claim the destination")
		}
		found = candidate
	}
	if found == "" {
		return "", fs.ErrNotExist
	}
	return found, nil
}

func (i *Instance) readBoundBackupRecord(id ids.ID, target string) (backupRecord, error) {
	var rec backupRecord
	if err := readBackupJSON(i.backupRecordPath(id), &rec); err != nil {
		return rec, err
	}
	if rec.Manifest.BackupID != id || rec.Manifest.Contract != BackupContract || !rec.Manifest.KeyID.Valid() {
		return rec, errors.New("operations: backup record identity mismatch")
	}
	if rec.Manifest.InstanceID != "" && rec.Manifest.InstanceID != i.InstanceID() {
		return rec, errors.New("operations: foreign backup record")
	}
	if target != "" && rec.Destination != target {
		return rec, errors.New("operations: resume destination differs from the original backup")
	}
	if !filepath.IsAbs(rec.Destination) || filepath.Clean(rec.Destination) != rec.Destination {
		return rec, errors.New("operations: unsafe recorded backup destination")
	}
	if _, err := independentTarget(i.layout.Home, rec.Destination); err != nil {
		return rec, err
	}
	switch rec.Manifest.State {
	case "capturing", "copying", "complete":
	default:
		return rec, errors.New("operations: backup is not resumable")
	}
	return rec, nil
}

// A reset may move only the same uncommitted capture. In particular it cannot
// retire a copying/complete point or follow a substituted directory/manifest.
// The initial capturing manifest precedes inventory and may have no instance ID;
// any nonempty identity must agree with the bound source record.
func validateResetCapture(dir string, want BackupManifest) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("operations: unsafe capture reset directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = regularPath(root, "manifest.json"); err != nil {
		return err
	}
	f, err := root.Open("manifest.json")
	if err != nil {
		return err
	}
	defer f.Close()
	var got BackupManifest
	if _, err = decodeBackupJSON(f, &got); err != nil {
		return err
	}
	if got.Contract != want.Contract || got.CreatedAt.IsZero() || got.BackupID != want.BackupID || got.KeyID != want.KeyID || got.State != "capturing" || (got.InstanceID != "" && got.InstanceID != want.InstanceID) {
		return errors.New("operations: capture reset manifest identity or state mismatch")
	}
	return nil
}
