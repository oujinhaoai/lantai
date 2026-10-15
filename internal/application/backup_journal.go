package application

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

// atomicBackupJournalTemp recognizes only the exact WriteFileAtomic protocol:
// .<canonical job ID>.json.tmp-<CreateTemp's decimal uint32 suffix>.
// A temporary file is never an authoritative job, even if it contains valid JSON.
func atomicBackupJournalTemp(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	id, suffix, ok := strings.Cut(strings.TrimPrefix(name, "."), ".json.tmp-")
	if !ok || !ids.ID(id).Valid() || len(suffix) == 0 || len(suffix) > 10 {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(suffix, 10, 32)
	return err == nil && strconv.FormatUint(n, 10) == suffix
}

// recoverJournalTemp preserves interrupted atomic writes outside the journal
// scan. Unknown entries and unsafe file types still fail closed. Quarantine is
// same-filesystem rename; no candidate bytes are adopted into a formal log.
func (b *BackupControl) recoverJournalTemp(entry fs.DirEntry) (bool, error) {
	if !atomicBackupJournalTemp(entry.Name()) {
		return false, nil
	}
	source := filepath.Join(b.journalDir(), entry.Name())
	st, err := os.Lstat(source)
	if err != nil {
		return true, err
	}
	if !st.Mode().IsRegular() {
		return true, errors.New("unsafe backup journal temporary file")
	}
	if err = fsutil.CheckPrivate(source); err != nil {
		return true, err
	}
	if (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && !backupOwner(st) {
		return true, errors.New("backup journal temporary file has a foreign owner")
	}
	quarantine := filepath.Join(b.app.Instance.Layout().Home, "backups", "online-interrupted")
	if err = os.Mkdir(quarantine, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return true, err
	}
	st, err = os.Lstat(quarantine)
	if err != nil {
		return true, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return true, errors.New("unsafe backup journal quarantine")
	}
	if err = fsutil.CheckPrivate(quarantine); err != nil {
		return true, err
	}
	if (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && !backupOwner(st) {
		return true, errors.New("backup journal quarantine has a foreign owner")
	}
	// Persist the new quarantine directory in its parent before removing the
	// source name. Otherwise a second crash could lose both directory entries.
	if err = fsutil.SyncDir(filepath.Dir(quarantine)); err != nil {
		return true, err
	}
	// The interrupted writer may have died before flushing its bytes. Preserve
	// the bytes that remain readable before moving their directory entry.
	flags := os.O_RDONLY
	if runtime.GOOS == "windows" {
		flags = os.O_RDWR
	} // FlushFileBuffers requires a writable handle.
	orphan, err := os.OpenFile(source, flags, 0)
	if err != nil {
		return true, err
	}
	err = errors.Join(orphan.Sync(), orphan.Close())
	if err != nil {
		return true, err
	}
	// Reserve a fresh name so repeated recovery cannot overwrite prior evidence.
	placeholder, err := os.CreateTemp(quarantine, entry.Name()+"-quarantine-*")
	if err != nil {
		return true, err
	}
	target := placeholder.Name()
	if err = placeholder.Close(); err != nil {
		_ = os.Remove(target)
		return true, err
	}
	if err = os.Rename(source, target); err != nil {
		_ = os.Remove(target)
		return true, err
	}
	return true, errors.Join(fsutil.SyncDir(quarantine), fsutil.SyncDir(b.journalDir()))
}

func readBackupJob(file string) (BackupJob, error) {
	var j BackupJob
	st, err := os.Lstat(file)
	if err != nil {
		return j, err
	}
	if !st.Mode().IsRegular() {
		return j, errors.New("unsafe backup journal file")
	}
	if err = fsutil.CheckPrivate(file); err != nil {
		return j, err
	}
	if (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && !backupOwner(st) {
		return j, errors.New("backup journal has a foreign owner")
	}
	f, err := os.Open(file)
	if err != nil {
		return j, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return j, err
	}
	if len(raw) > 64<<10 {
		return j, errors.New("backup journal exceeds size limit")
	}
	if _, err = canonjson.Decode(raw); err != nil {
		return j, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&j); err != nil {
		return j, err
	}
	if err = d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return j, errors.New("trailing backup journal data")
	}
	if !j.ID.Valid() || j.BackupID != "" && !j.BackupID.Valid() || j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() {
		return j, errors.New("invalid backup journal identity")
	}
	return j, nil
}
