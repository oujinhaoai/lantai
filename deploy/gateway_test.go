package deploy_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lockedBuffer struct {
	sync.Mutex
	b bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) { b.Lock(); defer b.Unlock(); return b.b.Write(p) }
func (b *lockedBuffer) String() string              { b.Lock(); defer b.Unlock(); return b.b.String() }

// 显式提供已核验的官方二进制；测试不会下载、安装或修改系统配置。
func TestCaddySingleHTTPSGateway(t *testing.T) {
	bin := os.Getenv("LANTAI_TEST_CADDY")
	if bin == "" {
		t.Skip("set LANTAI_TEST_CADDY to a verified Caddy 2.11.4 binary")
	}
	version, err := exec.Command(bin, "version").Output()
	if err != nil || !strings.HasPrefix(string(version), "v2.11.4 ") {
		t.Fatalf("expected Caddy 2.11.4: %q %v", version, err)
	}
	var apiCalls atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"api":true}`)
	}))
	defer api.Close()
	uploadStarted := make(chan struct{})
	streamCancelled := make(chan struct{})
	xfer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/xfer/v1/upload":
			if r.Header.Get("Authorization") != "Bearer secret-token" || r.Header.Get("Lantai-Part-Sha256") != "secret-digest" || r.Header.Get("Idempotency-Key") != "secret-key" {
				t.Error("upload headers changed")
			}
			var first [1]byte
			if _, err := io.ReadFull(r.Body, first[:]); err != nil {
				t.Error(err)
				return
			}
			close(uploadStarted)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(204)
		case "/xfer/v1/read":
			if r.Header.Get("Range") != "bytes=2-4" || r.URL.RawQuery != "sig=secret-query" {
				t.Error("range or signature changed")
			}
			w.Header().Set("Content-Range", "bytes 2-4/6")
			w.WriteHeader(206)
			_, _ = io.WriteString(w, "cde")
		case "/xfer/v1/stream":
			_, _ = io.WriteString(w, "a")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(streamCancelled)
		default:
			t.Error("unexpected transfer route")
			w.WriteHeader(404)
		}
	}))
	defer xfer.Close()
	dir := t.TempDir()
	certFile, keyFile, pool := certificate(t, dir)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	url := "https://localhost:" + big.NewInt(int64(port)).String()
	env := append(os.Environ(), "LANTAI_HTTPS_ADDRESS="+url, "LANTAI_GATEWAY_BIND=127.0.0.1", "LANTAI_TLS_CERT="+certFile, "LANTAI_TLS_KEY="+keyFile, "LANTAI_API_UPSTREAM="+strings.TrimPrefix(api.URL, "http://"), "LANTAI_TRANSFER_UPSTREAM="+strings.TrimPrefix(xfer.URL, "http://"), "XDG_DATA_HOME="+filepath.Join(dir, "data"), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
	validate := exec.Command(bin, "validate", "--config", "Caddyfile", "--adapter", "caddyfile")
	validate.Env = env
	if out, err := validate.CombinedOutput(); err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var logs lockedBuffer
	cmd := exec.CommandContext(ctx, bin, "run", "--config", "Caddyfile", "--adapter", "caddyfile")
	cmd.Env = env
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
		if strings.Contains(logs.String(), "secret-") {
			t.Errorf("gateway logs leaked request: %s", logs.String())
		}
	}()
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, Proxy: nil}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := c.Get(url + "/api/v1/meta")
		if err == nil {
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway did not start: %v\n%s", err, logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	before := apiCalls.Load()
	for _, path := range []string{"/", "/healthz", "/readyz", "/metrics", "/debug/pprof/", "/api/v2/meta", "/api/v1/../../metrics", "/xfer/v1/../../metrics"} {
		res, err := c.Get(url + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Errorf("private/unknown route %s status=%d", path, res.StatusCode)
		}
	}
	if apiCalls.Load() != before {
		t.Fatal("private route reached API upstream")
	}
	request, _ := http.NewRequest("GET", url+"/xfer/v1/read?sig=secret-query", nil)
	request.Header.Set("Range", "bytes=2-4")
	res, err := c.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 206 || string(body) != "cde" || res.Header.Get("Content-Range") != "bytes 2-4/6" {
		t.Fatalf("range changed: %d %q %v", res.StatusCode, body, err)
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	request, _ = http.NewRequest("PUT", url+"/xfer/v1/upload?sig=secret-query", pr)
	request.Header.Set("Authorization", "Bearer secret-token")
	request.Header.Set("Lantai-Part-Sha256", "secret-digest")
	request.Header.Set("Idempotency-Key", "secret-key")
	done := make(chan error, 1)
	go func() {
		res, err := c.Do(request)
		if err == nil {
			res.Body.Close()
			if res.StatusCode != 204 {
				err = io.ErrUnexpectedEOF
			}
		}
		done <- err
	}()
	if _, err := pw.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-uploadStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway buffered the upload")
	}
	res, err = c.Get(url + "/api/v1/meta")
	if err != nil {
		t.Fatal("API blocked during upload:", err)
	}
	res.Body.Close()
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(t.Context())
	defer stop()
	request, _ = http.NewRequestWithContext(sctx, "GET", url+"/xfer/v1/stream?sig=secret-query", nil)
	res, err = c.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var first [1]byte
	if _, err := io.ReadFull(res.Body, first[:]); err != nil {
		t.Fatal(err)
	}
	stop()
	res.Body.Close()
	select {
	case <-streamCancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not cancel disconnected transfer")
	}
	// 连接失败会触发网关运行日志；不能把授权头、签名 URL 写入它。
	xfer.Close()
	res, err = c.Get(url + "/xfer/v1/read?sig=secret-query")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 502 {
		t.Fatal(res.StatusCode)
	}
}

func certificate(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("bad certificate")
	}
	return certFile, keyFile, pool
}
