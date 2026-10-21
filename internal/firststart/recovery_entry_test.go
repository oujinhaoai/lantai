//go:build linux || darwin

package firststart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func entryCall(t *testing.T, e *Entry, path string, body any, authorized bool) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if b, ok := body.([]byte); ok {
		raw = b
	} else {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest("POST", origin+path, bytes.NewReader(raw))
	r.Header.Set("Origin", origin)
	if _, ok := body.([]byte); ok {
		r.Header.Set("Content-Type", "application/octet-stream")
	} else {
		r.Header.Set("Content-Type", "application/json")
	}
	if authorized {
		r.Header.Set("Authorization", "Bearer "+e.code)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	return w
}

func TestEntryCompletePackageRecoveryWithoutOriginalDevice(t *testing.T) {
	ctx := t.Context()
	sourceOpts, sourceSetup, sourceSecrets := fixture(t)
	source := openSession(t, sourceOpts, sourceSetup)
	commitAdmin(t, source)
	if err := source.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := application.Open(ctx, application.Options{Instance: sourceOpts})
	if err != nil {
		t.Fatal(err)
	}
	packDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(packDir, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := a.Backup(ctx, operations.BackupOptions{Destination: filepath.Join(packDir, "snapshot")})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(sourceOpts.Home + ".unavailable")
		_ = os.RemoveAll(sourceSecrets + ".unavailable")
	})
	if err = os.Rename(sourceOpts.Home, sourceOpts.Home+".unavailable"); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(sourceSecrets, sourceSecrets+".unavailable"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(operations.RecoveryPackagePath(filepath.Join(packDir, "snapshot")))
	if err != nil {
		t.Fatal(err)
	}
	opts, setup, secrets := fixture(t)
	// A container mounts only the immutable input template. The active config
	// must be owned by the data volume, so the recovery worker can remove and
	// atomically replace it while the root filesystem stays read-only.
	configPath := filepath.Join(opts.Home, "config.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(t.TempDir(), "config.template.yaml")
	if err = os.WriteFile(template, config, 0400); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err = SeedConfigTemplate(opts.Home, template); err != nil {
		t.Fatal(err)
	}
	e, err := OpenEntry(ctx, opts, setup)
	if err != nil || e == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	entries, err := os.ReadDir(opts.Home)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if ent.Name() != "config.yaml" && ent.Name() != "lantai.lock" {
			t.Fatal("entry created business files before choice", ent.Name())
		}
	}
	if w := entryCall(t, e, "/setup/recovery/upload", raw, false); w.Code != 401 {
		t.Fatalf("unclaimed upload: %d", w.Code)
	}
	if w := entryCall(t, e, "/setup/recovery/upload", raw, true); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if w := entryCall(t, e, "/setup/recovery/restore", map[string]any{"package_sha256": e.state.Package.SHA256, "confirm_new_target": true}, true); w.Code != 200 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	e.wg.Wait()
	if e.state.Phase != "administrator" {
		t.Fatalf("restore phase: %+v", e.state)
	}
	k, err := masterkey.Load(secrets)
	if err != nil || k.ID() == m.KeyID {
		t.Fatal("new target key not rotated", err)
	}
	w := entryCall(t, e, "/setup/recovery/admin", map[string]any{"name": "ada", "password": "new restored administrator password"}, true)
	if w.Code != 200 {
		t.Fatalf("admin begin: %d %s", w.Code, w.Body.String())
	}
	var enrollment struct {
		Secret string   `json:"totp_secret"`
		Codes  []string `json:"recovery_codes"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &enrollment); err != nil || len(enrollment.Codes) == 0 {
		t.Fatal("codes not shown before commit", err)
	}
	w = entryCall(t, e, "/setup/recovery/admin-confirm", map[string]any{"totp_code": otp(t, enrollment.Secret, opts.Clock.Now()), "recovery_codes_saved": true}, true)
	if w.Code != 200 {
		t.Fatalf("admin confirm: %d %s", w.Code, w.Body.String())
	}
	// A crash after administrator commit must recover the confirmed identity,
	// and an interrupted activation must return to review with business closed.
	e.mu.Lock()
	e.state.Phase = "activating"
	err = e.save()
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = OpenEntry(ctx, opts, setup)
	if err != nil || e.state.Phase != "review" {
		t.Fatal("review restart failed", err)
	}
	w = entryCall(t, e, "/setup/recovery/review", map[string]any{}, true)
	if w.Code != 200 {
		t.Fatalf("review: %d %s", w.Code, w.Body.String())
	}
	var review recoveryReview
	if err = json.Unmarshal(w.Body.Bytes(), &review); err != nil || !review.AutomaticChecksPassed || len(review.Unknown) != 3 {
		t.Fatal("invalid core review", err, w.Body.String())
	}
	if w = entryCall(t, e, "/setup/recovery/complete", map[string]any{"review_id": review.ID, "known_changes_reviewed": false, "source_offline": true, "activate": true}, true); w.Code != 409 {
		t.Fatal("unreviewed changes accepted")
	}
	w = entryCall(t, e, "/setup/recovery/complete", map[string]any{"review_id": review.ID, "known_changes_reviewed": true, "accept_recovery_point": true, "source_offline": true, "activate": true}, true)
	if w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	if got := entryCall(t, e, "/setup/recovery/review", map[string]any{}, true); got.Code != 401 {
		t.Fatal("completed claim still authorized", got.Code)
	}
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	opts.Clock.(*clock.Fake).Advance(30 * time.Second)
	restored, err := application.Open(ctx, application.Options{Instance: opts})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(context.Background())
	if !restored.Ready() {
		t.Fatal("restored instance is not ready")
	}
	_, err = restored.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: "new restored administrator password", Code: otp(t, enrollment.Secret, opts.Clock.Now()), Channel: identity.ChannelCLI, Scopes: []identity.Scope{identity.ScopeRead}})
	if err != nil {
		t.Fatal("new administrator cannot use restored instance", err)
	}
}

func TestEntryRefusesExistingInstanceAndPreservesNewSetup(t *testing.T) {
	opts, setup, _ := fixture(t)
	e, err := OpenEntry(t.Context(), opts, setup)
	if err != nil {
		t.Fatal(err)
	}
	w := entryCall(t, e, "/setup/begin", map[string]any{"name": "newadmin", "password": testPassword}, true)
	if w.Code != 200 {
		t.Fatalf("normal begin: %d %s", w.Code, w.Body.String())
	}
	var enrollment struct {
		ID     string `json:"enrollment_id"`
		Secret string `json:"totp_secret"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &enrollment)
	w = entryCall(t, e, "/setup/confirm", map[string]any{"enrollment_id": enrollment.ID, "totp_code": otp(t, enrollment.Secret, opts.Clock.Now()), "recovery_codes_saved": true}, true)
	if w.Code != 200 {
		t.Fatalf("normal confirm: %d %s", w.Code, w.Body.String())
	}
	_ = e.Close()
	next, err := OpenEntry(t.Context(), opts, setup)
	if err != nil || next != nil {
		t.Fatal("active instance re-entered recovery", err)
	}
}

func TestEntryInitializationCrashHandback(t *testing.T) {
	opts, setup, _ := fixture(t)
	e, err := OpenEntry(t.Context(), opts, setup)
	if err != nil {
		t.Fatal(err)
	}
	// Retain the last durable controller record before initialization. A crash
	// can leave this record while the normal setup has already created its
	// initializing marker and instance-bound claim.
	statePath := filepath.Join(setup, recoveryEntryFile)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	w := entryCall(t, e, "/setup/begin", map[string]any{"name": "newadmin", "password": testPassword}, true)
	if w.Code != 200 {
		t.Fatalf("normal begin: %d %s", w.Code, w.Body.String())
	}
	instanceID, claimCode := e.install.Instance.InstanceID(), e.install.code
	if err = e.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(statePath, before, 0600); err != nil {
		t.Fatal(err)
	}
	next, err := OpenEntry(t.Context(), opts, setup)
	if err != nil || next != nil {
		t.Fatal("interrupted initialization did not hand back to normal setup", err)
	}
	s := openSession(t, opts, setup)
	if s.Instance.InstanceID() != instanceID || s.code != claimCode {
		t.Fatal("restart replaced the initializing instance or claim")
	}
	commitAdmin(t, s)
	if err = s.finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// A stale selection record must never authorize recovery over an active
	// instance after normal setup is committed.
	if next, err = OpenEntry(t.Context(), opts, setup); err == nil || next != nil {
		t.Fatal("stale controller was accepted over an active instance")
	}
}

func TestEntryCancelTimeoutFailureAndResume(t *testing.T) {
	sourceOpts, sourceSetup, _ := fixture(t)
	source := openSession(t, sourceOpts, sourceSetup)
	commitAdmin(t, source)
	if err := source.finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = source.Close()
	a, err := application.Open(t.Context(), application.Options{Instance: sourceOpts})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(root, "snapshot")
	if _, err = a.Backup(t.Context(), operations.BackupOptions{Destination: snapshot}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(operations.RecoveryPackagePath(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"cancel", "timeout", "failure", "secrets-not-empty"} {
		t.Run(kind, func(t *testing.T) {
			opts, setup, secrets := fixture(t)
			e, err := OpenEntry(t.Context(), opts, setup)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = e.Close() }()
			if w := entryCall(t, e, "/setup/recovery/upload", raw, true); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			entered := make(chan struct{})
			if kind == "secrets-not-empty" {
				if err = os.WriteFile(filepath.Join(secrets, "keep"), []byte("synthetic existing target material"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			e.restoreStep = func(ctx context.Context) error {
				close(entered)
				if kind == "secrets-not-empty" {
					return nil
				}
				if kind == "failure" {
					return errors.New("synthetic failure")
				}
				<-ctx.Done()
				return ctx.Err()
			}
			if kind == "timeout" {
				e.restoreTimeout = 20 * time.Millisecond
			}
			begin := func() {
				t.Helper()
				if w := entryCall(t, e, "/setup/recovery/restore", map[string]any{"package_sha256": e.state.Package.SHA256, "confirm_new_target": true}, true); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			begin()
			<-entered
			if kind == "cancel" {
				if w := entryCall(t, e, "/setup/recovery/upload", raw, true); w.Code != 409 {
					t.Fatal("concurrent upload accepted", w.Code)
				}
				if w := entryCall(t, e, "/setup/recovery/cancel", map[string]any{}, true); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			e.wg.Wait()
			if e.state.Phase != "interrupted" && e.state.Phase != "failed" {
				t.Fatal("failure not closed", e.state.Phase)
			}
			// Simulate persisted intent followed by process loss before the
			// first target marker: restart must not remain "restoring" forever.
			if kind == "secrets-not-empty" {
				raw, er := os.ReadFile(filepath.Join(secrets, "keep"))
				if er != nil || string(raw) != "synthetic existing target material" {
					t.Fatal("existing target was modified", er)
				}
				if err = os.Remove(filepath.Join(secrets, "keep")); err != nil {
					t.Fatal(err)
				}
			}
			e.state.Phase = "restoring"
			if err = e.save(); err != nil {
				t.Fatal(err)
			}
			if err = e.Close(); err != nil {
				t.Fatal(err)
			}
			e, err = OpenEntry(t.Context(), opts, setup)
			if err != nil {
				t.Fatal("restart failed", err)
			}
			begin()
			e.wg.Wait()
			if e.state.Phase != "administrator" {
				t.Fatal("same-package resume failed", e.state.Phase, e.state.Error)
			}
		})
	}
}
