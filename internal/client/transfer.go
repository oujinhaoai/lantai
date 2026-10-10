package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

// UploadPart 使用服务端给出的完整分片 URL，先流式散列指定范围再流式发送。
func (c *Client) UploadPart(ctx context.Context, authorizedURL, path string, offset, size int64) error {
	if offset < 0 || size < 0 {
		return errors.New("client: invalid part range")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return c.uploadPart(ctx, authorizedURL, f, offset, size)
}
func (c *Client) uploadPart(ctx context.Context, authorizedURL string, f *os.File, offset, size int64) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || offset > st.Size() || size > st.Size()-offset {
		return errors.New("client: file no longer matches the upload range")
	}
	hash := sha256.New()
	if _, err = io.CopyBuffer(hash, contextReader{ctx, io.NewSectionReader(f, offset, size)}, make([]byte, 256<<10)); err != nil {
		return err
	}
	headers := http.Header{"Content-Type": []string{"application/octet-stream"}, "Lantai-Part-Sha256": []string{hex.EncodeToString(hash.Sum(nil))}}
	res, err := c.transferRequest(ctx, http.MethodPut, authorizedURL, contextReader{ctx, io.NewSectionReader(f, offset, size)}, size, headers)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(res.Body, MaxJSONBytes))
	return err
}

// PartURL 只替换服务端明确提供的 {part_number} 槽位，绝不拼接内部监听路径。
func (c *Client) PartURL(template string, number int) (string, error) {
	if number < 1 || strings.Count(template, "{part_number}") != 1 || strings.Contains(strings.ReplaceAll(template, "{part_number}", ""), "{") {
		return "", errors.New("client: invalid server part URL template")
	}
	raw := strings.Replace(template, "{part_number}", strconv.Itoa(number), 1)
	if _, err := c.authorizedURL(raw); err != nil {
		return "", err
	}
	return raw, nil
}

type DownloadGrant struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Path   string `json:"path"`
}
type downloadState struct {
	Origin string `json:"origin"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Download 恢复到 root 内的相对路径；中断字节保留于 .lantai-part，完整摘要
// 通过后才以不覆盖已有目标的链接原子发布。恢复文件不含会话或授权 URL。
func (c *Client) Download(ctx context.Context, g DownloadGrant, rootDir, relative string) error {
	if err := SafeRelative(relative); err != nil {
		return err
	}
	if !digest.ValidHex(g.SHA256) || g.Size < 0 {
		return errors.New("client: invalid authorized download size or digest")
	}
	if _, err := c.authorizedURL(g.URL); err != nil {
		return err
	}
	if err := os.MkdirAll(rootDir, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.FromSlash(relative)
	if err = root.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	// Keep all writers away from the partial inode, including after Link makes
	// it visible at the final name. A second writer must never truncate an inode
	// that another download has already verified and published.
	unlock, err := lockDownload(root, name)
	if err != nil {
		return err
	}
	defer unlock()
	if existing, err := root.Lstat(name); err == nil {
		if !existing.Mode().IsRegular() {
			return errors.New("client: refusing non-regular destination")
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		same, err := checkFile(ctx, file, g.SHA256, g.Size)
		file.Close()
		if err != nil {
			return err
		}
		if same {
			return nil
		}
		return errcode.New(errcode.PathConflict, "client: destination exists with different content; preserve it and use an empty destination")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	part, meta := name+".lantai-part", name+".lantai-download.json"
	wanted := downloadState{Origin: c.Origin(), SHA256: g.SHA256, Size: g.Size}
	if st, err := root.Lstat(meta); err == nil {
		if !st.Mode().IsRegular() {
			return errors.New("client: download state must be a regular file")
		}
		mf, err := root.Open(meta)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(mf, MaxJSONBytes+1))
		mf.Close()
		if err != nil || int64(len(raw)) > MaxJSONBytes {
			return errors.New("client: invalid download state size")
		}
		var got downloadState
		if json.Unmarshal(raw, &got) != nil || got != wanted {
			return errors.New("client: download state belongs to a different file or server")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		// Never adopt a preexisting partial without its binding metadata.
		if _, err = root.Lstat(part); err == nil {
			return errors.New("client: partial file has no matching download state")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		raw, _ := json.Marshal(wanted)
		mf, err := root.OpenFile(meta, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = mf.Write(raw)
		if err == nil {
			err = mf.Sync()
		}
		closeErr := mf.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else {
		return err
	}
	var f *os.File
	st, err := root.Lstat(part)
	if errors.Is(err, os.ErrNotExist) {
		f, err = root.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	} else if err == nil {
		if !st.Mode().IsRegular() {
			return errors.New("client: partial file must be regular")
		}
		f, err = root.OpenFile(part, os.O_RDWR, 0600)
		if err == nil {
			opened, e := f.Stat()
			if e != nil || !os.SameFile(st, opened) {
				f.Close()
				return errors.New("client: partial file changed while opening")
			}
		}
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err = f.Stat()
	if err != nil {
		return err
	}
	offset := st.Size()
	if offset > g.Size {
		return errors.New("client: partial file exceeds expected size")
	}
	if offset < g.Size {
		headers := http.Header{}
		if offset > 0 {
			headers.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		res, err := c.transferRequest(ctx, http.MethodGet, g.URL, nil, 0, headers)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if offset > 0 {
			expected := fmt.Sprintf("bytes %d-%d/%d", offset, g.Size-1, g.Size)
			if res.StatusCode != http.StatusPartialContent || res.Header.Get("Content-Range") != expected {
				return errors.New("client: server did not honor the requested resume range")
			}
		} else if res.StatusCode != http.StatusOK {
			return errors.New("client: unexpected download status")
		}
		remain := g.Size - offset
		if res.ContentLength >= 0 && res.ContentLength != remain {
			return errors.New("client: download length does not match authorized file")
		}
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		n, copyErr := io.CopyBuffer(f, contextReader{ctx, io.LimitReader(res.Body, remain)}, make([]byte, 256<<10))
		syncErr := f.Sync()
		if copyErr != nil {
			return safeNetworkError(ctx, copyErr)
		}
		if syncErr != nil {
			return syncErr
		}
		if n != remain {
			return safeNetworkError(ctx, io.ErrUnexpectedEOF)
		}
		var extra [1]byte
		if n, e := res.Body.Read(extra[:]); n != 0 || (e != nil && !errors.Is(e, io.EOF)) {
			return errors.New("client: response exceeds authorized size")
		}
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	match, err := checkFile(ctx, f, g.SHA256, g.Size)
	if err != nil {
		return err
	}
	if !match {
		_ = f.Truncate(0)
		_ = f.Sync()
		return errcode.New(errcode.HashMismatch, "download digest mismatch; partial data was reset for a clean retry")
	}
	if err = f.Close(); err != nil {
		return err
	}
	current, err := root.Lstat(part)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(st, current) {
		return errors.New("client: partial file changed before publication")
	}
	// Link fails if another process created the destination: never overwrite it.
	if err = root.Link(part, name); err != nil {
		return fmt.Errorf("client: verified download retained but destination could not be published: %w", err)
	}
	if err = root.Remove(part); err != nil {
		return err
	}
	return root.Remove(meta)
}
func checkFile(ctx context.Context, f *os.File, sha string, size int64) (bool, error) {
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if st.Size() != size {
		return false, nil
	}
	hash := sha256.New()
	_, err = io.CopyBuffer(hash, contextReader{ctx, f}, make([]byte, 256<<10))
	if err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == sha, nil
}
