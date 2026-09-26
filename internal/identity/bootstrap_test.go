package identity

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
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
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
)

// 空实例只能初始化一次；确认码错误时不写任何数据；实例对外开放写入后本机
// 初始化入口关闭。
func TestBootstrapOnlyOnce(t *testing.T) {
	ctx := t.Context()
	clk := clock.NewFake(time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC))
	gen := &ids.Generator{Clock: clk, Rand: rand.Reader}
	home := testHome(t)
	inst, err := operations.Create(ctx, operations.CreateOptions{Options: operations.Options{Home: home, Clock: clk, IDs: gen}})
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close(context.Background())
	key, _ := masterkey.Generate(gen, clk, nil)
	f := &fixture{t: t, inst: inst, clk: clk, gen: gen, key: key}
	svc := f.newService(key)

	for _, bad := range []BootstrapRequest{
		{Name: "Ada L", Password: adminPassword},
		{Name: "ada", Password: "short"},
	} {
		if _, err := svc.BeginBootstrap(ctx, bad); err == nil {
			t.Fatalf("bad bootstrap request accepted: %+v", bad.Name)
		}
	}
	b, err := svc.BeginBootstrap(ctx, BootstrapRequest{Name: "ada", Password: adminPassword})
	if err != nil {
		t.Fatal(err)
	}
	wrong := totp.Code(b.Enrollment.raw, totp.Counter(clk.Now())+5)
	if _, err := b.Confirm(ctx, wrong); !isCode(err, errcode.HumanProofRequired) {
		t.Fatalf("confirm with wrong code = %v", err)
	}
	var n int
	svc.main.QueryRow(`SELECT count(*) FROM identity_principals`).Scan(&n)
	if n != 0 {
		t.Fatal("a failed confirmation must not write anything")
	}
	res, err := b.Confirm(ctx, totp.Code(b.Enrollment.raw, totp.Counter(clk.Now())))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RecoveryCodes) != 10 || res.Principal.Kind != authz.Human {
		t.Fatalf("result = %+v", res)
	}
	if open, _ := svc.BootstrapOpen(ctx); open {
		t.Fatal("bootstrap must close after the first administrator")
	}
	if _, err := svc.BeginBootstrap(ctx, BootstrapRequest{Name: "other", Password: adminPassword}); !errors.Is(err, ErrBootstrapClosed) {
		t.Fatalf("second bootstrap = %v", err)
	}
	// 同一个确认对象也不能再用一次。
	if _, err := b.Confirm(ctx, totp.Code(b.Enrollment.raw, totp.Counter(clk.Now())+1)); !errors.Is(err, ErrBootstrapClosed) {
		t.Fatalf("replayed confirmation = %v", err)
	}
	inst.Activate(ctx)
	if err := inst.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.BeginBootstrap(ctx, BootstrapRequest{Name: "other", Password: adminPassword}); !errors.Is(err, ErrNotLocal) {
		t.Fatalf("bootstrap on a serving instance = %v", err)
	}
	// 种子只以密文保存：主库文件里找不到明文种子。
	inst.Close(ctx)
	raw, err := os.ReadFile(filepath.Join(home, "db", "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	wal, _ := os.ReadFile(filepath.Join(home, "db", "main.db-wal"))
	for _, needle := range [][]byte{b.Enrollment.raw, []byte(b.Enrollment.Secret), []byte(adminPassword), []byte(res.RecoveryCodes[0])} {
		if bytes.Contains(raw, needle) || bytes.Contains(wal, needle) {
			t.Fatal("authentication secrets must not be stored in plaintext")
		}
	}
}

// 主体类别由管理员登记决定；能力标签与角色分开，只有角色授予权限。
func TestPrincipalKindsNamesAndCapabilities(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin().Context
	cases := []struct {
		kind authz.PrincipalKind
		name string
	}{
		{authz.Agent, "claude-code@node-a"},
		{authz.Node, "node:node-b"},
		{authz.Worker, "worker:gpu@node-b"},
		{authz.Runner, "runner:main@node-a"},
		{authz.Service, "service:im-feishu"},
	}
	for _, c := range cases {
		p, secret := f.register(admin, c.kind, c.name)
		if p.Kind != c.kind || secret != "" {
			t.Fatalf("%s: %+v setup=%q", c.name, p, secret)
		}
	}
	for _, bad := range []struct {
		kind authz.PrincipalKind
		name string
	}{
		{authz.Agent, "claude-code"},      // 缺节点
		{authz.Node, "node-b"},            // 缺前缀
		{authz.Human, "worker:x@y"},       // 人名不能伪装成进程身份
		{authz.PrincipalKind("god"), "x"}, // 未知类别
	} {
		if _, err := f.svc.CreateChallenge(f.t.Context(), admin, &RegisterPrincipal{Kind: bad.kind, Name: bad.name}); !isCode(err, errcode.SchemaInvalid) {
			t.Fatalf("%s/%s: %v", bad.kind, bad.name, err)
		}
	}
	// 同名再登记被拒。
	if _, err := f.sudo(admin, f.adminSecret, &RegisterPrincipal{Kind: authz.Agent, Name: "claude-code@node-a"}); !isCode(err, errcode.PreconditionFailed) {
		t.Fatalf("duplicate name = %v", err)
	}

	project := ids.New()
	agent, _ := f.register(admin, authz.Agent, "codex@node-b", "blender", "gpu", "code")
	token := f.issueToken(admin, agent, []Scope{ScopeRead, ScopeIngest})
	s := f.exchange(token, SessionRequest{})
	if s.Context.PrincipalKind != authz.Agent || s.Context.TransferClass() != authz.Batch {
		t.Fatalf("agent session context = %+v", s.Context)
	}
	// 能力标签不授予任何权限：没有角色时连项目是否存在都看不到。
	if d := f.decision(s.Context, ActStorageUpload, project); d.Allowed || d.Code != errcode.NotFound {
		t.Fatalf("capabilities alone = %+v", d)
	}
	f.grantRole(admin, project, agent, RoleViewer)
	if d := f.decision(s.Context, ActStorageUpload, project); d.Allowed || d.Code != errcode.Forbidden {
		t.Fatalf("viewer upload = %+v", d)
	}
	if d := f.decision(s.Context, ActCatalogRead, project); !d.Allowed {
		t.Fatalf("viewer read = %+v", d)
	}
	f.grantRole(admin, project, agent, RoleContributor)
	if d := f.decision(s.Context, ActStorageUpload, project); !d.Allowed || d.PolicyRevision < 2 {
		t.Fatalf("contributor upload = %+v", d)
	}
	// 凭据范围只收窄：contributor 角色也不能越过 organize 范围。
	if d := f.decision(s.Context, ActCatalogPatchOwnMetadata, project); d.Allowed {
		t.Fatalf("session without organize scope patched metadata: %+v", d)
	}
	who, err := f.svc.WhoAmI(t.Context(), s.Context)
	if err != nil || len(who.ProjectRoles[project]) != 2 || len(who.Principal.Profile.Capabilities) != 3 {
		t.Fatalf("whoami = %+v %v", who, err)
	}
	members, rev, err := f.svc.Members(t.Context(), s.Context, project)
	if err != nil || len(members) != 1 || rev != 2 {
		t.Fatalf("members = %+v rev=%d %v", members, rev, err)
	}
}

// 管理员与业务动作的边界：Agent、节点永远拿不到 admin；需要人类授权的动作
// Authorize 直接返回 HUMAN_PROOF_REQUIRED。
func TestAdminBoundaries(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin().Context
	if d := f.decision(admin, ActRegisterPrincipal, ""); d.Allowed || d.Code != errcode.HumanProofRequired {
		t.Fatalf("admin without grant = %+v", d)
	}
	if d := f.decision(admin, ActReadPrincipals, ""); !d.Allowed {
		t.Fatalf("admin read = %+v", d)
	}
	agent, _ := f.register(admin, authz.Agent, "claude-code@node-a")
	sess := f.exchange(f.issueToken(admin, agent, []Scope{ScopeRead}), SessionRequest{})
	for _, a := range []authz.Action{ActRegisterPrincipal, ActReadPrincipals, ActIssueCredential, authz.Action("identity.unknown_action")} {
		if d := f.decision(sess.Context, a, ""); d.Allowed || d.Code != errcode.Forbidden {
			t.Fatalf("agent %s = %+v", a, d)
		}
	}
	if _, err := f.svc.CreateChallenge(t.Context(), sess.Context, &RegisterPrincipal{Kind: authz.Agent, Name: "x@y"}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("agent challenge = %v", err)
	}
	// Agent 不能被授予 admin；人不能拿长期凭据。
	if _, err := f.sudo(admin, f.adminSecret, &SetSystemRole{PrincipalID: agent.ID, ExpectedRevision: currentRev(t, f, agent.ID), Role: RoleAdmin, Grant: true}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("admin role for agent = %v", err)
	}
	if _, err := f.sudo(admin, f.adminSecret, &IssueCredential{PrincipalID: f.admin.ID, ExpectedRevision: currentRev(t, f, f.admin.ID), Scopes: []Scope{ScopeRead}}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("credential for a human = %v", err)
	}
	// 节点的凭据不能带 Agent 的范围。
	node, _ := f.register(admin, authz.Node, "node:nas")
	if _, err := f.sudo(admin, f.adminSecret, &IssueCredential{PrincipalID: node.ID, ExpectedRevision: currentRev(t, f, node.ID), Scopes: []Scope{ScopeRead}}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("node credential with read scope = %v", err)
	}
	// 最后一位管理员不能被停用或降级，自己也不能停用自己。
	if _, err := f.sudo(admin, f.adminSecret, &DisablePrincipal{PrincipalID: f.admin.ID, ExpectedRevision: currentRev(t, f, f.admin.ID), Reason: "test"}); !isCode(err, errcode.Forbidden) {
		t.Fatalf("self disable = %v", err)
	}
	if _, err := f.sudo(admin, f.adminSecret, &SetSystemRole{PrincipalID: f.admin.ID, ExpectedRevision: currentRev(t, f, f.admin.ID), Role: RoleAdmin}); !isCode(err, errcode.PreconditionFailed) {
		t.Fatalf("revoke last admin = %v", err)
	}
	list, err := f.svc.ListPrincipals(t.Context(), admin)
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %d %v", len(list), err)
	}
}

func currentRev(t *testing.T, f *fixture, id ids.ID) int64 {
	t.Helper()
	p, err := loadPrincipal(t.Context(), f.svc.main, id)
	if err != nil {
		t.Fatal(err)
	}
	return p.Revision
}

// 主密钥与因子不符时实例不开放服务；密钥文件与库分开保存。
func TestStartupRequiresMatchingMasterKey(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.CheckStartup(t.Context()); err != nil {
		t.Fatal(err)
	}
	other, _ := masterkey.Generate(f.gen, f.clk, nil)
	if err := f.newService(other).CheckStartup(t.Context()); err == nil {
		t.Fatal("a different master key must be rejected at startup")
	}
	if dir := f.inst.Config().SecretsDir; filepath.Dir(dir) != f.inst.Layout().Home || filepath.Base(dir) != "secrets" {
		t.Fatalf("secrets dir = %s", dir)
	}
	if _, err := os.Stat(filepath.Join(f.inst.Layout().DBDir(), masterkey.FileName)); !os.IsNotExist(err) {
		t.Fatal("the master key must not live next to the databases")
	}
	_ = ownership.Main
}
