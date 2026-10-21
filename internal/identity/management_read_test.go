package identity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestManagementPrincipalAndCredentialPagesUseRealAdminAuthorization(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	agent, _ := f.register(admin, authz.Agent, "manager-test@node")
	tokenA := f.issueToken(admin, agent, []Scope{ScopeRead})
	tokenB := f.issueToken(admin, agent, []Scope{ScopeRead, ScopeIngest})
	page, err := f.svc.PrincipalPage(ctx, admin, "", 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("principals=%+v %v", page, err)
	}
	next, err := f.svc.PrincipalPage(ctx, admin, ids.ID(page.NextCursor), 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID || next.NextCursor != "" {
		t.Fatalf("next principals=%+v %v", next, err)
	}
	creds, err := f.svc.CredentialPage(ctx, admin, agent.ID, "", 1)
	if err != nil || len(creds.Items) != 1 || creds.NextCursor == "" {
		t.Fatalf("credentials=%+v %v", creds, err)
	}
	rest, err := f.svc.CredentialPage(ctx, admin, agent.ID, ids.ID(creds.NextCursor), 1)
	if err != nil || len(rest.Items) != 1 || rest.Items[0].ID == creds.Items[0].ID || rest.NextCursor != "" {
		t.Fatalf("next credentials=%+v %v", rest, err)
	}
	raw, _ := json.Marshal([]CredentialPage{creds, rest})
	for _, secret := range []string{tokenA, tokenB, "secret_hash", "token_hash"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("credential list exposed secret material")
		}
	}
	f.mustSudo(admin, f.adminSecret, &RevokeCredential{CredentialID: creds.Items[0].ID, Reason: "synthetic rotation"})
	revoked, err := f.svc.CredentialPage(ctx, admin, agent.ID, "", 100)
	if err != nil || revoked.Items[0].RevokedAt == nil {
		t.Fatalf("current revocation metadata=%+v %v", revoked, err)
	}
	_, err = f.svc.CredentialPage(ctx, admin, ids.New(), "", 1)
	wantCode(t, err, errcode.NotFound)
	for _, n := range []int{0, -1, 101} {
		_, err = f.svc.PrincipalPage(ctx, admin, "", n)
		wantCode(t, err, errcode.SchemaInvalid)
	}
	_, err = f.svc.CredentialPage(ctx, admin, agent.ID, "bad", 1)
	wantCode(t, err, errcode.SchemaInvalid)
	// Agent sessions cannot enumerate any instance management records, even
	// though they may have project read/ingest scopes and valid credentials.
	reader := f.exchange(tokenB, SessionRequest{})
	_, err = f.svc.PrincipalPage(ctx, reader.Context, "", 1)
	wantCode(t, err, errcode.Forbidden)
	_, err = f.svc.CredentialPage(ctx, reader.Context, agent.ID, "", 1)
	wantCode(t, err, errcode.Forbidden)
	// A normal human without admin scope cannot borrow their system role.
	narrow, err := f.svc.Login(ctx, LoginRequest{Name: "ada", Password: adminPassword, Code: f.fresh(f.adminSecret), Channel: ChannelBrowser, Scopes: []Scope{ScopeSelf, ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.PrincipalPage(ctx, narrow.Context, "", 1)
	wantCode(t, err, errcode.Forbidden)
}

func TestManagementListingRejectsDelegationAndRevokedAdministrator(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	bob, code := f.register(admin, authz.Human, "bob")
	secret := f.completeSetup("bob", code, adminPassword)
	f.mustSudo(admin, f.adminSecret, &SetSystemRole{PrincipalID: bob.ID, ExpectedRevision: currentRev(t, f, bob.ID), Role: RoleAdmin, Grant: true})
	bobSession := f.login("bob", adminPassword, secret, ChannelBrowser)
	if _, err := f.svc.PrincipalPage(ctx, bobSession.Context, "", 10); err != nil {
		t.Fatal(err)
	}
	f.mustSudo(admin, f.adminSecret, &SetSystemRole{PrincipalID: bob.ID, ExpectedRevision: currentRev(t, f, bob.ID), Role: RoleAdmin, Grant: false})
	if _, err := f.svc.PrincipalPage(ctx, bobSession.Context, "", 10); err == nil {
		t.Fatal("revoked administrator kept access")
	}
	bobSession = f.login("bob", adminPassword, secret, ChannelBrowser)
	_, err := f.svc.PrincipalPage(ctx, bobSession.Context, "", 10)
	wantCode(t, err, errcode.Forbidden)
	plugin, _ := f.register(admin, authz.Service, "service:management-test")
	project := ids.New()
	f.grantRole(admin, project, f.admin, RoleOwner)
	delegated, err := f.svc.IssueDelegated(ctx, admin, DelegationRequest{Delegate: plugin.ID, Actions: []authz.Action{ActCatalogRead}, Projects: []ids.ID{project}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.PrincipalPage(ctx, delegated.Context, "", 10)
	wantCode(t, err, errcode.Forbidden)
	_, err = f.svc.CredentialPage(ctx, delegated.Context, plugin.ID, "", 10)
	wantCode(t, err, errcode.Forbidden)
}
