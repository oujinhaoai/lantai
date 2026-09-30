package integration

import (
	"fmt"
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// 真实五库下 24 个写者并发创建上传：写事务在进程内排队，不能因等锁超过
// busy_timeout 而失败，更不能以不可重试的 INTERNAL 返回。
func TestConcurrentCreateUploadsDoNotFailOnLockContention(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	_, session := e.agent("bulk@node-a", identity.RoleContributor)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := map[errcode.Code][]error{}
	for w := range 24 {
		wg.Go(func() {
			for n := range 40 {
				data := []byte(fmt.Sprintf("concurrent upload %d-%d", w, n))
				_, err := e.storage.CreateUpload(t.Context(), storage.CreateUploadRequest{Who: session.Context, IdempotencyKey: fmt.Sprintf("w%d-n%d", w, n), ProjectID: e.project.ProjectID,
					Files: []storage.FileSpec{{SHA256: shaOf(data), Size: int64(len(data))}}})
				if err != nil {
					mu.Lock()
					failures[errcode.CodeOf(err)] = append(failures[errcode.CodeOf(err)], err)
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("concurrent CreateUpload failed: %v", failures)
	}
}
