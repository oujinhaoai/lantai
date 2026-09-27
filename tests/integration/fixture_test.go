package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/password"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// 集成夹具：真实实例（operations）、真实身份模块（identity，含 HumanGrant 管理
// 命令与实时撤权）、真实存储、目录、SQLite 台账和来源限制模块。

const adminPassword = "correct horse battery staple"

// fastPassword 是测试用的低成本口令参数；生产参数见 password.Default。
var fastPassword = password.Params{Algorithm: "argon2id", Version: password.Default.Version, Memory: 64, Time: 1, Threads: 1, KeyLen: 32}

// authority 把身份模块的实时授权与实例的恢复代次组合成台账需要的接口。
type authority struct {
	*identity.Service
	epochs authz.EpochSource
}

func (a authority) RecoveryEpoch(ctx context.Context) (int64, error) {
	return a.epochs.RecoveryEpoch(ctx)
}

type env struct {
	t       *testing.T
	inst    *operations.Instance
	clk     *clock.Fake
	id      *identity.Service
	ledger  *ledger.Service
	rights  *provenance.Service
	storage *storage.Service
	catalog *catalog.Service
	sched   *transfer.Scheduler
	xfer    *httptest.Server
	secret  []byte
	admin   identity.IssuedSession
	project catalog.Project
	keyN    int
}

func newEnv(t *testing.T, cfg storage.Config, limits transfer.Limits) *env {
	t.Helper()
	ctx := t.Context()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
	gen := &ids.Generator{Clock: clk, Rand: rand.Reader}
	inst, err := operations.Create(ctx, operations.CreateOptions{Options: operations.Options{Home: home, Clock: clk, IDs: gen}, Name: "test"})
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
	e := &env{t: t, inst: inst, clk: clk}
	e.id, err = identity.New(identity.Deps{Main: inst.DB(ownership.Main), Runtime: inst.DB(ownership.Runtime), Gate: inst.Gate(),
		Epochs: inst, Clock: clk, IDs: gen, Key: key, InstanceID: inst.InstanceID(), InstanceName: "test"},
		identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.id.BeginBootstrap(ctx, identity.BootstrapRequest{Name: "ada", DisplayName: "Ada", Password: adminPassword})
	if err != nil {
		t.Fatal(err)
	}
	e.secret, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(b.Enrollment.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Confirm(ctx, totp.Code(e.secret, totp.Counter(clk.Now()))); err != nil {
		t.Fatal(err)
	}
	if err := inst.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(ctx, operations.Hook{Name: "identity", Run: e.id.CheckStartup}); err != nil {
		t.Fatal(err)
	}

	e.ledger, err = ledger.New(ledger.Deps{DB: inst.DB(ownership.Ledger), Gate: inst.Gate(), Authority: authority{Service: e.id, epochs: inst}, Clock: clk, IDs: gen})
	if err != nil {
		t.Fatal(err)
	}
	e.rights, err = provenance.New(provenance.Deps{DB: inst.DB(ownership.Ledger), Gate: inst.Gate(), Reader: e.ledger, Authz: e.id, Clock: clk, IDs: gen, InstanceID: inst.InstanceID()})
	if err != nil {
		t.Fatal(err)
	}
	grantKey, err := key.Derive("storage/read-grant/v1")
	if err != nil {
		t.Fatal(err)
	}
	e.storage, err = storage.New(storage.Deps{Runtime: inst.DB(ownership.Runtime), Home: inst.Layout().Home, Gate: inst.Gate(),
		Clock: clk, IDs: gen, Authz: e.id, Reads: e.id, Ledger: e.ledger, Rights: e.rights, ReadGrantKey: grantKey,
		InstanceID: inst.InstanceID()}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.ledger.SetInstaller(e.storage)
	e.rights.SetFiles(e.storage)
	e.catalog, err = catalog.New(catalog.Deps{Home: inst.Layout().Home, Gate: inst.Gate(), Storage: e.storage, Ledger: e.ledger,
		Authz: e.id, Rights: e.rights, Clock: clk, IDs: gen, InstanceID: inst.InstanceID()})
	if err != nil {
		t.Fatal(err)
	}
	e.ledger.SetRevisionVerifier(e.catalog)
	e.ledger.SetAcceptanceVerifier(e.catalog)
	e.rights.SetCatalog(e.catalog)

	guard, err := httpauth.NewGuard(e.id, httpauth.Policy{AllowedOrigins: []string{"https://lantai.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	e.sched = transfer.New(limits)
	auth := storage.AuthenticatorFunc(func(r *http.Request) (authz.Context, error) {
		c, err := guard.Authenticate(r)
		return c.Context, err
	})
	e.xfer = httptest.NewServer(storage.NewTransferHandler(e.storage, auth, e.sched))
	t.Cleanup(e.xfer.Close)

	e.admin = e.login()
	p, err := e.catalog.CreateProject(ctx, catalog.ProjectRequest{Who: e.admin.Context, IdempotencyKey: e.key(), Key: "pansi", Name: "盘丝洞"})
	if err != nil || p.DescriptionPending {
		t.Fatalf("create project: %+v %v", p, err)
	}
	e.project = p
	return e
}

func (e *env) key() string {
	e.keyN++
	return fmt.Sprintf("k-%d", e.keyN)
}

// fresh 推进到下一个时间步并返回该步的动态码（每个时间步只能成功一次）。
func (e *env) fresh() string {
	e.clk.Advance(totp.Period * time.Second)
	return totp.Code(e.secret, totp.Counter(e.clk.Now()))
}

func (e *env) login() identity.IssuedSession {
	e.t.Helper()
	s, err := e.id.Login(e.t.Context(), identity.LoginRequest{Name: "ada", Password: adminPassword, Code: e.fresh(), Channel: identity.ChannelCLI, Source: "192.0.2.10"})
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// sudo 以管理员本人的会话申请挑战、兑换并执行敏感命令。
func (e *env) sudo(cmd identity.Command) identity.Result {
	e.t.Helper()
	ctx := e.t.Context()
	who := e.login().Context
	ch, err := e.id.CreateChallenge(ctx, who, cmd)
	if err != nil {
		e.t.Fatal(err)
	}
	g, err := e.id.VerifyChallenge(ctx, who, ch.ChallengeID, e.fresh(), "192.0.2.10")
	if err != nil {
		e.t.Fatal(err)
	}
	res, err := e.id.Execute(ctx, who, g.GrantID, e.key(), cmd)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// agent 登记一个 Agent 主体、签发长期凭据并换取会话；role 非空时授予项目角色。
func (e *env) agent(name string, role identity.Role) (identity.Principal, identity.IssuedSession) {
	e.t.Helper()
	ctx := e.t.Context()
	e.sudo(&identity.RegisterPrincipal{Kind: authz.Agent, Name: name})
	var p identity.Principal
	principals, err := e.id.ListPrincipals(ctx, e.login().Context)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, x := range principals {
		if x.Name == name {
			p = x
		}
	}
	res := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision,
		Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize}})
	if role != "" {
		e.setRole(p.ID, role, true)
	}
	s, err := e.id.ExchangeToken(ctx, res.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		e.t.Fatal(err)
	}
	return p, s
}

func (e *env) setRole(principal ids.ID, role identity.Role, grant bool) {
	e.t.Helper()
	_, rev, err := e.id.Members(e.t.Context(), e.login().Context, e.project.ProjectID)
	if err != nil {
		e.t.Fatal(err)
	}
	e.sudo(&identity.SetProjectRole{ProjectID: e.project.ProjectID, ExpectedRevision: rev, PrincipalID: principal, Role: role, Grant: grant})
}

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type reply struct {
	status int
	body   []byte
}

func (e *env) http(method, url, token string, header map[string]string, body io.Reader, length int64) reply {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.xfer.URL+url, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req.ContentLength = length
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, body: b}
}

// putParts 经传输面上传内容的全部分片。
func (e *env) putParts(token string, u storage.Upload, content []byte) {
	e.t.Helper()
	sha := shaOf(content)
	for _, f := range u.Files {
		if f.SHA256 != sha {
			continue
		}
		for n := 1; n <= f.PartCount; n++ {
			start := int64(n-1) * f.PartSize
			end := min(start+f.PartSize, int64(len(content)))
			part := content[start:end]
			r := e.http("PUT", fmt.Sprintf("%s%s/parts/%d", u.PartsURL, sha, n), token,
				map[string]string{storage.PartDigestHeader: shaOf(part)}, bytes.NewReader(part), int64(len(part)))
			if r.status != 200 {
				e.t.Fatalf("part %d: %d %s", n, r.status, r.body)
			}
		}
		return
	}
	e.t.Fatalf("content not declared in upload")
}

func rightsOwned() *manifest.Rights {
	return &manifest.Rights{Usage: "production", License: "LicenseRef-Owned", Sensitivity: "normal"}
}
