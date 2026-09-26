package identity

import (
	"fmt"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/authz/authztest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
)

// identityHarness 让真实实现运行 authztest 的契约套件。
type identityHarness struct {
	f     *fixture
	box   *epochBox
	admin IssuedSession
	n     int
}

func (h *identityHarness) Authorizer() authz.Authorizer    { return h.f.svc }
func (h *identityHarness) Verifier() authz.SessionVerifier { return h.f.svc }
func (h *identityHarness) Action() authz.Action            { return ActLedgerCommitVersion }

// adminCtx 返回仍有效的管理员会话；恢复或到期后重新登录。
func (h *identityHarness) adminCtx() authz.Context {
	if _, err := h.f.svc.VerifySession(h.f.t.Context(), h.admin.Session.SessionID); err != nil {
		h.admin = h.f.adminLogin()
	}
	return h.admin.Context
}

func (h *identityHarness) Principal(t *testing.T, kind authz.PrincipalKind) ids.ID {
	h.n++
	name := fmt.Sprintf("agent-%d@contract", h.n)
	if kind != authz.Agent {
		t.Fatalf("the identity harness only registers agents, got %s", kind)
	}
	p, _ := h.f.register(h.adminCtx(), kind, name)
	return p.ID
}

func (h *identityHarness) Allow(t *testing.T, p, project ids.ID) {
	pr, err := loadPrincipal(t.Context(), h.f.svc.main, p)
	if err != nil {
		t.Fatal(err)
	}
	h.f.grantRole(h.adminCtx(), project, pr, RoleContributor)
}

func (h *identityHarness) Revoke(t *testing.T, p, project ids.ID) {
	rev, err := projectRevision(t.Context(), h.f.svc.main, project)
	if err != nil {
		t.Fatal(err)
	}
	h.f.mustSudo(h.adminCtx(), h.f.adminSecret, &SetProjectRole{ProjectID: project, ExpectedRevision: rev, PrincipalID: p, Role: RoleContributor})
}

func (h *identityHarness) Session(t *testing.T, p ids.ID, projects ...ids.ID) authz.Context {
	pr, err := loadPrincipal(t.Context(), h.f.svc.main, p)
	if err != nil {
		t.Fatal(err)
	}
	token := h.f.issueToken(h.adminCtx(), pr, []Scope{ScopeRead, ScopeIngest})
	return h.f.exchange(token, SessionRequest{Projects: projects}).Context
}

func (h *identityHarness) End(t *testing.T, who authz.Context) {
	if err := h.f.svc.EndSession(t.Context(), who, who.SessionID); err != nil {
		t.Fatal(err)
	}
}

func (h *identityHarness) Restore(*testing.T)                    { h.box.v.Add(1) }
func (h *identityHarness) Advance(_ *testing.T, d time.Duration) { h.f.clk.Advance(d) }

func TestIdentitySatisfiesAuthorizerContract(t *testing.T) {
	f := newFixture(t)
	box := &epochBox{}
	box.v.Store(1)
	svc, err := New(Deps{Main: f.inst.DB(ownership.Main), Runtime: f.inst.DB(ownership.Runtime), Gate: f.inst.Gate(),
		Epochs: box, Clock: f.clk, IDs: f.gen, Key: f.key, InstanceID: f.inst.InstanceID()}, Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	h := &identityHarness{f: f, box: box, admin: f.adminLogin()}
	authztest.RunAuthorizerContract(t, h)
}
