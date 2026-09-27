package integration

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// synth 是确定性的合成内容：第 i 个 16 字节块为 AES(k, i)。任何范围都可以在
// 不占用与文件大小成比例内存的情况下重新生成。
type synth struct {
	block cipher.Block
	size  int64
}

func newSynth(size int64) *synth {
	b, _ := aes.NewCipher([]byte("lantai-synthetic"))
	return &synth{block: b, size: size}
}

func (s *synth) ReadAt(p []byte, off int64) (int, error) {
	if off >= s.size {
		return 0, io.EOF
	}
	n := 0
	var in, out [16]byte
	for n < len(p) && off < s.size {
		blk := off / 16
		binary.BigEndian.PutUint64(in[8:], uint64(blk))
		s.block.Encrypt(out[:], in[:])
		c := copy(p[n:], out[off%16:])
		if rem := s.size - off; int64(c) > rem {
			c = int(rem)
		}
		n += c
		off += int64(c)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s *synth) section(off, n int64) io.Reader { return io.NewSectionReader(s, off, n) }

func (s *synth) sha(off, n int64) string {
	h := sha256.New()
	io.Copy(h, s.section(off, n))
	return hex.EncodeToString(h.Sum(nil))
}

// heapPeak 在后台采样堆占用，返回停止函数与峰值。
func heapPeak() (stop func() uint64) {
	var peak atomic.Uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	return func() uint64 {
		close(done)
		wg.Wait()
		return peak.Load()
	}
}

// 大文件分片续传：上传中途断开，续传只补缺的分片，整件哈希一致才入库；分段
// 下载与 Range 续传逐字节一致；传输过程中堆占用不随文件大小线性增长。默认
// 32 MiB，设置 LANTAI_TEST_LARGE_MB=1024 运行 1 GiB 的验收规模。
func TestLargeResumableTransfer(t *testing.T) {
	sizeMB := int64(32)
	partMB := int64(8)
	if v := os.Getenv("LANTAI_TEST_LARGE_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			t.Fatalf("LANTAI_TEST_LARGE_MB=%q", v)
		}
		sizeMB, partMB = n, 64
	}
	size := sizeMB<<20 + 12345 // 最后一个分片不满
	e := newEnv(t, storage.Config{PartSize: partMB << 20, SinglePartMax: partMB << 20, MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	_, ms := e.agent("bulk@node-a", identity.RoleContributor)
	src := newSynth(size)
	fullSHA := src.sha(0, size)

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	stop := heapPeak()
	started := time.Now()

	u, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: ms.Context, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID,
		Files: []storage.FileSpec{{SHA256: fullSHA, Size: size}}})
	if err != nil {
		t.Fatal(err)
	}
	f := u.Files[0]
	put := func(n int, body io.Reader, length int64) reply {
		off := int64(n-1) * f.PartSize
		want := min(f.PartSize, size-off)
		return e.http("PUT", fmt.Sprintf("%s%s/parts/%d", u.PartsURL, fullSHA, n), ms.Token,
			map[string]string{storage.PartDigestHeader: src.sha(off, want)}, body, length)
	}
	partOf := func(n int) (io.Reader, int64) {
		off := int64(n-1) * f.PartSize
		want := min(f.PartSize, size-off)
		return src.section(off, want), want
	}
	// 前两片正常上传。
	for n := 1; n <= 2 && n <= f.PartCount; n++ {
		body, l := partOf(n)
		if r := put(n, body, l); r.status != 200 {
			t.Fatalf("part %d: %d %s", n, r.status, r.body)
		}
	}
	// 第三片发到一半连接中断：服务端不记录。
	if f.PartCount >= 3 {
		body, l := partOf(3)
		cctx, cancel := context.WithCancel(ctx)
		req, _ := http.NewRequestWithContext(cctx, "PUT", e.xfer.URL+fmt.Sprintf("%s%s/parts/3", u.PartsURL, fullSHA),
			&cutAfter{r: body, n: l / 2, cancel: cancel})
		req.ContentLength = l
		req.Header.Set("Authorization", "Bearer "+ms.Token)
		req.Header.Set(storage.PartDigestHeader, src.sha(2*f.PartSize, l))
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			t.Fatalf("interrupted part was accepted: %d", resp.StatusCode)
		}
		cancel()
	}
	// 续传：按服务端记录只补缺的分片。
	got, err := e.storage.GetUpload(ctx, ms.Context, u.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	received := map[int]bool{}
	for _, n := range got.Files[0].Received {
		received[n] = true
	}
	if received[3] {
		t.Fatal("an interrupted part was recorded")
	}
	resent := 0
	for n := 1; n <= f.PartCount; n++ {
		if received[n] {
			continue
		}
		body, l := partOf(n)
		if r := put(n, body, l); r.status != 200 {
			t.Fatalf("resume part %d: %d %s", n, r.status, r.body)
		}
		resent++
	}
	if _, err := e.storage.CompleteFile(ctx, ms.Context, u.UploadID, fullSHA); err != nil {
		t.Fatal(err)
	}
	uploadTime := time.Since(started)
	res, err := e.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: ms.Context, IdempotencyKey: e.key(), UploadID: u.UploadID,
		Slug: "renders/large", Content: catalog.ContentInput{AssetType: manifest.TypeVideo, Rights: rightsOwned(),
			Files: []manifest.InputFile{{Path: "render.bin", Role: "primary", SHA256: fullSHA, Size: size}}}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: ms.Context, AssetID: res.AssetID, VersionID: res.VersionID,
		Path: "render.bin", Purpose: authz.PurposeProduction})
	if err != nil {
		t.Fatal(err)
	}
	// 分两段下载：先取前半，再以 Range 续传后半，拼接后整件哈希一致。
	h := sha256.New()
	half := size / 2
	for _, rg := range []string{fmt.Sprintf("bytes=0-%d", half-1), fmt.Sprintf("bytes=%d-", half)} {
		req, _ := http.NewRequestWithContext(ctx, "GET", e.xfer.URL+g.URL, nil)
		req.Header.Set("Authorization", "Bearer "+ms.Token)
		req.Header.Set("Range", rg)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 206 {
			resp.Body.Close()
			t.Fatalf("range %s: %d", rg, resp.StatusCode)
		}
		if _, err := io.Copy(h, resp.Body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	total := time.Since(started)
	peak := stop()
	if hex.EncodeToString(h.Sum(nil)) != fullSHA {
		t.Fatal("downloaded content differs from the upload")
	}
	growth := int64(peak) - int64(base.HeapInuse)
	t.Logf("size=%d parts=%d resent=%d upload=%s total=%s heap_base=%.1fMiB heap_peak=%.1fMiB growth=%.1fMiB",
		size, f.PartCount, resent, uploadTime.Round(time.Millisecond), total.Round(time.Millisecond),
		float64(base.HeapInuse)/(1<<20), float64(peak)/(1<<20), float64(growth)/(1<<20))
	if growth > 96<<20 {
		t.Fatalf("heap grew by %d bytes while streaming %d bytes; memory must not scale with file size", growth, size)
	}
}

// cutAfter 读出 n 字节后取消请求，模拟传输中途断开。
type cutAfter struct {
	r      io.Reader
	n      int64
	read   int64
	cancel func()
}

func (c *cutAfter) Read(p []byte) (int, error) {
	if c.read >= c.n {
		c.cancel()
		return 0, errors.New("connection lost")
	}
	if int64(len(p)) > c.n-c.read {
		p = p[:c.n-c.read]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}
