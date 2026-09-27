// Package fileop 是 storage 的文件适配器（T02.4）：把三个平台上的写入、刷盘、
// 改名、硬链接与错误差异收敛成少数几个有明确持久性语义的操作，并把操作系统
// 错误归类为稳定的错误码。
//
//   - 空间不足（含配额）→ STORAGE_FULL；文件被占用、只读介质、权限或 I/O 错误
//     → STORAGE_UNAVAILABLE。归类不了的错误原样返回，由调用方按 INTERNAL 处理，
//     任何情况下都不把未确认的写入报告为成功。
//   - 新文件先写同目录临时文件并刷盘，再以硬链接（目标已存在即失败）或改名
//     落位，最后刷新父目录；读者不会看到写了一半的文件。
//   - 硬链接只用于不可变内容去重；文件系统不支持或跨卷时退回复制，并对副本
//     重新计算 SHA-256 核验，不静默削弱完整性。
//
// 一次改名成功不等于三个平台上的断电原子性；是否持久由刷盘与上层的操作记录
// 共同保证。Windows 不支持刷新目录，按平台能力处理并在验收记录中说明。
package fileop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

// Classify 把操作系统错误归类为 STORAGE_FULL 或 STORAGE_UNAVAILABLE；
// 不能归类时返回空串。
func Classify(err error) errcode.Code {
	if err == nil {
		return ""
	}
	if e, ok := errcode.As(err); ok {
		return e.Code
	}
	switch {
	case isFull(err):
		return errcode.StorageFull
	case isUnavailable(err), errors.Is(err, fs.ErrPermission):
		return errcode.StorageUnavailable
	}
	return ""
}

// Wrap 把文件操作错误转换为结构化错误：可归类的给出登记错误码，其余保留为
// 内部错误（由上层映射为 INTERNAL，不对外暴露主机路径）。
func Wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errcode.As(err); ok {
		return err
	}
	switch Classify(err) {
	case errcode.StorageFull:
		return errcode.Wrap(errcode.StorageFull, "storage is full while "+what, err)
	case errcode.StorageUnavailable:
		return errcode.Wrap(errcode.StorageUnavailable, "storage is not writable or the file is in use while "+what, err)
	}
	return fmt.Errorf("fileop: %s: %w", what, err)
}

// Faults 注入文件系统故障，只供测试使用；nil 字段表示不注入。返回非 nil 错误
// 时对应操作不执行并返回该错误。
type Faults struct {
	// Link 在创建硬链接前调用（例如返回 EXDEV 模拟不支持硬链接或跨卷）。
	Link func(oldname, newname string) error
	// Write 在写入文件内容前调用（例如返回 ENOSPC）。
	Write func(path string) error
	// Rename 在改名前调用（例如返回 EBUSY 模拟文件被占用）。
	Rename func(oldpath, newpath string) error
	// Sync 在刷盘前调用。
	Sync func(path string) error
}

// FS 执行文件操作；零值即真实操作。
type FS struct {
	Faults *Faults
}

func (f FS) writeFault(path string) error {
	if f.Faults == nil || f.Faults.Write == nil {
		return nil
	}
	return f.Faults.Write(path)
}

func (f FS) syncFault(path string) error {
	if f.Faults == nil || f.Faults.Sync == nil {
		return nil
	}
	return f.Faults.Sync(path)
}

// Rename 改名；故障注入与错误归类同其他操作。
func (f FS) Rename(oldpath, newpath string) error {
	if f.Faults != nil && f.Faults.Rename != nil {
		if err := f.Faults.Rename(oldpath, newpath); err != nil {
			return Wrap("renaming", err)
		}
	}
	return Wrap("renaming", os.Rename(oldpath, newpath))
}

// SyncDir 刷新目录项（POSIX）；Windows 直接返回。
func (f FS) SyncDir(dir string) error {
	if err := f.syncFault(dir); err != nil {
		return Wrap("syncing a directory", err)
	}
	return Wrap("syncing a directory", fsutil.SyncDir(dir))
}

// MkdirAll 创建目录（0755）并刷新新建目录的上级，使目录项持久。
func (f FS) MkdirAll(dir string) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Wrap("creating a directory", err)
	}
	for _, d := range missing {
		if err := f.SyncDir(filepath.Dir(d)); err != nil {
			return err
		}
	}
	return nil
}

// ErrContentDiffers 表示目标已存在且内容不同；调用方据此判定冲突，不覆盖。
var ErrContentDiffers = errors.New("fileop: an existing file has different content")

// CreateOrMatch 只在 path 不存在时写入完整内容（写临时文件、刷盘、以硬链接
// 落位、刷新目录）。path 已存在时：内容相同返回 created=false，不同返回
// ErrContentDiffers。用于不可变的修订、记录与标记文件，重入幂等。
//
// 权限在临时链接删除之后才设置：Windows 的只读属性由同一文件的全部硬链接
// 共享，删除只读的临时链接会连带清掉目标的只读属性。
func (f FS) CreateOrMatch(path string, data []byte, perm fs.FileMode) (created bool, err error) {
	if same, exists, err := sameContent(path, data); exists || err != nil {
		if err != nil {
			return false, err
		}
		if !same {
			return false, ErrContentDiffers
		}
		// 上次写入可能已落位，但父目录刷盘失败；重试不能跳过持久化。
		return false, f.SyncDir(filepath.Dir(path))
	}
	dir := filepath.Dir(path)
	if err := f.MkdirAll(dir); err != nil {
		return false, err
	}
	tmp, err := f.writeTemp(dir, filepath.Base(path), data, 0o600)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
	if err := f.publishTemp(tmp, path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return false, err
		}
		// 并发写入者先落位：按内容判定，不覆盖。
		if same, exists, cerr := sameContent(path, data); cerr != nil {
			return false, cerr
		} else if exists {
			if !same {
				return false, ErrContentDiffers
			}
			return false, f.SyncDir(dir)
		}
		return false, err
	}
	if perm != 0o600 {
		_ = os.Chmod(path, perm)
	}
	return true, f.SyncDir(dir)
}

// 单核心进程是文件写入者。只在临时文件发布期间按目标串行化：无硬链接的
// 文件系统上，检查不存在与改名必须在同一个临界区，避免并发重试互相覆盖。
// 固定条带避免长期运行时按文件名积累锁；写入与哈希仍在锁外。
var publishLocks [64]sync.Mutex

func (f FS) publishTemp(tmp, path string) error {
	h := fnv.New64a()
	_, _ = h.Write([]byte(filepath.Clean(path)))
	mu := &publishLocks[h.Sum64()%uint64(len(publishLocks))]
	mu.Lock()
	defer mu.Unlock()
	if err := f.link(tmp, path); err != nil {
		if !errors.Is(err, errLinkUnsupported) {
			return err
		}
		if _, err := os.Lstat(path); err == nil {
			return fs.ErrExist
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Wrap("checking a destination", err)
		}
		return f.Rename(tmp, path)
	}
	return Wrap("removing a temporary link", os.Remove(tmp))
}

func sameContent(path string, data []byte) (same, exists bool, err error) {
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, false, nil
	case err != nil:
		return false, true, Wrap("reading an existing file", err)
	}
	return bytes.Equal(existing, data), true, nil
}

// ReplaceAtomic 以“写临时文件 → 刷盘 → 改名替换 → 刷新父目录”写入可读快照；
// 读者要么看到旧内容，要么看到完整的新内容。
func (f FS) ReplaceAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := f.MkdirAll(dir); err != nil {
		return err
	}
	tmp, err := f.writeTemp(dir, filepath.Base(path), data, perm)
	if err != nil {
		return err
	}
	if err := f.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return f.SyncDir(dir)
}

func (f FS) writeTemp(dir, base string, data []byte, perm fs.FileMode) (string, error) {
	if err := f.writeFault(filepath.Join(dir, base)); err != nil {
		return "", Wrap("writing a file", err)
	}
	t, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return "", Wrap("creating a temporary file", err)
	}
	name := t.Name()
	fail := func(what string, err error) (string, error) {
		t.Close()
		os.Remove(name)
		return "", Wrap(what, err)
	}
	if err := t.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		return fail("setting file permissions", err)
	}
	if _, err := t.Write(data); err != nil {
		return fail("writing a file", err)
	}
	if err := f.syncFault(name); err != nil {
		return fail("syncing a file", err)
	}
	if err := t.Sync(); err != nil {
		return fail("syncing a file", err)
	}
	if err := t.Close(); err != nil {
		os.Remove(name)
		return "", Wrap("closing a file", err)
	}
	return name, nil
}

// errLinkUnsupported 表示文件系统不能在这两个路径之间建立硬链接。
var errLinkUnsupported = errors.New("fileop: hard links are not available here")

func (f FS) link(oldname, newname string) error {
	if f.Faults != nil && f.Faults.Link != nil {
		if err := f.Faults.Link(oldname, newname); err != nil {
			if linkUnsupported(err) {
				return fmt.Errorf("%w: %v", errLinkUnsupported, err)
			}
			return Wrap("creating a hard link", err)
		}
	}
	err := os.Link(oldname, newname)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return err
	case linkUnsupported(err):
		return fmt.Errorf("%w: %v", errLinkUnsupported, err)
	}
	return Wrap("creating a hard link", err)
}

// ErrLinkUnsupported 表示文件系统不能在这两个路径之间建立硬链接（不支持或跨卷）。
var ErrLinkUnsupported = errLinkUnsupported

// Link 建立硬链接；不支持时返回包裹 ErrLinkUnsupported 的错误，目标已存在返回
// fs.ErrExist，其余错误已归类。
func (f FS) Link(oldname, newname string) error { return f.link(oldname, newname) }

// CopyVerified 把不可变内容复制为 dst 的只读副本，并对副本重新计算 SHA-256
// 与大小核验；不符返回 HASH_MISMATCH 并删除副本。dst 已存在返回 fs.ErrExist。
func (f FS) CopyVerified(src, dst, sha256Hex string, size int64) error {
	return f.copyVerified(src, dst, sha256Hex, size)
}

// Mode 是文件落位方式。
type Mode string

const (
	ModeHardlink Mode = "hardlink"
	ModeCopy     Mode = "copy"
)

// LinkOrCopy 让 dst 成为 src（不可变内容）的只读副本：优先硬链接；文件系统
// 不支持硬链接或跨卷时复制，并对副本重新计算 SHA-256 与大小核验，不符返回
// HASH_MISMATCH 并删除副本。dst 已存在时返回 fs.ErrExist，不覆盖。
func (f FS) LinkOrCopy(src, dst, sha256Hex string, size int64) (Mode, error) {
	err := f.link(src, dst)
	switch {
	case err == nil:
		return ModeHardlink, nil
	case errors.Is(err, fs.ErrExist):
		return "", err
	case !errors.Is(err, errLinkUnsupported):
		return "", err
	}
	if err := f.copyVerified(src, dst, sha256Hex, size); err != nil {
		return "", err
	}
	return ModeCopy, nil
}

func (f FS) copyVerified(src, dst, sha256Hex string, size int64) (err error) {
	if err := f.writeFault(dst); err != nil {
		return Wrap("copying a file", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return Wrap("opening content", err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".copy-*")
	if err != nil {
		return Wrap("creating a copy", err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(name)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), in)
	if err != nil {
		return Wrap("copying a file", err)
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != sha256Hex {
		return errcode.New(errcode.HashMismatch, "copied content does not match its hash")
	}
	if err := f.syncFault(name); err != nil {
		return Wrap("syncing a copy", err)
	}
	if err := tmp.Sync(); err != nil {
		return Wrap("syncing a copy", err)
	}
	if err := tmp.Close(); err != nil {
		return Wrap("closing a copy", err)
	}
	if err := f.publishTemp(name, dst); err != nil {
		return err
	}
	// 只读在落位之后设置：Windows 的属性由同一文件的全部硬链接共享，先设只读会
	// 让临时链接删不掉。
	_ = os.Chmod(dst, 0o444)
	return f.SyncDir(filepath.Dir(dst))
}

// HashFile 流式计算文件的 SHA-256 与大小，内存占用与文件大小无关。
func HashFile(path string) (sha256Hex string, size int64, err error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer fh.Close()
	h := sha256.New()
	n, err := io.Copy(h, fh)
	if err != nil {
		return "", 0, Wrap("reading content", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// FreeBytes 返回 path 所在文件系统的可用字节数。
func FreeBytes(path string) (uint64, error) { return fsutil.FreeBytes(path) }
