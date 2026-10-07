package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// WriteFileAtomic 以“写临时文件 → 刷盘 → 改名替换 → 刷新父目录”的顺序写入
// path：读者要么看到旧内容，要么看到完整的新内容，不会看到写了一半的文件。
// 临时文件与目标在同一目录；失败时删除临时文件。Windows 不支持刷新目录，
// 改名的断电持久性按平台能力，不在此宣称。
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("fsutil: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("fsutil: chmod temp file: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("fsutil: write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("fsutil: sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("fsutil: close temp file: %w", err)
	}
	if err = replaceAtomicFile(tmpName, path); err != nil {
		return fmt.Errorf("fsutil: replace %s: %w", path, err)
	}
	return SyncDir(dir)
}

// CreateFileExclusive 只在 path 尚不存在时写入完整内容（先写临时文件再以
// 硬链接或改名落位）；已存在返回 fs.ErrExist，不覆盖。用于只能生成一次的
// 密钥等文件。
func CreateFileExclusive(path string, data []byte, perm fs.FileMode) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("fsutil: %s: %w", path, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("fsutil: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 硬链接在目标已存在时失败，保证不覆盖；不支持硬链接的文件系统上
	// 退回“再次确认不存在后改名”。
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("fsutil: %s: %w", path, fs.ErrExist)
		}
		if _, statErr := os.Lstat(path); statErr == nil {
			return fmt.Errorf("fsutil: %s: %w", path, fs.ErrExist)
		}
		if err := os.Rename(tmpName, path); err != nil {
			return fmt.Errorf("fsutil: place %s: %w", path, err)
		}
	}
	return SyncDir(dir)
}

// SyncDir 刷新目录项，使改名与新建在断电后仍然可见（POSIX）。Windows 无此能力，直接返回。
func SyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("fsutil: open dir for sync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsutil: sync dir %s: %w", dir, err)
	}
	return nil
}

// EnsurePrivateDir 创建只允许本用户访问的目录（Unix 0700）。目录已存在时
// 不修改权限，由调用方用 CheckPrivate 核对。
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("fsutil: create %s: %w", dir, err)
	}
	return nil
}

// ErrInsecurePermissions 表示密钥等文件允许其他用户访问。
var ErrInsecurePermissions = errors.New("fsutil: file is accessible by other users")

// CheckPrivate 在 Unix 上确认 path 不对组与其他用户开放任何权限；Windows
// 的访问控制由 ACL 决定，这里不做判断。
func CheckPrivate(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %v", ErrInsecurePermissions, path, st.Mode().Perm())
	}
	return nil
}
