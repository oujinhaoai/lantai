package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// 内容库（CAS）：原件按 SHA-256 存放，文件名就是哈希，只增不改。只有经服务端
// 完整核验的字节才能进入内容库；版本目录里的文件是它的硬链接或经核验的副本。

// placeBlob 把已核验的暂存文件放入内容库。调用方已流式核对 sha256 与 size，
// 并持有该哈希的 blob 锁。内容库已有同一哈希时去重（只核对大小），不覆盖。
// 返回 true 表示本次新建了原件。
func (s *Service) placeBlob(src, sha string, size int64) (bool, error) {
	dst := s.layout.BlobPath(sha)
	if st, err := os.Lstat(dst); err == nil {
		if st.Size() != size || !st.Mode().IsRegular() {
			return false, errcode.New(errcode.OperationNeedsReconciliation, "stored content does not match its recorded size").
				WithDetails(errcode.Detail{Reason: "blob_size_mismatch"})
		}
		// 重入也补刷目录：上次落位可能成功但目录刷盘失败，尚未允许签发授权。
		return false, s.fs.SyncDir(filepath.Dir(dst))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fileop.Wrap("checking stored content", err)
	}
	if err := s.fs.MkdirAll(filepath.Dir(dst)); err != nil {
		return false, err
	}
	_, err := s.fs.LinkOrCopy(src, dst, sha, size)
	switch {
	case errors.Is(err, fs.ErrExist):
		// 同哈希写入由调用方串行化；意外出现的目标也须核对，不能直接认作原件。
		return s.placeBlob(src, sha, size)
	case err != nil:
		return false, err
	}
	return true, s.fs.SyncDir(filepath.Dir(dst))
}

// sealBlob 把原件设为只读（Windows 的只读属性由同一文件的全部硬链接共享，
// 因此在暂存链接删除之后再设）。只读只是防误改的附加措施，完整性由哈希保证。
func (s *Service) sealBlob(sha string) {
	_ = os.Chmod(s.layout.BlobPath(sha), 0o444)
}

// blobSize 返回内容库中原件的大小；不存在时 exists 为 false。
func (s *Service) blobSize(sha string) (size int64, exists bool, err error) {
	st, err := os.Lstat(s.layout.BlobPath(sha))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, false, nil
	case err != nil:
		return 0, false, fileop.Wrap("checking stored content", err)
	case !st.Mode().IsRegular():
		return 0, false, errcode.New(errcode.OperationNeedsReconciliation, "stored content is not a regular file")
	}
	return st.Size(), true, nil
}

// VerifyBlob 重新计算内容库中原件的 SHA-256；供对账与 fsck 使用，不在最终
// 接受边界的锁内调用。
func (s *Service) VerifyBlob(ctx context.Context, sha string) error {
	if !digest.ValidHex(sha) {
		return invalid("content hash is not valid")
	}
	if _, exists, err := s.blobSize(sha); err != nil {
		return err
	} else if !exists {
		return errcode.New(errcode.OperationNeedsReconciliation, "stored content is missing")
	}
	if err := regularPath(s.layout.Home, s.layout.BlobPath(sha)); err != nil {
		return err
	}
	got, _, err := hashMaintenanceFile(ctx, s.layout.BlobPath(sha))
	if errors.Is(err, fs.ErrNotExist) {
		return errcode.New(errcode.OperationNeedsReconciliation, "stored content is missing")
	}
	if err != nil {
		return err
	}
	if got != sha {
		return errcode.New(errcode.HashMismatch, "stored content no longer matches its hash")
	}
	return ctx.Err()
}

// blobLock 是内容库按哈希的写锁请求（锁顺序的最后一层）。
func blobLock(shas ...string) commands.Request { return commands.Request{Blobs: shas} }
