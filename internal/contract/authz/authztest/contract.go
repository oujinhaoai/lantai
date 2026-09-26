package authztest

import (
	"context"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Harness 为授权契约套件准备主体、权限与会话；桩与真实实现各自提供。
type Harness interface {
	Authorizer() authz.Authorizer
	Verifier() authz.SessionVerifier
	// Action 返回套件使用的项目内动作（真实实现中须已登记）。
	Action() authz.Action
	// Principal 登记一个主体。
	Principal(t *testing.T, kind authz.PrincipalKind) ids.ID
	// Allow 让主体在项目内可以执行 Action()。
	Allow(t *testing.T, p, project ids.ID)
	// Revoke 撤销主体在项目内的权限；返回后新的判定必须拒绝。
	Revoke(t *testing.T, p, project ids.ID)
	// Session 为主体开启会话，projects 非空时收窄到这些项目。
	Session(t *testing.T, p ids.ID, projects ...ids.ID) authz.Context
	// End 结束会话。
	End(t *testing.T, who authz.Context)
	// Restore 模拟整馆从备份恢复：推进 recovery_epoch。
	Restore(t *testing.T)
	// Advance 推进时钟，超过会话有效期。
	Advance(t *testing.T, d time.Duration)
}

// RunAuthorizerContract 验证 authz 接口的共同语义：每次判定读取当前状态，撤权、
// 结束会话、到期与整馆恢复之后旧上下文一律失效；收窄只能更窄；未知会话要求
// 重新认证；传输档位只由主体类别决定。
func RunAuthorizerContract(t *testing.T, h Harness) {
	ctx := context.Background()
	decide := func(t *testing.T, who authz.Context, project ids.ID) authz.Decision {
		t.Helper()
		d, err := h.Authorizer().Authorize(ctx, who, h.Action(), authz.Resource{ProjectID: project, Kind: "asset", ID: ids.New()})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	refused := func(t *testing.T, d authz.Decision, codes ...errcode.Code) {
		t.Helper()
		if d.Allowed {
			t.Fatal("decision must refuse")
		}
		for _, c := range codes {
			if d.Code == c {
				return
			}
		}
		t.Fatalf("refusal code %s, want one of %v", d.Code, codes)
	}

	t.Run("grant and revoke apply to later decisions", func(t *testing.T) {
		p, project, other := h.Principal(t, authz.Agent), ids.New(), ids.New()
		who := h.Session(t, p)
		refused(t, decide(t, who, project), errcode.Forbidden, errcode.NotFound)
		h.Allow(t, p, project)
		d := decide(t, who, project)
		if !d.Allowed || d.PolicyRevision < 1 || d.Err() != nil {
			t.Fatalf("allowed decision = %+v", d)
		}
		refused(t, decide(t, who, other), errcode.Forbidden, errcode.NotFound)
		h.Revoke(t, p, project)
		refused(t, decide(t, who, project), errcode.Forbidden, errcode.NotFound, errcode.TokenRevoked)
		if e, ok := errcode.As(decide(t, who, project).Err()); !ok || e.Code == "" {
			t.Fatal("a refusal must convert to a structured error")
		}
	})

	t.Run("narrowed session stays narrow", func(t *testing.T) {
		p, a, b := h.Principal(t, authz.Agent), ids.New(), ids.New()
		h.Allow(t, p, a)
		h.Allow(t, p, b)
		who := h.Session(t, p, a)
		if !who.InScope(a) || who.InScope(b) {
			t.Fatalf("scope of %+v", who.Projects)
		}
		if !decide(t, who, a).Allowed {
			t.Fatal("in-scope project refused")
		}
		refused(t, decide(t, who, b), errcode.Forbidden, errcode.NotFound)
	})

	t.Run("verify returns the trusted context", func(t *testing.T) {
		p := h.Principal(t, authz.Agent)
		who := h.Session(t, p)
		got, err := h.Verifier().VerifySession(ctx, who.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PrincipalID != p || got.PrincipalKind != authz.Agent || got.SessionID != who.SessionID || got.Validate() != nil {
			t.Fatalf("verified context = %+v", got)
		}
		if got.TransferClass() != authz.Batch {
			t.Fatal("agent sessions use the batch transfer class")
		}
		if _, err := h.Verifier().VerifySession(ctx, ids.New()); errcode.CodeOf(err) != errcode.AuthRequired {
			t.Fatalf("unknown session = %v", err)
		}
	})

	t.Run("ended session", func(t *testing.T) {
		p, project := h.Principal(t, authz.Agent), ids.New()
		h.Allow(t, p, project)
		who := h.Session(t, p)
		h.End(t, who)
		if _, err := h.Verifier().VerifySession(ctx, who.SessionID); errcode.CodeOf(err) != errcode.TokenRevoked {
			t.Fatalf("ended session = %v", err)
		}
		refused(t, decide(t, who, project), errcode.TokenRevoked)
	})

	t.Run("expired session", func(t *testing.T) {
		p, project := h.Principal(t, authz.Agent), ids.New()
		h.Allow(t, p, project)
		who := h.Session(t, p)
		h.Advance(t, 13*time.Hour)
		if _, err := h.Verifier().VerifySession(ctx, who.SessionID); errcode.CodeOf(err) != errcode.TokenExpired {
			t.Fatalf("expired session = %v", err)
		}
		refused(t, decide(t, who, project), errcode.TokenExpired)
	})

	t.Run("restore invalidates older sessions", func(t *testing.T) {
		p, project := h.Principal(t, authz.Agent), ids.New()
		h.Allow(t, p, project)
		who := h.Session(t, p)
		h.Restore(t)
		if _, err := h.Verifier().VerifySession(ctx, who.SessionID); errcode.CodeOf(err) != errcode.TokenRevoked {
			t.Fatalf("session from before the restore = %v", err)
		}
		refused(t, decide(t, who, project), errcode.TokenRevoked)
		fresh := h.Session(t, p)
		if !decide(t, fresh, project).Allowed {
			t.Fatal("a new session after the restore must work")
		}
	})
}

// staticHarness 让桩运行同一套件。
type staticHarness struct {
	s     *Static
	clock interface{ Advance(time.Duration) time.Time }
}

// NewStaticHarness 用给定的桩与可控时钟构造契约套件的 Harness。
func NewStaticHarness(s *Static, clk interface{ Advance(time.Duration) time.Time }) Harness {
	return &staticHarness{s: s, clock: clk}
}

func (h *staticHarness) Authorizer() authz.Authorizer    { return h.s }
func (h *staticHarness) Verifier() authz.SessionVerifier { return h.s }
func (h *staticHarness) Action() authz.Action            { return "ledger.commit_version" }
func (h *staticHarness) Principal(_ *testing.T, kind authz.PrincipalKind) ids.ID {
	id := ids.New()
	h.s.AddPrincipal(id, kind)
	return id
}
func (h *staticHarness) Allow(_ *testing.T, p, project ids.ID) { h.s.Grant(p, project, h.Action()) }
func (h *staticHarness) Revoke(_ *testing.T, p, project ids.ID) {
	h.s.Revoke(p, project)
}
func (h *staticHarness) Session(_ *testing.T, p ids.ID, projects ...ids.ID) authz.Context {
	return h.s.OpenSession(p, 12*time.Hour, projects...)
}
func (h *staticHarness) End(_ *testing.T, who authz.Context)   { h.s.EndSession(who.SessionID) }
func (h *staticHarness) Restore(*testing.T)                    { h.s.Restore() }
func (h *staticHarness) Advance(_ *testing.T, d time.Duration) { h.clock.Advance(d) }
