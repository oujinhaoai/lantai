//go:build linux || darwin

package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func testBackupControl(t *testing.T, a *App) *BackupControl {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, BackupRootMarker), []byte("synthetic-volume\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := a.EnableBackups(t.Context(), root, "synthetic-volume")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func waitBackup(t *testing.T, b *BackupControl, id string) BackupJob {
	t.Helper()
	until := time.After(20 * time.Second)
	for {
		b.mu.Lock()
		j := b.jobs[ids.ID(id)]
		active := b.active == j.ID
		b.mu.Unlock()
		if !active {
			return j
		}
		select {
		case <-until:
			t.Fatal("backup did not finish")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestOnlineBackupDrainsHTTPAndBackgroundKeepsWholeBackupExclusive(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	entered, release := make(chan struct{}), make(chan struct{})
	oldDone := make(chan struct{})
	handler := a.business.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release; w.WriteHeader(204) }))
	go func() {
		defer close(oldDone)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/download", nil))
	}()
	<-entered
	_, held, err := a.Instance.Gate().Acquire(t.Context(), commands.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	copying, finish := make(chan struct{}), make(chan struct{})
	b.run = func(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
		if err := a.Instance.Gate().RequireMaintenance(ctx); err != nil {
			return operations.BackupManifest{}, err
		}
		close(copying)
		<-finish
		return operations.BackupManifest{}, ctx.Err()
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Ready() || a.Readiness().State != operations.StateMaintenance {
		t.Fatal("draining advertised ready")
	}
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, "/business", nil))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "MAINTENANCE_MODE") {
			t.Fatalf("new %s admitted: %d %s", method, w.Code, w.Body)
		}
	}
	if _, err = b.Start(BackupRequest{}); !errors.Is(err, ErrBackupBusy) {
		t.Fatalf("concurrent trigger: %v", err)
	}
	select {
	case <-copying:
		t.Fatal("backup passed active reader")
	default:
	}
	close(release)
	<-oldDone
	// Reader drain is followed by the common writer barrier. The synthetic held
	// operation is the same Gate contract used by background consumers.
	deadline := time.Now().Add(3 * time.Second)
	for a.Instance.State() != operations.StateMaintenance {
		if time.Now().After(deadline) {
			t.Fatal("no maintenance state")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-copying:
		t.Fatal("backup passed active background writer")
	default:
	}
	held.Release()
	<-copying
	if _, _, err = a.Instance.Gate().Acquire(t.Context(), commands.Request{}); errcode.CodeOf(err) != errcode.MaintenanceMode {
		t.Fatalf("writer admitted: %v", err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/business", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	close(finish)
	got := waitBackup(t, b, string(j.ID))
	if got.State != "complete" || !a.Instance.Readiness().Ready {
		t.Fatalf("end: %+v ready=%v", got, a.Instance.Readiness())
	}
}

func TestOnlineBackupCancelWaitsAndTimeoutRecovers(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	copying, finish := make(chan struct{}), make(chan struct{})
	b.run = func(ctx context.Context, _ operations.BackupOptions) (operations.BackupManifest, error) {
		close(copying)
		<-finish
		return operations.BackupManifest{}, ctx.Err()
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	<-copying
	if _, err = b.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if a.Instance.Readiness().Ready {
		t.Fatal("cancel released active copier")
	}
	if _, err = b.Start(BackupRequest{}); !errors.Is(err, ErrBackupBusy) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	if err = a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close: %v", err)
	}
	cancel()
	if _, err = operations.Open(t.Context(), operations.Options{Home: a.Instance.Layout().Home}); err == nil {
		t.Fatal("instance lock released early")
	}
	close(finish)
	got := waitBackup(t, b, string(j.ID))
	if got.State != "cancelled" {
		t.Fatal(got)
	}

}

func TestOnlineBackupDrainTimeoutRecovers(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)

	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done, ok := a.business.enter()
		if !ok {
			return
		}
		close(entered)
		<-release
		done()
	}()
	<-entered
	j, err := b.Start(BackupRequest{DrainSeconds: 1, TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := waitBackup(t, b, string(j.ID))
	close(release)
	<-finished
	if got.Error != "timeout" || !a.Instance.Readiness().Ready {
		t.Fatalf("timeout %+v", got)
	}
}

func TestOnlineBackupRealSnapshotAndResumeAfterRestartJournal(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := waitBackup(t, b, string(j.ID))
	if got.State != "complete" {
		t.Fatal(got)
	}
	dest := filepath.Join(b.root, string(j.ID))
	m, err := operations.VerifyBackup(t.Context(), dest)
	if err != nil {
		t.Fatal(err)
	}
	if m.BackupID != got.BackupID {
		t.Fatal("manifest/job mismatch")
	}
	if _, err = os.Stat(filepath.Join(dest, "secrets")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("secret material copied")
	}
	// Simulate a crash after COMPLETE, before persisting the control result.
	got.State = "backing_up"
	got.BackupID = ""
	if err = b.save(got); err != nil {
		t.Fatal(err)
	}
	root := b.root
	if err = a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	a2, err := Open(t.Context(), Options{Instance: operations.Options{Home: a.Instance.Layout().Home}, Identity: identity.Config{Password: testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close(context.Background())
	b2, err := a2.EnableBackups(t.Context(), root, "synthetic-volume")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := b2.Status(j.ID)
	if err != nil || recovered.State != "interrupted" {
		t.Fatal(recovered, err)
	}
	if _, err = b2.Start(BackupRequest{Resume: j.ID}); err != nil {
		t.Fatal(err)
	}
	got = waitBackup(t, b2, string(j.ID))
	if got.State != "complete" || got.BackupID != m.BackupID {
		t.Fatal(got)
	}
}

func TestOnlineBackupBoundaryRejectsPathsAndPublicAuthority(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	for _, body := range []string{`{"destination":"/tmp/escape"}`, `{"resume":"../../escape"}`, `{} {}`} {
		req := httptest.NewRequest("POST", "http://lantai-backup/backup", strings.NewReader(body))
		req.Header.Set("X-Lantai-Instance", string(a.Instance.InstanceID()))
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatalf("unsafe body %s: %d", body, w.Code)
		}
	}
	for _, origin := range []string{"https://example.test", "null"} {
		req := httptest.NewRequest("POST", "http://lantai-backup/backup", strings.NewReader(`{}`))
		req.Header.Set("Origin", origin)
		req.Header.Set("X-Lantai-Instance", string(a.Instance.InstanceID()))
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if err := os.WriteFile(filepath.Join(b.root, BackupRootMarker), []byte("wrong-volume"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(BackupRequest{}); err == nil {
		t.Fatal("wrong volume accepted")
	}
	if !a.Instance.Readiness().Ready {
		t.Fatal("preflight closed business")
	}
}

func TestOnlineBackupFailurePanicAndExpiredCopyDoNotLeakOrReleaseEarly(t *testing.T) {
	for _, mode := range []string{"failure", "panic", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			a := openTestApp(t)
			b := testBackupControl(t, a)
			entered, release := make(chan struct{}), make(chan struct{})
			b.run = func(ctx context.Context, _ operations.BackupOptions) (operations.BackupManifest, error) {
				close(entered)
				<-release
				if mode == "panic" {
					panic("synthetic failure")
				}
				if mode == "failure" {
					return operations.BackupManifest{}, errors.New("synthetic I/O failure")
				}
				return operations.BackupManifest{}, ctx.Err()
			}
			j, err := b.Start(BackupRequest{DrainSeconds: 1, TimeoutSeconds: 1})
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			if mode == "timeout" {
				time.Sleep(1100 * time.Millisecond)
			}
			if a.Instance.Readiness().Ready {
				t.Fatal("still active work lost maintenance")
			}
			if _, err = b.Start(BackupRequest{}); !errors.Is(err, ErrBackupBusy) {
				t.Fatal(err)
			}
			// Background attempts are refused without poisoning post-backup health.
			if err = a.syncCore(t.Context(), false); errcode.CodeOf(err) != errcode.MaintenanceMode {
				t.Fatal(err)
			}
			if err = a.syncTail(t.Context()); errcode.CodeOf(err) != errcode.MaintenanceMode {
				t.Fatal(err)
			}
			close(release)
			got := waitBackup(t, b, string(j.ID))
			if got.State != "failed" || !a.Ready() {
				t.Fatalf("failed cleanup %+v ready=%v", got, a.Ready())
			}
			if mode == "timeout" && got.Error != "timeout" {
				t.Fatal(got)
			}
		})
	}
}

func TestOnlineBackupDestinationLossAndExplicitResume(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	realRun := b.run
	b.run = func(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
		original := opts.CheckDestination
		checks := 0
		opts.CheckDestination = func(c context.Context) error {
			checks++
			if checks == 2 {
				if err := os.Remove(filepath.Join(b.root, BackupRootMarker)); err != nil {
					return err
				}
			}
			return original(c)
		}
		return realRun(ctx, opts)
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := waitBackup(t, b, string(j.ID))
	if got.State != "failed" || !got.BackupID.Valid() || !a.Ready() {
		t.Fatal(got)
	}
	if _, err = os.Stat(filepath.Join(b.root, string(j.ID), "COMPLETE")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete copy advertised complete")
	}
	if err = os.WriteFile(filepath.Join(b.root, BackupRootMarker), []byte("synthetic-volume\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b.run = realRun
	if _, err = b.Start(BackupRequest{Resume: j.ID}); err != nil {
		t.Fatal(err)
	}
	got = waitBackup(t, b, string(j.ID))
	if got.State != "complete" {
		t.Fatal(got)
	}
	if _, err = operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(j.ID))); err != nil {
		t.Fatal(err)
	}
}

func TestOnlineBackupPreflightRejectsAliasingAndRootReplacement(t *testing.T) {
	a := openTestApp(t)
	home, _ := filepath.EvalSymlinks(a.Instance.Layout().Home)
	for _, root := range []string{home, a.Instance.Config().SecretsDir, filepath.Join(home, "backups"), filepath.Join(t.TempDir(), "missing")} {
		if _, err := a.EnableBackups(t.Context(), root, "synthetic-volume"); err == nil {
			t.Fatal("unsafe root accepted", root)
		}
	}
	b := testBackupControl(t, a)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(b.root, link); err != nil {
		t.Fatal(err)
	}
	a.backupControl = nil
	if _, err := a.EnableBackups(t.Context(), link, "synthetic-volume"); err == nil {
		t.Fatal("root symlink accepted")
	}
	a.backupControl = b
	moved := b.root + "-old"
	if err := os.Rename(b.root, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	if err := os.Mkdir(b.root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.root, BackupRootMarker), []byte("synthetic-volume\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(BackupRequest{}); err == nil {
		t.Fatal("changed inode accepted with copied marker")
	}
}

// A real process is killed after the archive is complete but before the core
// records its result. Restart must discover an interrupted task, revalidate the
// existing point and finish it without creating a second backup ID.
func TestOnlineBackupKilledProcessResume(t *testing.T) {
	const env = "LANTAI_TEST_BACKUP_CRASH_HANDOFF"
	if handoff := os.Getenv(env); handoff != "" {
		a := openTestApp(t)
		b := testBackupControl(t, a)
		realRun := b.run
		b.run = func(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
			m, err := realRun(ctx, opts)
			if err != nil {
				return m, err
			}
			raw, _ := json.Marshal(map[string]string{"home": a.Instance.Layout().Home, "root": b.root, "id": filepath.Base(opts.Destination), "backup_id": string(m.BackupID)})
			if err = os.WriteFile(handoff, raw, 0600); err != nil {
				return m, err
			}
			select {}
		}
		if _, err := b.Start(BackupRequest{}); err != nil {
			t.Fatal(err)
		}
		select {}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	handoff := filepath.Join(t.TempDir(), "handoff.json")
	cmd := exec.Command(exe, "-test.run=^TestOnlineBackupKilledProcessResume$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), env+"="+handoff)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(40 * time.Second)
	var data map[string]string
	for time.Now().Before(deadline) {
		raw, e := os.ReadFile(handoff)
		if e == nil && json.Unmarshal(raw, &data) == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatal("helper did not reach durable point")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	killed = true
	defer os.RemoveAll(data["home"])
	defer os.RemoveAll(data["root"])
	a, err := Open(t.Context(), Options{Instance: operations.Options{Home: data["home"]}, Identity: identity.Config{Password: testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	b, err := a.EnableBackups(t.Context(), data["root"], "synthetic-volume")
	if err != nil {
		t.Fatal(err)
	}
	jobID := ids.ID(data["id"])
	j, err := b.Status(jobID)
	if err != nil || j.State != "interrupted" {
		t.Fatal(j, err)
	}
	if _, err = b.Start(BackupRequest{Resume: jobID}); err != nil {
		t.Fatal(err)
	}
	j = waitBackup(t, b, string(jobID))
	if j.State != "complete" || string(j.BackupID) != data["backup_id"] {
		t.Fatal(j)
	}
}

func TestOnlineBackupDrainsWholeBackgroundIterationBeforeApplicationMutex(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		a.backgroundOperation(func(context.Context) {
			a.coreMu.Lock()
			defer a.coreMu.Unlock()
			close(started)
			<-release
		})(t.Context())
	}()
	<-started
	capturing := make(chan struct{})
	b.run = func(ctx context.Context, _ operations.BackupOptions) (operations.BackupManifest, error) {
		a.coreMu.Lock()
		if err := a.Instance.Gate().RequireMaintenance(ctx); err != nil {
			a.coreMu.Unlock()
			return operations.BackupManifest{}, err
		}
		a.coreMu.Unlock()
		close(capturing)
		return operations.BackupManifest{}, nil
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-capturing:
		t.Fatal("backup passed an active background iteration")
	case <-time.After(20 * time.Millisecond):
	}
	ran := false
	a.backgroundOperation(func(context.Context) { ran = true })(t.Context())
	if ran {
		t.Fatal("new background iteration admitted")
	}
	close(release)
	<-finished
	got := waitBackup(t, b, string(j.ID))
	if got.State != "complete" {
		t.Fatal(got)
	}
}
