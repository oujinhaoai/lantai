//go:build linux || darwin

package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func stopBackupScheduler(b *BackupControl) { b.schedulerCancel(); <-b.schedulerDone }
func freezeBackupClock(b *BackupControl, at time.Time) {
	stopBackupScheduler(b)
	b.now = func() time.Time { return at }
}
func TestBackupManagementDurableIdempotencyAndScheduleCAS(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	freezeBackupClock(b, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	j, err := b.Trigger(BackupRequest{}, "approved-op-one")
	if err != nil {
		t.Fatal(err)
	}
	j = waitBackup(t, b, string(j.ID))
	if j.State != "complete" {
		t.Fatal(j)
	}
	same, err := b.Trigger(BackupRequest{}, "approved-op-one")
	if err != nil || same.ID != j.ID {
		t.Fatal(same, err)
	}
	if _, err = b.Trigger(BackupRequest{TimeoutSeconds: 3600}, "approved-op-one"); !errors.Is(err, backupmgmt.ErrIdempotencyConflict) {
		t.Fatal(err)
	}
	schedule := backupmgmt.Schedule{Enabled: true, Timezone: "Asia/Shanghai", Time: "03:00"}
	saved, err := saveBackupTestSchedule(t, b, schedule, "approved-plan-one")
	if err != nil || saved.Revision != 1 || saved.NextRunAt == nil {
		t.Fatal(saved, err)
	}
	if _, err = saveBackupTestSchedule(t, b, schedule, "approved-plan-two"); !errors.Is(err, backupmgmt.ErrRevisionConflict) {
		t.Fatal(err)
	}
	if _, err = saveBackupTestSchedule(t, b, backupmgmt.Schedule{Enabled: true, Timezone: "Local", Time: "03:00"}, "invalid-zone"); !errors.Is(err, backupmgmt.ErrInvalid) {
		t.Fatal(err)
	}
	root, home := b.root, a.Instance.Layout().Home
	if err = a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	a2, err := Open(t.Context(), Options{Instance: operations.Options{Home: home}, Identity: identity.Config{Password: testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close(context.Background())
	b2, err := a2.EnableBackups(t.Context(), root, "synthetic-volume")
	if err != nil {
		t.Fatal(err)
	}
	stopBackupScheduler(b2)
	same, err = b2.Trigger(BackupRequest{}, "approved-op-one")
	if err != nil || same.ID != j.ID {
		t.Fatal(same, err)
	}
	again, err := saveBackupTestSchedule(t, b2, schedule, "approved-plan-one")
	if err != nil || again.Revision != saved.Revision {
		t.Fatal(again, err)
	}
	overview, err := b2.Overview()
	if err != nil || !overview.Availability.Available || overview.LastSuccess == nil || overview.LastSuccess.ID != j.ID {
		t.Fatal(overview, err)
	}
}
func TestBackupManagementConcurrentIdempotencySingleRun(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	stopBackupScheduler(b)
	entered, release := make(chan struct{}), make(chan struct{})
	b.run = func(context.Context, operations.BackupOptions) (operations.BackupManifest, error) {
		close(entered)
		<-release
		return operations.BackupManifest{}, nil
	}
	const count = 12
	results := make(chan ids.ID, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := b.Trigger(BackupRequest{}, "one-shared-operation")
			results <- j.ID
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	var first ids.ID
	for id := range results {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("multiple jobs")
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	<-entered
	if _, err := b.Trigger(BackupRequest{}, "another-operation"); !errors.Is(err, ErrBackupBusy) {
		t.Fatal(err)
	}
	close(release)
	waitBackup(t, b, string(first))
	history, _ := b.History()
	if len(history) != 1 {
		t.Fatal(history)
	}
}
func TestBackupManagementOptionalRootAndCorruptJournal(t *testing.T) {
	for _, mode := range []string{"missing", "unconfigured", "wrong-marker", "corrupt", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			a := openTestApp(t)
			root := filepath.Join(t.TempDir(), "missing")
			rootID := "volume"
			if mode == "unconfigured" {
				root, rootID = "", ""
			}
			if mode == "wrong-marker" {
				root, _ = filepath.EvalSymlinks(t.TempDir())
				if err := os.WriteFile(filepath.Join(root, BackupRootMarker), []byte("wrong\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupt" || mode == "symlink" {
				backups := filepath.Join(a.Instance.Layout().Home, "backups")
				if err := os.MkdirAll(backups, 0700); err != nil {
					t.Fatal(err)
				}
				if mode == "corrupt" {
					if err := os.WriteFile(filepath.Join(backups, "management.json"), []byte(`{"version":1,"version":2}`), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Symlink(t.TempDir(), filepath.Join(backups, "online")); err != nil {
						t.Fatal(err)
					}
				}
			}
			b := a.ConfigureBackups(t.Context(), root, rootID)
			stopBackupScheduler(b)
			view, _ := b.Overview()
			if view.Availability.Available || view.Availability.Error == "" {
				t.Fatal(view)
			}
			if !a.Ready() {
				t.Fatal("optional backup configuration blocked business")
			}
			if _, err := b.Trigger(BackupRequest{}, "try-unavailable"); !errors.Is(err, backupmgmt.ErrUnavailable) {
				t.Fatal(err)
			}
			if mode == "missing" {
				if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing mount was created")
				}
			}
			if mode == "corrupt" || mode == "symlink" {
				if view.Availability.Error != "journal_invalid" {
					t.Fatal(view)
				}
			}
		})
	}
}
func TestBackupManagementMissingRootCanRecoverWithoutRestart(t *testing.T) {
	a := openTestApp(t)
	root := filepath.Join(t.TempDir(), "later")
	root, _ = filepath.Abs(root)
	root = filepath.Join(filepath.Dir(root), "later")
	// Resolve the existing temp parent because macOS /var aliases /private/var.
	parent, _ := filepath.EvalSymlinks(filepath.Dir(root))
	root = filepath.Join(parent, "later")
	b := a.ConfigureBackups(t.Context(), root, "volume")
	stopBackupScheduler(b)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, BackupRootMarker), []byte("volume\n"), 0600); err != nil {
		t.Fatal(err)
	}
	j, err := b.Trigger(BackupRequest{}, "after-mount")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitBackup(t, b, string(j.ID)); got.State != "complete" {
		t.Fatal(got)
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, BackupRootMarker), []byte("volume\n"), 0600); err != nil {
		t.Fatal(err)
	}
	view, _ := b.Overview()
	if view.Availability.Available {
		t.Fatal("replaced root accepted")
	}
}
func TestBackupScheduleDSTBoundaries(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	spring, exists := dailyOccurrence("2026-03-08", "02:30", loc)
	if exists || spring.In(loc).Format("15:04") != "03:00" {
		t.Fatal(spring, exists)
	}
	fall, exists := dailyOccurrence("2026-11-01", "01:30", loc)
	if !exists || fall.UTC().Format(time.RFC3339) != "2026-11-01T05:30:00Z" {
		t.Fatal(fall, exists)
	}
	for _, zone := range []string{"Asia/Shanghai", "Pacific/Apia", "Europe/Berlin", "Australia/Lord_Howe"} {
		loc, err = time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		at, exists := dailyOccurrence("2026-10-01", "03:30", loc)
		if !exists || at.In(loc).Format("2006-01-02 15:04") != "2026-10-01 03:30" {
			t.Fatal(zone, at, exists)
		}
	}
}
func TestBackupScheduleSingleDailySlotBusyMissedAndRestart(t *testing.T) {
	for _, mode := range []string{"normal", "busy", "missed", "restart", "dst"} {
		t.Run(mode, func(t *testing.T) {
			a := openTestApp(t)
			b := testBackupControl(t, a)
			now := time.Date(2026, 10, 1, 1, 59, 0, 0, time.UTC)
			zone, wall := "UTC", "02:00"
			if mode == "dst" {
				now = time.Date(2026, 3, 8, 6, 59, 0, 0, time.UTC)
				zone, wall = "America/New_York", "02:30"
			}
			freezeBackupClock(b, now)
			s, err := saveBackupTestSchedule(t, b, backupmgmt.Schedule{Enabled: true, Timezone: zone, Time: wall}, "plan")
			if err != nil {
				t.Fatal(err)
			}
			var manual BackupJob
			release := make(chan struct{})
			if mode == "busy" {
				entered := make(chan struct{})
				b.run = func(context.Context, operations.BackupOptions) (operations.BackupManifest, error) {
					close(entered)
					<-release
					return operations.BackupManifest{}, nil
				}
				manual, err = b.Start(BackupRequest{})
				if err != nil {
					t.Fatal(err)
				}
				<-entered
			}
			tick := s.NextRunAt.Add(10 * time.Second)
			if mode == "missed" {
				tick = tick.Add(time.Hour)
			}
			b.runSchedule(tick, mode == "restart")
			if mode == "busy" {
				close(release)
				waitBackup(t, b, string(manual.ID))
			}
			overview, _ := b.Overview()
			if len(overview.RecentScheduleEvents) != 1 {
				t.Fatal(overview)
			}
			event := overview.RecentScheduleEvents[0]
			want := map[string]string{"normal": "started", "busy": "skipped_busy", "missed": "missed", "restart": "missed", "dst": "dst_missing"}[mode]
			if event.State != want {
				t.Fatal(event, want)
			}
			if mode == "normal" {
				j := waitBackup(t, b, string(event.JobID))
				if j.State != "complete" {
					t.Fatal(j)
				}
				if _, err := operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(j.ID))); err != nil {
					t.Fatal(err)
				}
				info, err := operations.VerifyRecoveryPackage(t.Context(), operations.RecoveryPackagePath(filepath.Join(b.root, string(j.ID))))
				if err != nil || j.Package == nil || info.SHA256 != j.Package.SHA256 || info.Manifest.BackupID != j.BackupID {
					t.Fatal("scheduled backup lacks complete recovery artifact", err)
				}
				t.Logf("scheduled complete package: job=%s backup=%s sha256=%s", j.ID, j.BackupID, info.SHA256)
			}
			// Repeated and backward-clock ticks must not create another job for this day.
			for range 3 {
				b.runSchedule(tick, false)
			}
			after, _ := b.Overview()
			if len(after.RecentScheduleEvents) != 1 || after.Schedule.NextRunAt == nil || !after.Schedule.NextRunAt.After(tick) {
				t.Fatal(after)
			}
		})
	}
}
func TestBackupManagementMalformedJournalCannotInventJobs(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	stopBackupScheduler(b)
	j, err := b.Trigger(BackupRequest{}, "durable-op")
	if err != nil {
		t.Fatal(err)
	}
	waitBackup(t, b, string(j.ID))
	root, home := b.root, a.Instance.Layout().Home
	if err = a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, "backups", "online", string(j.ID)+".json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"request_receipts":[`, `"request_receipts":[{"key":"forged","hash":"bad"},`, 1))
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	a2, err := Open(t.Context(), Options{Instance: operations.Options{Home: home}, Identity: identity.Config{Password: testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close(context.Background())
	b2 := a2.ConfigureBackups(t.Context(), root, "synthetic-volume")
	stopBackupScheduler(b2)
	view, _ := b2.Overview()
	if view.Availability.Error != "journal_invalid" || !a2.Ready() {
		t.Fatal(view)
	}
	if _, err = b2.Trigger(BackupRequest{}, "durable-op"); !errors.Is(err, backupmgmt.ErrUnavailable) {
		t.Fatal(err)
	}
}

func saveBackupTestSchedule(t *testing.T, b *BackupControl, s backupmgmt.Schedule, key string) (backupmgmt.Schedule, error) {
	t.Helper()
	var owner ids.ID
	if err := b.app.Instance.DB(ownership.Main).QueryRowContext(t.Context(), "SELECT principal_id FROM identity_system_roles WHERE role = 'admin' LIMIT 1").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	s.AuthorizedBy = owner
	return b.SaveSchedule(s, key)
}

func TestBackupManagementRevokedOwnerAndDisabledPlanNeverTrigger(t *testing.T) {
	for _, mode := range []string{"revoked", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			a := openTestApp(t)
			b := testBackupControl(t, a)
			freezeBackupClock(b, time.Date(2026, 10, 1, 1, 59, 0, 0, time.UTC))
			plan, err := saveBackupTestSchedule(t, b, backupmgmt.Schedule{Enabled: true, Timezone: "UTC", Time: "02:00"}, "approved-plan")
			if err != nil {
				t.Fatal(err)
			}
			due := *plan.NextRunAt
			if mode == "disabled" {
				plan.Enabled = false
				if _, err = saveBackupTestSchedule(t, b, plan, "disable-plan"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = a.Instance.DB(ownership.Main).ExecContext(t.Context(), "DELETE FROM identity_system_roles WHERE principal_id=? AND role='admin'", plan.AuthorizedBy); err != nil {
					t.Fatal(err)
				}
			}
			b.runSchedule(due, false)
			view, _ := b.Overview()
			history, _ := b.History()
			if len(history) != 0 || view.Schedule.Enabled {
				t.Fatal(view, history)
			}
			if mode == "revoked" && (view.Schedule.DisabledReason != "authorization_revoked" || len(view.RecentScheduleEvents) != 1 || view.RecentScheduleEvents[0].State != "authorization_revoked") {
				t.Fatal(view)
			}
		})
	}
}
func TestBackupManagementUnsafeRootPermissionsAreRejected(t *testing.T) {
	for _, mode := range []string{"root", "marker", "hardlink", "large-marker"} {
		t.Run(mode, func(t *testing.T) {
			a := openTestApp(t)
			b := testBackupControl(t, a)
			stopBackupScheduler(b)
			marker := filepath.Join(b.root, BackupRootMarker)
			switch mode {
			case "root":
				if err := os.Chmod(b.root, 0777); err != nil {
					t.Fatal(err)
				}
			case "marker":
				if err := os.Chmod(marker, 0644); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(marker, filepath.Join(b.root, "marker-link")); err != nil {
					t.Fatal(err)
				}
			case "large-marker":
				if err := os.WriteFile(marker, []byte(strings.Repeat("x", 1<<20)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			view, _ := b.Overview()
			if view.Availability.Available {
				t.Fatal(view)
			}
			if _, err := b.Trigger(BackupRequest{}, "unsafe-root"); !errors.Is(err, backupmgmt.ErrUnavailable) {
				t.Fatal(err)
			}
			if !a.Ready() {
				t.Fatal("failed root preflight changed business availability")
			}
		})
	}
}

func TestBackupScheduleTransientAuthorityFailureDoesNotConsumeFutureDay(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	now := time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC)
	freezeBackupClock(b, now)
	plan, err := saveBackupTestSchedule(t, b, backupmgmt.Schedule{Enabled: true, Timezone: "UTC", Time: "02:00"}, "plan-transient")
	if err != nil {
		t.Fatal(err)
	}
	b.authorizeSchedule = func(context.Context, ids.ID, func() error) error { return errors.New("temporary database busy") }
	b.runSchedule(now, false)
	view, _ := b.Overview()
	if !view.Schedule.Enabled || view.Schedule.DisabledReason != "authorization_check_failed" || len(view.RecentScheduleEvents) != 0 || !view.Schedule.NextRunAt.Equal(*plan.NextRunAt) {
		t.Fatal(view)
	}
	b.authorizeSchedule = nil
	b.runSchedule(now.Add(10*time.Second), false)
	view, _ = b.Overview()
	if view.Schedule.DisabledReason != "" || len(view.RecentScheduleEvents) != 0 {
		t.Fatal(view)
	}
	b.runSchedule(*plan.NextRunAt, false)
	view, _ = b.Overview()
	if len(view.RecentScheduleEvents) != 1 || view.RecentScheduleEvents[0].State != "started" {
		t.Fatal(view)
	}
	j := waitBackup(t, b, string(view.RecentScheduleEvents[0].JobID))
	if j.State != "complete" || j.ScheduleRevision != plan.Revision {
		t.Fatal(j)
	}
}

func TestBackupScheduleDisableWinsBeforeFinalAdmission(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	freezeBackupClock(b, time.Date(2026, 10, 1, 1, 59, 0, 0, time.UTC))
	plan, err := saveBackupTestSchedule(t, b, backupmgmt.Schedule{Enabled: true, Timezone: "UTC", Time: "02:00"}, "plan-before-disable")
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b.authorizeSchedule = func(ctx context.Context, id ids.ID, admit func() error) error {
		close(entered)
		<-release
		return a.Identity.WithBackupScheduleOwner(ctx, id, admit)
	}
	go func() { defer close(done); b.runSchedule(*plan.NextRunAt, false) }()
	<-entered
	plan.Enabled = false
	if _, err = saveBackupTestSchedule(t, b, plan, "disable-before-admission"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	history, _ := b.History()
	view, _ := b.Overview()
	if len(history) != 0 || view.Schedule.Enabled {
		t.Fatal(view, history)
	}
}

func TestBackupManagementCancelSurvivesJournalWriteFailure(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	stopBackupScheduler(b)
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b.run = func(ctx context.Context, _ operations.BackupOptions) (operations.BackupManifest, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return operations.BackupManifest{}, ctx.Err()
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	file := filepath.Join(b.journalDir(), string(j.ID)+".json")
	if err = os.Rename(file, file+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Cancel(j.ID); err == nil {
		t.Fatal("expected durable journal failure")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("failed journal prevented cancellation")
	}
	if a.Ready() {
		t.Fatal("cancellation released active copier early")
	}
	close(release)
	got := waitBackup(t, b, string(j.ID))
	if got.Error != "journal_write_failed" || !a.Ready() {
		t.Fatal(got)
	}
	view, _ := b.Overview()
	if view.Availability.Error != "journal_invalid" {
		t.Fatal(view)
	}
}

func TestBackupManagementActiveOverviewDefersExternalFilesystemProbe(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	stopBackupScheduler(b)
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b.run = func(ctx context.Context, _ operations.BackupOptions) (operations.BackupManifest, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return operations.BackupManifest{}, ctx.Err()
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	// Losing the external marker would make a fresh probe fail. Management must
	// return the explicitly deferred admission result without touching that root.
	if err = os.Remove(filepath.Join(b.root, BackupRootMarker)); err != nil {
		t.Fatal(err)
	}
	result := make(chan backupmgmt.Overview, 1)
	go func() { view, _ := b.Overview(); result <- view }()
	select {
	case view := <-result:
		if !view.Availability.Available || !view.Availability.CheckDeferred || view.Availability.CheckedAt == nil || view.Current == nil || view.Current.ID != j.ID {
			t.Fatal(view)
		}
	case <-time.After(time.Second):
		t.Fatal("active overview blocked")
	}
	if got, err := b.Status(j.ID); err != nil || got.ID != j.ID {
		t.Fatal(got, err)
	}
	if _, err = b.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel waited on external storage")
	}
	if a.Ready() {
		t.Fatal("cancel released worker protection early")
	}
	close(release)
	waitBackup(t, b, string(j.ID))
	view, _ := b.Overview()
	if view.Availability.Available || view.Availability.CheckDeferred {
		t.Fatal("idle probe did not observe destination loss", view)
	}
}
