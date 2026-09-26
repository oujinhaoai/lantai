package identity

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
)

// 长期凭据只换会话：范围、项目与有效期只能收窄；长期凭据与浏览器会话令牌
// 不能当作 Bearer 调用业务接口。
func TestExchangeNarrowsAndRejectsLongLivedTokens(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	p1, p2 := ids.New(), ids.New()
	agent, _ := f.register(admin, authz.Agent, "claude-code@node-a")
	token := f.issueToken(admin, agent, []Scope{ScopeRead, ScopeIngest}, p1)

	s := f.exchange(token, SessionRequest{Scopes: []Scope{ScopeRead}, TTL: time.Hour, Purpose: "ingest batch", Model: "claude-opus-5-5"})
	if got := s.Session.Scopes; len(got) != 2 || got[0] != ScopeRead || got[1] != ScopeSelf {
		t.Fatalf("scopes = %v", got)
	}
	if len(s.Session.Projects) != 1 || s.Session.Projects[0] != p1 || !s.Session.ExpiresAt.Equal(f.clk.Now().Add(time.Hour)) {
		t.Fatalf("session = %+v", s.Session)
	}
	if !strings.HasPrefix(s.Token, SessionPrefix) || s.CSRFToken != "" {
		t.Fatal("cli sessions get a bearer token and no CSRF token")
	}
	for name, req := range map[string]SessionRequest{
		"widen scope":   {Scopes: []Scope{ScopeOrganize}, Channel: ChannelCLI},
		"admin scope":   {Scopes: []Scope{ScopeAdmin}, Channel: ChannelCLI},
		"widen project": {Projects: []ids.ID{p2}, Channel: ChannelCLI},
	} {
		if _, err := f.svc.ExchangeToken(ctx, token, req); !isCode(err, errcode.Forbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.ExchangeToken(ctx, token, SessionRequest{Channel: ChannelBrowser}); !isCode(err, errcode.SchemaInvalid) {
		t.Fatalf("agent browser session = %v", err)
	}
	// 12 小时上限。
	long := f.exchange(token, SessionRequest{TTL: 72 * time.Hour})
	if !long.Session.ExpiresAt.Equal(f.clk.Now().Add(12 * time.Hour)) {
		t.Fatalf("ttl not capped: %v", long.Session.ExpiresAt)
	}
	ac, err := f.svc.AuthenticateToken(ctx, s.Token)
	if err != nil || ac.SessionID != s.Session.SessionID || ac.PrincipalKind != authz.Agent {
		t.Fatalf("authenticate = %+v %v", ac, err)
	}
	wantReason(t, func() error { _, err := f.svc.AuthenticateToken(ctx, token); return err }(), errcode.AuthRequired, "long_lived_token_not_accepted")
	for _, bad := range []string{"", "lts_short", s.Token + "x", "Bearer " + s.Token} {
		if _, err := f.svc.AuthenticateToken(ctx, bad); !isCode(err, errcode.AuthRequired) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := f.svc.ExchangeToken(ctx, "ltk_"+strings.Repeat("A", 43), SessionRequest{Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("unknown credential = %v", err)
	}
	// 错误信息不回显凭据。
	_, err = f.svc.AuthenticateToken(ctx, s.Token[:20]+"zz")
	if e, _ := errcode.As(err); e == nil || strings.Contains(e.Message, s.Token[:20]) {
		t.Fatalf("error leaks the token: %v", err)
	}
	// 浏览器会话只接受 Cookie；Cookie 通道不接受 CLI 会话。
	web := f.login("ada", adminPassword, f.adminSecret, ChannelBrowser)
	wantReason(t, func() error { _, err := f.svc.AuthenticateToken(ctx, web.Token); return err }(), errcode.AuthRequired, "channel_mismatch")
	wantReason(t, func() error { _, err := f.svc.AuthenticateBrowser(ctx, s.Token, "", false); return err }(), errcode.AuthRequired, "channel_mismatch")
	if web.CSRFToken == "" || web.Context.TransferClass() != authz.Interactive {
		t.Fatalf("browser login = %+v", web)
	}
}

// 会话收窄与子会话：只能更窄，父会话结束子会话随之失效。
func TestChildSessions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	p1, p2 := ids.New(), ids.New()
	agent, _ := f.register(admin, authz.Agent, "claude-code@node-a")
	f.grantRole(admin, p1, agent, RoleContributor)
	f.grantRole(admin, p2, agent, RoleContributor)
	parent := f.exchange(f.issueToken(admin, agent, []Scope{ScopeRead, ScopeIngest}), SessionRequest{Projects: []ids.ID{p1, p2}})
	child, err := f.svc.NarrowSession(ctx, parent.Context, SessionRequest{Scopes: []Scope{ScopeRead}, Projects: []ids.ID{p1}, Channel: ChannelCLI, Purpose: "sub-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if d := f.decision(child.Context, ActCatalogRead, p1); !d.Allowed {
		t.Fatalf("child read p1 = %+v", d)
	}
	if d := f.decision(child.Context, ActCatalogRead, p2); d.Code != errcode.NotFound {
		t.Fatalf("child read outside its projects = %+v", d)
	}
	if d := f.decision(child.Context, ActStorageUpload, p1); d.Code != errcode.Forbidden {
		t.Fatalf("child upload without ingest scope = %+v", d)
	}
	if _, err := f.svc.NarrowSession(ctx, child.Context, SessionRequest{Scopes: []Scope{ScopeIngest}, Channel: ChannelCLI}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("child widening scope = %v", err)
	}
	if _, err := f.svc.NarrowSession(ctx, parent.Context, SessionRequest{Projects: []ids.ID{ids.New()}, Channel: ChannelCLI}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("child widening projects = %v", err)
	}
	// 孙会话在深度上限之内有效；再往下派生被拒。
	grand, err := f.svc.NarrowSession(ctx, child.Context, SessionRequest{Channel: ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifySession(ctx, grand.Session.SessionID); err != nil {
		t.Fatalf("grandchild within depth = %v", err)
	}
	if _, err := f.svc.NarrowSession(ctx, grand.Context, SessionRequest{Channel: ChannelCLI}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("session nested beyond the depth limit = %v", err)
	}
	// 父会话结束：子会话与孙会话一并失效。
	if err := f.svc.EndSession(ctx, parent.Context, parent.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	for _, s := range []IssuedSession{parent, child, grand} {
		if _, err := f.svc.VerifySession(ctx, s.Session.SessionID); !isCode(err, errcode.TokenRevoked) {
			t.Fatalf("session after parent ended = %v", err)
		}
	}
	// 人的会话（CLI 或浏览器）都不能派生子令牌：子令牌会交给子进程或 Agent，
	// 不能带着“人本人”的身份通过人审与管理检查。
	for _, ch := range []string{ChannelCLI, ChannelBrowser} {
		human := f.login("ada", adminPassword, f.adminSecret, ch)
		if _, err := f.svc.NarrowSession(ctx, human.Context, SessionRequest{Channel: ChannelCLI}); !isCode(err, errcode.Forbidden) {
			t.Fatalf("human %s child session = %v", ch, err)
		}
	}
	// 结束别人的会话：本人看不到（NOT_FOUND）。
	other := f.exchange(f.issueToken(admin, agent, []Scope{ScopeRead}), SessionRequest{})
	if err := f.svc.EndSession(ctx, admin, other.Session.SessionID); !isCode(err, errcode.NotFound) {
		t.Fatalf("ending someone else's session = %v", err)
	}
}

// 插件、adapter 等代为调用者只拿到短期、限动作与项目的委托能力；主体与委托方
// 分别记录，任一失效能力随即失效；不能委托管理或人类授权动作。
func TestDelegatedCapability(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	p1, p2 := ids.New(), ids.New()
	agent, _ := f.register(admin, authz.Agent, "claude-code@node-a")
	plugin, _ := f.register(admin, authz.Service, "service:gltf-validator")
	f.grantRole(admin, p1, agent, RoleContributor)
	parent := f.exchange(f.issueToken(admin, agent, []Scope{ScopeRead, ScopeIngest}), SessionRequest{})

	req := DelegationRequest{Delegate: plugin.ID, Actions: []authz.Action{ActCatalogRead}, Projects: []ids.ID{p1}, TTL: 10 * time.Hour, Purpose: "validate"}
	d, err := f.svc.IssueDelegated(ctx, parent.Context, req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Context.PrincipalID != agent.ID || d.Context.DelegatedBy != plugin.ID || d.Context.TransferClass() != authz.Batch {
		t.Fatalf("delegated context = %+v", d.Context)
	}
	if !d.Session.ExpiresAt.Equal(f.clk.Now().Add(time.Hour)) {
		t.Fatalf("delegation must be short-lived: %v", d.Session.ExpiresAt)
	}
	if dec := f.decision(d.Context, ActCatalogRead, p1); !dec.Allowed {
		t.Fatalf("delegated read = %+v", dec)
	}
	if dec := f.decision(d.Context, ActStorageUpload, p1); dec.Allowed {
		t.Fatal("delegation must be limited to the listed actions")
	}
	if dec := f.decision(d.Context, ActCatalogRead, p2); dec.Allowed {
		t.Fatal("delegation must be limited to the listed projects")
	}
	for name, bad := range map[string]DelegationRequest{
		"admin action":    {Delegate: plugin.ID, Actions: []authz.Action{ActRegisterPrincipal}, Projects: []ids.ID{p1}},
		"grant action":    {Delegate: plugin.ID, Actions: []authz.Action{ActGrantProjectRole}, Projects: []ids.ID{p1}},
		"not permitted":   {Delegate: plugin.ID, Actions: []authz.Action{ActCatalogPatchMetadata}, Projects: []ids.ID{p1}},
		"other project":   {Delegate: plugin.ID, Actions: []authz.Action{ActCatalogRead}, Projects: []ids.ID{p2}},
		"agent delegate":  {Delegate: agent.ID, Actions: []authz.Action{ActCatalogRead}, Projects: []ids.ID{p1}},
		"unknown":         {Delegate: ids.New(), Actions: []authz.Action{ActCatalogRead}, Projects: []ids.ID{p1}},
		"missing project": {Delegate: plugin.ID, Actions: []authz.Action{ActCatalogRead}},
	} {
		if _, err := f.svc.IssueDelegated(ctx, parent.Context, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// 委托会话可以查看自己、结束自己（不受动作白名单限制），但不能再委托、
	// 不能派生子会话、不能申请人类授权。
	if who, err := f.svc.WhoAmI(ctx, d.Context); err != nil || who.Session.DelegatedBy != plugin.ID {
		t.Fatalf("delegated whoami = %+v %v", who.Session, err)
	}
	if _, err := f.svc.IssueDelegated(ctx, d.Context, req); !isCode(err, errcode.Forbidden) {
		t.Fatalf("re-delegation = %v", err)
	}
	if _, err := f.svc.NarrowSession(ctx, d.Context, SessionRequest{Channel: ChannelCLI}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("delegated child = %v", err)
	}
	// 人的委托也不能通过人才能做的检查。
	human := f.adminLogin()
	f.grantRole(admin, p1, f.admin, RoleOwner)
	hd, err := f.svc.IssueDelegated(ctx, human.Context, DelegationRequest{Delegate: plugin.ID, Actions: []authz.Action{ActCatalogRead}, Projects: []ids.ID{p1}})
	if err != nil {
		t.Fatal(err)
	}
	if hd.Context.PrincipalKind != authz.Human || hd.Context.TransferClass() != authz.Batch {
		t.Fatalf("human delegation = %+v", hd.Context)
	}
	if _, err := f.svc.CreateChallenge(ctx, hd.Context, &SetPolicy{ProjectID: p1, Key: "publish.mode", Value: []byte(`"manual"`)}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("delegated human challenge = %v", err)
	}
	short, err := f.svc.IssueDelegated(ctx, parent.Context, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.EndSession(ctx, short.Context, short.Session.SessionID); err != nil {
		t.Fatalf("a delegate ending its own session = %v", err)
	}
	// 委托方停用 → 能力失效；父会话结束 → 能力失效。
	f.mustSudo(admin, f.adminSecret, &DisablePrincipal{PrincipalID: plugin.ID, ExpectedRevision: currentRev(t, f, plugin.ID), Reason: "rotate"})
	if _, err := f.svc.VerifySession(ctx, d.Session.SessionID); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("delegation after delegate disabled = %v", err)
	}
	if err := f.svc.EndSession(ctx, human.Context, human.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifySession(ctx, hd.Session.SessionID); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("delegation after parent ended = %v", err)
	}
}

// 撤权使旧会话、旧上下文立即失效，不靠缓存放行；重新启用也不复活旧会话。
func TestRevocationInvalidatesSessions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	agent, _ := f.register(admin, authz.Agent, "claude-code@node-a")
	token := f.issueToken(admin, agent, []Scope{ScopeRead})
	s1 := f.exchange(token, SessionRequest{})
	cached := s1.Context // 调用方手里的旧上下文

	f.mustSudo(admin, f.adminSecret, &DisablePrincipal{PrincipalID: agent.ID, ExpectedRevision: currentRev(t, f, agent.ID), Reason: "compromised"})
	if _, err := f.svc.AuthenticateToken(ctx, s1.Token); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("session after disable = %v", err)
	}
	if d := f.decision(cached, ActCatalogRead, ids.New()); d.Allowed || d.Code != errcode.TokenRevoked {
		t.Fatalf("cached context after disable = %+v", d)
	}
	if _, err := f.svc.ExchangeToken(ctx, token, SessionRequest{Channel: ChannelCLI}); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("exchange after disable = %v", err)
	}
	f.mustSudo(admin, f.adminSecret, &EnablePrincipal{PrincipalID: agent.ID, ExpectedRevision: currentRev(t, f, agent.ID)})
	if _, err := f.svc.AuthenticateToken(ctx, s1.Token); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("old session must stay dead after re-enable: %v", err)
	}
	s2 := f.exchange(token, SessionRequest{})

	// 吊销凭据：由它换出的会话失效，凭据不能再换会话。
	var credID ids.ID
	f.svc.main.QueryRow(`SELECT credential_id FROM identity_credentials WHERE principal_id = ?`, agent.ID).Scan(&credID)
	f.mustSudo(admin, f.adminSecret, &RevokeCredential{CredentialID: credID, Reason: "leaked"})
	if _, err := f.svc.AuthenticateToken(ctx, s2.Token); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("session after credential revoke = %v", err)
	}
	if _, err := f.svc.ExchangeToken(ctx, token, SessionRequest{Channel: ChannelCLI}); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("exchange with revoked credential = %v", err)
	}

	// 管理员吊销单个会话。
	s3 := f.exchange(f.issueToken(admin, agent, []Scope{ScopeRead}), SessionRequest{})
	f.mustSudo(admin, f.adminSecret, &RevokeSession{SessionID: s3.Session.SessionID, Reason: "stuck"})
	if _, err := f.svc.VerifySession(ctx, s3.Session.SessionID); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("revoked session = %v", err)
	}

	// 到期。
	s4 := f.exchange(f.issueToken(admin, agent, []Scope{ScopeRead}), SessionRequest{TTL: time.Minute})
	f.clk.Advance(2 * time.Minute)
	if _, err := f.svc.VerifySession(ctx, s4.Session.SessionID); !isCode(err, errcode.TokenExpired) {
		t.Fatalf("expired session = %v", err)
	}
}

type epochBox struct{ v atomic.Int64 }

func (e *epochBox) RecoveryEpoch(context.Context) (int64, error) { return e.v.Load(), nil }

// 从备份恢复推进 recovery_epoch 后，旧会话与旧人类授权一律失效。
func TestRecoveryEpochInvalidatesOldSessions(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	box := &epochBox{}
	box.v.Store(1)
	svc, err := New(Deps{Main: f.inst.DB(ownership.Main), Runtime: f.inst.DB(ownership.Runtime), Gate: f.inst.Gate(),
		Epochs: box, Clock: f.clk, IDs: f.gen, Key: f.key, InstanceID: f.inst.InstanceID()}, Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	s := f.adminLogin()
	ch, err := svc.CreateChallenge(ctx, s.Context, &SetPolicy{Key: "publish.mode", Value: []byte(`"manual"`)})
	if err != nil {
		t.Fatal(err)
	}
	g, err := svc.VerifyChallenge(ctx, s.Context, ch.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}
	box.v.Store(2)
	if _, err := svc.VerifySession(ctx, s.Session.SessionID); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("session from before the restore = %v", err)
	}
	s2 := f.adminLogin()
	if _, err := svc.Execute(ctx, s2.Context, g.GrantID, "k", &SetPolicy{Key: "publish.mode", Value: []byte(`"manual"`)}); !isCode(err, errcode.HumanGrantMismatch) {
		t.Fatalf("grant from before the restore used from a new session = %v", err)
	}
}

// 最终接受与撤权经同一协调边界：撤权等待在锁内的提交完成并保留其结果；
// 撤权返回后开始的接受（含下载开始）一律拒绝，不靠缓存放行。
func TestRevocationAndFinalAcceptanceAreSerialized(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin()
	project := ids.New()
	agent, _ := f.register(admin.Context, authz.Agent, "claude-code@node-a")
	f.grantRole(admin.Context, project, agent, RoleContributor)
	s := f.exchange(f.issueToken(admin.Context, agent, []Scope{ScopeRead, ScopeIngest}), SessionRequest{})
	res := authz.Resource{ProjectID: project, Kind: "asset", ID: ids.New()}

	// 准备好停用命令的人类授权，使撤权可以在提交进行中立即发起。
	disable := &DisablePrincipal{PrincipalID: agent.ID, ExpectedRevision: currentRev(t, f, agent.ID), Reason: "revoked"}
	ch, err := f.svc.CreateChallenge(ctx, admin.Context, disable)
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.svc.VerifyChallenge(ctx, admin.Context, ch.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}

	inCommit := make(chan struct{})
	release := make(chan struct{})
	var committed atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := f.svc.Accept(context.Background(), s.Context, ActLedgerCommitVersion, res, commands.Request{Assets: []string{string(res.ID)}},
			func(ctx context.Context, d authz.Decision) error {
				close(inCommit)
				<-release
				committed.Store(true)
				return nil
			})
		if err != nil {
			t.Errorf("accept in progress before revocation: %v", err)
		}
	}()
	<-inCommit
	revoked := make(chan error, 1)
	go func() {
		_, err := f.svc.Execute(context.Background(), admin.Context, g.GrantID, "disable-1", disable)
		revoked <- err
	}()
	select {
	case err := <-revoked:
		t.Fatalf("revocation finished while a commit held the security guard: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if !committed.Load() {
		t.Fatal("the commit accepted before the revocation must keep its result")
	}
	called := false
	err = f.svc.Accept(ctx, s.Context, ActLedgerCommitVersion, res, commands.Request{}, func(context.Context, authz.Decision) error {
		called = true
		return nil
	})
	if !isCode(err, errcode.TokenRevoked) || called {
		t.Fatalf("accept after revocation = %v (commit called: %v)", err, called)
	}
	err = f.svc.BeginRead(ctx, s.Context, ActStorageReadContent, res, func(context.Context, authz.Decision) error {
		called = true
		return nil
	})
	if !isCode(err, errcode.TokenRevoked) || called {
		t.Fatalf("download after revocation = %v", err)
	}
}

// 下载开始与业务提交使用同一当前授权语义：角色撤销后新的读取立即拒绝。
func TestRoleRevocationAppliesToReadsAndWrites(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	project := ids.New()
	agent, _ := f.register(admin, authz.Agent, "claude-code@node-a")
	f.grantRole(admin, project, agent, RoleContributor)
	s := f.exchange(f.issueToken(admin, agent, []Scope{ScopeRead, ScopeIngest}), SessionRequest{})
	res := authz.Resource{ProjectID: project}
	ok := func(context.Context, authz.Decision) error { return nil }
	if err := f.svc.BeginRead(ctx, s.Context, ActStorageReadContent, res, ok); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Accept(ctx, s.Context, ActStorageUpload, res, commands.Request{}, ok); err != nil {
		t.Fatal(err)
	}
	f.mustSudo(admin, f.adminSecret, &SetProjectRole{ProjectID: project, ExpectedRevision: 1, PrincipalID: agent.ID, Role: RoleContributor})
	if err := f.svc.BeginRead(ctx, s.Context, ActStorageReadContent, res, ok); !isCode(err, errcode.NotFound) {
		t.Fatalf("read after role removal = %v", err)
	}
	if err := f.svc.Accept(ctx, s.Context, ActStorageUpload, res, commands.Request{}, ok); !isCode(err, errcode.NotFound) {
		t.Fatalf("write after role removal = %v", err)
	}
	// 维护期间写入被拒，读取照常（读取不经维护屏障）。
	m, err := f.inst.Maintain(ctx, commands.ReasonMaintenance)
	if err != nil {
		t.Fatal(err)
	}
	defer m.End()
	if _, err := f.svc.ExchangeToken(ctx, "ltk_"+strings.Repeat("A", 43), SessionRequest{Channel: ChannelCLI}); !isCode(err, errcode.MaintenanceMode) {
		t.Fatalf("session exchange during maintenance = %v", err)
	}
	if _, err := f.svc.VerifySession(ctx, s.Session.SessionID); err != nil {
		t.Fatalf("verification during maintenance = %v", err)
	}
	if err := f.svc.BeginRead(ctx, admin, ActReadMembers, res, ok); err != nil {
		t.Fatalf("read during maintenance = %v", err)
	}
	if err := f.svc.Accept(ctx, admin, ActReadMembers, res, commands.Request{}, ok); !isCode(err, errcode.MaintenanceMode) {
		t.Fatalf("write during maintenance = %v", err)
	}
}
