//go:build linux || darwin

package firststart

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
)

const origin = "https://library.example:32444"
const testPassword = "a-synthetic-password-for-tests"

func fixture(t *testing.T) (operations.Options, string, string) {
	t.Helper()
	root := t.TempDir()
	home, secret, setup := filepath.Join(root, "data"), filepath.Join(root, "secrets"), filepath.Join(root, "setup")
	for _, dir := range []string{home, secret, setup} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "contract: lantai.config/v1\nsecrets:\n  dir: " + secret + "\nstorage:\n  min_free_bytes: 1048576\nhttp:\n  allowed_origins:\n    - " + origin + "\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return operations.Options{Home: home, Clock: clock.NewFake(time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC))}, setup, secret
}

func openSession(t *testing.T, opts operations.Options, dir string) *Session {
	t.Helper()
	s, err := Open(t.Context(), opts, dir)
	if err != nil || s == nil {
		t.Fatalf("open pending session: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func otp(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return totp.Code(raw, totp.Counter(at))
}

func commitAdmin(t *testing.T, s *Session) []string {
	t.Helper()
	b, err := s.Identity.BeginBootstrap(t.Context(), identity.BootstrapRequest{Name: "ada", Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}
	codes, err := b.PrepareRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.Confirm(t.Context(), otp(t, b.Enrollment.Secret, s.Instance.Clock().Now()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(codes, result.RecoveryCodes) {
		t.Fatal("committed recovery codes differ from pre-displayed codes")
	}
	return codes
}

func TestRestartAndCommittedRecovery(t *testing.T) {
	opts, dir, _ := fixture(t)
	s := openSession(t, opts, dir)
	code, id := s.code, s.Instance.InstanceID()
	if open, _ := s.Instance.Gate().State(); open {
		t.Fatal("business gate opened during setup")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openSession(t, opts, dir)
	if s.code != code || s.Instance.InstanceID() != id {
		t.Fatal("restart replaced pending setup identity")
	}
	commitAdmin(t, s)
	// Simulate termination after commit but before activation/HTTP response.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	done, err := Open(t.Context(), opts, dir)
	if err != nil || done != nil {
		t.Fatalf("resume committed setup: %v", err)
	}
	l, _ := operations.NewLayout(opts.Home)
	m, err := operations.ReadMarker(l)
	if err != nil || m.State != operations.MarkerActive || m.InstanceID != id {
		t.Fatal("committed setup was not activated")
	}
	// Active upgrades bypass setup, including its old or missing claim directory.
	if next, err := Open(t.Context(), opts, filepath.Join(dir, "absent")); err != nil || next != nil {
		t.Fatal("active instance re-entered setup")
	}
}

func TestClosedBootstrapNeverReplacesMissingKey(t *testing.T) {
	opts, dir, secrets := fixture(t)
	s := openSession(t, opts, dir)
	commitAdmin(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(masterkey.Path(secrets)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), opts, dir); err == nil {
		t.Fatal("missing committed key accepted")
	}
	if _, err := os.Stat(masterkey.Path(secrets)); !os.IsNotExist(err) {
		t.Fatal("missing committed key was replaced")
	}
}

func TestIncompleteAndUnknownDataFailClosed(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		opts, dir, _ := fixture(t)
		path := filepath.Join(opts.Home, "valuable.txt")
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(t.Context(), opts, dir); err == nil {
			t.Fatal("unknown nonempty data accepted")
		}
		if _, err := os.Stat(filepath.Join(opts.Home, "instance.json")); !os.IsNotExist(err) {
			t.Fatal("unknown data was initialized")
		}
	})
	t.Run("missing database", func(t *testing.T) {
		opts, dir, _ := fixture(t)
		s := openSession(t, opts, dir)
		path := s.Instance.Layout().DBPath(ownership.Main)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(t.Context(), opts, dir); !operations.HasReason(err, operations.CodeDatabaseMissing) {
			t.Fatalf("missing database: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("missing database silently recreated")
		}
	})
	t.Run("origin validation precedes initialization", func(t *testing.T) {
		opts, dir, _ := fixture(t)
		if err := os.WriteFile(filepath.Join(opts.Home, "config.yaml"), []byte("contract: lantai.config/v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(t.Context(), opts, dir); err == nil {
			t.Fatal("missing origin accepted")
		}
		if _, err := os.Stat(filepath.Join(opts.Home, "instance.json")); !os.IsNotExist(err) {
			t.Fatal("invalid origin created an instance")
		}
	})
}

func request(t *testing.T, h *Handler, path string, body any, change func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, origin+path, bytes.NewReader(raw))
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+h.session.code)
	if change != nil {
		change(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func begin(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	w := request(t, h, "/setup/begin", map[string]any{"name": "ada", "password": testPassword}, nil)
	if w.Code != 200 {
		t.Fatalf("begin: %d %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHTTPGuardsAndCompletion(t *testing.T) {
	opts, dir, _ := fixture(t)
	s := openSession(t, opts, dir)
	h, err := NewHandler(s, []string{origin})
	if err != nil {
		t.Fatal(err)
	}
	for name, alter := range map[string]func(*http.Request){
		"no origin":        func(r *http.Request) { r.Header.Del("Origin") },
		"wrong origin":     func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"wrong port":       func(r *http.Request) { r.Host = "library.example" },
		"duplicate origin": func(r *http.Request) { r.Header.Add("Origin", origin) },
		"cross site":       func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"no claim":         func(r *http.Request) { r.Header.Del("Authorization") },
		"duplicate claim":  func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+s.code) },
	} {
		t.Run(name, func(t *testing.T) {
			w := request(t, h, "/setup/begin", map[string]string{}, alter)
			if w.Code != 403 && w.Code != 401 {
				t.Fatalf("guard status: %d", w.Code)
			}
			if strings.Contains(w.Body.String(), s.code) {
				t.Fatal("claim leaked")
			}
		})
	}
	for _, path := range []string{"/api/v1/meta", "/setup/../api/v1/meta", "/xfer/v1/x"} {
		w := request(t, h, path, map[string]string{}, nil)
		if w.Code != 404 {
			t.Fatalf("unexpected setup route %s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", origin+"/setup/", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || strings.Contains(w.Body.String(), s.code) {
		t.Fatal("unsafe setup page")
	}
	for _, directive := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "form-action 'none'"} {
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), directive) {
			t.Fatal("setup CSP source boundary missing")
		}
	}
	b := begin(t, h)
	if w := request(t, h, "/setup/begin", map[string]string{}, nil); w.Code != 429 {
		t.Fatal("begin not rate limited")
	}
	payload := map[string]any{"enrollment_id": b["enrollment_id"], "totp_code": otp(t, b["totp_secret"].(string), opts.Clock.Now()), "recovery_codes_saved": false}
	if w := request(t, h, "/setup/confirm", payload, nil); w.Code != 400 {
		t.Fatal("recovery acknowledgement skipped")
	}
	payload["recovery_codes_saved"] = true
	if w := request(t, h, "/setup/confirm", payload, nil); w.Code != 200 {
		t.Fatalf("confirm failed: %d", w.Code)
	}
	if w := request(t, h, "/setup/confirm", payload, nil); w.Code != 404 {
		t.Fatal("setup remained open after commit")
	}
	if _, err := os.Stat(filepath.Join(dir, ClaimFile)); !os.IsNotExist(err) {
		t.Fatal("claim file retained after successful activation")
	}
	var count int
	if err := s.Instance.DB(ownership.Main).QueryRow("SELECT count(*) FROM identity_principals").Scan(&count); err != nil || count != 1 {
		t.Fatal("unexpected administrator count")
	}
}

func TestConcurrentConfirmCreatesOneAdministrator(t *testing.T) {
	opts, dir, _ := fixture(t)
	s := openSession(t, opts, dir)
	h, _ := NewHandler(s, []string{origin})
	b := begin(t, h)
	payload := map[string]any{"enrollment_id": b["enrollment_id"], "totp_code": otp(t, b["totp_secret"].(string), opts.Clock.Now()), "recovery_codes_saved": true}
	results := make(chan int, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { <-start; results <- request(t, h, "/setup/confirm", payload, nil).Code })
	}
	close(start)
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for status := range results {
		counts[status]++
	}
	if counts[200] != 1 || counts[404] != 1 {
		t.Fatal("concurrent confirmation was not single-use")
	}
	var count int
	if err := s.Instance.DB(ownership.Main).QueryRow("SELECT count(*) FROM identity_principals").Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate administrator")
	}
}

func TestRestartInvalidatesUncommittedEnrollment(t *testing.T) {
	opts, dir, _ := fixture(t)
	s := openSession(t, opts, dir)
	h, _ := NewHandler(s, []string{origin})
	b := begin(t, h)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next := openSession(t, opts, dir)
	h2, _ := NewHandler(next, []string{origin})
	if next.code != s.code {
		t.Fatal("pending claim changed")
	}
	payload := map[string]any{"enrollment_id": b["enrollment_id"], "totp_code": otp(t, b["totp_secret"].(string), opts.Clock.Now()), "recovery_codes_saved": true}
	if w := request(t, h2, "/setup/confirm", payload, nil); w.Code != 409 {
		t.Fatal("restart retained uncommitted enrollment")
	}
	if open, err := next.Identity.BootstrapOpen(t.Context()); err != nil || !open {
		t.Fatal("uncommitted enrollment wrote identity state")
	}
}

func TestCommittedFailureStaysClosedAndResumes(t *testing.T) {
	opts, dir, _ := fixture(t)
	s := openSession(t, opts, dir)
	h, _ := NewHandler(s, []string{origin})
	b := begin(t, h)
	marker := s.Instance.Layout().MarkerPath()
	// Force atomic marker replacement to fail after the identity transaction,
	// without altering the database or relying on platform permission bypass.
	if err := os.Rename(marker, marker+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"enrollment_id": b["enrollment_id"], "totp_code": otp(t, b["totp_secret"].(string), opts.Clock.Now()), "recovery_codes_saved": true}
	w := request(t, h, "/setup/confirm", payload, nil)
	if w.Code != 503 {
		t.Fatalf("activation failure status: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), s.code) || strings.Contains(w.Body.String(), b["totp_secret"].(string)) {
		t.Fatal("failure response leaked a secret")
	}
	if w = request(t, h, "/setup/confirm", payload, nil); w.Code != 404 {
		t.Fatal("failed activation reopened setup")
	}
	if open, err := s.Identity.BootstrapOpen(t.Context()); err != nil || open {
		t.Fatal("identity commit was not authoritative")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(marker+".saved", marker); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if next, err := Open(t.Context(), opts, dir); err != nil || next != nil {
		t.Fatalf("committed failure did not resume safely: %v", err)
	}
	l, _ := operations.NewLayout(opts.Home)
	m, err := operations.ReadMarker(l)
	if err != nil || m.State != operations.MarkerActive {
		t.Fatal("resumed commit did not activate marker")
	}
}

func TestEnrollmentExpiryAndAttemptLimit(t *testing.T) {
	opts, dir, _ := fixture(t)
	s := openSession(t, opts, dir)
	h, _ := NewHandler(s, []string{origin})
	b := begin(t, h)
	payload := map[string]any{"enrollment_id": b["enrollment_id"], "totp_code": "invalid", "recovery_codes_saved": true}
	for range 5 {
		if w := request(t, h, "/setup/confirm", payload, nil); w.Code != 400 {
			t.Fatal("unexpected attempt result")
		}
	}
	payload["totp_code"] = otp(t, b["totp_secret"].(string), opts.Clock.Now())
	if w := request(t, h, "/setup/confirm", payload, nil); w.Code != 409 {
		t.Fatal("attempt limit bypassed")
	}
	h.attempts = 0
	h.expires = time.Now().Add(-time.Second)
	if w := request(t, h, "/setup/confirm", payload, nil); w.Code != 409 {
		t.Fatal("expired enrollment accepted")
	}
	h2, _ := NewHandler(s, []string{origin})
	if w := request(t, h2, "/setup/confirm", payload, nil); w.Code != 409 {
		t.Fatal("enrollment survived server restart")
	}
	if open, err := s.Identity.BootstrapOpen(context.Background()); err != nil || !open {
		t.Fatal("failed confirmation committed identity")
	}
}

func TestClaimFileValidation(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "hardlink", "interrupted write", "foreign", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			c, err := loadClaim(dir, "instance-a")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ClaimFile)
			switch kind {
			case "mode":
				err = os.Chmod(path, 0o644)
			case "symlink":
				err = os.Rename(path, path+".old")
				if err == nil {
					err = os.Symlink(path+".old", path)
				}
			case "hardlink":
				err = os.Link(path, path+".copy")
			case "interrupted write":
				// Exclusive placement may leave its own temporary hardlink
				// after a crash. Refuse ambiguous links without deleting data.
				err = os.Link(path, filepath.Join(dir, ".setup-code.json.tmp-interrupted"))
			case "foreign":
				c.InstanceID = "instance-b"
				var raw []byte
				raw, err = json.Marshal(c)
				if err == nil {
					err = os.WriteFile(path, raw, 0o600)
				}
			case "oversize":
				err = os.WriteFile(path, bytes.Repeat([]byte("x"), 2048), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadClaim(dir, "instance-a"); err == nil {
				t.Fatal("unsafe claim accepted")
			}
		})
	}
}

func TestPreparePreservesExistingContents(t *testing.T) {
	a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
	if err := PrepareVolumes([]string{a, b, c}, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(a, "existing")
	if err := os.WriteFile(file, []byte("keep"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := PrepareVolumes([]string{a, b, c}, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(file)
	raw, _ := os.ReadFile(file)
	if info.Mode().Perm() != 0o640 || string(raw) != "keep" {
		t.Fatal("existing content changed")
	}
	if err := os.Chmod(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareVolumes([]string{a, b, c}, os.Geteuid(), os.Getegid()); err != nil {
		t.Fatal("legacy data root should remain usable", err)
	}
	if err := os.Chmod(a, 0o757); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareVolumes([]string{b, a, c}, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("incompatible existing root accepted")
	}
	info, _ = os.Stat(b)
	if info.Mode().Perm() != 0o755 {
		t.Fatal("changed empty root before validating all inputs")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(c, link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareVolumes([]string{link, b, c}, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("symlink volume accepted")
	}
}
