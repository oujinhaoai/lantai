//go:build linux || darwin

package application

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

func atomicRecordTarget(b *BackupControl, scope, path string, final bool) bool {
	switch scope {
	case "job_initial":
		return filepath.Dir(path) == b.journalDir() && !final
	case "job_final":
		return filepath.Dir(path) == b.journalDir() && final
	case "schedule_slot":
		return path == b.managementFile()
	default:
		return false
	}
}

func readWholeAtomicJSON(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("partial JSON at %s", filepath.Base(path))
	}
	return raw
}

func TestBackupAtomicSyncCallFailures(t *testing.T) {
	for _, scope := range []string{"job_initial", "job_final", "schedule_slot"} {
		for _, stage := range []fsutil.AtomicWriteStage{fsutil.AtomicFileSync, fsutil.AtomicDirectorySync} {
			t.Run(scope+"/"+string(stage), func(t *testing.T) {
				a := openTestApp(t)
				b := testBackupControl(t, a)
				stopBackupScheduler(b)
				var finalRecord atomic.Bool
				var mu sync.Mutex
				var target string
				var old []byte
				calls := 0
				b.testBoundary = func(point string, j BackupJob) {
					if point == "job.final.before_record" {
						if scope == "job_final" {
							raw := readWholeAtomicJSON(t, filepath.Join(b.journalDir(), string(j.ID)+".json"))
							mu.Lock()
							old = raw
							mu.Unlock()
						}
						finalRecord.Store(true)
					}
				}
				var plan backupmgmt.Schedule
				if scope == "schedule_slot" {
					var err error
					plan, err = saveBackupTestSchedule(t, b, backupmgmt.Schedule{Enabled: true, Timezone: "UTC", Time: "11:00"}, "atomic-plan-approval")
					if err != nil {
						t.Fatal(err)
					}
					old = readWholeAtomicJSON(t, b.managementFile())
				}
				b.atomicWriteObserver = func(path string, at fsutil.AtomicWriteStage) error {
					if at == stage && atomicRecordTarget(b, scope, path, finalRecord.Load()) {
						mu.Lock()
						target = path
						calls++
						mu.Unlock()
						return syscall.EIO // Call-level fault, not a claim of hardware failure.
					}
					return nil
				}
				var job BackupJob
				var triggerErr error
				if scope == "schedule_slot" {
					a.Instance.Clock().(*clock.Fake).Set(*plan.NextRunAt)
					b.runSchedule(*plan.NextRunAt, false)
					history, _ := b.History()
					if len(history) != 1 {
						t.Fatal(history)
					}
					job = history[0]
					job = waitBackup(t, b, string(job.ID))
					if job.State != "complete" {
						t.Fatal(job)
					}
				} else {
					job, triggerErr = b.Trigger(BackupRequest{}, durableIntent)
					if scope == "job_initial" {
						if !errors.Is(triggerErr, syscall.EIO) {
							t.Fatal("initial call did not report injected EIO", triggerErr)
						}
					} else {
						if triggerErr != nil {
							t.Fatal(triggerErr)
						}
						job = waitBackup(t, b, string(job.ID))
						if job.State != "failed" || job.Error != "journal_write_failed" {
							t.Fatal(job)
						}
					}
				}
				mu.Lock()
				recordPath, count, oldRecord := target, calls, append([]byte(nil), old...)
				mu.Unlock()
				if count != 1 || recordPath == "" {
					t.Fatal("wrong fault target/count", recordPath, count)
				}
				var published []byte
				if scope == "job_initial" && stage == fsutil.AtomicFileSync {
					if _, err := os.Stat(recordPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("unrenamed initial record became authoritative", err)
					}
				} else {
					published = readWholeAtomicJSON(t, recordPath)
					if stage == fsutil.AtomicFileSync && string(published) != string(oldRecord) {
						t.Fatal("failed temp Sync changed the old record")
					}
					if stage == fsutil.AtomicDirectorySync && string(published) == string(oldRecord) {
						t.Fatal("directory Sync fault was injected before rename")
					}
				}
				tmp, err := filepath.Glob(filepath.Join(filepath.Dir(recordPath), "."+filepath.Base(recordPath)+".tmp-*"))
				if err != nil || len(tmp) != 0 {
					t.Fatal("failed call left a live temp", tmp, err)
				}
				if !a.Ready() {
					t.Fatal("record failure leaked business maintenance")
				}
				view, _ := b.Overview()
				if view.Availability.Error != "journal_invalid" {
					t.Fatal(view)
				}
				if _, err = b.Trigger(BackupRequest{}, "new-intent-after-EIO"); !errors.Is(err, backupmgmt.ErrUnavailable) {
					t.Fatal("uncertain write did not latch", err)
				}
				evidence := durableBoundaryEvidence{Home: a.Instance.Layout().Home, Root: b.root, Clock: a.Instance.Clock().Now()}
				var priorBackup ids.ID
				if scope != "job_initial" {
					manifest, err := operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(job.ID)))
					if err != nil {
						t.Fatal(err)
					}
					priorBackup = manifest.BackupID
				}
				if err = a.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				_, reopened := reopenDurableBoundary(t, evidence)
				if scope == "schedule_slot" {
					reopened.runSchedule(evidence.Clock.Add(20*time.Second), false)
					history, _ := reopened.History()
					if len(history) != 1 || history[0].ID != job.ID {
						t.Fatal("schedule replay duplicated a joint commit", history)
					}
					if history[0].BackupID != priorBackup {
						t.Fatal("schedule replay rebound the archive")
					}
					request := backupmgmt.Schedule{Enabled: true, Timezone: "UTC", Time: "11:00", AuthorizedBy: plan.AuthorizedBy}
					replay, err := reopened.SaveSchedule(request, "atomic-plan-approval")
					if err != nil || replay.Revision != plan.Revision {
						t.Fatal(replay, err)
					}
				} else {
					history, _ := reopened.History()
					if scope == "job_initial" && stage == fsutil.AtomicFileSync {
						if len(history) != 0 {
							t.Fatal(history)
						}
					} else {
						if len(history) != 1 {
							t.Fatal(history)
						}
					}
					replay, err := reopened.Trigger(BackupRequest{}, durableIntent)
					if err != nil {
						t.Fatal(err)
					}
					if scope == "job_initial" && stage == fsutil.AtomicFileSync {
						job = replay
					} else {
						if string(replay.ID) != strings.TrimSuffix(filepath.Base(recordPath), ".json") {
							t.Fatal("key lost its durable job binding", replay)
						}
					}
					switch replay.State {
					case "interrupted":
						replay, err = reopened.Trigger(BackupRequest{Resume: replay.ID}, "resume-after-sync-EIO")
						if err != nil {
							t.Fatal(err)
						}
					case "draining", "complete":
					default:
						t.Fatal(replay)
					}
					replay = waitBackup(t, reopened, string(replay.ID))
					if replay.State != "complete" {
						t.Fatal(replay)
					}
					manifest, err := operations.VerifyBackup(t.Context(), filepath.Join(reopened.root, string(replay.ID)))
					if err != nil {
						t.Fatal(err)
					}
					if priorBackup != "" && manifest.BackupID != priorBackup {
						t.Fatal("resumption captured a new point")
					}
					history, _ = reopened.History()
					if len(history) != 1 {
						t.Fatal("same key created a second job", history)
					}
				}
				t.Logf("scope=%s fault=%s errno=EIO rename_visible=%t replay_single_job=true", scope, stage, stage == fsutil.AtomicDirectorySync)
			})
		}
	}
}

func TestBackupAtomicAfterFileSyncHardStops(t *testing.T) {
	for _, scope := range []string{"job_initial", "job_final", "schedule_slot"} {
		t.Run(scope, func(t *testing.T) {
			evidence := stopAtDurableBoundaryRequest(t, durableBoundaryRequest{Boundary: "atomic." + scope + ".after_file_sync", AtomicScope: scope})
			// The temp had a successful real Sync, but rename was not executed. Existing
			// authoritative bytes must remain the old complete JSON or be absent.
			pending := readWholeAtomicJSON(t, evidence.TempPath)
			if backupDigest(pending) != evidence.TempSHA256 {
				t.Fatal("pending bytes changed after kill")
			}
			if evidence.TargetExists {
				if got := readWholeAtomicJSON(t, evidence.RecordPath); backupDigest(got) != evidence.TargetSHA256 {
					t.Fatal("unrenamed temp replaced target")
				}
			} else {
				if _, err := os.Stat(evidence.RecordPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			_, b := reopenDurableBoundary(t, evidence)
			history, _ := b.History()
			if scope == "job_initial" {
				if len(history) != 0 {
					t.Fatal("synced temp was adopted as accepted job", history)
				}
				job, err := b.Trigger(BackupRequest{}, durableIntent)
				if err != nil {
					t.Fatal(err)
				}
				if job.ID == evidence.JobID {
					t.Fatal("unaccepted temporary receipt was trusted")
				}
				job = waitBackup(t, b, string(job.ID))
				if job.State != "complete" {
					t.Fatal(job)
				}
				if _, err = operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(job.ID))); err != nil {
					t.Fatal(err)
				}
			} else {
				if len(history) != 1 || history[0].ID != evidence.JobID || history[0].State != "interrupted" {
					t.Fatal(history)
				}
				if scope == "schedule_slot" {
					b.runSchedule(evidence.Clock.Add(20*time.Second), false)
					history, _ = b.History()
					if len(history) != 1 {
						t.Fatal("same day duplicated after slot temp crash", history)
					}
				} else {
					replay, err := b.Trigger(BackupRequest{}, durableIntent)
					if err != nil || replay.ID != evidence.JobID {
						t.Fatal(replay, err)
					}
				}
				resumed, err := b.Trigger(BackupRequest{Resume: evidence.JobID}, "resume-after-file-sync-stop")
				if err != nil {
					t.Fatal(err)
				}
				resumed = waitBackup(t, b, string(resumed.ID))
				if resumed.State != "complete" {
					t.Fatal(resumed)
				}
				manifest, err := operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(resumed.ID)))
				if err != nil {
					t.Fatal(err)
				}
				if evidence.BackupID != "" && manifest.BackupID != evidence.BackupID {
					t.Fatal("old captured point was rebound")
				}
				history, _ = b.History()
				if len(history) != 1 {
					t.Fatal("resumption created duplicate", history)
				}
			}
			t.Logf("scope=%s stage=after_file_sync-before_rename signal=SIGKILL temporary_receipt_not_adopted=true", scope)
		})
	}
}
