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
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c, err := New(Config{BaseURL: s.URL, AllowHTTP: true, SessionToken: "private-session", TransferConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func sha(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestCredentialsAndTransferOrigin(t *testing.T) {
	var leaked bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true; writeJSON(w, map[string]any{}) }))
	defer evil.Close()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-session" {
			t.Error("missing bearer")
		}
		http.Redirect(w, r, evil.URL+"/stolen", http.StatusTemporaryRedirect)
	})
	if _, err := c.Do(context.Background(), "GET", "/api/v1/whoami", nil, Options{}); err == nil {
		t.Fatal("redirect accepted")
	}
	for _, u := range []string{evil.URL + "/xfer/part", "https://user:secret@example.test/xfer/a", "/api/v1/whoami", "//example.test/xfer/p"} {
		if _, err := c.authorizedURL(u); err == nil {
			t.Fatalf("unsafe URL accepted: %s", u)
		}
	}
	if _, err := c.transferRequest(context.Background(), "GET", "/xfer/test?signature=private", nil, 0, nil); err == nil {
		t.Fatal("transfer redirect accepted")
	}
	if leaked {
		t.Fatal("credentials crossed origin")
	}
	if c.api.Transport == c.transfer.Transport {
		t.Fatal("shared connection pool")
	}
	if c.api.Transport.(*http.Transport).Proxy != nil || c.transfer.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("proxy enabled")
	}
}

func TestAPIRejectsDotSegmentsBeforeNetwork(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("invalid API path reached network: %s", r.URL.Path)
		writeJSON(w, map[string]any{})
	})
	for _, path := range []string{
		"/api/v1/../../ops/foo",
		"/api/v1/%2e%2e/%2E%2E/ops/foo",
		"/api/v1/assets/../../../ops/foo",
		"/api/v1/./whoami",
	} {
		if _, err := c.Do(context.Background(), http.MethodGet, path, nil, Options{}); err == nil {
			t.Errorf("accepted path %q", path)
		}
	}
}
func TestErrorRedactsRequestSecretsAndAuthorizedURL(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		writeJSON(w, errcode.New(errcode.SchemaInvalid, "private-session password123 https://gateway.invalid/xfer/read?signature=abc /xfer/read?signature=def").Envelope("req"))
	})
	_, err := c.Do(context.Background(), "POST", "/api/v1/sessions/login", map[string]string{"password": "password123"}, Options{})
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(api.Body)
	for _, secret := range []string{"private-session", "password123", "signature=", "gateway.invalid"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret in error: %s", raw)
		}
	}
}
func TestTransferDoesNotBlockAPIAndCancellation(t *testing.T) {
	started := make(chan struct{})
	done := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		close(started)
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(done)
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- c.Download(ctx, DownloadGrant{URL: "/xfer/a", SHA256: sha(strings.Repeat("a", 100)), Size: 100}, t.TempDir(), "a.bin")
	}()
	<-started
	apiCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := c.Do(apiCtx, "GET", "/api/v1/whoami", nil, Options{}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("transport context not canceled")
	}
}
func TestDownloadResumeAndDigest(t *testing.T) {
	dir := t.TempDir()
	data := "abcdefghij"
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "abc")
			return
		}
		if got := r.Header.Get("Range"); got != "bytes=3-" {
			t.Errorf("Range=%q", got)
		}
		w.Header().Set("Content-Range", "bytes 3-9/10")
		w.Header().Set("Content-Length", "7")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, data[3:])
	})
	g := DownloadGrant{URL: "/xfer/v1/read?sig=notpersisted", SHA256: sha(data), Size: 10, Path: "nested/a.bin"}
	if err := c.Download(context.Background(), g, dir, g.Path); err == nil {
		t.Fatal("truncation accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, g.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("published partial")
	}
	state, err := os.ReadFile(filepath.Join(dir, g.Path+".lantai-download.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), "sig=") {
		t.Fatal("persisted signature")
	}
	if err = c.Download(context.Background(), g, dir, g.Path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, g.Path))
	if string(b) != data {
		t.Fatalf("download=%q", b)
	}
	if err = c.Download(context.Background(), g, dir, g.Path); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("unnecessary download: %d", calls)
	}
	bad := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "wrong") })
	badDir := t.TempDir()
	err = bad.Download(context.Background(), DownloadGrant{URL: "/xfer/a", SHA256: sha("right"), Size: 5}, badDir, "b")
	if !errors.Is(err, errcode.New(errcode.HashMismatch, "")) {
		t.Fatalf("digest=%v", err)
	}
	st, _ := os.Stat(filepath.Join(badDir, "b.lantai-part"))
	if st.Size() != 0 {
		t.Fatal("bad partial not reset")
	}
}
func TestDownloadRejectsUnsafeAndUnhonoredRange(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "abc") })
	for _, p := range []string{"../secret", "/absolute", "a\\b", "CON.txt", "a.", "a:b", "a.lantai-part", "bad\nname"} {
		if err := SafeRelative(p); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err == nil {
		if err = c.Download(context.Background(), DownloadGrant{URL: "/xfer/a", SHA256: sha("abc"), Size: 3}, dir, "escape/file"); err == nil {
			t.Fatal("escaped download root")
		}
	}
	meta, _ := json.Marshal(downloadState{Origin: c.Origin(), SHA256: sha("abc"), Size: 3})
	_ = os.WriteFile(filepath.Join(dir, "a.lantai-download.json"), meta, 0600)
	_ = os.WriteFile(filepath.Join(dir, "a.lantai-part"), []byte("a"), 0600)
	if err := c.Download(context.Background(), DownloadGrant{URL: "/xfer/a", SHA256: sha("abc"), Size: 3}, dir, "a"); err == nil {
		t.Fatal("ignored range accepted")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.lantai-part"))
	if string(b) != "a" {
		t.Fatal("partial altered after bad range")
	}
}

func TestPushResumesPartsAndLostCommitResponse(t *testing.T) {
	producer := json.RawMessage(`{"extension_id":"io.github.oujinhaoai.lantai.manifest-check","extension_version":"1.0.0","package_digest":"sha256:abababababababababababababababababababababababababababababababab","source":"builtin_release","contribution_id":"io.github.oujinhaoai.lantai.manifest-check.structure","core_release_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}`)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.bin"), []byte("abcdef"), 0600)
	state := filepath.Join(dir, "state.json")
	var mu sync.Mutex
	received := map[int]bool{}
	var createKeys, commitKeys []string
	partCalls := []int{}
	creates, commits, ops := 0, 0, 0
	losePart := true
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		file := UploadFile{SHA256: sha("abcdef"), Size: 6, PartSize: 3, PartCount: 2, State: "pending", PartURLTemplate: "/xfer/parts/{part_number}"}
		for p := 1; p <= 2; p++ {
			if received[p] {
				file.ReceivedParts = append(file.ReceivedParts, p)
			}
		}
		u := Upload{UploadID: "upload1", OperationID: "operation1", State: "open", Files: []UploadFile{file}}
		switch {
		case r.URL.Path == "/api/v1/uploads" && r.Method == "POST":
			creates++
			createKeys = append(createKeys, r.Header.Get("Idempotency-Key"))
			writeJSON(w, u)
		case r.URL.Path == "/api/v1/uploads/upload1":
			writeJSON(w, u)
		case strings.HasPrefix(r.URL.Path, "/xfer/parts/"):
			var p int
			fmt.Sscanf(r.URL.Path, "/xfer/parts/%d", &p)
			partCalls = append(partCalls, p)
			b, _ := io.ReadAll(r.Body)
			if sha(string(b)) != r.Header.Get("Lantai-Part-Sha256") {
				t.Error("part hash header mismatch")
			}
			received[p] = true
			if p == 1 && losePart {
				losePart = false
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			writeJSON(w, map[string]bool{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			writeJSON(w, map[string]bool{"ok": true})
		case strings.HasSuffix(r.URL.Path, "/commit"):
			var body struct {
				Content struct {
					Producer json.RawMessage `json:"producer"`
				} `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body.Content.Producer) != string(producer) {
				t.Errorf("producer changed in commit: %s %v", body.Content.Producer, err)
			}
			commits++
			commitKeys = append(commitKeys, r.Header.Get("Idempotency-Key"))
			if commits == 1 {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			writeJSON(w, map[string]string{"version_id": "version1", "operation_id": "operation1"})
		case r.URL.Path == "/api/v1/operations/operation1":
			ops++
			writeJSON(w, map[string]string{"operation_id": "operation1", "stage": "committed"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	in := PushInput{ProjectID: "project1", Slug: "first", Content: ContentInput{AssetType: "document", Producer: producer, Files: []InputFile{{Path: "a.bin", Role: "original"}}}}
	if _, err := c.Push(context.Background(), in, dir, state, false); err == nil {
		t.Fatal("expected lost part response")
	}
	var saved PushState
	if err := LoadState(state, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.UploadID == "" || saved.CreateKey == "" || saved.CommitKey == "" {
		t.Fatal("keys not saved before requests")
	}
	if _, err := c.Push(context.Background(), in, dir, state, false); err == nil {
		t.Fatal("expected lost commit response")
	}
	r, err := c.Push(context.Background(), in, dir, state, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Body), "version1") {
		t.Fatal("lost receipt")
	}
	if creates != 1 || commits != 2 || ops != 1 || !reflect.DeepEqual(partCalls, []int{1, 2}) || commitKeys[0] != commitKeys[1] {
		t.Fatalf("creates=%d commits=%d ops=%d parts=%v keys=%v", creates, commits, ops, partCalls, commitKeys)
	}
	changedProducer := in
	changedProducer.Content.Producer = json.RawMessage(strings.Replace(string(producer), "1.0.0", "2.0.0", 1))
	if _, err = c.Push(context.Background(), changedProducer, dir, state, false); err == nil {
		t.Fatal("changed producer reused committed state")
	}
	_ = os.WriteFile(filepath.Join(dir, "a.bin"), []byte("mutate"), 0600)
	if _, err = c.Push(context.Background(), in, dir, state, false); err == nil {
		t.Fatal("changed local request reused state")
	}
	if len(createKeys) != 1 || createKeys[0] == commitKeys[0] {
		t.Fatal("distinct stable keys missing")
	}
}
func TestPrivateFileAndStateLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := WritePrivate(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(path); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := lockState(path); err == nil {
		u()
		t.Fatal("concurrent writer acquired state")
	}
	unlock()
	unlock, err = lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestCreateResponseLossReusesPersistedKeyAndEmptyPart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "push.json")
	var keys []string
	partCalls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		u := Upload{UploadID: "upload", OperationID: "operation", Files: []UploadFile{{SHA256: sha(""), Size: 0, PartSize: 1, PartCount: 1, State: "pending", PartURLTemplate: "/xfer/parts/{part_number}"}}}
		switch {
		case r.URL.Path == "/api/v1/uploads" && r.Method == "POST":
			var saved PushState
			if err := LoadState(state, &saved); err != nil {
				t.Error(err)
			}
			key := r.Header.Get("Idempotency-Key")
			if key != saved.CreateKey {
				t.Error("request preceded durable key")
			}
			keys = append(keys, key)
			if len(keys) == 1 {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			writeJSON(w, u)
		case r.URL.Path == "/xfer/parts/1":
			partCalls++
			if r.ContentLength != 0 {
				t.Errorf("empty length=%d", r.ContentLength)
			}
			if r.Header.Get("Lantai-Part-Sha256") != sha("") {
				t.Error("missing empty hash")
			}
			writeJSON(w, map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			writeJSON(w, map[string]any{})
		case r.URL.Path == "/api/v1/uploads/upload":
			u.Files[0].State = "verified"
			writeJSON(w, u)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	in := PushInput{ProjectID: "project", Content: ContentInput{AssetType: "document", Files: []InputFile{{Path: "empty", Role: "original"}}}}
	if _, err := c.Push(context.Background(), in, dir, state, true); err == nil {
		t.Fatal("lost response accepted")
	}
	if _, err := c.Push(context.Background(), in, dir, state, true); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != keys[1] || partCalls != 1 {
		t.Fatalf("keys=%v parts=%d", keys, partCalls)
	}
}

func TestPreparePushCannotReadOutsideWorkingCopy(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "input")); err != nil {
		t.Skip("symlink creation unavailable on this platform")
	}
	in := PushInput{ProjectID: "project", Content: ContentInput{AssetType: "document", Files: []InputFile{{Path: "input", Role: "original"}}}}
	if _, err := PreparePush(context.Background(), in, dir); err == nil {
		t.Fatal("upload followed symlink outside working copy")
	}
}

func TestConcurrentDownloadsCannotAlterPublishedInode(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var count int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		count++
		close(started)
		<-release
		_, _ = io.WriteString(w, "safe")
	})
	dir := t.TempDir()
	g := DownloadGrant{URL: "/xfer/file", SHA256: sha("safe"), Size: 4}
	result := make(chan error, 1)
	go func() { result <- c.Download(context.Background(), g, dir, "asset") }()
	<-started
	if err := c.Download(context.Background(), g, dir, "asset"); err == nil || !strings.Contains(err.Error(), "another process") {
		close(release)
		<-result
		t.Fatalf("concurrent writer admitted: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "asset"))
	if err != nil || string(data) != "safe" {
		t.Fatalf("published data=%q %v", data, err)
	}
	if count != 1 {
		t.Fatalf("concurrent request sent: %d", count)
	}
	if err = c.Download(context.Background(), g, dir, "asset"); err != nil {
		t.Fatal(err)
	}
}
