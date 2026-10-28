//go:build linux || darwin

package application

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/identity/totp/totptest"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func TestRecoveryPackageHTTPDownloadRequiresBoundFreshApproval(t *testing.T) {
	a, clk, secret := openBackupHTTPApp(t)
	b := testBackupControl(t, a)
	j, e := b.Start(BackupRequest{})
	if e != nil {
		t.Fatal(e)
	}
	j = waitBackup(t, b, string(j.ID))
	if j.State != "complete" || j.Package == nil {
		t.Fatalf("incomplete package: %+v", j)
	}
	login := func(scopes []identity.Scope) identity.IssuedSession {
		s, e := a.Identity.Login(t.Context(), identity.LoginRequest{Name: "ada", Password: "correct horse battery staple", Code: totptest.Fresh(t, clk, secret), Channel: identity.ChannelBrowser, Scopes: scopes})
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	admin := login([]identity.Scope{identity.ScopeSelf, identity.ScopeRead, identity.ScopeAdmin})
	other := login([]identity.Scope{identity.ScopeSelf, identity.ScopeRead, identity.ScopeAdmin})
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	base := "http://" + srv.Addresses.API
	path := "/api/v1/admin/backups/" + string(j.ID)
	get := func(session identity.IssuedSession, token, control, byteRange string) (int, []byte, http.Header) {
		r, e := http.NewRequestWithContext(t.Context(), "GET", base+path+"/package", nil)
		if e != nil {
			t.Fatal(e)
		}
		r.AddCookie(&http.Cookie{Name: httpauth.CookieName, Value: session.Token})
		if token != "" {
			r.Header.Set("X-Lantai-Backup-Download", token)
		}
		if control != "" {
			r.AddCookie(&http.Cookie{Name: "lantai_backup_control", Value: control})
		}
		if byteRange != "" {
			r.Header.Set("Range", byteRange)
		}
		v, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer v.Body.Close()
		raw, _ := io.ReadAll(v.Body)
		return v.StatusCode, raw, v.Header
	}
	if status, _, _ := get(admin, "", "", ""); status != 403 {
		t.Fatal("unapproved download", status)
	}
	cmd := &identity.ApproveBackup{Kind: "download", JobID: j.ID, PackageSHA256: j.Package.SHA256}
	ch, e := a.Identity.CreateChallenge(t.Context(), admin.Context, cmd)
	if e != nil {
		t.Fatal(e)
	}
	grant, e := a.Identity.VerifyChallenge(t.Context(), admin.Context, ch.ChallengeID, totptest.Fresh(t, clk, secret), "")
	if e != nil {
		t.Fatal(e)
	}
	status, raw := backupHTTP(t, base, "POST", path+"/download-request", admin, true, map[string]any{"grant_id": grant.GrantID, "package_sha256": j.Package.SHA256})
	if status != 200 {
		t.Fatalf("approval %d %s", status, raw)
	}
	var permit struct {
		Token string `json:"download_token"`
	}
	if e = json.Unmarshal(raw, &permit); e != nil || permit.Token == "" {
		t.Fatal("missing download token", e)
	}
	if status, raw, headers := get(admin, permit.Token, "", "bytes=0-63"); status != 206 || len(raw) != 64 || headers.Get("ETag") != `"`+j.Package.SHA256+`"` {
		t.Fatalf("range %d %d %s", status, len(raw), headers.Get("ETag"))
	}
	file := operations.RecoveryPackagePath(filepath.Join(b.root, string(j.ID)))
	if err := os.Link(file, file+".extra"); err != nil {
		t.Fatal(err)
	}
	status, raw, _ = get(admin, permit.Token, "", "")
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &failure); err != nil || status != 503 || failure.Error.Code != "STORAGE_UNAVAILABLE" || len(failure.Error.Details) != 1 || failure.Error.Details[0].Reason != "backup_package_hardlinks" {
		t.Fatalf("unsafe package diagnostic: %d %s", status, raw)
	}
	if err := os.Remove(file + ".extra"); err != nil {
		t.Fatal(err)
	}
	if status, raw, _ := get(admin, permit.Token, "", ""); status != 200 || int64(len(raw)) != j.Package.Size {
		t.Fatal("restored package download", status)
	}
	if status, _, _ := get(other, permit.Token, "", ""); status != 403 {
		t.Fatal("download token crossed sessions", status)
	}
	if status, _, _ := get(admin, permit.Token, "unrelated", ""); status != 403 {
		t.Fatal("maintenance identity downloaded", status)
	}
	if status, _ := backupHTTP(t, base, "DELETE", "/api/v1/sessions/current", admin, true, nil); status != 204 {
		t.Fatal("logout", status)
	}
	if status, _, _ := get(admin, permit.Token, "", ""); status != 401 {
		t.Fatal("ended session downloaded", status)
	}
}
