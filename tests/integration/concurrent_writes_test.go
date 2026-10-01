package integration

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// 真实五库下 24 个写者并发创建上传：写事务在进程内排队，锁竞争不能以
// SQLITE_BUSY 或不可重试的 INTERNAL 结束。存储很慢时（如 CI 的 Windows
// 托管机）排队可能超过上限，这时只允许有上限的写入排队超时，并且对外是
// 可重试的 STORAGE_UNAVAILABLE；去掉排队时出现的则是 SQLITE_BUSY。
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
	queued := 0
	for code, errs := range failures {
		for _, err := range errs {
			if e := sqlite.Structured(err); !errors.Is(err, sqlite.ErrWriteQueueTimeout) || e.Code != errcode.StorageUnavailable || !e.Retryable() {
				t.Fatalf("lock contention surfaced as %s: %v", code, err)
			}
			queued++
		}
	}
	if queued > 0 {
		t.Logf("%d of %d uploads waited past the write queue limit and got a retryable error", queued, 24*40)
	}
}
