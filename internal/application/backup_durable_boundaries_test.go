//go:build linux || darwin

package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

// These process tests name control-plane durable boundaries. They do not claim
// to inject at every fsync inside SQLite, the copy engine or the OS filesystem.
const durableBoundaryEnv = "LANTAI_TEST_DURABLE_BACKUP_BOUNDARY"
const durableIntent = "synthetic-durable-backup-intent"

type durableBoundaryRequest struct {
	Boundary    string `json:"boundary"`
	AtomicScope string `json:"atomic_scope,omitempty"`
	Handoff     string `json:"handoff"`
}
type durableBoundaryEvidence struct {
	AtomicScope      string    `json:"atomic_scope,omitempty"`
	RecordPath       string    `json:"record_path,omitempty"`
	TempPath         string    `json:"temp_path,omitempty"`
	TargetExists     bool      `json:"target_exists,omitempty"`
	TargetSHA256     string    `json:"target_sha256,omitempty"`
	TempSHA256       string    `json:"temp_sha256,omitempty"`
	Boundary         string    `json:"boundary"`
	Home             string    `json:"home"`
	Root             string    `json:"root"`
	JobID            ids.ID    `json:"job_id"`
	BackupID         ids.ID    `json:"backup_id,omitempty"`
	ManifestState    string    `json:"manifest_state,omitempty"`
	State            string    `json:"state"`
	Date             string    `json:"date,omitempty"`
	ScheduleRevision int64     `json:"schedule_revision,omitempty"`
	Clock            time.Time `json:"clock"`
}

func TestBackupDurableBoundaryChild(t *testing.T) {
	encoded := os.Getenv(durableBoundaryEnv)
	if encoded == "" {
		t.Skip("subprocess-only boundary fixture")
	}
	var request durableBoundaryRequest
	if err := json.Unmarshal([]byte(encoded), &request); err != nil {
		t.Fatal(err)
	}
	a := openTestApp(t)
	b := testBackupControl(t, a)
	stopBackupScheduler(b)
	fake := a.Instance.Clock().(*clock.Fake)
	fake.Set(time.Date(2026, 10, 1, 1, 59, 0, 0, time.UTC))
	var once sync.Once
	var finalRecord atomic.Bool
	var scheduleSlotArmed atomic.Bool
	var atomicPath string
	reached := func(point string, j BackupJob) {
		if point != request.Boundary {
			return
		}
		once.Do(func() {
			if j.ID == "" {
				for _, candidate := range b.jobs {
					if candidate.Source == "schedule" {
						j = candidate
						break
					}
				}
			}
			evidence := durableBoundaryEvidence{Boundary: point, Home: a.Instance.Layout().Home, Root: b.root, JobID: j.ID, BackupID: j.BackupID, State: j.State, Date: j.ScheduledFor, ScheduleRevision: j.ScheduleRevision, Clock: fake.Now()}
			if request.AtomicScope != "" {
				evidence.AtomicScope = request.AtomicScope
				evidence.RecordPath = atomicPath
				raw, e := os.ReadFile(atomicPath)
				if e == nil {
					if !json.Valid(raw) {
						t.Fatal("old target is partial JSON")
					}
					evidence.TargetExists = true
					evidence.TargetSHA256 = backupDigest(raw)
				} else if !errors.Is(e, os.ErrNotExist) {
					t.Fatal(e)
				}
				matches, e := filepath.Glob(filepath.Join(filepath.Dir(atomicPath), "."+filepath.Base(atomicPath)+".tmp-*"))
				if e != nil || len(matches) != 1 {
					t.Fatal("missing synced temp", matches, e)
				}
				evidence.TempPath = matches[0]
				raw, e = os.ReadFile(matches[0])
				if e != nil || !json.Valid(raw) {
					t.Fatal("synced temp is incomplete JSON", e)
				}
				evidence.TempSHA256 = backupDigest(raw)
				if request.AtomicScope != "schedule_slot" {
					var record backupRecord
					if e = json.Unmarshal(raw, &record); e != nil {
						t.Fatal(e)
					}
					j = record.Job
					evidence.JobID = j.ID
					evidence.State = j.State
					evidence.BackupID = j.BackupID
				}
			}
			// Reading the destination manifest is safe fixture evidence; this never
			// reads/copies an active SQLite database or alters a business record.
			raw, e := os.ReadFile(filepath.Join(b.root, string(j.ID), "manifest.json"))
			if e == nil {
				var m operations.BackupManifest
				if json.Unmarshal(raw, &m) == nil {
					evidence.BackupID = m.BackupID
					evidence.ManifestState = m.State
				}
			}
			raw, e = json.Marshal(evidence)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(request.Handoff, raw, 0600); e != nil {
				t.Fatal(e)
			}
			// The parent sends SIGKILL only after the handoff is visible. No graceful
			// shutdown, controller Close, or defer can turn this into a clean stop.
			for {
				time.Sleep(time.Second)
			}
		})
	}
	b.testBoundary = func(point string, j BackupJob) {
		if point == "job.final.before_record" {
			finalRecord.Store(true)
		}
		reached(point, j)
	}
	if request.AtomicScope != "" {
		b.atomicWriteObserver = func(path string, stage fsutil.AtomicWriteStage) error {
			if request.AtomicScope == "schedule_slot" && !scheduleSlotArmed.Load() {
				return nil
			}
			if stage != fsutil.AtomicAfterFileSync || !atomicRecordTarget(b, request.AtomicScope, path, finalRecord.Load()) {
				return nil
			}
			atomicPath = path
			var j BackupJob
			if request.AtomicScope != "schedule_slot" {
				j.ID = ids.ID(strings.TrimSuffix(filepath.Base(path), ".json"))
			}
			reached(request.Boundary, j)
			return nil
		}
	}
	if request.Boundary == "job.draining" {
		if _, ok := a.business.enter(); !ok {
			t.Fatal("fixture reader rejected")
		}
	}
	if request.Boundary == "job.copying" || request.Boundary == "job.cancelling" {
		realRun := b.run
		copying := make(chan struct{})
		var copyOnce sync.Once
		b.run = func(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
			original := opts.CheckDestination
			opts.CheckDestination = func(c context.Context) error {
				if err := original(c); err != nil {
					return err
				}
				raw, err := os.ReadFile(filepath.Join(opts.Destination, "manifest.json"))
				if err != nil {
					return err
				}
				var m operations.BackupManifest
				if err = json.Unmarshal(raw, &m); err != nil {
					return err
				}
				if m.State == "copying" {
					if request.Boundary == "job.copying" {
						reached("job.copying", BackupJob{ID: ids.ID(filepath.Base(opts.Destination)), BackupID: m.BackupID, State: "backing_up"})
					}
					copyOnce.Do(func() { close(copying) })
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			}
			return realRun(ctx, opts)
		}
		if request.Boundary == "job.cancelling" {
			go func() {
				<-copying
				b.mu.Lock()
				id := b.active
				b.mu.Unlock()
				if _, err := b.Cancel(id); err != nil {
					t.Error(err)
				}
			}()
		}
	}
	if request.AtomicScope == "schedule_slot" || len(request.Boundary) >= 9 && request.Boundary[:9] == "schedule." {
		plan, err := saveBackupTestSchedule(t, b, backupmgmt.Schedule{Enabled: true, Timezone: "UTC", Time: "02:00"}, "synthetic-schedule-approval")
		if err != nil {
			t.Fatal(err)
		}
		scheduleSlotArmed.Store(true)
		fake.Set(*plan.NextRunAt)
		b.runSchedule(fake.Now(), false)
	} else {
		if _, err := b.Trigger(BackupRequest{}, durableIntent); err != nil {
			t.Fatal(err)
		}
	}
	// Async workers reach their boundary after Trigger acknowledges admission.
	for {
		time.Sleep(time.Second)
	}
}

func stopAtDurableBoundary(t *testing.T, point string) durableBoundaryEvidence {
	t.Helper()
	return stopAtDurableBoundaryRequest(t, durableBoundaryRequest{Boundary: point})
}

func stopAtDurableBoundaryRequest(t *testing.T, requestConfig durableBoundaryRequest) durableBoundaryEvidence {
	t.Helper()
	point := requestConfig.Boundary
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	handoff := filepath.Join(t.TempDir(), "durable-boundary.json")
	requestConfig.Handoff = handoff
	request, _ := json.Marshal(requestConfig)
	cmd := exec.Command(exe, "-test.run=^TestBackupDurableBoundaryChild$", "-test.timeout=75s")
	cmd.Env = append(os.Environ(), durableBoundaryEnv+"="+string(request))
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	var evidence durableBoundaryEvidence
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		raw, e := os.ReadFile(handoff)
		if e == nil && json.Unmarshal(raw, &evidence) == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if evidence.Boundary != point {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		stopped = true
		t.Fatalf("boundary %s not reached: %s", point, output.String())
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	stopped = true
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("fixture did not terminate by SIGKILL: %v", cmd.ProcessState)
	}
	t.Cleanup(func() { _ = os.RemoveAll(evidence.Home); _ = os.RemoveAll(evidence.Root) })
	t.Logf("hard stop boundary=%s state=%s job=%s backup=%s schedule_revision=%d manifest_state=%s", point, evidence.State, evidence.JobID, evidence.BackupID, evidence.ScheduleRevision, evidence.ManifestState)
	return evidence
}

func reopenDurableBoundary(t *testing.T, e durableBoundaryEvidence) (*App, *BackupControl) {
	t.Helper()
	fake := clock.NewFake(e.Clock.Add(10 * time.Second))
	a, err := Open(t.Context(), Options{Instance: operations.Options{Home: e.Home, Clock: fake}, Identity: identity.Config{Password: testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	b, err := a.EnableBackups(t.Context(), e.Root, "synthetic-volume")
	if err != nil {
		t.Fatal(err)
	}
	stopBackupScheduler(b)
	return a, b
}

func TestBackupDurableJobBoundaryReplay(t *testing.T) {
	for _, point := range []string{"job.accept.before_record", "job.accept.after_record", "job.draining", "job.backing_up", "job.copying", "job.cancelling", "job.worker.completed", "job.final.before_record", "job.final.after_record"} {
		t.Run(point, func(t *testing.T) {
			evidence := stopAtDurableBoundary(t, point)
			a, b := reopenDurableBoundary(t, evidence)
			history, err := b.History()
			if err != nil {
				t.Fatal(err)
			}
			if point == "job.accept.before_record" {
				if len(history) != 0 {
					t.Fatal("unaccepted intent created a durable job", history)
				}
				j, err := b.Trigger(BackupRequest{}, durableIntent)
				if err != nil {
					t.Fatal(err)
				}
				if j.ID == evidence.JobID {
					t.Fatal("unaccepted generated ID was treated as a durable receipt")
				}
				got := waitBackup(t, b, string(j.ID))
				if got.State != "complete" {
					t.Fatal(got)
				}
				if _, err = operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(j.ID))); err != nil {
					t.Fatal(err)
				}
				return
			}
			if len(history) != 1 || history[0].ID != evidence.JobID {
				t.Fatal("restart lost or duplicated durable job", history)
			}
			replay, err := b.Trigger(BackupRequest{}, durableIntent)
			if err != nil || replay.ID != evidence.JobID {
				t.Fatal(replay, err)
			}
			if point == "job.copying" || point == "job.cancelling" {
				if evidence.ManifestState != "copying" {
					t.Fatalf("fixture never reached actual frozen-point copying: %+v", evidence)
				}
				if _, err := operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(replay.ID))); err == nil {
					t.Fatal("incomplete copying fixture was already advertised complete")
				}
			}
			if point == "job.worker.completed" || point == "job.final.before_record" || point == "job.final.after_record" {
				manifest, err := operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(replay.ID)))
				if err != nil || manifest.BackupID != evidence.BackupID {
					t.Fatal("completed worker did not leave the original verified point", manifest.BackupID, err)
				}
			}
			b.mu.Lock()
			active := b.active
			b.mu.Unlock()
			if active != "" || !a.Ready() {
				t.Fatal("restart automatically restarted work or held business closed")
			}
			if point == "job.final.after_record" {
				if replay.State != "complete" {
					t.Fatal(replay)
				}
			} else {
				if replay.State != "interrupted" {
					t.Fatal(replay)
				}
				if _, err = b.Trigger(BackupRequest{Resume: replay.ID}, "synthetic-resume-operation"); err != nil {
					t.Fatal(err)
				}
				replay = waitBackup(t, b, string(replay.ID))
				if point == "job.backing_up" {
					// The capture-start control record is durable, but there is no bound
					// source point yet. Conservative refusal is required; never recapture
					// newer data silently under this already-started job.
					if replay.State != "failed" || replay.Error != "resume_point_invalid" {
						t.Fatal(replay)
					}
					if _, err = os.Stat(filepath.Join(b.root, string(replay.ID), "COMPLETE")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("uncaptured job advertised a complete point")
					}
					if !a.Ready() {
						t.Fatal("safe resume refusal left business unavailable")
					}
					return
				}
				if replay.State != "complete" {
					t.Fatal(replay)
				}
			}
			manifest, err := operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(replay.ID)))
			if err != nil {
				t.Fatal(err)
			}
			if evidence.BackupID != "" && manifest.BackupID != evidence.BackupID {
				t.Fatal("restart rebound the captured point", manifest.BackupID, evidence.BackupID)
			}
			if replay.BackupID != manifest.BackupID {
				t.Fatal("control journal differs from verified archive")
			}
			history, _ = b.History()
			if len(history) != 1 {
				t.Fatal("replay/resume created a duplicate job", history)
			}
			if !a.Ready() {
				t.Fatal("restart recovery left business unavailable")
			}
		})
	}
}

func TestBackupDurableScheduleSlotReplay(t *testing.T) {
	for _, point := range []string{"schedule.job_recorded", "schedule.slot.before_record", "schedule.slot.after_record"} {
		t.Run(point, func(t *testing.T) {
			evidence := stopAtDurableBoundary(t, point)
			_, b := reopenDurableBoundary(t, evidence)
			// A durable scheduled job is itself a consumed-day receipt, even if the
			// separate summary/event write was interrupted. Replaying the tick must not
			// produce another automatic job or automatically resume the interrupted one.
			b.runSchedule(evidence.Clock.Add(20*time.Second), false)
			history, _ := b.History()
			if len(history) != 1 || history[0].ID != evidence.JobID || history[0].State != "interrupted" {
				t.Fatal(history)
			}
			if history[0].ScheduledFor != evidence.Date || history[0].ScheduleRevision != evidence.ScheduleRevision {
				t.Fatal("scheduled authority provenance changed", history[0])
			}
			view, _ := b.Overview()
			if view.Schedule.NextRunAt == nil || !view.Schedule.NextRunAt.After(evidence.Clock) {
				t.Fatal(view)
			}
			if _, err := b.Trigger(BackupRequest{Resume: evidence.JobID}, "explicit-scheduled-resume"); err != nil {
				t.Fatal(err)
			}
			got := waitBackup(t, b, string(evidence.JobID))
			if got.State != "complete" {
				t.Fatal(got)
			}
			if _, err := operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(got.ID))); err != nil {
				t.Fatal(err)
			}
			b.runSchedule(evidence.Clock.Add(time.Hour), false)
			history, _ = b.History()
			if len(history) != 1 {
				t.Fatal("same local day ran twice", history)
			}
		})
	}
}

func TestBackupDurableControlRecordWriteFailures(t *testing.T) {
	for _, point := range []string{"job.accept.before_record", "job.backing.before_record", "job.final.before_record"} {
		t.Run(point, func(t *testing.T) {
			a := openTestApp(t)
			b := testBackupControl(t, a)
			stopBackupScheduler(b)
			var once sync.Once
			var blocked, preserved string
			var targetID ids.ID
			b.testBoundary = func(name string, j BackupJob) {
				if name != point {
					return
				}
				once.Do(func() {
					targetID = j.ID
					blocked = filepath.Join(b.journalDir(), string(j.ID)+".json")
					if err := os.MkdirAll(b.journalDir(), 0700); err != nil {
						t.Fatal(err)
					}
					if point != "job.accept.before_record" {
						evidenceDir := filepath.Join(a.Instance.Layout().Home, "backups", "synthetic-write-failure")
						if err := os.Mkdir(evidenceDir, 0700); err != nil {
							t.Fatal(err)
						}
						preserved = filepath.Join(evidenceDir, filepath.Base(blocked))
						if err := os.Rename(blocked, preserved); err != nil {
							t.Fatal(err)
						}
					}
					// A directory at the atomic record destination makes the real rename
					// fail on all test platforms, without chmod or business database writes.
					if err := os.Mkdir(blocked, 0700); err != nil {
						t.Fatal(err)
					}
				})
			}
			job, err := b.Trigger(BackupRequest{}, durableIntent)
			if point == "job.accept.before_record" {
				if err == nil {
					t.Fatal("unpersisted intent was accepted")
				}
				history, _ := b.History()
				if len(history) != 0 {
					t.Fatal("failed admission created a visible job", history)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				got := waitBackup(t, b, string(job.ID))
				if got.State != "failed" || got.Error != "journal_write_failed" {
					t.Fatal(got)
				}
				if point == "job.final.before_record" {
					if _, err := operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(job.ID))); err != nil {
						t.Fatal("real completed archive was lost", err)
					}
				}
			}
			if !a.Ready() {
				t.Fatal("failed control record leaked business maintenance")
			}
			view, _ := b.Overview()
			if view.Availability.Error != "journal_invalid" {
				t.Fatal(view)
			}
			if _, err = b.Trigger(BackupRequest{}, "new-while-record-broken"); !errors.Is(err, backupmgmt.ErrUnavailable) {
				t.Fatal("uncertain persistence allowed a second operation", err)
			}
			// Repair only this synthetic path obstruction and reopen. The product does
			// not pretend it can automatically repair arbitrary user journal damage.
			if err = os.Remove(blocked); err != nil {
				t.Fatal(err)
			}
			if preserved != "" {
				if err = os.Rename(preserved, blocked); err != nil {
					t.Fatal(err)
				}
			}
			evidence := durableBoundaryEvidence{Home: a.Instance.Layout().Home, Root: b.root, JobID: targetID, Clock: a.Instance.Clock().Now()}
			if err = a.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, reopened := reopenDurableBoundary(t, evidence)
			if point == "job.accept.before_record" {
				job, err = reopened.Trigger(BackupRequest{}, durableIntent)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				replay, e := reopened.Trigger(BackupRequest{}, durableIntent)
				if e != nil || replay.ID != targetID || replay.State != "interrupted" {
					t.Fatal(replay, e)
				}
				job, err = reopened.Trigger(BackupRequest{Resume: targetID}, "resume-after-synthetic-repair")
				if err != nil {
					t.Fatal(err)
				}
			}
			got := waitBackup(t, reopened, string(job.ID))
			if got.State != "complete" {
				t.Fatal(got)
			}
			if _, err = operations.VerifyBackup(t.Context(), filepath.Join(reopened.root, string(job.ID))); err != nil {
				t.Fatal(err)
			}
			t.Logf("real atomic-write obstruction recovered boundary=%s same_job=%t", point, point != "job.accept.before_record")
		})
	}
}

func TestBackupControlScopeCoversDrainAndFencesAllExits(t *testing.T) {
	t.Run("draining_while_waiting_for_existing_writer", func(t *testing.T) {
		a := openTestApp(t)
		b := testBackupControl(t, a)
		stopBackupScheduler(b)
		heldCtx, held, err := a.Instance.Gate().Acquire(t.Context(), commands.Request{})
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		copied := make(chan struct{}, 1)
		b.run = func(context.Context, operations.BackupOptions) (operations.BackupManifest, error) {
			copied <- struct{}{}
			return operations.BackupManifest{}, nil
		}
		job, err := b.Trigger(BackupRequest{}, "scope-waiting-writer")
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for a.Instance.State() != operations.StateMaintenance {
			if time.Now().After(deadline) {
				t.Fatal("worker did not start maintenance drain")
			}
			time.Sleep(time.Millisecond)
		}
		if scope, ok := a.BackupControlScope(); !ok || scope.JobID != job.ID || scope.Attempt != job.Attempt {
			t.Fatal("draining scope unavailable", scope, ok)
		}
		select {
		case <-copied:
			t.Fatal("backup crossed existing writer")
		default:
		}
		if _, err = b.CancelScoped(job.ID, job.Attempt); err != nil {
			t.Fatal(err)
		}
		got := waitBackup(t, b, string(job.ID))
		if got.State != "cancelled" {
			t.Fatal(got)
		}
		if heldCtx.Err() != nil {
			t.Fatal("cancelling backup cancelled an existing business operation")
		}
		held.Release()
	})
	t.Run("scope_closes_before_proof_drain_finishes", func(t *testing.T) {
		a := openTestApp(t)
		b := testBackupControl(t, a)
		stopBackupScheduler(b)
		realRun := b.run
		copied, finish := make(chan struct{}), make(chan struct{})
		b.run = func(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
			m, err := realRun(ctx, opts)
			if err != nil {
				return m, err
			}
			close(copied)
			<-finish
			return m, nil
		}
		job, err := b.Trigger(BackupRequest{}, "scope-exit-fence")
		if err != nil {
			t.Fatal(err)
		}
		<-copied
		if scope, ok := a.BackupControlScope(); !ok || scope.JobID != job.ID {
			t.Fatal("full-maintenance scope did not open", scope, ok)
		}
		release := a.Identity.DrainBackupControl()
		var once sync.Once
		unblock := func() { once.Do(release) }
		defer unblock()
		close(finish)
		deadline := time.Now().Add(5 * time.Second)
		for {
			b.mu.Lock()
			open := b.controlAuthOpen
			b.mu.Unlock()
			if !open {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("scope stayed open behind the proof drain")
			}
			time.Sleep(time.Millisecond)
		}
		if scope, ok := a.BackupControlScope(); ok {
			t.Fatal("scope remained available during exit fence", scope)
		}
		if a.Ready() {
			t.Fatal("ordinary writes reopened before proof drain completed")
		}
		unblock()
		got := waitBackup(t, b, string(job.ID))
		if got.State != "complete" || !a.Ready() {
			t.Fatal(got)
		}
	})
}

func TestBackupScopedCancelCannotCrossIntoResumedAttempt(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	stopBackupScheduler(b)
	realRun := b.run
	installHeldRun := func() (chan struct{}, chan struct{}) {
		entered, release := make(chan struct{}), make(chan struct{})
		b.run = func(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
			m, err := realRun(ctx, opts)
			if err != nil {
				return m, err
			}
			close(entered)
			<-release
			return m, ctx.Err()
		}
		return entered, release
	}
	entered, release := installHeldRun()
	first, err := b.Trigger(BackupRequest{}, "scoped-cancel-first")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err = b.CancelScoped(first.ID, first.Attempt+1); !errors.Is(err, backupmgmt.ErrScopeStale) {
		t.Fatal("wrong attempt was allowed", err)
	}
	if _, err = b.CancelScoped(ids.New(), first.Attempt); !errors.Is(err, backupmgmt.ErrScopeStale) {
		t.Fatal("wrong job was allowed", err)
	}
	if _, err = b.CancelScoped(first.ID, first.Attempt); err != nil {
		t.Fatal(err)
	}
	close(release)
	finished := waitBackup(t, b, string(first.ID))
	if finished.State != "cancelled" {
		t.Fatal(finished)
	}
	entered, release = installHeldRun()
	resumed, err := b.Trigger(BackupRequest{Resume: first.ID}, "scoped-cancel-resumed")
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if resumed.Attempt <= first.Attempt {
		t.Fatal("resumed attempt did not advance")
	}
	if _, err = b.CancelScoped(first.ID, first.Attempt); !errors.Is(err, backupmgmt.ErrScopeStale) {
		t.Fatal("old scope cancelled resumed attempt", err)
	}
	current, _ := b.Status(first.ID)
	if current.State != "backing_up" || current.Attempt != resumed.Attempt {
		t.Fatal(current)
	}
	if _, err = b.CancelScoped(resumed.ID, resumed.Attempt); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := waitBackup(t, b, string(resumed.ID)); got.State != "cancelled" {
		t.Fatal(got)
	}
	if _, err = b.CancelScoped(resumed.ID, resumed.Attempt); !errors.Is(err, backupmgmt.ErrScopeStale) {
		t.Fatal("completed scope remained actionable", err)
	}
}

func TestBackupControlEarlyExitAlwaysDrainsProof(t *testing.T) {
	for _, mode := range []string{"drain_cancel", "drain_timeout", "maintain_cancel", "panic", "close"} {
		t.Run(mode, func(t *testing.T) {
			a := openTestApp(t)
			b := testBackupControl(t, a)
			stopBackupScheduler(b)
			var releaseHold func()
			var finish chan struct{}
			var entered chan struct{}
			if mode == "maintain_cancel" {
				_, held, err := a.Instance.Gate().Acquire(t.Context(), commands.Request{})
				if err != nil {
					t.Fatal(err)
				}
				releaseHold = held.Release
			} else if mode == "panic" {
				entered, finish = make(chan struct{}), make(chan struct{})
				b.run = func(context.Context, operations.BackupOptions) (operations.BackupManifest, error) {
					close(entered)
					<-finish
					panic("synthetic worker panic")
				}
			} else {
				done, ok := a.business.enter()
				if !ok {
					t.Fatal("fixture operation rejected")
				}
				releaseHold = done
			}
			var holdOnce sync.Once
			releaseHolding := func() {
				if releaseHold != nil {
					holdOnce.Do(releaseHold)
				}
			}
			defer releaseHolding()
			req := BackupRequest{}
			if mode == "drain_timeout" {
				req.DrainSeconds = 1
				req.TimeoutSeconds = 5
			}
			job, err := b.Trigger(req, "early-exit-"+mode)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "maintain_cancel" {
				deadline := time.Now().Add(5 * time.Second)
				for a.Instance.State() != operations.StateMaintenance {
					if time.Now().After(deadline) {
						t.Fatal("Maintain was not waiting")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if mode == "panic" {
				<-entered
			}
			if scope, ok := a.BackupControlScope(); !ok || scope.JobID != job.ID {
				t.Fatal("active scope was unavailable", scope, ok)
			}
			release := a.Identity.DrainBackupControl()
			var proofOnce sync.Once
			unblock := func() { proofOnce.Do(release) }
			defer unblock()
			var closeDone chan error
			switch mode {
			case "drain_cancel", "maintain_cancel":
				if _, err = b.CancelScoped(job.ID, job.Attempt); err != nil {
					t.Fatal(err)
				}
			case "panic":
				close(finish)
			case "close":
				closeDone = make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					closeDone <- b.Close(ctx)
				}()
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				b.mu.Lock()
				open := b.controlAuthOpen
				b.mu.Unlock()
				if !open {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("early exit did not close control scope")
				}
				time.Sleep(time.Millisecond)
			}
			if scope, ok := a.BackupControlScope(); ok {
				t.Fatal("new control proof could enter exit fence", scope)
			}
			if a.Ready() {
				t.Fatal("business reopened before pending proof drained")
			}
			if closeDone != nil {
				select {
				case err := <-closeDone:
					t.Fatal("Close returned before proof drained", err)
				default:
				}
			}
			releaseHolding()
			unblock()
			got := waitBackup(t, b, string(job.ID))
			expected := "cancelled"
			if mode == "panic" || mode == "drain_timeout" {
				expected = "failed"
			}
			if got.State != expected {
				t.Fatal(got)
			}
			if mode == "drain_timeout" && got.Error != "timeout" {
				t.Fatal(got)
			}
			if closeDone != nil {
				if err := <-closeDone; err != nil {
					t.Fatal(err)
				}
			} else if !a.Ready() {
				t.Fatal("normal early exit did not restore business")
			}
		})
	}
}
