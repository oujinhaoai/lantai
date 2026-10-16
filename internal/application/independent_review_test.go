//go:build linux || darwin

package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
)

// Review-only tests: every instance, credential and backup is synthetic.
func TestIndependentReviewResumeAfterCaptureResetRename(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	realRun := b.run
	b.run = func(c context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
		opts.CheckDestination = func(context.Context) error { return errors.New("review: interrupt before capture") }
		return realRun(c, opts)
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	j = waitBackup(t, b, string(j.ID))
	if j.State != "failed" || !j.BackupID.Valid() {
		t.Fatal(j)
	}

	// Materialize the durable crash state immediately after backup.go's
	// os.Rename(rec.Destination, rec.RetiredDestination), before mkdir.
	target := filepath.Join(b.root, string(j.ID))
	record := filepath.Join(a.Instance.Layout().Home, "backups", "records", string(j.BackupID)+".json")
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err = json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	attempt, err := a.Instance.IDs().New()
	if err != nil {
		t.Fatal(err)
	}
	retired := target + ".incomplete-" + string(attempt)
	rec["retired_destination"] = retired
	raw, err = json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(record, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(target, retired); err != nil {
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
	t.Cleanup(func() { _ = a2.Close(context.Background()) })
	b2, err := a2.EnableBackups(t.Context(), root, "synthetic-volume")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b2.Start(BackupRequest{Resume: j.ID}); err != nil {
		t.Fatal(err)
	}
	got := waitBackup(t, b2, string(j.ID))
	t.Logf("online resume after durable reset rename: state=%s error=%s backup_id=%s", got.State, got.Error, got.BackupID)
	// Prove the lower-level existing reset journal is recoverable without
	// changing any captured files or replacing the backup ID.
	_, directErr := a2.Backup(t.Context(), operations.BackupOptions{BackupID: j.BackupID, Destination: target})
	t.Logf("same backup through lower-level journal recovery: err=%v", directErr)
	if directErr != nil {
		t.Fatal(directErr)
	}
	if got.State != "complete" {
		t.Fatalf("BUG: online coordinator rejected recoverable reset journal: %+v", got)
	}
}

func TestIndependentReviewRealCopyCancelHoldsUntilReturn(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	realRun := b.run
	entered, release := make(chan struct{}), make(chan struct{})
	b.run = func(c context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
		original := opts.CheckDestination
		checks := 0
		opts.CheckDestination = func(c context.Context) error {
			checks++
			if checks == 2 {
				close(entered)
				<-release
			}
			return original(c)
		}
		return realRun(c, opts)
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("real copy did not enter")
	}
	if _, err = b.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if a.Ready() {
		t.Fatal("cancel resumed business before real copy returned")
	}
	if _, _, err = a.Instance.Gate().Acquire(t.Context(), commands.Request{}); errcode.CodeOf(err) != errcode.MaintenanceMode {
		t.Fatal(err)
	}
	if _, err = b.Start(BackupRequest{}); !errors.Is(err, ErrBackupBusy) {
		t.Fatal(err)
	}
	close(release)
	got := waitBackup(t, b, string(j.ID))
	if got.State != "cancelled" || !a.Ready() {
		t.Fatalf("%+v ready=%v", got, a.Ready())
	}
	cancelledID := got.BackupID
	if _, err = operations.VerifyBackup(t.Context(), filepath.Join(b.root, string(j.ID))); err == nil {
		t.Fatal("cancelled partial backup verified")
	}
	b.run = realRun
	if _, err = b.Start(BackupRequest{Resume: j.ID}); err != nil {
		t.Fatal(err)
	}
	got = waitBackup(t, b, string(j.ID))
	if got.State != "complete" || got.BackupID != cancelledID {
		t.Fatal(got)
	}
	t.Log("real copy cancel held maintenance, partial refused, same task resumed and verified")
}

func TestIndependentReviewSourceSymlinkRejected(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	secret := filepath.Join(t.TempDir(), "synthetic-key")
	if err := os.WriteFile(secret, []byte("synthetic-never-copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a.Instance.Layout().Home, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(a.Instance.Layout().Home, "logs", "unsafe-link")); err != nil {
		t.Fatal(err)
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := waitBackup(t, b, string(j.ID))
	if got.State != "failed" || got.CaptureStarted || !a.Ready() {
		t.Fatalf("unsafe source not rejected before capture: %+v", got)
	}
	t.Log("source symlink refused before any snapshot; readiness restored")
}

func TestIndependentReviewOrphanAtomicJournalTempDoesNotBlockStartup(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitBackup(t, b, string(j.ID)); got.State != "complete" {
		t.Fatal(got)
	}
	// fsutil.WriteFileAtomic creates .<id>.json.tmp-<random> before renaming.
	// A process kill or power loss can leave this file alongside the last valid journal.
	tmp, err := os.CreateTemp(b.journalDir(), "."+string(j.ID)+".json.tmp-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tmp.Write([]byte(`{"id":"interrupted`)); err != nil {
		t.Fatal(err)
	}
	if err = tmp.Close(); err != nil {
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
	t.Cleanup(func() { _ = a2.Close(context.Background()) })
	_, err = a2.EnableBackups(t.Context(), root, "synthetic-volume")
	t.Logf("enable backup controller after interrupted atomic journal write: %v", err)
	if err != nil {
		t.Fatalf("BUG: leftover atomic temp prevents configured core from starting: %v", err)
	}
}

func TestIndependentReviewRepeatedDrainTimeoutAndRecovery(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	old, ok := a.business.enter()
	if !ok {
		t.Fatal("business rejected")
	}
	j, err := b.Start(BackupRequest{DrainSeconds: 1, TimeoutSeconds: 1})
	if err != nil {
		old()
		t.Fatal(err)
	}
	got := waitBackup(t, b, string(j.ID))
	if got.Error != "timeout" {
		old()
		t.Fatal(got)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	b.run = func(c context.Context, _ operations.BackupOptions) (operations.BackupManifest, error) {
		close(entered)
		<-release
		return operations.BackupManifest{}, c.Err()
	}
	j, err = b.Start(BackupRequest{})
	if err != nil {
		old()
		t.Fatal(err)
	}
	select {
	case <-entered:
		old()
		close(release)
		t.Fatal("second backup bypassed surviving reader")
	case <-time.After(30 * time.Millisecond):
	}
	old()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old reader release did not drain next generation")
	}
	close(release)
	if got = waitBackup(t, b, string(j.ID)); got.State != "complete" {
		t.Fatal(got)
	}
	t.Log("old reader survived timeout and was included in next drain")
}

func TestIndependentReviewPrivateHTTPInputBoundaries(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	for _, body := range []string{`{"destination":"/tmp/review-outside"}`, `{"timeout_seconds":-1}`, `{"drain_seconds":3,"timeout_seconds":2}`, `{} {}`, `{"resume":"../../etc/passwd"}`, strings.Repeat(" ", 2048) + `{}`} {
		r := httptest.NewRequest("POST", "http://lantai-backup/backup", strings.NewReader(body))
		r.Header.Set("X-Lantai-Instance", string(a.Instance.InstanceID()))
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid body accepted: %q status=%d", body, w.Code)
		}
	}
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		r := httptest.NewRequest(method, "http://localhost/backup", strings.NewReader(`{}`))
		r.Header.Set("Origin", "https://example.invalid")
		r.Header.Set("X-Lantai-Instance", string(a.Instance.InstanceID()))
		w := httptest.NewRecorder()
		b.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(method, w.Code)
		}
	}
	t.Log("paths, invalid timeouts, trailing/oversize JSON and browser-origin requests rejected")
}

func TestIndependentReviewLiveListenersStayGatedThroughFinalReturn(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: "split", true: "merged"}[merged], func(t *testing.T) {
			a := openTestApp(t)
			b := testBackupControl(t, a)
			cfg := a.Instance.Config()
			cfg.Listen.API, cfg.Listen.Transfer, cfg.Listen.Operations = "127.0.0.1:0", "127.0.0.1:0", "127.0.0.1:0"
			cfg.Listen.Merged = merged
			s, err := a.StartHTTP(t.Context(), cfg, WithAccessLog(io.Discard))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Shutdown(context.Background())
			realRun := b.run
			verified, release := make(chan struct{}), make(chan struct{})
			b.run = func(c context.Context, o operations.BackupOptions) (operations.BackupManifest, error) {
				m, e := realRun(c, o)
				if e == nil {
					close(verified)
					<-release
				}
				return m, e
			}
			j, err := b.Start(BackupRequest{})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-verified:
			case <-time.After(20 * time.Second):
				t.Fatal("real backup not verified")
			}
			client := &http.Client{Timeout: 3 * time.Second}
			for _, addr := range []string{s.Addresses.API, s.Addresses.Transfer} {
				for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
					r, _ := http.NewRequestWithContext(t.Context(), method, "http://"+addr+"/api/v1/projects", nil)
					resp, e := client.Do(r)
					if e != nil {
						close(release)
						t.Fatal(e)
					}
					_ = resp.Body.Close()
					if resp.StatusCode != 503 {
						close(release)
						t.Fatalf("business listener %s %s: %d", addr, method, resp.StatusCode)
					}
				}
			}
			if _, _, e := a.Instance.Gate().Acquire(t.Context(), commands.Request{}); errcode.CodeOf(e) != errcode.MaintenanceMode {
				close(release)
				t.Fatal(e)
			}
			close(release)
			if got := waitBackup(t, b, string(j.ID)); got.State != "complete" || !a.Ready() {
				t.Fatal(got)
			}
			t.Log("actual API/transfer listeners rejected every tested method until verified backup returned")
		})
	}
}

func TestIndependentReviewDestinationPermissions(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	defer os.Chmod(b.root, 0700)
	if err := os.Chmod(b.root, 0500); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(BackupRequest{}); err == nil {
		t.Fatal("unwritable destination accepted")
	}
	if !a.Ready() {
		t.Fatal("preflight failure changed business readiness")
	}
	if err := os.Chmod(b.root, 0777); err != nil {
		t.Fatal(err)
	}
	if err := b.preflight(); err == nil {
		t.Fatal("shared destination root accepted")
	}
	if !a.Ready() {
		t.Fatal("shared root preflight changed business readiness")
	}
	t.Log("unwritable and shared roots rejected without changing business readiness")
}
