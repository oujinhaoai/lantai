package operations

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// BackupDestination is only available while the caller owns maintenance. It
// resolves a source-owned record, never an arbitrary client path.
func (i *Instance) BackupDestination(ctx context.Context, id ids.ID) (string, error) {
	if err := i.gate.RequireMaintenance(ctx); err != nil {
		return "", err
	}
	if !id.Valid() {
		return "", errors.New("operations: invalid backup ID")
	}
	var rec backupRecord
	if err := readBackupJSON(i.backupRecordPath(id), &rec); err != nil {
		return "", err
	}
	if rec.Manifest.BackupID != id || rec.Manifest.InstanceID != i.InstanceID() {
		return "", errors.New("operations: backup record identity mismatch")
	}
	return independentTarget(i.layout.Home, rec.Destination)
}

// A valid archive is not necessarily this backup. Compare the complete frozen
// point with its authoritative source record before accepting or modifying a
// destination. Only copying -> complete may add a completion timestamp.
func validateBackupPoint(want, got BackupManifest) error {
	if got.State != "copying" && got.State != "complete" {
		return errors.New("operations: destination has no published backup point")
	}
	if want.State == "copying" {
		got.State, got.CompletedAt = want.State, want.CompletedAt
	}
	return equalBackupManifest(want, got)
}

func equalBackupManifest(want, got BackupManifest) error {
	w, err := canonjson.CanonicalizeValue(want)
	if err != nil {
		return err
	}
	g, err := canonjson.CanonicalizeValue(got)
	if err != nil {
		return err
	}
	if !bytes.Equal(w, g) {
		return errors.New("operations: destination differs from the recorded backup point")
	}
	return nil
}

func readDestinationManifest(dir string) (BackupManifest, error) {
	var m BackupManifest
	root, err := os.OpenRoot(dir)
	if err != nil {
		return m, err
	}
	defer root.Close()
	if err = regularPath(root, "manifest.json"); err != nil {
		return m, err
	}
	f, err := root.Open("manifest.json")
	if err != nil {
		return m, err
	}
	defer f.Close()
	_, err = decodeBackupJSON(f, &m)
	return m, err
}

// Before the source journal publishes copying, it has already persisted the
// instance, epoch, key, creation time, watermarks and CAS pin. A destination may
// supply the newly captured file/database inventory, but cannot replace any of
// those established bindings.
func validatePublishedCapture(want, got BackupManifest) error {
	want.Files, got.Files = nil, nil
	want.Matrix.Databases, got.Matrix.Databases = nil, nil
	want.State, got.State = "capturing", "capturing"
	want.CompletedAt, got.CompletedAt = time.Time{}, time.Time{}
	return equalBackupManifest(want, got)
}

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
