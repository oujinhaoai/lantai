package integration

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/identity/totp/totptest"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Exercise the actual application listener, cookie login, identity permission
// checks and maintenance admission. Synthetic loopback HTTP is not TLS or
// browser acceptance. Fixture preparation uses real identity commands, not SQL.
func TestManagementInfoHTTPAuthorizationAndMaintenance(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	_, agentSession := e.agent("instance-view@node", identity.RoleViewer)
	member := e.sudo(&identity.RegisterPrincipal{Kind: authz.Human, Name: "instance-viewer", DisplayName: "Synthetic viewer"})
	var registered struct {
		ID ids.ID `json:"principal_id"`
	}
	if err := json.Unmarshal(member.Summary, &registered); err != nil || !registered.ID.Valid() {
		t.Fatalf("register member: %v", err)
	}
	setup, err := e.id.StartSetup(ctx, identity.SetupRequest{Name: "instance-viewer", Code: member.Secret, Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.id.SetPassword(ctx, setup.Context, adminPassword); err != nil {
		t.Fatal(err)
	}
	enrollment, err := e.id.EnrollFactor(ctx, setup.Context)
	if err != nil {
		t.Fatal(err)
	}
	memberSecret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.id.ConfirmFactor(ctx, setup.Context, totptest.Fresh(t, e.clk, memberSecret)); err != nil {
		t.Fatal(err)
	}
	e.setRole(registered.ID, identity.RoleViewer, true)
	a := reopenApplication(t, e)
	servers := backupHTTPServers(t, a)
	base := "http://" + servers.Addresses.API
	admin := backupLogin(t, e, base, []identity.Scope{identity.ScopeRead, identity.ScopeAdmin})
	narrow := backupLogin(t, e, base, []identity.Scope{identity.ScopeRead})
	anonymous := backupBrowser{t: t, base: base}
	agent := backupBrowser{t: t, base: base, token: agentSession.Token}

	// ScopeAdmin is explicitly requested, so the ordinary-member denial below
	// proves that a scope is not a replacement for the live system admin role.
	ordinary := backupBrowser{t: t, base: base}
	status, raw, cookies := ordinary.request(http.MethodPost, "sessions/login", "", map[string]any{
		"name": "instance-viewer", "password": adminPassword, "code": totptest.Fresh(t, e.clk, memberSecret),
		"channel": identity.ChannelBrowser, "scopes": []identity.Scope{identity.ScopeRead, identity.ScopeAdmin}, "ttl_seconds": 86400,
	})
	if status != http.StatusCreated {
		t.Fatalf("member browser login: status %d", status)
	}
	var login struct {
		Token string `json:"token"`
		CSRF  string `json:"csrf_token"`
	}
	if err := json.Unmarshal(raw, &login); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		if cookie.Name == httpauth.CookieName {
			ordinary.cookie = cookie
		}
	}
	ordinary.csrf = login.CSRF
	if ordinary.cookie == nil || ordinary.csrf == "" || login.Token != "" {
		t.Fatal("member login did not return a cookie-only browser session")
	}

	read := func(browser backupBrowser) (operations.ManagementInfo, []byte) {
		t.Helper()
		status, raw, _ := browser.request(http.MethodGet, "admin/instance", "", nil)
		if status != http.StatusOK {
			t.Fatalf("instance view: status %d: %s", status, raw)
		}
		var out operations.ManagementInfo
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if out.InstanceID != a.Instance.InstanceID() || out.RecoveryEpoch != a.Instance.Marker().RecoveryEpoch || out.State != operations.StateReady || out.ObservedAt.IsZero() || out.FreeBytes == nil || out.CapacityError != "" || out.MinFreeBytes != a.Instance.Config().MinFreeBytes || out.RecoveryReceipts == nil || len(out.RecoveryReceipts) != 0 || out.RecoveryError != "" || out.RecoveryPartial {
			t.Fatalf("unexpected current instance projection: %+v", out)
		}
		for _, private := range []string{a.Instance.Layout().Home, a.Instance.Config().SecretsDir, adminPassword, enrollment.Secret, admin.cookie.Value, ordinary.cookie.Value, agentSession.Token, "secret_hash", "administrator", "note", "source_key_id"} {
			if strings.Contains(string(raw), private) {
				t.Fatal("instance view leaked private host or identity details")
			}
		}
		return out, raw
	}
	deny := func(browser backupBrowser, want int, code errcode.Code) {
		t.Helper()
		status, raw, _ := browser.request(http.MethodGet, "admin/instance", "", nil)
		var envelope struct {
			Error struct {
				Code errcode.Code `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if status != want || envelope.Error.Code != code {
			t.Fatalf("denied instance view: status %d code %s; want %d %s", status, envelope.Error.Code, want, code)
		}
		for _, field := range []string{"instance_id", "free_bytes", "recovery_receipts"} {
			if strings.Contains(string(raw), `"`+field+`"`) {
				t.Fatal("denial included an instance data projection")
			}
		}
	}
	t.Run("normal_human_admin", func(t *testing.T) { read(admin) })
	t.Run("no_session", func(t *testing.T) { deny(anonymous, http.StatusUnauthorized, errcode.AuthRequired) })
	t.Run("admin_without_scope", func(t *testing.T) { deny(narrow, http.StatusForbidden, errcode.Forbidden) })
	t.Run("agent", func(t *testing.T) { deny(agent, http.StatusForbidden, errcode.Forbidden) })
	t.Run("ordinary_human_with_admin_scope", func(t *testing.T) { deny(ordinary, http.StatusForbidden, errcode.Forbidden) })

	setAdmin := func(grant bool) {
		t.Helper()
		p, err := e.id.GetPrincipal(ctx, e.login().Context, registered.ID)
		if err != nil {
			t.Fatal(err)
		}
		e.sudo(&identity.SetSystemRole{PrincipalID: p.ID, ExpectedRevision: p.Revision, Role: identity.RoleAdmin, Grant: grant})
	}
	t.Run("live_role_revocation", func(t *testing.T) {
		setAdmin(true)
		read(ordinary)
		setAdmin(false)
		deny(ordinary, http.StatusForbidden, errcode.Forbidden)
	})
	t.Run("maintenance_denies_instance_view", func(t *testing.T) {
		if err := a.Maintenance(ctx, func(context.Context) error {
			deny(admin, http.StatusServiceUnavailable, errcode.MaintenanceMode)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		read(admin)
	})
}
