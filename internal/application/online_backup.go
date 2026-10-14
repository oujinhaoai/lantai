package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

var ErrBackupBusy = errors.New("backup already active")

const BackupRootMarker = ".lantai-backup-root"

// BackupRequest contains no host path or credentials. The deployment chooses
// one destination root; each run uses a generated child directory.
type BackupRequest struct {
	Resume         ids.ID `json:"resume,omitempty"`
	DrainSeconds   int    `json:"drain_seconds,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type BackupJob struct {
	ID             ids.ID    `json:"id"`
	BackupID       ids.ID    `json:"backup_id,omitempty"`
	CaptureStarted bool      `json:"capture_started,omitempty"`
	State          string    `json:"state"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// BackupControl is the single owner shared by local HTTP and CLI. Its mutex
// serializes triggers; businessGate and Instance.Maintain provide exclusion
// against actual foreground operations and background writers respectively.
type BackupControl struct {
	app          *App
	root, rootID string
	rootInfo     os.FileInfo
	mu           sync.Mutex
	jobs         map[ids.ID]BackupJob
	active       ids.ID
	cancel       context.CancelFunc
	done         chan struct{}
	closing      bool
	ctx          context.Context
	// Internal fault injection, never a configurable external hook.
	run func(context.Context, operations.BackupOptions) (operations.BackupManifest, error)
}

func (a *App) EnableBackups(ctx context.Context, root, rootID string) (*BackupControl, error) {
	if a.backupControl != nil {
		return nil, errors.New("backup controller already enabled")
	}
	if root == "" {
		return nil, errors.New("backup root is required")
	}
	root, err := filepath.Abs(root)
	if err != nil || rootID == "" || len(rootID) > 128 || strings.TrimSpace(rootID) != rootID {
		return nil, errors.New("invalid backup root identity")
	}
	b := &BackupControl{app: a, root: root, rootID: rootID, jobs: map[ids.ID]BackupJob{}, ctx: ctx, run: a.Backup}
	if err := b.preflight(); err != nil {
		return nil, err
	}
	b.rootInfo, err = os.Stat(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(b.journalDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, errors.New("invalid backup journal")
		}
		id := ids.ID(strings.TrimSuffix(entry.Name(), ".json"))
		if !id.Valid() {
			return nil, errors.New("invalid backup journal ID")
		}
		var j BackupJob
		raw, e := os.ReadFile(filepath.Join(b.journalDir(), entry.Name()))
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(raw, &j); e != nil || j.ID != id {
			return nil, errors.New("invalid backup journal")
		}
		switch j.State {
		case "draining", "backing_up", "cancelling":
			j.State, j.Error, j.UpdatedAt = "interrupted", "process_interrupted", time.Now().UTC()
			if e = b.save(j); e != nil {
				return nil, e
			}
		case "complete", "failed", "cancelled", "interrupted":
		default:
			return nil, errors.New("invalid backup journal state")
		}
		b.jobs[id] = j
	}
	a.backupControl = b
	return b, nil
}

func (b *BackupControl) journalDir() string {
	return filepath.Join(b.app.Instance.Layout().Home, "backups", "online")
}
func (b *BackupControl) save(j BackupJob) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(b.journalDir(), 0700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(b.journalDir(), string(j.ID)+".json"), raw, 0600)
}

// preflight never creates the root. A separately provisioned marker prevents a
// missing mount from silently falling back to a newly created local directory.
func (b *BackupControl) preflight() error {
	resolved, err := filepath.EvalSymlinks(b.root)
	if err != nil || resolved != b.root {
		return errors.New("backup root must be an existing real directory without symlink aliases")
	}
	st, err := os.Stat(b.root)
	if err != nil || !st.IsDir() {
		return errors.New("backup root is not a directory")
	}
	if b.rootInfo != nil && !os.SameFile(b.rootInfo, st) {
		return errors.New("backup root was replaced or remounted")
	}
	for _, source := range []string{b.app.Instance.Layout().Home, b.app.Instance.Config().SecretsDir} {
		source, err = filepath.EvalSymlinks(source)
		if err != nil {
			return err
		}
		if overlaps(source, b.root) {
			return errors.New("backup root overlaps data or secrets")
		}
	}
	marker := filepath.Join(b.root, BackupRootMarker)
	st, err = os.Lstat(marker)
	if err != nil || !st.Mode().IsRegular() {
		return errors.New("backup root identity marker missing or unsafe")
	}
	raw, err := os.ReadFile(marker)
	if err != nil || len(raw) > 129 || strings.TrimSpace(string(raw)) != b.rootID {
		return errors.New("backup root identity mismatch")
	}
	free, err := fsutil.FreeBytes(b.root)
	if err != nil {
		return err
	}
	if free < b.app.Instance.Config().MinFreeBytes {
		return errors.New("backup destination has insufficient free space")
	}
	probe, err := os.CreateTemp(b.root, ".lantai-probe-")
	if err != nil {
		return err
	}
	name := probe.Name()
	defer os.Remove(name)
	_, err = probe.Write([]byte("backup probe\n"))
	if err == nil {
		err = probe.Sync()
	}
	return errors.Join(err, probe.Close())
}
func (b *BackupControl) checkDestination() error {
	st, err := os.Stat(b.root)
	if err != nil || !os.SameFile(b.rootInfo, st) {
		return errors.New("backup root unavailable or replaced")
	}
	raw, err := os.ReadFile(filepath.Join(b.root, BackupRootMarker))
	if err != nil || strings.TrimSpace(string(raw)) != b.rootID {
		return errors.New("backup root identity changed")
	}
	free, err := fsutil.FreeBytes(b.root)
	if err != nil {
		return err
	}
	if free < b.app.Instance.Config().MinFreeBytes {
		return errors.New("backup destination reserve exhausted")
	}
	return nil
}

// Reserve the configured low-water mark plus twice the captured bytes. This
// conservative preflight includes WAL and temporary copy overhead. Per-blob and
// final checks still handle ENOSPC and external consumers after preflight.
func (b *BackupControl) checkCapacity(ctx context.Context) error {
	var bytes uint64
	for _, base := range []string{"db", "blobs", "instance.json", "config.yaml", "projects", "trash", "staging", "quarantine", "audit", "logs"} {
		err := filepath.WalkDir(filepath.Join(b.app.Instance.Layout().Home, base), func(path string, d fs.DirEntry, e error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if errors.Is(e, fs.ErrNotExist) {
				return nil
			}
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("unsafe source entry")
			}
			if d.IsDir() {
				return nil
			}
			st, err := d.Info()
			if err != nil {
				return err
			}
			if !st.Mode().IsRegular() {
				return errors.New("unsafe source file")
			}
			n := uint64(st.Size())
			if n > (^uint64(0)-bytes)/2 {
				return errors.New("backup size overflow")
			}
			bytes += n * 2
			return nil
		})
		if err != nil {
			return err
		}
	}
	reserve := b.app.Instance.Config().MinFreeBytes
	free, err := fsutil.FreeBytes(b.root)
	if err != nil {
		return err
	}
	if free < reserve || bytes > free-reserve {
		return errors.New("insufficient backup capacity including reserve")
	}
	return nil
}

func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator))
}

func (b *BackupControl) Start(req BackupRequest) (BackupJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing || b.ctx.Err() != nil {
		return BackupJob{}, errors.New("backup controller stopping")
	}
	if b.active != "" {
		return BackupJob{}, ErrBackupBusy
	}
	if req.DrainSeconds == 0 {
		req.DrainSeconds = 120
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = 7200
	}
	if req.DrainSeconds < 1 || req.DrainSeconds > 3600 || req.TimeoutSeconds < 1 || req.TimeoutSeconds > 86400 || req.DrainSeconds > req.TimeoutSeconds {
		return BackupJob{}, errors.New("invalid backup timeout")
	}
	if err := b.preflight(); err != nil {
		return BackupJob{}, err
	}
	var j BackupJob
	if req.Resume != "" {
		var ok bool
		j, ok = b.jobs[req.Resume]
		if !ok || j.State == "complete" {
			return BackupJob{}, errors.New("backup is not resumable")
		}
	} else {
		id, err := b.app.Instance.IDs().New()
		if err != nil {
			return BackupJob{}, err
		}
		j = BackupJob{ID: id, CreatedAt: time.Now().UTC()}
	}
	j.State, j.Error, j.UpdatedAt = "draining", "", time.Now().UTC()
	if err := b.save(j); err != nil {
		return BackupJob{}, err
	}
	c, cancel := context.WithTimeout(b.ctx, time.Duration(req.TimeoutSeconds)*time.Second)
	b.jobs[j.ID], b.active, b.cancel, b.done = j, j.ID, cancel, make(chan struct{})
	// Close admission before acknowledging the trigger, eliminating a new-request race.
	b.app.business.close()
	go b.execute(c, j, req)
	return j, nil
}

func (b *BackupControl) Status(id ids.ID) (BackupJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return j, fs.ErrNotExist
	}
	return j, nil
}
func (b *BackupControl) Cancel(id ids.ID) (BackupJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return j, fs.ErrNotExist
	}
	if b.active == id {
		j.State, j.UpdatedAt = "cancelling", time.Now().UTC()
		if err := b.save(j); err != nil {
			return j, err
		}
		b.jobs[id] = j
		b.cancel()
	}
	return j, nil
}

func (b *BackupControl) execute(ctx context.Context, j BackupJob, req BackupRequest) {
	var err error
	var m operations.BackupManifest
	// Finalization runs after all work and maintenance defers, including panic
	// unwinding. Cancellation never releases an active copier's barrier early.
	defer func() {
		if recover() != nil {
			err = errors.New("backup panic")
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		switch {
		case err == nil:
			j.State = "complete"
		case errors.Is(err, context.Canceled):
			j.State, j.Error = "cancelled", "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			j.State, j.Error = "failed", "timeout"
		default:
			j.State, j.Error = "failed", "backup_failed"
		}
		if m.BackupID != "" {
			j.BackupID = m.BackupID
		}
		j.UpdatedAt = time.Now().UTC()
		if e := b.save(j); e != nil {
			j.State, j.Error = "failed", "journal_write_failed"
		}
		b.jobs[j.ID] = j
		b.cancel()
		b.cancel = nil
		b.active = ""
		if !b.closing {
			b.app.business.open()
		}
		close(b.done)
	}()
	drain, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(time.Duration(req.DrainSeconds)*time.Second, cancel)
	defer timer.Stop()
	if err = b.app.business.wait(drain); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			err = context.DeadlineExceeded
		}
		return
	}
	var maintenance *operations.Maintenance
	maintenance, err = b.app.Instance.Maintain(drain, "backup")
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if drain.Err() != nil {
			err = context.DeadlineExceeded
		}
		return
	}
	defer maintenance.End()
	if !timer.Stop() || drain.Err() != nil {
		err = context.DeadlineExceeded
		return
	}
	c := maintenance.Context()
	if err = b.preflight(); err != nil {
		return
	}
	if err = b.checkCapacity(c); err != nil {
		return
	}
	b.mu.Lock()
	attempted := j.CaptureStarted
	j.State, j.UpdatedAt = "backing_up", time.Now().UTC()
	j.CaptureStarted = true
	err = b.save(j)
	b.jobs[j.ID] = j
	b.mu.Unlock()
	if err != nil {
		return
	}
	target := filepath.Join(b.root, string(j.ID))
	st, e := os.Lstat(target)
	if e == nil {
		resolved, e := filepath.EvalSymlinks(target)
		if e != nil || !st.IsDir() || resolved != target {
			err = errors.New("unsafe backup target")
			return
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		err = e
		return
	}
	if req.Resume != "" {
		// If capture was attempted, a missing destination cannot be silently replaced
		// by a fresh point. Start a new job explicitly or restore the original volume.
		raw, e := os.ReadFile(filepath.Join(target, "manifest.json"))
		if e == nil {
			var old operations.BackupManifest
			if e = json.Unmarshal(raw, &old); e != nil || !old.BackupID.Valid() {
				err = errors.New("invalid resumed manifest")
				return
			}
			j.BackupID = old.BackupID
		} else if attempted {
			err = errors.New("resume requires original backup destination")
			return
		}
	}
	m, err = b.run(c, operations.BackupOptions{Destination: target, BackupID: j.BackupID, CheckDestination: func(context.Context) error { return b.checkDestination() }})
}

func (b *BackupControl) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closing = true
	done := b.done
	if b.cancel != nil {
		b.cancel()
	}
	active := b.active != ""
	b.mu.Unlock()
	if active {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("backup has not drained: %w", ctx.Err())
		}
	}
	return nil
}
