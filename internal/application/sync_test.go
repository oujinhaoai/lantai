package application

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/password"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
)

var testPassword = password.Params{Algorithm: "argon2id", Version: password.Default.Version, Memory: 64, Time: 1, Threads: 1, KeyLen: 32}

// openTestApp 初始化一个真实实例（首个管理员与主密钥）并以完整应用打开。
func openTestApp(t *testing.T) *App {
	t.Helper()
	ctx := t.Context()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC))
	gen := &ids.Generator{Clock: clk, Rand: rand.Reader}
	inst, err := operations.Create(ctx, operations.CreateOptions{Options: operations.Options{Home: home, Clock: clk, IDs: gen}, Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := masterkey.Generate(gen, clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = masterkey.Save(inst.Config().SecretsDir, key); err != nil {
		t.Fatal(err)
	}
	id, err := identity.New(identity.Deps{Main: inst.DB(ownership.Main), Runtime: inst.DB(ownership.Runtime), Gate: inst.Gate(),
		Epochs: inst, Clock: clk, IDs: gen, Key: key, InstanceID: inst.InstanceID(), InstanceName: "test"}, identity.Config{Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}
	b, err := id.BeginBootstrap(ctx, identity.BootstrapRequest{Name: "ada", DisplayName: "Ada", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(b.Enrollment.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Confirm(ctx, totp.Code(secret, totp.Counter(clk.Now()))); err != nil {
		t.Fatal(err)
	}
	if err = inst.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := Open(ctx, Options{Instance: operations.Options{Home: home, Clock: clk}, Identity: identity.Config{Password: testPassword}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return a
}

// 收件箱逐事件提交与审计导出较慢时，outbox 收录与查询追赶不能排在它们后面
// 等待（BUG-20260930-06）；任何一段失败都使实例不就绪。
func TestOutboxAndQuerySyncIsNotBlockedByInboxAndAudit(t *testing.T) {
	a := openTestApp(t)
	a.tailMu.Lock() // 模拟一轮长时间未完成的收件箱/审计推进
	done := make(chan error, 1)
	go func() { done <- a.syncCore(t.Context(), false) }()
	select {
	case err := <-done:
		if err != nil {
			a.tailMu.Unlock()
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		a.tailMu.Unlock()
		t.Fatal("outbox/query sync waited for the inbox/audit pass")
	}
	a.tailMu.Unlock()
	if err := a.Sync(t.Context()); err != nil || !a.Ready() {
		t.Fatalf("full sync: ready=%v %v", a.Ready(), err)
	}
	a.healthMu.Lock()
	a.tailFailed = true
	a.healthMu.Unlock()
	if a.Ready() {
		t.Fatal("a failed inbox/audit pass must keep the instance unready")
	}
	a.healthMu.Lock()
	a.tailFailed, a.coreFailed = false, true
	a.healthMu.Unlock()
	if a.Ready() {
		t.Fatal("a failed outbox/query pass must keep the instance unready")
	}
	if err := a.Sync(t.Context()); err != nil || !a.Ready() {
		t.Fatalf("recovered sync: ready=%v %v", a.Ready(), err)
	}
}
