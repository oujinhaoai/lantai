package identity

import (
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func limitedAgent(t *testing.T, f *fixture, limit int) (authz.Context, Principal, string) {
	t.Helper()
	admin := f.adminLogin().Context
	p, _ := f.register(admin, authz.Agent, "quota-agent@node-a")
	f.mustSudo(admin, f.adminSecret, &UpdatePrincipal{PrincipalID: p.ID, ExpectedRevision: p.Revision, Profile: Profile{MaxSessions: limit}})
	return admin, p, f.issueToken(admin, p, []Scope{ScopeRead})
}

func TestSessionQuotaExcludesRevokedSession(t *testing.T) {
	f := newFixture(t)
	admin, _, token := limitedAgent(t, f, 1)
	old := f.exchange(token, SessionRequest{})
	f.mustSudo(admin, f.adminSecret, &RevokeSession{SessionID: old.Session.SessionID, Reason: "replace-session"})
	if _, err := f.svc.ExchangeToken(t.Context(), token, SessionRequest{Channel: ChannelCLI}); err != nil {
		t.Fatalf("a revoked session still consumes quota: %v", err)
	}
	_, err := f.svc.ExchangeToken(t.Context(), token, SessionRequest{Channel: ChannelCLI})
	wantCode(t, err, errcode.QuotaExceeded)
}

func TestSessionQuotaExcludesRevokedCredential(t *testing.T) {
	f := newFixture(t)
	admin, p, oldToken := limitedAgent(t, f, 1)
	old := f.exchange(oldToken, SessionRequest{})
	f.mustSudo(admin, f.adminSecret, &RevokeCredential{CredentialID: old.Session.CredentialID, Reason: "rotate-credential"})
	newToken := f.issueToken(admin, p, []Scope{ScopeRead})
	if _, err := f.svc.ExchangeToken(t.Context(), newToken, SessionRequest{Channel: ChannelCLI}); err != nil {
		t.Fatalf("a session derived from a revoked credential still consumes quota: %v", err)
	}
}

func TestSessionQuotaExcludesPreviousRecoveryEpoch(t *testing.T) {
	f := newFixture(t)
	_, _, token := limitedAgent(t, f, 1)
	epoch := &epochBox{}
	epoch.v.Store(1)
	f.svc.epochs = epoch
	old := f.exchange(token, SessionRequest{})
	epoch.v.Store(2)
	_, err := f.svc.VerifySession(t.Context(), old.Session.SessionID)
	wantCode(t, err, errcode.TokenRevoked)
	if _, err := f.svc.ExchangeToken(t.Context(), token, SessionRequest{Channel: ChannelCLI}); err != nil {
		t.Fatalf("a session from before instance recovery still consumes quota: %v", err)
	}
}

func TestSessionQuotaPreservesParentAndChildAccounting(t *testing.T) {
	f := newFixture(t)
	_, _, token := limitedAgent(t, f, 1)
	parent := f.exchange(token, SessionRequest{})
	child, err := f.svc.NarrowSession(t.Context(), parent.Context, SessionRequest{Channel: ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.ExchangeToken(t.Context(), token, SessionRequest{Channel: ChannelCLI})
	wantCode(t, err, errcode.QuotaExceeded)
	if err := f.svc.EndSession(t.Context(), parent.Context, parent.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.VerifySession(t.Context(), child.Session.SessionID)
	wantCode(t, err, errcode.TokenRevoked)
	if _, err := f.svc.ExchangeToken(t.Context(), token, SessionRequest{Channel: ChannelCLI}); err != nil {
		t.Fatalf("a child of an ended parent consumes independent quota: %v", err)
	}
}

func TestSessionQuotaIsAtomicDuringConcurrentIssuance(t *testing.T) {
	f := newFixture(t)
	admin, _, token := limitedAgent(t, f, 2)
	old := f.exchange(token, SessionRequest{})
	f.mustSudo(admin, f.adminSecret, &RevokeSession{SessionID: old.Session.SessionID, Reason: "replace-session"})
	const callers = 12
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-start
			_, err := f.svc.ExchangeToken(t.Context(), token, SessionRequest{Channel: ChannelCLI})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !isCode(err, errcode.QuotaExceeded) {
			t.Errorf("concurrent session issuance: %v", err)
		}
	}
	if success != 2 {
		t.Fatalf("concurrent callers issued %d sessions, want exactly 2", success)
	}
}
