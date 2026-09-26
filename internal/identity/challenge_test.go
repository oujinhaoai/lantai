package identity

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

// 人类授权绑定动作、目标集合、请求摘要与会话：换任何一项都不能复用；已提交
// 的操作只重放原回执，不产生第二次效果，一次性凭据也不会再返回。
func TestGrantBindingAndReplay(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	s1 := f.adminLogin()
	admin := s1.Context
	a, _ := f.register(admin, authz.Agent, "claude-code@node-a")
	b, _ := f.register(admin, authz.Agent, "codex@node-a")

	cmd := &IssueCredential{PrincipalID: a.ID, ExpectedRevision: currentRev(t, f, a.ID), Scopes: []Scope{ScopeRead}}
	ch, err := f.svc.CreateChallenge(ctx, admin, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ch.Summary, "claude-code@node-a") || !ch.RequestHash.Valid() || !ch.TargetSetDigest.Valid() ||
		!ch.ExpiresAt.Equal(f.clk.Now().Add(5*time.Minute)) {
		t.Fatalf("challenge = %+v", ch)
	}
	g, err := f.svc.VerifyChallenge(ctx, admin, ch.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}
	// 同一挑战不能兑换第二次。
	if _, err := f.svc.VerifyChallenge(ctx, admin, ch.ChallengeID, f.fresh(f.adminSecret), ""); !isCode(err, errcode.HumanGrantMismatch) {
		t.Fatalf("second redemption = %v", err)
	}
	s2 := f.adminLogin()
	mismatches := map[string]struct {
		who authz.Context
		cmd Command
	}{
		"request_hash": {admin, &IssueCredential{PrincipalID: a.ID, ExpectedRevision: currentRev(t, f, a.ID), Scopes: []Scope{ScopeRead, ScopeIngest}}},
		"target_set":   {admin, &IssueCredential{PrincipalID: b.ID, ExpectedRevision: currentRev(t, f, b.ID), Scopes: []Scope{ScopeRead}}},
		"action":       {admin, &DisablePrincipal{PrincipalID: a.ID, ExpectedRevision: currentRev(t, f, a.ID), Reason: "x"}},
		"session":      {s2.Context, cmd},
	}
	for reason, m := range mismatches {
		_, err := f.svc.Execute(ctx, m.who, g.GrantID, "key-1", m.cmd)
		wantReason(t, err, errcode.HumanGrantMismatch, reason)
	}
	res, err := f.svc.Execute(ctx, admin, g.GrantID, "key-1", cmd)
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || !strings.HasPrefix(res.Secret, CredentialPrefix) || res.OperationID != ch.OperationID {
		t.Fatalf("first execution = %+v", res)
	}
	// 响应丢失后重放：原结果、没有第二次效果、不再返回令牌；授权过期也不影响重放。
	f.clk.Advance(10 * time.Minute)
	again, err := f.svc.Execute(ctx, admin, g.GrantID, "key-1", cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Secret != "" || string(again.Summary) != string(res.Summary) {
		t.Fatalf("replay = %+v", again)
	}
	var n int
	f.svc.main.QueryRow(`SELECT count(*) FROM identity_credentials WHERE principal_id = ?`, a.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d credentials after replay, want 1", n)
	}
	// 同一授权换幂等键：拒绝，不能借一次授权执行两次。
	if _, err := f.svc.Execute(ctx, admin, g.GrantID, "key-2", cmd); !isCode(err, errcode.IdempotencyConflict) {
		t.Fatalf("grant reused with another key = %v", err)
	}
	// 回执、事件都不含令牌。
	for _, q := range []string{`SELECT coalesce(response_summary, '') FROM command_receipts`, `SELECT envelope FROM outbox`} {
		rows, err := f.svc.main.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var text string
			rows.Scan(&text)
			if strings.Contains(text, res.Secret) || strings.Contains(text, res.Secret[len(CredentialPrefix):]) {
				t.Fatalf("a persisted record contains the credential: %s", q)
			}
		}
		rows.Close()
	}
}

// 同一成功时间步不能再换取挑战：登录用过的码、兑换过别的挑战的码都被拒。
func TestTOTPTimeStepCannotBeReused(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	s := f.adminLogin() // 消耗了当前时间步
	admin := s.Context
	cmd := &SetPolicy{Key: "publish.mode", Value: json.RawMessage(`"manual"`)}
	chA, err := f.svc.CreateChallenge(ctx, admin, cmd)
	if err != nil {
		t.Fatal(err)
	}
	used := f.now(f.adminSecret)
	if _, err := f.svc.VerifyChallenge(ctx, admin, chA.ChallengeID, used, ""); !isCode(err, errcode.TOTPReplay) {
		t.Fatalf("code used for login = %v", err)
	}
	next := f.fresh(f.adminSecret)
	if _, err := f.svc.VerifyChallenge(ctx, admin, chA.ChallengeID, next, ""); err != nil {
		t.Fatal(err)
	}
	chB, err := f.svc.CreateChallenge(ctx, admin, &SetPolicy{Key: "lock.by_flow", Value: json.RawMessage(`true`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifyChallenge(ctx, admin, chB.ChallengeID, next, ""); !isCode(err, errcode.TOTPReplay) {
		t.Fatalf("same step for another challenge = %v", err)
	}
	// 上一个时间步（漂移窗口内）也不能在更晚的成功之后使用。
	prev := totp.Code(f.adminSecret, totp.Counter(f.clk.Now())-1)
	if _, err := f.svc.VerifyChallenge(ctx, admin, chB.ChallengeID, prev, ""); !isCode(err, errcode.TOTPReplay) {
		t.Fatalf("older step after a newer success = %v", err)
	}
	if _, err := f.svc.VerifyChallenge(ctx, admin, chB.ChallengeID, f.fresh(f.adminSecret), ""); err != nil {
		t.Fatal(err)
	}
}

// 挑战只能由发起它的会话兑换；别人的挑战看不到；过期的挑战与授权需要重新挑战，
// 且重新挑战只能针对同一操作的同一摘要。
func TestChallengeSessionAndExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	s1 := f.adminLogin()
	cmd := &SetPolicy{Key: "trash.retention_days", Value: json.RawMessage(`45`)}
	ch, err := f.svc.CreateChallenge(ctx, s1.Context, cmd)
	if err != nil {
		t.Fatal(err)
	}
	s2 := f.adminLogin()
	if _, err := f.svc.VerifyChallenge(ctx, s2.Context, ch.ChallengeID, f.fresh(f.adminSecret), ""); !isCode(err, errcode.HumanGrantMismatch) {
		t.Fatalf("redeemed from another session = %v", err)
	}
	// 另一个人看不到这个挑战。
	bob, setup := f.register(s1.Context, authz.Human, "bob")
	bobSecret := f.completeSetup(bob.Name, setup, "bob password 123")
	bs := f.login("bob", "bob password 123", bobSecret, ChannelCLI)
	if _, err := f.svc.VerifyChallenge(ctx, bs.Context, ch.ChallengeID, f.fresh(bobSecret), ""); !isCode(err, errcode.NotFound) {
		t.Fatalf("someone else's challenge = %v", err)
	}
	// 挑战过期。
	f.clk.Advance(6 * time.Minute)
	wantReason(t, func() error {
		_, err := f.svc.VerifyChallenge(ctx, s1.Context, ch.ChallengeID, f.fresh(f.adminSecret), "")
		return err
	}(), errcode.HumanProofRequired, "challenge_expired")
	// 授权过期：同一操作按同一摘要重新挑战后执行成功，操作 ID 不变。
	s1 = f.adminLogin()
	ch, err = f.svc.CreateChallenge(ctx, s1.Context, cmd)
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.svc.VerifyChallenge(ctx, s1.Context, ch.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(6 * time.Minute)
	wantReason(t, func() error { _, err := f.svc.Execute(ctx, s1.Context, g.GrantID, "k1", cmd); return err }(), errcode.HumanProofRequired, "grant_expired")
	if _, err := f.svc.Rechallenge(ctx, s1.Context, ch.OperationID, &SetPolicy{Key: "trash.retention_days", Value: json.RawMessage(`60`)}); !isCode(err, errcode.HumanGrantMismatch) {
		t.Fatalf("re-challenge with another request = %v", err)
	}
	ch2, err := f.svc.Rechallenge(ctx, s1.Context, ch.OperationID, cmd)
	if err != nil || ch2.OperationID != ch.OperationID {
		t.Fatalf("re-challenge = %+v %v", ch2, err)
	}
	g2, err := f.svc.VerifyChallenge(ctx, s1.Context, ch2.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Execute(ctx, s1.Context, g2.GrantID, "k1", cmd)
	if err != nil || res.OperationID != ch.OperationID {
		t.Fatalf("execute after re-challenge = %+v %v", res, err)
	}
	if _, err := f.svc.Rechallenge(ctx, s1.Context, ch.OperationID, cmd); !isCode(err, errcode.InvalidStateTransition) {
		t.Fatalf("re-challenge of a committed operation = %v", err)
	}
	// 撤回尚未使用的授权。
	ch3, _ := f.svc.CreateChallenge(ctx, s1.Context, &SetPolicy{Key: "lock.by_flow", Value: json.RawMessage(`true`)})
	g3, err := f.svc.VerifyChallenge(ctx, s1.Context, ch3.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RevokeGrant(ctx, s1.Context, g3.GrantID); err != nil {
		t.Fatal(err)
	}
	wantReason(t, func() error {
		_, err := f.svc.Execute(ctx, s1.Context, g3.GrantID, "k3", &SetPolicy{Key: "lock.by_flow", Value: json.RawMessage(`true`)})
		return err
	}(), errcode.HumanProofRequired, "grant_revoked")
}

// 每个挑战最多 5 次失败；主体与来源另有 10 分钟 10 次失败的限速，新建挑战
// 不能重置限速。
func TestChallengeAttemptsAndRateLimit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	bad := func() string { return totp.Code(f.adminSecret, totp.Counter(f.clk.Now())+7) }
	newCh := func(v string) Challenge {
		ch, err := f.svc.CreateChallenge(ctx, admin, &SetPolicy{Key: "task.max_attempts", Value: json.RawMessage(v)})
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	ch := newCh(`4`)
	for i := range 5 {
		wantReason(t, func() error { _, err := f.svc.VerifyChallenge(ctx, admin, ch.ChallengeID, bad(), ""); return err }(), errcode.HumanProofRequired, "code_invalid")
		_ = i
	}
	wantReason(t, func() error {
		_, err := f.svc.VerifyChallenge(ctx, admin, ch.ChallengeID, f.fresh(f.adminSecret), "")
		return err
	}(), errcode.HumanProofRequired, "challenge_locked")
	ch2 := newCh(`5`)
	for range 5 {
		if _, err := f.svc.VerifyChallenge(ctx, admin, ch2.ChallengeID, bad(), ""); err == nil {
			t.Fatal("bad code accepted")
		}
	}
	// 已有 10 次失败：新挑战即使输入正确的码也被限速，且不计入挑战的尝试次数。
	ch3 := newCh(`6`)
	e := wantCode(t, func() error {
		_, err := f.svc.VerifyChallenge(ctx, admin, ch3.ChallengeID, f.fresh(f.adminSecret), "")
		return err
	}(), errcode.RateLimited)
	if e.RetryAfter <= 0 {
		t.Fatal("rate limit must say when to retry")
	}
	// 两组计数分开：会话内兑换挑战的失败不影响登录入口。
	f.login("ada", adminPassword, f.adminSecret, ChannelCLI)
	f.clk.Advance(11 * time.Minute)
	ch4 := newCh(`7`)
	if _, err := f.svc.VerifyChallenge(ctx, admin, ch4.ChallengeID, f.fresh(f.adminSecret), ""); err != nil {
		t.Fatalf("after the window = %v", err)
	}
}

// 登录失败按来源限速，即使主体名不存在；错误不区分“没有这个人”与“口令错”。
func TestLoginFailuresAndSourceLimit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for i := range 10 {
		_, err := f.svc.Login(ctx, LoginRequest{Name: "nobody" + string(rune('a'+i)), Password: "wrong password 1", Code: "000000", Channel: ChannelCLI, Source: "198.51.100.7"})
		wantReason(t, err, errcode.AuthRequired, "invalid_credentials")
	}
	if _, err := f.svc.Login(ctx, LoginRequest{Name: "ada", Password: adminPassword, Code: f.fresh(f.adminSecret), Channel: ChannelCLI, Source: "198.51.100.7"}); !isCode(err, errcode.RateLimited) {
		t.Fatalf("source limit = %v", err)
	}
	// 其他来源不受影响；口令错与动态码错返回同一个错误。
	_, err := f.svc.Login(ctx, LoginRequest{Name: "ada", Password: "wrong password 1", Code: f.fresh(f.adminSecret), Channel: ChannelCLI, Source: "198.51.100.8"})
	wantReason(t, err, errcode.AuthRequired, "invalid_credentials")
	_, err = f.svc.Login(ctx, LoginRequest{Name: "ada", Password: adminPassword, Code: "123456", Channel: ChannelCLI, Source: "198.51.100.8"})
	wantReason(t, err, errcode.AuthRequired, "invalid_credentials")
	f.login("ada", adminPassword, f.adminSecret, ChannelCLI)
}

// 策略：类型与范围校验、固定策略不可覆盖、预期修订；项目负责人（非管理员的人）
// 只能管理自己的项目。
func TestPolicyAndProjectOwnership(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	for key, v := range map[string]string{
		"destructive.require_human": `false`,
		"trash.fresh_hours":         `6`,
		"trash.retention_days":      `5`,
		"publish.mode":              `"scheduled"`,
		"no.such.policy":            `true`,
		"plugins.allowed":           `["Bad Id"]`,
	} {
		if _, err := f.svc.CreateChallenge(ctx, admin, &SetPolicy{Key: key, Value: json.RawMessage(v)}); !isCode(err, errcode.SchemaInvalid) {
			t.Errorf("%s=%s: %v", key, v, err)
		}
	}
	f.mustSudo(admin, f.adminSecret, &SetPolicy{Key: "trash.retention_days", Value: json.RawMessage(`60`)})
	if _, err := f.sudo(admin, f.adminSecret, &SetPolicy{Key: "trash.retention_days", Value: json.RawMessage(`90`)}); !isCode(err, errcode.PreconditionFailed) {
		t.Fatalf("stale expected revision = %v", err)
	}
	f.mustSudo(admin, f.adminSecret, &SetPolicy{Key: "trash.retention_days", Value: json.RawMessage(`90`), ExpectedRevision: 1})

	project, other := ids.New(), ids.New()
	alice, setup := f.register(admin, authz.Human, "alice")
	aliceSecret := f.completeSetup("alice", setup, "alice password 1")
	f.grantRole(admin, project, alice, RoleOwner)
	as := f.login("alice", "alice password 1", aliceSecret, ChannelCLI).Context
	f.mustSudo(as, aliceSecret, &SetPolicy{ProjectID: project, Key: "publish.mode", Value: json.RawMessage(`"manual"`)})
	pol, err := f.svc.Policies(ctx, as, project)
	if err != nil {
		t.Fatal(err)
	}
	if string(pol["publish.mode"].Value) != `"manual"` || pol["publish.mode"].Source != "project" ||
		string(pol["trash.retention_days"].Value) != `90` || pol["trash.retention_days"].Source != "instance" ||
		string(pol["destructive.require_human"].Value) != `true` {
		t.Fatalf("effective policies = %+v", pol)
	}
	// 负责人可以在自己的项目里授予角色，但不是管理员：别的项目看不到，实例级策略不能改。
	bot, _ := f.register(admin, authz.Agent, "hermes@node-b")
	f.grantRoleAs(as, aliceSecret, project, bot, RoleCurator)
	if _, err := f.svc.CreateChallenge(ctx, as, &SetProjectRole{ProjectID: other, PrincipalID: bot.ID, Role: RoleViewer, Grant: true}); !isCode(err, errcode.NotFound) {
		t.Fatalf("owner acting on another project = %v", err)
	}
	if _, err := f.svc.CreateChallenge(ctx, as, &SetPolicy{Key: "lock.by_flow", Value: json.RawMessage(`true`)}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("owner changing instance policy = %v", err)
	}
	if _, err := f.svc.Policies(ctx, as, other); !isCode(err, errcode.NotFound) {
		t.Fatalf("policies of another project = %v", err)
	}
}

// 口令校验很慢：并发的错误尝试也必须逐次计数，不能都在检查之后才记失败而
// 超出上限；成功的登录不留下失败记录。
func TestConcurrentLoginAttemptsAreCounted(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for range 12 {
		f.login("ada", adminPassword, f.adminSecret, ChannelCLI)
	}
	var n int
	f.svc.main.QueryRow(`SELECT count(*) FROM identity_auth_failures`).Scan(&n)
	if n != 0 {
		t.Fatalf("successful logins left %d failure records", n)
	}
	type outcome struct{ code errcode.Code }
	results := make(chan outcome, 20)
	for range 20 {
		go func() {
			_, err := f.svc.Login(ctx, LoginRequest{Name: "ada", Password: "wrong password 9", Code: "000000", Channel: ChannelCLI, Source: "203.0.113.9"})
			e, _ := errcode.As(err)
			results <- outcome{e.Code}
		}()
	}
	counts := map[errcode.Code]int{}
	for range 20 {
		counts[(<-results).code]++
	}
	if counts[errcode.AuthRequired] != 10 || counts[errcode.RateLimited] != 10 {
		t.Fatalf("concurrent attempts = %v, want exactly 10 evaluated and 10 rate limited", counts)
	}
}

// 匿名者只知道管理员名，用错误口令刷满登录限速，也不能锁住管理员用已有会话
// 兑换人类授权（例如紧急吊销被盗凭据）。
func TestAnonymousFailuresDoNotLockHumanProof(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	admin := f.adminLogin().Context
	agent, _ := f.register(admin, authz.Agent, "claude-code@node-a")
	f.issueToken(admin, agent, []Scope{ScopeRead})
	for i := range 10 {
		_, err := f.svc.Login(ctx, LoginRequest{Name: "ada", Password: "guess password " + string(rune('a'+i)), Code: "000000",
			Channel: ChannelBrowser, Source: "198.51.100." + string(rune('1'+i))})
		wantReason(t, err, errcode.AuthRequired, "invalid_credentials")
	}
	if _, err := f.svc.Login(ctx, LoginRequest{Name: "ada", Password: adminPassword, Code: f.fresh(f.adminSecret), Channel: ChannelCLI}); !isCode(err, errcode.RateLimited) {
		t.Fatalf("login after anonymous failures = %v", err)
	}
	var credID ids.ID
	f.svc.main.QueryRow(`SELECT credential_id FROM identity_credentials WHERE principal_id = ?`, agent.ID).Scan(&credID)
	if _, err := f.sudo(admin, f.adminSecret, &RevokeCredential{CredentialID: credID, Reason: "stolen"}); err != nil {
		t.Fatalf("emergency revocation from an existing session = %v", err)
	}
}

// 授权与会话的代次通常一起失效（会话先被拒）；授权自身也核对 auth_epoch 与
// recovery_epoch，作为纵深防御。
func TestCheckGrantEpochs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	s := f.adminLogin()
	cmd := &SetPolicy{Key: "lock.by_flow", Value: json.RawMessage(`true`)}
	ch, err := f.svc.CreateChallenge(ctx, s.Context, cmd)
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.svc.VerifyChallenge(ctx, s.Context, ch.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := loadGrant(ctx, f.svc.main, g.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.svc.current(ctx, s.Context)
	if err != nil {
		t.Fatal(err)
	}
	b, err := bind(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.checkGrant(ctx, row, v, b); err != nil {
		t.Fatalf("matching grant = %v", err)
	}
	stale := row
	stale.AuthEpoch--
	wantReason(t, f.svc.checkGrant(ctx, stale, v, b), errcode.HumanProofRequired, "grant_revoked")
	stale = row
	stale.RecoveryEpoch++
	wantReason(t, f.svc.checkGrant(ctx, stale, v, b), errcode.HumanProofRequired, "grant_revoked")
	stale = row
	stale.State = "revoked"
	wantReason(t, f.svc.checkGrant(ctx, stale, v, b), errcode.HumanProofRequired, "grant_revoked")
}
