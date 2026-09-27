package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

// BackupFile names a regular file in the original data root and its immutable
// copy in the backup. Paths are slash separated and never absolute.
type BackupFile struct {
	Path   string `json:"path"`
	Stored string `json:"stored"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func safeRelative(p string) bool {
	return p != "." && fs.ValidPath(p) && !strings.ContainsAny(p, "\\:\x00") && path.Clean(p) == p
}

func regularPath(root *os.Root, p string) error {
	if !safeRelative(p) {
		return errors.New("operations: unsafe archive path")
	}
	parts := strings.Split(p, "/")
	for n := range parts {
		st, err := root.Lstat(strings.Join(parts[:n+1], "/"))
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("operations: symbolic links are not supported in backups")
		}
		if n == len(parts)-1 && !st.Mode().IsRegular() {
			return errors.New("operations: backup entry is not a regular file")
		}
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func hashBackupFile(ctx context.Context, root *os.Root, p string) (BackupFile, error) {
	if err := regularPath(root, p); err != nil {
		return BackupFile{}, err
	}
	f, err := root.Open(p)
	if err != nil {
		return BackupFile{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, contextReader{ctx, f})
	if err != nil {
		return BackupFile{}, err
	}
	return BackupFile{Path: p, Stored: p, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// copyBackupFile uses bounded memory, rejects symlinks and installs only a
// complete verified file. Temporary files remain private to this backup.
func copyBackupFile(ctx context.Context, src *os.Root, from string, dst *os.Root, to string, want *BackupFile) (out BackupFile, err error) {
	if !safeRelative(to) {
		return out, errors.New("operations: unsafe destination path")
	}
	if err = regularPath(src, from); err != nil {
		return out, err
	}
	in, err := src.Open(from)
	if err != nil {
		return out, err
	}
	defer in.Close()
	if err = makeBackupParents(dst, path.Dir(to)); err != nil {
		return out, err
	}
	// Each capture/copy is serialized by its owner; O_EXCL prevents following
	// preexisting temporary entries, including links supplied with an archive.
	tmp := to + ".partial"
	if st, e := dst.Lstat(tmp); e == nil {
		if !st.Mode().IsRegular() {
			return out, errors.New("operations: unsafe partial file")
		}
		if err = dst.Remove(tmp); err != nil {
			return out, err
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		return out, e
	}
	f, err := dst.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return out, err
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = dst.Remove(tmp)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), contextReader{ctx, in})
	if err != nil {
		return out, err
	}
	out = BackupFile{Path: from, Stored: to, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}
	if want != nil && (out.SHA256 != want.SHA256 || n != want.Size) {
		return out, errors.New("operations: copied file does not match the frozen backup manifest")
	}
	if err = f.Sync(); err != nil {
		return out, err
	}
	if err = f.Close(); err != nil {
		return out, err
	}
	if st, e := dst.Lstat(to); e == nil && !st.Mode().IsRegular() {
		return out, errors.New("operations: unsafe destination file")
	} else if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return out, e
	}
	if err = dst.Rename(tmp, to); err != nil {
		return out, err
	}
	return out, fsutil.SyncDir(filepath.Join(dst.Name(), filepath.FromSlash(path.Dir(to))))
}

// Root confines access to the destination tree, but intentionally permits links
// within that tree. A resumed archive must also reject those links: otherwise
// one manifest path can overwrite another already verified destination entry.
func makeBackupParents(root *os.Root, dir string) error {
	if dir == "." {
		return nil
	}
	parts := strings.Split(dir, "/")
	for n := range parts {
		p := strings.Join(parts[:n+1], "/")
		st, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			if err = root.Mkdir(p, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			st, err = root.Lstat(p)
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return errors.New("operations: backup destination parent must be a real directory")
		}
	}
	return nil
}

func writeBackupJSON(file string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw, err = canonjson.Canonicalize(raw)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(file, append(raw, '\n'), 0o600)
}

func readBackupJSON(file string, v any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = decodeBackupJSON(f, v)
	return err
}

// Return the exact bytes decoded so completion verification cannot accidentally
// bind a second, concurrently replaced manifest to a previously decoded object.
func decodeBackupJSON(r io.Reader, v any) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, (64<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 64<<20 {
		return nil, errors.New("operations: backup manifest exceeds size limit")
	}
	if _, err = canonjson.Decode(raw); err != nil {
		return nil, err
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	return raw, d.Decode(v)
}

func independentTarget(home, dest string) (string, error) {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	// Resolve existing parents as well as lexical paths, so aliases cannot put
	// the backup inside the source tree (or restore over the archive itself).
	resolve := func(p string) (string, error) {
		tail := []string{}
		for {
			resolved, e := filepath.EvalSymlinks(p)
			if e == nil {
				for n := len(tail) - 1; n >= 0; n-- {
					resolved = filepath.Join(resolved, tail[n])
				}
				return resolved, nil
			}
			if !errors.Is(e, fs.ErrNotExist) {
				return "", e
			}
			parent := filepath.Dir(p)
			if parent == p {
				return "", e
			}
			tail = append(tail, filepath.Base(p))
			p = parent
		}
	}
	a, err := resolve(home)
	if err != nil {
		return "", err
	}
	b, err := resolve(abs)
	if err != nil {
		return "", err
	}
	inside := func(a, b string) bool {
		r, e := filepath.Rel(a, b)
		return e == nil && (r == "." || r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)))
	}
	if inside(a, b) || inside(b, a) {
		return "", fmt.Errorf("operations: backup and data root must be independent directories")
	}
	return abs, nil
}
