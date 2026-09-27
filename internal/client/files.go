package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ReadPrivate 拒绝符号链接、非普通文件与对其他用户开放的凭据文件。
func ReadPrivate(path string) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("client: credential/state must be a regular file, not a symbolic link")
	}
	if err = checkPrivate(path, st); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, errors.New("client: credential/state changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > MaxJSONBytes {
		return nil, errors.New("client: credential/state file is too large")
	}
	return b, nil
}

// WritePrivate 原子保存本机恢复状态或显式请求保存的会话。它不保存传输签名URL。
func WritePrivate(path string, b []byte) error {
	if int64(len(b)) > MaxJSONBytes {
		return errors.New("client: local state exceeds size limit")
	}
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() {
			return errors.New("client: refusing to replace a non-regular state file")
		}
		if err = checkPrivate(path, st); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := privateTemp(dir)
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}
func SaveState(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WritePrivate(path, append(b, '\n'))
}
func LoadState(path string, v any) error {
	b, err := ReadPrivate(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// HashFile 流式核对本机输入；取消会中止当前散列，不分配与文件大小成比例的内存。
func HashFile(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	return hashFile(ctx, f)
}
func hashFile(ctx context.Context, f *os.File) (string, int64, error) {
	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, errors.New("client: input must be a regular file")
	}
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, contextReader{ctx, f}, make([]byte, 256<<10))
	if err != nil {
		return "", n, err
	}
	after, err := f.Stat()
	if err != nil {
		return "", n, err
	}
	if n != st.Size() || after.Size() != st.Size() || !after.ModTime().Equal(st.ModTime()) {
		return "", n, errors.New("client: input changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

// SafeRelative 只接受跨平台的相对工作副本路径；不能从服务端路径逃出下载根目录。
func SafeRelative(path string) error {
	if !utf8.ValidString(path) || !norm.NFC.IsNormalString(path) || path == "" || strings.ContainsAny(path, "\\\x00:<>\"|?*") || strings.HasPrefix(path, "/") {
		return errors.New("client: unsafe relative file path")
	}
	for _, r := range path {
		if r < 32 || r == 127 {
			return errors.New("client: unsafe relative file path")
		}
	}
	for _, part := range strings.Split(path, "/") {
		if strings.HasSuffix(strings.ToLower(part), ".lantai-part") || strings.HasSuffix(strings.ToLower(part), ".lantai-download.json") || strings.HasSuffix(strings.ToLower(part), ".lantai-lock") {
			return errors.New("client: file path uses a reserved transfer suffix")
		}
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return errors.New("client: unsafe relative file path")
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return errors.New("client: reserved relative file path")
		}
	}
	return nil
}

// ValidatePaths catches cross-platform collisions and file/parent conflicts
// before the first file transfer changes the working copy.
func ValidatePaths(paths []string) error {
	seen := map[string]bool{}
	fold := cases.Fold()
	for _, path := range paths {
		if err := SafeRelative(path); err != nil {
			return err
		}
		key := fold.String(path)
		if seen[key] {
			return errors.New("client: file paths collide on a case-insensitive filesystem")
		}
		seen[key] = true
	}
	for key := range seen {
		parts := strings.Split(key, "/")
		for i := 1; i < len(parts); i++ {
			if seen[strings.Join(parts[:i], "/")] {
				return errors.New("client: file path conflicts with another file's directory")
			}
		}
	}
	return nil
}

func lockState(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	name := path + ".lock"
	if st, err := os.Lstat(name); err == nil && !st.Mode().IsRegular() {
		return nil, errors.New("client: state lock must be a regular file")
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(name)
	opened, e := f.Stat()
	if err != nil || e != nil || !st.Mode().IsRegular() || !os.SameFile(st, opened) {
		f.Close()
		return nil, errors.New("client: state lock changed while opening")
	}
	if err = lockFile(f); err != nil {
		f.Close()
		return nil, errors.New("client: another process is using this transfer state")
	}
	// Keep the inode in place: unlinking it would allow concurrent locks on two inodes.
	return func() { unlockFile(f); f.Close() }, nil
}

func lockDownload(root *os.Root, name string) (func(), error) {
	name += ".lantai-lock"
	if st, err := root.Lstat(name); err == nil && !st.Mode().IsRegular() {
		return nil, errors.New("client: download lock must be regular")
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	st, err := root.Lstat(name)
	opened, e := f.Stat()
	if err != nil || e != nil || !st.Mode().IsRegular() || !os.SameFile(st, opened) {
		f.Close()
		return nil, errors.New("client: download lock changed while opening")
	}
	if err = lockFile(f); err != nil {
		f.Close()
		return nil, errors.New("client: another process is downloading this target")
	}
	return func() { unlockFile(f); f.Close() }, nil
}
