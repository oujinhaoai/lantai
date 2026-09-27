package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

type cutReadCloser struct {
	io.ReadCloser
	left int64
}

func (r *cutReadCloser) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.ReadCloser.Read(p)
	r.left -= int64(n)
	return n, err
}

// 真实 HTTPS 单入口 + 分面核心，客户端流式上传/下载两边都注入网络中断。
// 默认 32MiB，LANTAI_TEST_LARGE_MB=1024 用同一用例运行 1GiB 规模。
func TestRemoteResumableTransfer(t *testing.T) {
	sizeMB, partMB := int64(32), int64(8)
	if value := os.Getenv("LANTAI_TEST_LARGE_MB"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 1 {
			t.Fatalf("invalid large test size %q", value)
		}
		sizeMB, partMB = n, 64
	}
	size := sizeMB<<20 + 12345
	cfg := storage.Config{PartSize: partMB << 20, SinglePartMax: partMB << 20, MinFreeBytes: 1 << 20}
	e := newEnv(t, cfg, transfer.Limits{})
	_, agent := e.agent("remote@bulk", identity.RoleContributor)
	a := reopenApplication(t, e, cfg)
	s := remoteServers(t, a, false)
	apiURL, _ := url.Parse("http://" + s.Addresses.API)
	xferURL, _ := url.Parse("http://" + s.Addresses.Transfer)
	apiProxy := httputil.NewSingleHostReverseProxy(apiURL)
	xferProxy := httputil.NewSingleHostReverseProxy(xferURL)
	xferProxy.ErrorLog = log.New(io.Discard, "", 0)
	var uploadCut, downloadCut atomic.Bool
	var partOne, rangeResumes atomic.Int64
	xferProxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == http.MethodGet && strings.Contains(r.Request.URL.Path, "/reads/") && r.StatusCode == 200 && downloadCut.CompareAndSwap(false, true) {
			r.Body = &cutReadCloser{ReadCloser: r.Body, left: 256 << 10}
		}
		return nil
	}
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			apiProxy.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/xfer/") {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPut {
			if strings.HasSuffix(r.URL.Path, "/parts/1") {
				partOne.Add(1)
			}
			if strings.HasSuffix(r.URL.Path, "/parts/2") && uploadCut.CompareAndSwap(false, true) {
				_, _ = io.CopyN(io.Discard, r.Body, 1024)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
		}
		if r.Header.Get("Range") != "" {
			rangeResumes.Add(1)
		}
		xferProxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	roots := x509.NewCertPool()
	roots.AddCert(front.Certificate())
	c, err := client.New(client.Config{BaseURL: front.URL, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, SessionToken: agent.Token})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	work := t.TempDir()
	path := filepath.Join(work, "source.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	src := newSynth(size)
	_, err = io.Copy(f, src.section(0, size))
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("source: %v %v", err, closeErr)
	}
	input := client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "remote/big", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "source.bin", Role: "source"}}, Rights: json.RawMessage(`{"usage":"production","license":"LicenseRef-Owned","sensitivity":"normal"}`)}}
	state := filepath.Join(work, "push.json")
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stopPeak := heapPeak()
	defer func() {
		if stopPeak != nil {
			_ = stopPeak()
		}
	}()
	started := time.Now()
	if _, err = c.Push(t.Context(), input, work, state, false); err == nil {
		t.Fatal("upload interruption was not observed")
	}
	if !uploadCut.Load() {
		t.Fatal("upload fault not reached")
	}
	// 中断一条文件请求不破坏普通 JSON 客户端池。
	if _, err = c.Do(t.Context(), http.MethodGet, "/api/v1/whoami", nil, client.Options{}); err != nil {
		t.Fatal(err)
	}
	r, err := c.Push(t.Context(), input, work, state, false)
	if err != nil {
		t.Fatal(err)
	}
	var committed catalog.VersionResult
	if err = r.Decode(&committed); err != nil {
		t.Fatal(err)
	}
	if partOne.Load() != 1 {
		t.Fatalf("already accepted first part was resent %d times", partOne.Load())
	}
	grantPath := "/api/v1/assets/" + string(committed.AssetID) + "/versions/" + string(committed.VersionID) + "/read-grants"
	r, err = c.Do(t.Context(), http.MethodPost, grantPath, map[string]string{"path": "source.bin", "purpose": "archive_review"}, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var g client.DownloadGrant
	if err = r.Decode(&g); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(work, "pulled")
	if err = c.Download(t.Context(), g, dest, "source.bin"); err == nil {
		t.Fatal("download interruption was not observed")
	}
	if !downloadCut.Load() {
		t.Fatal("download fault not reached")
	}
	if err = c.Download(t.Context(), g, dest, "source.bin"); err != nil {
		t.Fatal(err)
	}
	if rangeResumes.Load() != 1 {
		t.Fatalf("Range resume count %d", rangeResumes.Load())
	}
	sha, n, err := client.HashFile(context.Background(), filepath.Join(dest, "source.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if sha != g.SHA256 || n != size {
		t.Fatal("resumed content differs from committed exact version")
	}
	peak := stopPeak()
	stopPeak = nil
	growth := int64(peak) - int64(before.HeapInuse)
	t.Logf("bytes=%d elapsed=%s heap_growth=%d first_part_requests=%d range_resumes=%d", size, time.Since(started), growth, partOne.Load(), rangeResumes.Load())
	if sizeMB >= 1024 && growth > 128<<20 {
		t.Fatalf("unbounded transfer heap growth: %d", growth)
	}
}
