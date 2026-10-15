//go:build linux || darwin

package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/identity/totp/totptest"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func openBackupHTTPApp(t *testing.T) (*App, *clock.Fake, []byte) {
	t.Helper()
	ctx := t.Context()
	home := t.TempDir()
	if e := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0600); e != nil {
		t.Fatal(e)
	}
	clk := clock.NewFake(time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC))
	gen := &ids.Generator{Clock: clk, Rand: rand.Reader}
	inst, e := operations.Create(ctx, operations.CreateOptions{Options: operations.Options{Home: home, Clock: clk, IDs: gen}, Name: "backup-http"})
	if e != nil {
		t.Fatal(e)
	}
	key, e := masterkey.Generate(gen, clk, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = masterkey.Save(inst.Config().SecretsDir, key); e != nil {
		t.Fatal(e)
	}
	svc, e := identity.New(identity.Deps{Main: inst.DB(ownership.Main), Runtime: inst.DB(ownership.Runtime), Gate: inst.Gate(), Epochs: inst, Clock: clk, IDs: gen, Key: key, InstanceID: inst.InstanceID()}, identity.Config{Password: testPassword})
	if e != nil {
		t.Fatal(e)
	}
	boot, e := svc.BeginBootstrap(ctx, identity.BootstrapRequest{Name: "ada", Password: "correct horse battery staple"})
	if e != nil {
		t.Fatal(e)
	}
	secret, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(boot.Enrollment.Secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = boot.Confirm(ctx, totp.Code(secret, totp.Counter(clk.Now()))); e != nil {
		t.Fatal(e)
	}
	if e = inst.Activate(ctx); e != nil {
		t.Fatal(e)
	}
	if e = inst.Close(ctx); e != nil {
		t.Fatal(e)
	}
	a, e := Open(ctx, Options{Instance: operations.Options{Home: home, Clock: clk}, Identity: identity.Config{Password: testPassword}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if e := a.Close(c); e != nil {
			t.Error(e)
		}
	})
	return a, clk, secret
}
func backupHTTP(t *testing.T, base, method, path string, session identity.IssuedSession, csrf bool, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if e := json.NewEncoder(&buf).Encode(body); e != nil {
			t.Fatal(e)
		}
	}
	req, e := http.NewRequestWithContext(t.Context(), method, base+path, &buf)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "backup-http-key")
	if session.Token != "" {
		req.AddCookie(&http.Cookie{Name: httpauth.CookieName, Value: session.Token})
	}
	if csrf {
		req.Header.Set("Origin", "https://lantai.test")
		req.Header.Set(httpauth.CSRFHeader, session.CSRFToken)
	}
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	data, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	return res.StatusCode, data
}

func TestBackupOptionalSocketFailureDoesNotBlockNormalHTTP(t *testing.T) {
	a := openTestApp(t)
	testBackupControl(t, a)
	socket := BackupSocketPath(a.Instance.InstanceID())
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(socket, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("synthetic occupied path"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socket) })
	cfg := a.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: true}
	srv, err := a.StartHTTP(t.Context(), cfg, WithAccessLog(io.Discard))
	if err != nil {
		t.Fatal("optional control socket blocked normal startup", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	client := &http.Client{Timeout: 3 * time.Second}
	r, err := client.Get("http://" + srv.Addresses.API + "/api/v1/meta")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK || !a.Ready() {
		t.Fatal("normal core is unavailable")
	}
	if data, err := os.ReadFile(socket); err != nil || string(data) != "synthetic occupied path" {
		t.Fatal("occupied path was modified", err)
	}
}

func TestBackupHTTPMaintenanceKeepsLiveAuthAndCancelWithoutIdentityWrites(t *testing.T) {
	a, clk, secret := openBackupHTTPApp(t)
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, BackupRootMarker), []byte("synthetic-volume\n"), 0600); e != nil {
		t.Fatal(e)
	}
	b, e := a.EnableBackups(t.Context(), root, "synthetic-volume")
	if e != nil {
		t.Fatal(e)
	}
	login := func(scopes []identity.Scope) identity.IssuedSession {
		v, e := a.Identity.Login(t.Context(), identity.LoginRequest{Name: "ada", Password: "correct horse battery staple", Code: totptest.Fresh(t, clk, secret), Channel: identity.ChannelBrowser, Scopes: scopes})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	admin := login([]identity.Scope{identity.ScopeSelf, identity.ScopeRead, identity.ScopeAdmin})
	limited := login([]identity.Scope{identity.ScopeSelf, identity.ScopeRead})
	cfg := a.Instance.Config()
	cfg.Listen.API = "127.0.0.1:0"
	cfg.Listen.Operations = "127.0.0.1:0"
	cfg.Listen.Merged = true
	cfg.HTTP.AllowedOrigins = []string{"https://lantai.test"}
	srv, e := a.StartHTTP(t.Context(), cfg, WithAccessLog(io.Discard))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if e := srv.Shutdown(c); e != nil {
			t.Error(e)
		}
	})
	base := "http://" + srv.Addresses.API
	path := "/api/v1/admin/backups"
	if status, raw := backupHTTP(t, base, "GET", path, limited, false, nil); status != 403 {
		t.Fatalf("restricted admin=%d %s", status, raw)
	}
	status, raw := backupHTTP(t, base, "POST", "/api/v1/identity/challenges", admin, true, map[string]any{"action": identity.ActBackupStart, "command": map[string]any{"kind": "start"}})
	if status != 201 {
		t.Fatalf("challenge=%d %s", status, raw)
	}
	var ch identity.Challenge
	if e = json.Unmarshal(raw, &ch); e != nil {
		t.Fatal(e)
	}
	status, raw = backupHTTP(t, base, "POST", "/api/v1/identity/challenges/"+string(ch.ChallengeID)+"/verify", admin, true, map[string]any{"code": totptest.Fresh(t, clk, secret)})
	if status != 201 {
		t.Fatalf("verify=%d %s", status, raw)
	}
	var g identity.Grant
	if e = json.Unmarshal(raw, &g); e != nil {
		t.Fatal(e)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	b.run = func(ctx context.Context, opts operations.BackupOptions) (operations.BackupManifest, error) {
		check := opts.CheckDestination
		paused := false
		opts.CheckDestination = func(c context.Context) error {
			if check != nil {
				if err := check(c); err != nil {
					return err
				}
			}
			// Hold the real copier after publishing its frozen point. This is
			// test-only composition of the existing destination check: no fake
			// success or external fault-injection endpoint enters production.
			data, err := os.ReadFile(filepath.Join(opts.Destination, "manifest.json"))
			var manifest operations.BackupManifest
			if !paused && err == nil && json.Unmarshal(data, &manifest) == nil && manifest.State == "copying" {
				paused = true
				close(entered)
				<-finish
				return c.Err()
			}
			return nil
		}
		return a.Backup(ctx, opts)
	}
	defer func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	}()
	status, raw = backupHTTP(t, base, "POST", path, admin, true, map[string]any{"grant_id": g.GrantID})
	if status != 202 {
		t.Fatalf("start=%d %s", status, raw)
	}
	var job backupmgmt.Job
	if e = json.Unmarshal(raw, &job); e != nil {
		t.Fatal(e)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backup never entered maintenance")
	}
	for _, db := range []ownership.Database{ownership.Main, ownership.Runtime} {
		a.Instance.DB(db).SetMaxOpenConns(1)
		if _, e = a.Instance.DB(db).ExecContext(t.Context(), "PRAGMA query_only=ON"); e != nil {
			t.Fatal(e)
		}
	}
	defer func() {
		for _, db := range []ownership.Database{ownership.Main, ownership.Runtime} {
			_, _ = a.Instance.DB(db).ExecContext(context.Background(), "PRAGMA query_only=OFF")
		}
	}()
	for _, p := range []string{path, path + "/history", path + "/schedule", path + "/" + string(job.ID), "/api/v1/whoami"} {
		if s, r := backupHTTP(t, base, "GET", p, admin, false, nil); s != 200 {
			t.Fatalf("read-only %s=%d %s", p, s, r)
		}
	}
	if s, r := backupHTTP(t, base, "GET", path, identity.IssuedSession{}, false, nil); s != 401 {
		t.Fatalf("anonymous=%d %s", s, r)
	}
	if s, r := backupHTTP(t, base, "GET", path, limited, false, nil); s != 403 {
		t.Fatalf("limited during maintenance=%d %s", s, r)
	}
	for _, p := range []string{"/api/v1/projects", path + "/unknown", path + "/history/", path + "//history"} {
		if s, r := backupHTTP(t, base, "GET", p, admin, false, nil); s != 503 {
			t.Fatalf("non-whitelist %s=%d %s", p, s, r)
		}
	}
	for _, p := range []string{path, "/api/v1/identity/challenges", path + "/" + string(job.ID) + "/resume"} {
		if s, r := backupHTTP(t, base, "POST", p, admin, true, map[string]any{}); s != 503 {
			t.Fatalf("blocked mutation=%d %s", s, r)
		}
	}
	cancelPath := path + "/" + string(job.ID) + "/cancel"
	if s, r := backupHTTP(t, base, "POST", cancelPath, admin, false, map[string]any{}); s != 403 {
		t.Fatalf("missing csrf=%d %s", s, r)
	}
	if s, r := backupHTTP(t, base, "POST", cancelPath, limited, true, map[string]any{}); s != 403 {
		t.Fatalf("limited cancel=%d %s", s, r)
	}
	if s, r := backupHTTP(t, base, "POST", cancelPath, admin, true, map[string]any{}); s != 202 {
		t.Fatalf("cancel=%d %s", s, r)
	}
	if a.Readiness().Ready {
		t.Fatal("cancel released live worker")
	}
	close(finish)
	if got := waitBackup(t, b, string(job.ID)); got.State != "cancelled" {
		t.Fatalf("terminal %+v", got)
	}
	for _, db := range []ownership.Database{ownership.Main, ownership.Runtime} {
		_, _ = a.Instance.DB(db).ExecContext(t.Context(), "PRAGMA query_only=OFF")
	}
	if e = a.Identity.EndSession(t.Context(), admin.Context, admin.Session.SessionID); e != nil {
		t.Fatal(e)
	}
	if s, r := backupHTTP(t, base, "GET", path, admin, false, nil); s != 401 || !strings.Contains(string(r), "TOKEN_REVOKED") {
		t.Fatalf("revoked=%d %s", s, r)
	}
}

func TestBackupHTTPExpiredAdminCanReauthenticateOnlyForCurrentControl(t *testing.T) {
	a, clk, secret := openBackupHTTPApp(t)
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(root, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, BackupRootMarker), []byte("control-auth-test\n"), 0600); e != nil {
		t.Fatal(e)
	}
	b, e := a.EnableBackups(t.Context(), root, "control-auth-test")
	if e != nil {
		t.Fatal(e)
	}
	old, e := a.Identity.Login(t.Context(), identity.LoginRequest{Name: "ada", Password: "correct horse battery staple", Code: totptest.Fresh(t, clk, secret), Channel: identity.ChannelBrowser, Scopes: []identity.Scope{identity.ScopeSelf, identity.ScopeRead, identity.ScopeAdmin}})
	if e != nil {
		t.Fatal(e)
	}
	cfg := a.Instance.Config()
	cfg.Listen.API = "127.0.0.1:0"
	cfg.Listen.Operations = "127.0.0.1:0"
	cfg.Listen.Merged = true
	cfg.HTTP.AllowedOrigins = []string{"https://lantai.test"}
	server, e := a.StartHTTP(t.Context(), cfg, WithAccessLog(io.Discard))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(c)
	})
	base := "http://" + server.Addresses.API
	entered, finish := make(chan struct{}), make(chan struct{})
	b.run = func(ctx context.Context, _ operations.BackupOptions) (operations.BackupManifest, error) {
		close(entered)
		<-finish
		return operations.BackupManifest{}, ctx.Err()
	}
	defer func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	}()
	job, e := b.Start(BackupRequest{})
	if e != nil {
		t.Fatal(e)
	}
	<-entered
	for _, db := range []ownership.Database{ownership.Main, ownership.Runtime} {
		a.Instance.DB(db).SetMaxOpenConns(1)
		if _, e = a.Instance.DB(db).ExecContext(t.Context(), "PRAGMA query_only=ON"); e != nil {
			t.Fatal(e)
		}
	}
	defer func() {
		for _, db := range []ownership.Database{ownership.Main, ownership.Runtime} {
			_, _ = a.Instance.DB(db).ExecContext(context.Background(), "PRAGMA query_only=OFF")
		}
	}()
	clk.Advance(13 * time.Hour)
	if status, _ := backupHTTP(t, base, "GET", "/api/v1/admin/backups", old, false, nil); status != 401 {
		t.Fatal("old session did not expire", status)
	}
	call := func(method, path, token, csrf string, body any, origin bool) (int, []byte, []*http.Cookie) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			if e := json.NewEncoder(&buf).Encode(body); e != nil {
				t.Fatal(e)
			}
		}
		req, e := http.NewRequestWithContext(t.Context(), method, base+path, &buf)
		if e != nil {
			t.Fatal(e)
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: httpauth.CookieName, Value: old.Token})
		if token != "" {
			req.AddCookie(&http.Cookie{Name: "lantai_backup_control", Value: token})
		}
		if origin {
			req.Header.Set("Origin", "https://lantai.test")
		}
		if csrf != "" {
			req.Header.Set(httpauth.CSRFHeader, csrf)
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		raw, e := io.ReadAll(res.Body)
		if e != nil {
			t.Fatal(e)
		}
		return res.StatusCode, raw, res.Cookies()
	}
	route := "/api/v1/admin/backups/control-session"
	body := map[string]any{"name": "ada", "password": "correct horse battery staple", "code": totptest.Fresh(t, clk, secret)}
	if status, _, _ := call("POST", route, "", "", body, false); status != 403 {
		t.Fatal("missing origin accepted", status)
	}
	status, raw, cookies := call("POST", route, "", "", body, true)
	if status != 201 {
		t.Fatalf("control login %d %s", status, raw)
	}
	var session identity.BackupControlSession
	if e = json.Unmarshal(raw, &session); e != nil {
		t.Fatal(e)
	}
	if len(cookies) != 1 || cookies[0].Path != "/api/v1/admin/backups" || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unsafe cookie %+v", cookies)
	}
	token := cookies[0].Value
	for _, path := range []string{route, "/api/v1/admin/backups", "/api/v1/admin/backups/history", "/api/v1/admin/backups/schedule", "/api/v1/admin/backups/" + string(job.ID)} {
		if status, r, _ := call("GET", path, token, "", nil, false); status != 200 {
			t.Fatalf("control read %s %d %s", path, status, r)
		}
	}
	if status, _, _ := call("GET", "/api/v1/whoami", token, "", nil, false); status != 401 {
		t.Fatal("control token became regular session", status)
	}
	if status, _, _ := call("POST", "/api/v1/admin/backups", token, session.CSRFToken, map[string]any{}, true); status != 503 {
		t.Fatal("control token started backup", status)
	}
	cancelPath := "/api/v1/admin/backups/" + string(job.ID) + "/cancel"
	if status, _, _ := call("POST", "/api/v1/admin/backups/"+string(ids.New())+"/cancel", token, session.CSRFToken, map[string]any{}, true); status != 403 {
		t.Fatal("control session cancelled another job", status)
	}

	if status, _, _ := call("POST", cancelPath, token, "wrong", map[string]any{}, true); status != 403 {
		t.Fatal("invalid csrf accepted", status)
	}
	if status, _, _ := call("DELETE", route, token, session.CSRFToken, nil, true); status != 204 {
		t.Fatal("control logout", status)
	}
	if status, _, _ := call("GET", route, token, "", nil, false); status != 401 {
		t.Fatal("logged out control accepted", status)
	}
	body["code"] = totptest.Fresh(t, clk, secret)
	status, raw, cookies = call("POST", route, "", "", body, true)
	if status != 201 {
		t.Fatalf("second login %d %s", status, raw)
	}
	_ = json.Unmarshal(raw, &session)
	token = cookies[0].Value
	clk.Advance(11 * time.Minute)
	if status, _, _ := call("GET", route, token, "", nil, false); status != 401 {
		t.Fatal("control token failed expiry", status)
	}
	body["code"] = totptest.Fresh(t, clk, secret)
	status, raw, cookies = call("POST", route, "", "", body, true)
	if status != 201 {
		t.Fatalf("third login %d %s", status, raw)
	}
	_ = json.Unmarshal(raw, &session)
	token = cookies[0].Value
	if status, r, _ := call("POST", cancelPath, token, session.CSRFToken, map[string]any{}, true); status != 202 {
		t.Fatalf("fresh control cancellation %d %s", status, r)
	}
	if a.Readiness().Ready {
		t.Fatal("cancel released active copier")
	}
	close(finish)
	waitBackup(t, b, string(job.ID))
	if status, _, _ := call("GET", route, token, "", nil, false); status != 401 {
		t.Fatal("terminal backup did not revoke control", status)
	}
	for _, db := range []ownership.Database{ownership.Main, ownership.Runtime} {
		_, _ = a.Instance.DB(db).ExecContext(t.Context(), "PRAGMA query_only=OFF")
	}
	_, e = a.Identity.Login(t.Context(), identity.LoginRequest{Name: "ada", Password: "correct horse battery staple", Code: body["code"].(string), Channel: identity.ChannelBrowser})
	if errcode.CodeOf(e) != errcode.TOTPReplay {
		t.Fatal("maintenance code reused in ordinary login", e)
	}
	normalBody := map[string]any{"name": "ada", "password": "correct horse battery staple", "code": totptest.Fresh(t, clk, secret), "channel": "browser", "scopes": []string{"self", "read", "admin"}}
	status, raw, cookies = call("POST", "/api/v1/sessions/login", "", "", normalBody, true)
	if status != 201 {
		t.Fatalf("ordinary re-login %d %s", status, raw)
	}
	cleared := false
	for _, c := range cookies {
		if c.Name == "lantai_backup_control" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("ordinary login did not clear obsolete control cookie")
	}

}

func TestBackupHTTPControlLoginSupportsCancellingBeforeBarrier(t *testing.T) {
	a, clk, secret := openBackupHTTPApp(t)
	b := testBackupControl(t, a)
	cfg := a.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: true}
	cfg.HTTP.AllowedOrigins = []string{"https://lantai.test"}
	server, e := a.StartHTTP(t.Context(), cfg, WithAccessLog(io.Discard))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	_, held, e := a.Instance.Gate().Acquire(t.Context(), commands.Request{})
	if e != nil {
		t.Fatal(e)
	}
	defer held.Release()
	job, e := b.Start(BackupRequest{})
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		open, reason := a.Instance.Gate().State()
		if !open && reason == "backup" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never began draining instance barrier")
		}
		time.Sleep(time.Millisecond)
	}
	// Model cancellation delivery delayed by the scheduler while retaining the
	// real Cancel state transition and the real in-flight shared barrier holder.
	b.mu.Lock()
	originalCancel := b.cancel
	b.cancel = func() {}
	b.mu.Unlock()
	if _, e = b.Cancel(job.ID); e != nil {
		t.Fatal(e)
	}
	defer func() { b.mu.Lock(); b.cancel = originalCancel; b.mu.Unlock(); originalCancel() }()
	status, raw := backupHTTP(t, "http://"+server.Addresses.API, "POST", "/api/v1/admin/backups/control-session", identity.IssuedSession{CSRFToken: "unused"}, true, map[string]any{"name": "ada", "password": "correct horse battery staple", "code": totptest.Fresh(t, clk, secret)})
	if status != 201 {
		t.Fatalf("cancelling drain reauthentication unavailable: %d %s", status, raw)
	}
	if scope, ok := a.BackupControlScope(); !ok || scope.JobID != job.ID {
		t.Fatalf("missing active drain scope: %+v", scope)
	}
	if a.Readiness().Ready {
		t.Fatal("reauthentication opened business admission")
	}
	if _, e = b.Cancel(job.ID); e != nil {
		t.Fatal(e)
	}
	if a.Readiness().Ready {
		t.Fatal("cancellation opened business before in-flight writer exited")
	}
}
