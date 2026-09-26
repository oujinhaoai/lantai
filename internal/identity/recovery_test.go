package identity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/operations"
)

// 口令加恢复码只换取受限恢复会话：它不能访问资源、提权或申请人类授权；开始
// 恢复即作废旧会话与旧授权；完成后旧恢复码、旧因子全部失效。
func TestSelfRecoveryWithRecoveryCode(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	old := f.adminLogin()
	project := ids.New()
	f.grantRole(old.Context, project, f.admin, RoleOwner)
	pending, err := f.svc.CreateChallenge(ctx, old.Context, &SetPolicy{Key: "lock.by_flow", Value: json.RawMessage(`true`)})
	if err != nil {
		t.Fatal(err)
	}
	oldGrant, err := f.svc.VerifyChallenge(ctx, old.Context, pending.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.StartRecovery(ctx, RecoveryRequest{Name: "ada", Password: "wrong password 1", Code: f.codes[0], Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("recovery with wrong password = %v", err)
	}
	if _, err := f.svc.StartRecovery(ctx, RecoveryRequest{Name: "ada", Password: adminPassword, Code: "AAAA-BBBB-CCCC-DDDD", Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("recovery with unknown code = %v", err)
	}
	r, err := f.svc.StartRecovery(ctx, RecoveryRequest{Name: "ada", Password: adminPassword, Code: f.codes[0], Channel: ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	if r.Session.Kind != sessionRecovery || !r.Session.ExpiresAt.Equal(f.clk.Now().Add(15*time.Minute)) {
		t.Fatalf("recovery session = %+v", r.Session)
	}
	// 旧会话与旧授权立即失效；正常登录在设置完成前不可用。
	if _, err := f.svc.VerifySession(ctx, old.Session.SessionID); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("old session = %v", err)
	}
	var state string
	f.svc.main.QueryRow(`SELECT state FROM identity_human_grants WHERE grant_id = ?`, oldGrant.GrantID).Scan(&state)
	if state != "revoked" {
		t.Fatalf("old grant state = %s", state)
	}
	wantReason(t, func() error {
		_, err := f.svc.Login(ctx, LoginRequest{Name: "ada", Password: adminPassword, Code: f.fresh(f.adminSecret), Channel: ChannelCLI})
		return err
	}(), errcode.AuthRequired, "factor_setup_required")
	// 受限会话：没有资源访问、敏感授权或派生能力。
	for _, a := range []authz.Action{ActCatalogRead, ActStorageReadContent} {
		if d := f.decision(r.Context, a, project); d.Allowed || d.Code != errcode.Forbidden {
			t.Fatalf("recovery session %s = %+v", a, d)
		}
	}
	if d := f.decision(r.Context, ActReadPrincipals, ""); d.Allowed {
		t.Fatal("recovery session must not read principals")
	}
	if _, err := f.svc.CreateChallenge(ctx, r.Context, &SetPolicy{Key: "lock.by_flow", Value: json.RawMessage(`true`)}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("recovery session challenge = %v", err)
	}
	if _, err := f.svc.NarrowSession(ctx, r.Context, SessionRequest{Channel: ChannelCLI}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("recovery child session = %v", err)
	}
	if who, err := f.svc.WhoAmI(ctx, r.Context); err != nil || who.FactorState != factorResetPending {
		t.Fatalf("whoami in recovery = %+v %v", who, err)
	}
	// 同一恢复操作可以继续：原码或本批其他未用码都回到同一操作。
	r2, err := f.svc.StartRecovery(ctx, RecoveryRequest{Name: "ada", Password: adminPassword, Code: f.codes[0], Channel: ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	r3, err := f.svc.StartRecovery(ctx, RecoveryRequest{Name: "ada", Password: adminPassword, Code: f.codes[1], Channel: ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	var op1, op2, op3 ids.ID
	for sid, dst := range map[ids.ID]*ids.ID{r.Session.SessionID: &op1, r2.Session.SessionID: &op2, r3.Session.SessionID: &op3} {
		f.svc.runtime.QueryRow(`SELECT operation_id FROM identity_sessions WHERE session_id = ?`, sid).Scan(dst)
	}
	if op1 == "" || op1 != op2 || op1 != op3 {
		t.Fatalf("recovery must continue one operation: %s %s %s", op1, op2, op3)
	}
	e, err := f.svc.EnrollFactor(ctx, r3.Context)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ConfirmFactor(ctx, r3.Context, f.now(f.adminSecret)); !isCode(err, errcode.HumanProofRequired) {
		t.Fatalf("confirm with the old authenticator = %v", err)
	}
	newCodes, err := f.svc.ConfirmFactor(ctx, r3.Context, f.now(e.raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(newCodes) != 10 {
		t.Fatalf("new recovery codes = %d", len(newCodes))
	}
	for _, s := range []IssuedSession{r, r2, r3} {
		if _, err := f.svc.VerifySession(ctx, s.Session.SessionID); !isCode(err, errcode.TokenRevoked) {
			t.Fatalf("recovery session after completion = %v", err)
		}
	}
	// 旧恢复码（用过的、没用过的）与旧因子全部失效。
	for _, code := range []string{f.codes[0], f.codes[1], f.codes[2]} {
		if _, err := f.svc.StartRecovery(ctx, RecoveryRequest{Name: "ada", Password: adminPassword, Code: code, Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
			t.Fatalf("old recovery code after completion = %v", err)
		}
	}
	if _, err := f.svc.Login(ctx, LoginRequest{Name: "ada", Password: adminPassword, Code: f.fresh(f.adminSecret), Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("login with the old authenticator = %v", err)
	}
	f.adminSecret, f.codes = e.raw, newCodes
	s := f.adminLogin()
	if d := f.decision(s.Context, ActCatalogRead, project); !d.Allowed {
		t.Fatalf("after recovery = %+v", d)
	}
	if _, err := f.svc.Execute(ctx, s.Context, oldGrant.GrantID, "k", &SetPolicy{Key: "lock.by_flow", Value: json.RawMessage(`true`)}); !isCode(err, errcode.HumanGrantMismatch) {
		t.Fatalf("old grant after recovery = %v", err)
	}
}

// 新成员与管理员重置都走一次性设置码：必须先设口令、再登记并确认验证器；
// 设置码只能用一次；管理员不能重置自己。
func TestSetupCodeAndAdminReset(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	alice, setup := f.register(admin, authz.Human, "alice")
	if setup == "" {
		t.Fatal("registering a person must return a setup code")
	}
	// 待设置的人不能直接登录，恢复码路径也不适用。
	if _, err := f.svc.StartRecovery(ctx, RecoveryRequest{Name: "alice", Password: "whatever pass 1", Code: setup, Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("recovery before setup = %v", err)
	}
	if _, err := f.svc.StartSetup(ctx, SetupRequest{Name: "alice", Code: "AAAA-BBBB-CCCC-DDDD", Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("wrong setup code = %v", err)
	}
	s, err := f.svc.StartSetup(ctx, SetupRequest{Name: "alice", Code: setup, Channel: ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.svc.EnrollFactor(ctx, s.Context)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ConfirmFactor(ctx, s.Context, f.now(e.raw)); !isCode(err, errcode.InvalidStateTransition) {
		t.Fatalf("confirm before setting a password = %v", err)
	}
	if err := f.svc.SetPassword(ctx, s.Context, "short"); !isCode(err, errcode.SchemaInvalid) {
		t.Fatalf("weak password = %v", err)
	}
	if err := f.svc.SetPassword(ctx, s.Context, "alice password 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ConfirmFactor(ctx, s.Context, f.now(e.raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.StartSetup(ctx, SetupRequest{Name: "alice", Code: setup, Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("setup code reuse = %v", err)
	}
	as := f.login("alice", "alice password 1", e.raw, ChannelCLI)

	// 管理员重置 alice：她的会话失效，恢复码作废，拿到新的设置码。
	res := f.mustSudo(admin, f.adminSecret, &ResetHumanFactor{PrincipalID: alice.ID, ExpectedRevision: currentRev(t, f, alice.ID), Reason: "lost phone"})
	if res.Secret == "" {
		t.Fatal("reset must return a setup code")
	}
	if _, err := f.svc.VerifySession(ctx, as.Session.SessionID); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("alice's session after reset = %v", err)
	}
	if _, err := f.svc.Login(ctx, LoginRequest{Name: "alice", Password: "alice password 1", Code: f.fresh(e.raw), Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("login after reset = %v", err)
	}
	f.completeSetup("alice", res.Secret, "alice password 2")
	// 管理员不能用这条路径重置自己；非管理员不能重置别人。
	if _, err := f.svc.CreateChallenge(ctx, admin, &ResetHumanFactor{PrincipalID: f.admin.ID, ExpectedRevision: currentRev(t, f, f.admin.ID), Reason: "x"}); err != nil {
		t.Fatal(err) // 资格检查通过，自我重置在执行时拒绝
	}
	if _, err := f.sudo(admin, f.adminSecret, &ResetHumanFactor{PrincipalID: f.admin.ID, ExpectedRevision: currentRev(t, f, f.admin.ID), Reason: "x"}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("self reset = %v", err)
	}
}

// 恢复码也丢失、只有一位管理员时的本机离线恢复：只能在未启动服务的本机实例
// 上进行；完成后旧会话、旧因子与旧恢复码失效。
func TestOfflineReset(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	old := f.adminLogin()
	if _, err := f.svc.BeginOfflineReset(ctx, "ada", "new admin password"); !errors.Is(err, ErrNotLocal) {
		t.Fatalf("offline reset on a serving instance = %v", err)
	}
	home := f.inst.Layout().Home
	if err := f.inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	inst, err := operations.Open(ctx, operations.Options{Home: home, Clock: f.clk, IDs: f.gen})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(context.Background())
	f.inst = inst
	f.svc = f.newService(f.key)
	if _, err := f.svc.BeginOfflineReset(ctx, "nobody", "new admin password"); !isCode(err, errcode.NotFound) {
		t.Fatalf("unknown principal = %v", err)
	}
	o, err := f.svc.BeginOfflineReset(ctx, "ada", "new admin password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Confirm(ctx, f.now(f.adminSecret), "lost phone and codes"); !isCode(err, errcode.HumanProofRequired) {
		t.Fatalf("confirm with the old authenticator = %v", err)
	}
	codes, err := o.Confirm(ctx, f.now(o.Enrollment.raw), "lost phone and codes")
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifySession(ctx, old.Session.SessionID); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("old session after offline reset = %v", err)
	}
	if _, err := f.svc.StartRecovery(ctx, RecoveryRequest{Name: "ada", Password: "new admin password", Code: f.codes[3], Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("old recovery code = %v", err)
	}
	f.adminSecret, f.codes = o.Enrollment.raw, codes
	s := f.login("ada", "new admin password", f.adminSecret, ChannelCLI)
	if s.Context.PrincipalID != f.admin.ID {
		t.Fatal("offline reset must keep the principal identity")
	}
	var n int
	f.inst.DB(ownership.Main).QueryRow(`SELECT count(*) FROM outbox WHERE event_type = ?`, EvOfflineRecovery).Scan(&n)
	if n != 1 {
		t.Fatalf("offline recovery audit events = %d", n)
	}
}
