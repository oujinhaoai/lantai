package identity

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/password"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
)

// fastPassword 是测试用的低成本口令参数；生产参数见 password.Default。
var fastPassword = password.Params{Algorithm: "argon2id", Version: password.Default.Version, Memory: 64, Time: 1, Threads: 1, KeyLen: 32}

const adminPassword = "correct horse battery staple"

type fixture struct {
	t     *testing.T
	inst  *operations.Instance
	svc   *Service
	clk   *clock.Fake
	gen   *ids.Generator
	key   *masterkey.Key
	admin Principal
	// adminSecret 是管理员验证器的种子；codes 是初始化时的恢复码。
	adminSecret []byte
	codes       []string
	keyN        int
}

// newFixture 建立真实实例：operations 建库迁移、身份模块初始化首个管理员、
// 启动并开放写入。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	clk := clock.NewFake(time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC))
	gen := &ids.Generator{Clock: clk, Rand: rand.Reader}
	inst, err := operations.Create(ctx, operations.CreateOptions{Options: operations.Options{Home: testHome(t), Clock: clk, IDs: gen}, Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inst.Close(context.Background()) })
	key, err := masterkey.Generate(gen, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := masterkey.Save(inst.Config().SecretsDir, key); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, inst: inst, clk: clk, gen: gen, key: key}
	f.svc = f.newService(key)
	b, err := f.svc.BeginBootstrap(ctx, BootstrapRequest{Name: "ada", DisplayName: "Ada", Password: adminPassword})
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Confirm(ctx, totp.Code(b.Enrollment.raw, totp.Counter(clk.Now())))
	if err != nil {
		t.Fatal(err)
	}
	f.admin, f.adminSecret, f.codes = res.Principal, b.Enrollment.raw, res.RecoveryCodes
	if err := inst.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(ctx, operations.Hook{Name: "identity", Run: f.svc.CheckStartup}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) newService(key *masterkey.Key) *Service {
	f.t.Helper()
	svc, err := New(Deps{Main: f.inst.DB(ownership.Main), Runtime: f.inst.DB(ownership.Runtime), Gate: f.inst.Gate(),
		Epochs: f.inst, Clock: f.clk, IDs: f.gen, Key: key, InstanceID: f.inst.InstanceID(), InstanceName: "test"},
		Config{Password: fastPassword})
	if err != nil {
		f.t.Fatal(err)
	}
	return svc
}

// fresh 把时钟推进到下一个时间步并返回该步的动态码：每个时间步只能成功一次。
func (f *fixture) fresh(secret []byte) string {
	f.clk.Advance(totp.Period * time.Second)
	return totp.Code(secret, totp.Counter(f.clk.Now()))
}

// now 返回当前时间步的动态码（不推进时钟）。
func (f *fixture) now(secret []byte) string { return totp.Code(secret, totp.Counter(f.clk.Now())) }

func (f *fixture) login(name, pw string, secret []byte, channel string) IssuedSession {
	f.t.Helper()
	s, err := f.svc.Login(f.t.Context(), LoginRequest{Name: name, Password: pw, Code: f.fresh(secret), Channel: channel, Source: "192.0.2.10"})
	if err != nil {
		f.t.Fatalf("login %s: %v", name, err)
	}
	return s
}

func (f *fixture) adminLogin() IssuedSession {
	return f.login("ada", adminPassword, f.adminSecret, ChannelCLI)
}

func (f *fixture) idemKey() string {
	f.keyN++
	return fmt.Sprintf("k-%d", f.keyN)
}

// sudo 以人的会话为命令申请挑战、用新动态码兑换并执行。
func (f *fixture) sudo(who authz.Context, secret []byte, cmd Command) (Result, error) {
	f.t.Helper()
	ctx := f.t.Context()
	ch, err := f.svc.CreateChallenge(ctx, who, cmd)
	if err != nil {
		return Result{}, err
	}
	g, err := f.svc.VerifyChallenge(ctx, who, ch.ChallengeID, f.fresh(secret), "192.0.2.10")
	if err != nil {
		f.t.Fatalf("verify challenge for %s: %v", ch.Action, err)
	}
	return f.svc.Execute(ctx, who, g.GrantID, f.idemKey(), cmd)
}

func (f *fixture) mustSudo(who authz.Context, secret []byte, cmd Command) Result {
	f.t.Helper()
	r, err := f.sudo(who, secret, cmd)
	if err != nil {
		f.t.Fatalf("%T: %v", cmd, err)
	}
	return r
}

// register 由管理员登记主体，返回主体与一次性设置码（人）。
func (f *fixture) register(admin authz.Context, kind authz.PrincipalKind, name string, caps ...string) (Principal, string) {
	f.t.Helper()
	res := f.mustSudo(admin, f.adminSecret, &RegisterPrincipal{Kind: kind, Name: name, Profile: Profile{Capabilities: caps}})
	p, err := loadPrincipalByName(f.t.Context(), f.svc.main, name)
	if err != nil {
		f.t.Fatal(err)
	}
	return p, res.Secret
}

// issueToken 为非人主体签发长期凭据。
func (f *fixture) issueToken(admin authz.Context, p Principal, scopes []Scope, projects ...ids.ID) string {
	f.t.Helper()
	cur, err := loadPrincipal(f.t.Context(), f.svc.main, p.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	res := f.mustSudo(admin, f.adminSecret, &IssueCredential{PrincipalID: p.ID, ExpectedRevision: cur.Revision, Scopes: scopes, Projects: projects})
	if res.Secret == "" {
		f.t.Fatal("no token returned")
	}
	return res.Secret
}

func (f *fixture) grantRole(admin authz.Context, project ids.ID, p Principal, role Role) {
	f.t.Helper()
	f.grantRoleAs(admin, f.adminSecret, project, p, role)
}

func (f *fixture) grantRoleAs(who authz.Context, secret []byte, project ids.ID, p Principal, role Role) {
	f.t.Helper()
	rev, err := projectRevision(f.t.Context(), f.svc.main, project)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mustSudo(who, secret, &SetProjectRole{ProjectID: project, ExpectedRevision: rev, PrincipalID: p.ID, Role: role, Grant: true})
}

func (f *fixture) exchange(token string, req SessionRequest) IssuedSession {
	f.t.Helper()
	if req.Channel == "" {
		req.Channel = ChannelCLI
	}
	s, err := f.svc.ExchangeToken(f.t.Context(), token, req)
	if err != nil {
		f.t.Fatalf("exchange: %v", err)
	}
	return s
}

func (f *fixture) decision(who authz.Context, a authz.Action, project ids.ID) authz.Decision {
	f.t.Helper()
	d, err := f.svc.Authorize(f.t.Context(), who, a, authz.Resource{ProjectID: project})
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

func wantCode(t *testing.T, err error, code errcode.Code) *errcode.Error {
	t.Helper()
	e, ok := errcode.As(err)
	if !ok || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	return e
}

func wantReason(t *testing.T, err error, code errcode.Code, reason string) {
	t.Helper()
	e := wantCode(t, err, code)
	for _, d := range e.Details {
		if d.Reason == reason {
			return
		}
	}
	t.Fatalf("%s without reason %s: %+v", code, reason, e.Details)
}

func isCode(err error, code errcode.Code) bool {
	var e *errcode.Error
	return errors.As(err, &e) && e.Code == code
}

// completeSetup 用设置码走完新成员设置：设置口令、登记并确认验证器，返回种子。
func (f *fixture) completeSetup(name, setupCode, pw string) []byte {
	f.t.Helper()
	ctx := f.t.Context()
	s, err := f.svc.StartSetup(ctx, SetupRequest{Name: name, Code: setupCode, Channel: ChannelCLI})
	if err != nil {
		f.t.Fatalf("start setup: %v", err)
	}
	if err := f.svc.SetPassword(ctx, s.Context, pw); err != nil {
		f.t.Fatalf("set password: %v", err)
	}
	e, err := f.svc.EnrollFactor(ctx, s.Context)
	if err != nil {
		f.t.Fatalf("enroll: %v", err)
	}
	if _, err := f.svc.ConfirmFactor(ctx, s.Context, f.now(e.raw)); err != nil {
		f.t.Fatalf("confirm: %v", err)
	}
	return e.raw
}

// testHome 返回带宽松磁盘余量配置的临时数据根，测试不依赖临时盘至少有 1 GiB 可用。
func testHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}
