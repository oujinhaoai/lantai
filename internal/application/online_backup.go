package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

var ErrBackupBusy = backupmgmt.ErrBusy

const BackupRootMarker = ".lantai-backup-root"

type BackupRequest = backupmgmt.Request
type BackupJob = backupmgmt.Job

// BackupControl is the single owner shared by local HTTP and CLI. Its mutex
// serializes triggers; businessGate and Instance.Maintain provide exclusion
// against actual foreground operations and background writers respectively.
type BackupControl struct {
	app              *App
	root, rootID     string
	rootInfo         os.FileInfo
	mu               sync.Mutex
	jobs             map[ids.ID]BackupJob
	active           ids.ID
	cancel           context.CancelFunc
	done             chan struct{}
	closing          bool
	ctx              context.Context
	receipts         map[ids.ID][]backupReceipt
	management       backupManagementState
	managementErr    error
	lastAvailability backupmgmt.Availability
	controlAuthOpen  bool
	schedulerCancel  context.CancelFunc
	schedulerDone    chan struct{}
	now              func() time.Time
	// Internal test-only boundary synchronization. There is no environment,
	// configuration, HTTP or CLI path that can install this hook.
	testBoundary func(string, BackupJob)
	// Optional observer scoped to this controller, only assigned by package tests.
	atomicWriteObserver func(string, fsutil.AtomicWriteStage) error
	// Internal test-only fault injection, never an external configuration hook.
	authorizeSchedule func(context.Context, ids.ID, func() error) error
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
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("backup root must be a canonical absolute path")
	}
	var err error
	if rootID == "" || len(rootID) > 128 || strings.TrimSpace(rootID) != rootID {
		return nil, errors.New("invalid backup root identity")
	}
	b := &BackupControl{app: a, root: root, rootID: rootID, jobs: map[ids.ID]BackupJob{}, receipts: map[ids.ID][]backupReceipt{}, ctx: ctx, run: a.Backup, now: func() time.Time { return a.Instance.Clock().Now() }}
	if err := b.preflight(); err != nil {
		return nil, err
	}
	b.rootInfo, err = os.Stat(root)
	if err != nil {
		return nil, err
	}
	if err := b.loadJobs(); err != nil {
		return nil, err
	}
	if err := b.loadManagement(); err != nil {
		return nil, err
	}
	a.backupControl = b
	b.startScheduler()
	return b, nil
}

func (b *BackupControl) journalDir() string {
	return filepath.Join(b.app.Instance.Layout().Home, "backups", "online")
}
func (b *BackupControl) save(j BackupJob) error {
	if err := b.checkJournalDirs(true); err != nil {
		return err
	}
	raw, err := json.Marshal(backupRecord{Job: j, Receipts: b.receipts[j.ID]})
	if len(raw) > 64<<10 {
		return errors.New("backup journal receipt capacity")
	}
	if err != nil {
		return err
	}
	if err = os.MkdirAll(b.journalDir(), 0700); err != nil {
		return err
	}
	return b.writeControlRecord(filepath.Join(b.journalDir(), string(j.ID)+".json"), raw)
}

// preflight never creates the root. A separately provisioned marker prevents a
// missing mount from silently falling back to a newly created local directory.
func (b *BackupControl) preflight() error {
	if !filepath.IsAbs(b.root) || filepath.Clean(b.root) != b.root || !validBackupRootID(b.rootID) {
		return errors.New("invalid backup root identity or absolute path")
	}
	resolved, err := filepath.EvalSymlinks(b.root)
	if err != nil || resolved != b.root {
		return errors.New("backup root must be an existing real directory without symlink aliases")
	}
	st, err := os.Stat(b.root)
	if err != nil || !st.IsDir() {
		return errors.New("backup root is not a directory")
	}
	if err := fsutil.CheckPrivate(b.root); err != nil || !backupPrivateOwner(st) {
		return errors.New("backup root permissions must be private and owned by service user")
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
	if err != nil || !st.Mode().IsRegular() || !backupSingleLink(st) {
		return errors.New("backup root identity marker missing or unsafe")
	}
	if err := fsutil.CheckPrivate(marker); err != nil || !backupPrivateOwner(st) {
		return errors.New("backup marker permissions must be private and owned by service user")
	}
	raw, err := readBackupRootMarker(marker)
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
	if b.rootInfo == nil {
		b.rootInfo = stRoot(b.root)
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
func stRoot(root string) os.FileInfo { st, _ := os.Stat(root); return st }

func (b *BackupControl) checkDestination() error {
	st, err := os.Stat(b.root)
	if err != nil || !os.SameFile(b.rootInfo, st) {
		return errors.New("backup root unavailable or replaced")
	}
	if err := fsutil.CheckPrivate(b.root); err != nil || !backupPrivateOwner(st) {
		return errors.New("backup root permissions changed")
	}
	marker := filepath.Join(b.root, BackupRootMarker)
	st, err = os.Lstat(marker)
	if err != nil || !st.Mode().IsRegular() || !backupSingleLink(st) {
		return errors.New("backup marker unsafe")
	}
	if err = fsutil.CheckPrivate(marker); err != nil || !backupPrivateOwner(st) {
		return errors.New("backup marker permissions changed")
	}
	raw, err := readBackupRootMarker(marker)
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

// Reserve the configured low-water mark plus three times the captured bytes. This
// conservative preflight includes the snapshot, complete package and copy/WAL overhead. Per-blob and
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
			if n > (^uint64(0)-bytes)/3 {
				return errors.New("backup size overflow")
			}
			bytes += n * 3
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
	return b.startLocked(req, "", "", "")
}

func (b *BackupControl) startLocked(req BackupRequest, key, source, scheduledFor string) (BackupJob, error) {
	if req.Resume != "" && !req.Resume.Valid() {
		return BackupJob{}, backupmgmt.ErrInvalid
	}
	if b.closing || b.ctx.Err() != nil {
		return BackupJob{}, fmt.Errorf("%w: stopping", backupmgmt.ErrUnavailable)
	}
	if b.managementErr != nil {
		return BackupJob{}, fmt.Errorf("%w: journal_invalid", backupmgmt.ErrUnavailable)
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
		return BackupJob{}, backupmgmt.ErrInvalid
	}
	if availability := b.availabilityLocked(); !availability.Available {
		return BackupJob{}, fmt.Errorf("%w: %s", backupmgmt.ErrUnavailable, availability.Error)
	}
	var j BackupJob
	if req.Resume != "" {
		var ok bool
		j, ok = b.jobs[req.Resume]
		if !ok {
			return BackupJob{}, backupmgmt.ErrNotFound
		}
		if j.State == "complete" {
			return BackupJob{}, fmt.Errorf("%w: backup is not resumable", backupmgmt.ErrInvalid)
		}
	} else {
		id, err := b.app.Instance.IDs().New()
		if err != nil {
			return BackupJob{}, err
		}
		j = BackupJob{ID: id, CreatedAt: b.now(), Source: source, ScheduledFor: scheduledFor}
		if source == "schedule" {
			j.ScheduleTimezone = b.management.Schedule.Timezone
			j.ScheduleRevision = b.management.Schedule.Revision
		}
	}
	j.State, j.Error, j.UpdatedAt = "draining", "", b.now()
	j.Attempt++
	oldReceipts := b.receipts[j.ID]
	if key != "" {
		b.receipts[j.ID] = append(append([]backupReceipt(nil), oldReceipts...), backupReceipt{Key: key, Hash: requestHash(req)})
	}
	b.reachTestBoundary("job.accept.before_record", j)
	if err := b.save(j); err != nil {
		b.receipts[j.ID] = oldReceipts
		b.managementErr = err
		return BackupJob{}, err
	}
	b.reachTestBoundary("job.accept.after_record", j)
	c, cancel := context.WithTimeout(b.ctx, time.Duration(req.TimeoutSeconds)*time.Second)
	b.jobs[j.ID], b.active, b.cancel, b.done = j, j.ID, cancel, make(chan struct{})
	// Close admission before acknowledging the trigger, eliminating a new-request race.
	b.app.business.close()
	b.controlAuthOpen = true
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
	return b.cancelLocked(id)
}

// CancelScoped is the maintenance-only authority boundary. Reading a scope
// and cancelling are one atomic decision, so an old token cannot cross into a
// resumed attempt of the same job while authentication finishes.
func (b *BackupControl) CancelScoped(id ids.ID, attempt int) (BackupJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok || b.active != id || j.Attempt != attempt || !b.controlAuthOpen {
		return j, backupmgmt.ErrScopeStale
	}
	return b.cancelLocked(id)
}

func (b *BackupControl) cancelLocked(id ids.ID) (BackupJob, error) {
	j, ok := b.jobs[id]
	if !ok {
		return j, fs.ErrNotExist
	}
	if b.active == id {
		j.State, j.UpdatedAt = "cancelling", b.now()
		if err := b.save(j); err != nil {
			// A full/unwritable journal must not prevent cancellation. Keep
			// admission closed until the worker actually exits; only the
			// durable status is unavailable, not the cancellation signal.
			j.Error = "journal_write_failed"
			b.jobs[id] = j
			b.managementErr = err
			b.cancel()
			return j, err
		}
		b.jobs[id] = j
		b.reachTestBoundary("job.cancelling", j)
		b.cancel()
	}
	return j, nil
}

func (b *BackupControl) execute(ctx context.Context, j BackupJob, req BackupRequest) {
	var err error
	var m operations.BackupManifest
	var maintenance *operations.Maintenance
	failureCode := "backup_failed"
	// Finalization owns the exit fence for every path, including panic and
	// a drain that never acquired maintenance. It runs only after work exits.
	defer func() {
		if recover() != nil {
			err = errors.New("backup panic")
		}
		// Once resolved from the source journal, a job can never be rebound by
		// the worker's result, even on a failed or otherwise valid backup.
		if m.BackupID != "" && j.BackupID != "" && m.BackupID != j.BackupID {
			err = errors.New("backup result differs from the original job")
		}
		// Every exit, including early drain failure, must close the control
		// scope and finish the proof ledger before ordinary writes reopen.
		// Never wait for proof while holding b.mu: proof readers take b.mu.
		b.mu.Lock()
		b.controlAuthOpen = false
		b.mu.Unlock()
		releaseProof := b.app.Identity.DrainBackupControl()
		defer releaseProof()
		if maintenance != nil {
			maintenance.End()
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
			j.State, j.Error = "failed", failureCode
		}
		if m.BackupID != "" && j.BackupID == "" {
			j.BackupID = m.BackupID
		}
		if err == nil && m.RecoveryPackage != nil {
			p := m.RecoveryPackage
			j.Package = &backupmgmt.Package{ID: p.Manifest.PackageID, Format: p.Manifest.Contract, SHA256: p.SHA256, Size: p.Size, Sensitive: true}
		}
		j.UpdatedAt = b.now()
		b.reachTestBoundary("job.final.before_record", j)
		if e := b.save(j); e != nil {
			j.State, j.Error = "failed", "journal_write_failed"
			b.managementErr = e
		} else {
			b.reachTestBoundary("job.final.after_record", j)
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
	b.reachTestBoundary("job.draining", j)
	if err = b.app.business.wait(drain); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			err = context.DeadlineExceeded
		}
		return
	}
	maintenance, err = b.app.Instance.Maintain(drain, "backup")
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if drain.Err() != nil {
			err = context.DeadlineExceeded
		}
		return
	}
	if !timer.Stop() || drain.Err() != nil {
		err = context.DeadlineExceeded
		return
	}
	c := maintenance.Context()
	if err = b.preflight(); err != nil {
		failureCode = availabilityCode(err)
		return
	}
	if err = b.checkCapacity(c); err != nil {
		failureCode = "capacity_check_failed"
		return
	}
	target := filepath.Join(b.root, string(j.ID))
	if st, e := os.Lstat(target); e == nil {
		resolved, e := filepath.EvalSymlinks(target)
		if e != nil || !st.IsDir() || resolved != target {
			failureCode = "unsafe_backup_target"
			err = errors.New("unsafe backup target")
			return
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		err = e
		return
	}
	if req.Resume != "" {
		resolved, e := b.app.Instance.ResolveBackupID(c, j.BackupID, target)
		if e != nil {
			if !errors.Is(e, fs.ErrNotExist) || j.CaptureStarted {
				failureCode = "resume_point_invalid"
				err = e
				return
			}
		} else {
			j.BackupID = resolved
		}
	}
	b.mu.Lock()
	j.State, j.UpdatedAt = "backing_up", b.now()
	j.CaptureStarted = true
	b.reachTestBoundary("job.backing.before_record", j)
	err = b.save(j)
	b.jobs[j.ID] = j
	b.mu.Unlock()
	if err != nil {
		return
	}
	b.reachTestBoundary("job.backing_up", j)
	m, err = b.run(c, operations.BackupOptions{Destination: target, BackupID: j.BackupID, CheckDestination: func(context.Context) error { return b.checkDestination() }})
	var access *operations.RecoveryPackageAccessError
	if err == nil {
		completed := j
		completed.BackupID = m.BackupID
		b.reachTestBoundary("job.worker.completed", completed)
		// Confirm the installed artifact, not only the earlier temporary file,
		// before the durable job can advertise success. Resume follows the same
		// checks and must not bless an old unsafe package.
		if p := m.RecoveryPackage; p != nil {
			var f *os.File
			var actual operations.RecoveryPackageInfo
			f, actual, err = operations.OpenExportRecoveryPackage(c, operations.RecoveryPackagePath(target))
			if err == nil {
				_ = f.Close()
				if actual.SHA256 != p.SHA256 || actual.Size != p.Size || actual.Manifest.PackageID != p.Manifest.PackageID || actual.Manifest.BackupID != m.BackupID {
					err = &operations.RecoveryPackageAccessError{Reason: "backup_package_identity"}
				}
			}
		}
	}
	if errors.As(err, &access) {
		failureCode = access.Reason
	}
	if errors.Is(err, fs.ErrPermission) {
		failureCode = "destination_permission_denied"
	}
}

func (b *BackupControl) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closing = true
	if b.schedulerCancel != nil {
		b.schedulerCancel()
	}
	schedulerDone := b.schedulerDone
	done := b.done
	if b.cancel != nil {
		b.cancel()
	}
	active := b.active != ""
	b.mu.Unlock()
	if schedulerDone != nil {
		select {
		case <-schedulerDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if active {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("backup has not drained: %w", ctx.Err())
		}
	}
	return nil
}

func readBackupRootMarker(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 129 || !backupSingleLink(st) || !backupPrivateOwner(st) {
		return nil, errors.New("unsafe backup marker")
	}
	if err = fsutil.CheckPrivate(path); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, 130))
	if err != nil {
		return nil, err
	}
	if len(raw) > 129 {
		return nil, errors.New("backup marker exceeds size limit")
	}
	return raw, nil
}

func (b *BackupControl) reachTestBoundary(name string, j BackupJob) {
	if b.testBoundary != nil {
		b.testBoundary(name, j)
	}
}

func (b *BackupControl) writeControlRecord(path string, raw []byte) error {
	if b.atomicWriteObserver == nil {
		return fsutil.WriteFileAtomic(path, raw, 0600)
	}
	return fsutil.WriteFileAtomicWithObserver(path, raw, 0600, func(stage fsutil.AtomicWriteStage) error { return b.atomicWriteObserver(path, stage) })
}
