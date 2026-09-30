package integration

import (
	"fmt"
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// 真实身份模块下，同一操作者以同一幂等键并发提交同一上传：只产生一个版本，
// 其余请求得到原版本或可重试的忙碌，不能被报告为权限错误（重放路径复核的
// 动作必须已登记）。撤权后重放拒绝。
func TestConcurrentSameKeyCommitReplaysWithRealIdentity(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	owner := e.login().Context
	agent, agentSession := e.agent("maker@pc", identity.RoleContributor)
	for _, c := range []struct {
		name string
		who  authz.Context
	}{{"human owner", owner}, {"agent contributor", agentSession.Context}} {
		t.Run(c.name, func(t *testing.T) {
			data := []byte("concurrent same-key commit by " + c.name)
			req := catalog.VersionRequest{Who: c.who, IdempotencyKey: e.key(), UploadID: e.upload(c.who, data), Slug: "concurrent/" + fmt.Sprint(len(c.name)),
				Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}}
			var wg sync.WaitGroup
			var mu sync.Mutex
			versions := map[string]int{}
			var failures []error
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					v, err := e.catalog.CommitVersion(t.Context(), req)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						failures = append(failures, err)
						return
					}
					versions[string(v.VersionID)]++
				}()
			}
			wg.Wait()
			if len(versions) != 1 {
				t.Fatalf("expected exactly one version, got %v (failures %v)", versions, failures)
			}
			for _, err := range failures {
				if errcode.CodeOf(err) != errcode.ResourceBusy {
					t.Errorf("concurrent duplicate must replay or report a retryable busy, got %s: %v", errcode.CodeOf(err), err)
				}
			}
			again, err := e.catalog.CommitVersion(t.Context(), req)
			if err != nil || versions[string(again.VersionID)] == 0 {
				t.Fatalf("sequential retry: %+v %v", again, err)
			}
		})
	}
	// 撤权后同键重放不能取回结果。
	data := []byte("replay after revocation")
	req := catalog.VersionRequest{Who: agentSession.Context, IdempotencyKey: e.key(), UploadID: e.upload(agentSession.Context, data), Slug: "concurrent/revoked",
		Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}}
	if _, err := e.catalog.CommitVersion(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	e.setRole(agent.ID, identity.RoleContributor, false)
	if _, err := e.catalog.CommitVersion(t.Context(), req); err == nil {
		t.Fatal("a revoked actor replayed a committed result")
	}
}
