package identity

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
)

// 浏览器会话经真实服务端防护：只有同源、带正确 CSRF 令牌的 Cookie 写请求被接受；
// Bearer CLI 请求不需要 CSRF 但仍须有效会话。
func TestHTTPGuardWithRealSessions(t *testing.T) {
	f := newFixture(t)
	web := f.login("ada", adminPassword, f.adminSecret, ChannelBrowser)
	agent, _ := f.register(f.adminLogin().Context, authz.Agent, "claude-code@node-a")
	cli := f.exchange(f.issueToken(f.adminLogin().Context, agent, []Scope{ScopeRead}), SessionRequest{})
	g, err := httpauth.NewGuard(f.svc, httpauth.Policy{AllowedOrigins: []string{"https://lantai.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	var seen authz.Context
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := httpauth.FromContext(r.Context())
		seen = c.Context
		w.WriteHeader(http.StatusNoContent)
	}))
	send := func(method, origin, csrf string, cookie, bearer string) int {
		req := httptest.NewRequest(method, "https://lantai.example.test/api/v1/x", nil)
		if cookie != "" {
			req.AddCookie(httpauth.SessionCookie(cookie, web.Session.ExpiresAt))
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf != "" {
			req.Header.Set(httpauth.CSRFHeader, csrf)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const origin = "https://lantai.example.test"
	if c := send("POST", origin, web.CSRFToken, web.Token, ""); c != 204 || seen.SessionID != web.Session.SessionID {
		t.Fatalf("same-origin write with CSRF = %d", c)
	}
	if c := send("POST", origin, f.svc.CSRFToken(cli.Session.SessionID), web.Token, ""); c != 403 {
		t.Fatalf("CSRF token of another session = %d", c)
	}
	if c := send("POST", "https://evil.example.test", web.CSRFToken, web.Token, ""); c != 403 {
		t.Fatalf("cross-origin write = %d", c)
	}
	if c := send("POST", "", web.CSRFToken, web.Token, ""); c != 403 {
		t.Fatalf("write without Origin = %d", c)
	}
	if c := send("GET", "", "", web.Token, ""); c != 204 {
		t.Fatalf("cookie read = %d", c)
	}
	if c := send("POST", "", "", "", cli.Token); c != 204 || seen.PrincipalKind != authz.Agent {
		t.Fatalf("bearer write = %d", c)
	}
	if c := send("POST", origin, web.CSRFToken, cli.Token, ""); c != 401 {
		t.Fatalf("CLI token in the session cookie = %d", c)
	}
	if c := send("GET", "", "", "", web.Token); c != 401 {
		t.Fatalf("browser token as bearer = %d", c)
	}
	if err := f.svc.EndSession(t.Context(), web.Context, web.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	if c := send("POST", origin, web.CSRFToken, web.Token, ""); c != 401 {
		t.Fatalf("ended browser session = %d", c)
	}
}

// 口令、TOTP 种子、恢复码、设置码与令牌都不能出现在事件、回执或库文件中。
func TestSecretsNeverPersistInPlaintext(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin()
	agent, _ := f.register(admin.Context, authz.Agent, "claude-code@node-a")
	token := f.issueToken(admin.Context, agent, []Scope{ScopeRead})
	session := f.exchange(token, SessionRequest{})
	_, setup := f.register(admin.Context, authz.Human, "alice")
	aliceSecret := f.completeSetup("alice", setup, "alice password 1")
	r, err := f.svc.StartRecovery(ctx, RecoveryRequest{Name: "ada", Password: adminPassword, Code: f.codes[4], Channel: ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.svc.EnrollFactor(ctx, r.Context)
	if err != nil {
		t.Fatal(err)
	}
	newCodes, err := f.svc.ConfirmFactor(ctx, r.Context, f.now(e.raw))
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{adminPassword, "alice password 1", token, token[len(CredentialPrefix):], session.Token,
		session.Token[len(SessionPrefix):], setup, strings.ReplaceAll(setup, "-", ""), r.Token, e.Secret}
	secrets = append(secrets, f.codes...)
	secrets = append(secrets, newCodes...)
	for _, raw := range [][]byte{f.adminSecret, aliceSecret, e.raw} {
		secrets = append(secrets, string(raw))
	}
	for _, db := range []ownership.Database{ownership.Main, ownership.Runtime} {
		conn := f.inst.DB(db)
		for _, q := range []string{`SELECT envelope FROM outbox`, `SELECT coalesce(response_summary, '') FROM command_receipts`} {
			rows, err := conn.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var text string
				rows.Scan(&text)
				for _, s := range secrets {
					if s != "" && strings.Contains(text, s) {
						t.Fatalf("%s: %q leaks a secret", db, q)
					}
				}
			}
			rows.Close()
		}
	}
	home := f.inst.Layout().Home
	if err := f.inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.db", "main.db-wal", "runtime.db", "runtime.db-wal", "events.db", "ledger.db", "index.db"} {
		raw, err := os.ReadFile(filepath.Join(home, "db", name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if s != "" && bytes.Contains(raw, []byte(s)) {
				t.Fatalf("%s contains a secret in plaintext", name)
			}
		}
	}
}
