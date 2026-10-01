package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func reopenApplication(t *testing.T, e *env, config ...storage.Config) *application.App {
	t.Helper()
	cfg := storage.Config{PartSize: 64 << 10}
	if len(config) > 0 {
		cfg = config[0]
	}
	return reopenApplicationWith(t, e, application.Options{Storage: cfg})
}

// reopenApplicationWith 关闭夹具实例，以完整应用（含后台任务）在同一数据根与
// 可控时钟上重新打开；实例、身份与时钟参数由夹具补齐。
func reopenApplicationWith(t *testing.T, e *env, opts application.Options) *application.App {
	t.Helper()
	e.xfer.Close()
	home := e.inst.Layout().Home
	if err := e.inst.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	opts.Instance, opts.Identity = operations.Options{Home: home, Clock: e.clk}, identity.Config{Password: fastPassword}
	a, err := application.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	e.inst, e.id, e.ledger, e.rights, e.storage, e.catalog = a.Instance, a.Identity, a.Ledger, a.Rights, a.Storage, a.Catalog
	return a
}

func remoteServers(t *testing.T, a *application.App, merged bool) *application.HTTPServers {
	t.Helper()
	cfg := a.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: merged}
	cfg.HTTP.MaxJSONBytes = 2048
	s, err := a.StartHTTP(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.WritePrivate(path, b); err != nil {
		t.Fatal(err)
	}
}

// 两种内部监听都通过同一个外部入口调用；测试网关只转发 API/xfer，运维不转发。
// 合成 HTTP loopback 是显式开发模式；生产 TLS 网关另由 T08 验收。
func TestRemoteCLIStorage(t *testing.T) {
	for _, merged := range []bool{true, false} {
		name := "split"
		if merged {
			name = "merged"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, storage.Config{}, transfer.Limits{})
			e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
			a := reopenApplication(t, e)
			s := remoteServers(t, a, merged)
			apiURL, _ := url.Parse("http://" + s.Addresses.API)
			xferURL, _ := url.Parse("http://" + s.Addresses.Transfer)
			apiProxy := httputil.NewSingleHostReverseProxy(apiURL)
			xferProxy := httputil.NewSingleHostReverseProxy(xferURL)
			var currentAPI, currentXfer atomic.Pointer[httputil.ReverseProxy]
			currentAPI.Store(apiProxy)
			currentXfer.Store(xferProxy)
			var lost atomic.Bool
			apiProxy.ModifyResponse = func(r *http.Response) error {
				if strings.HasSuffix(r.Request.URL.Path, "/commit") && r.StatusCode == 201 && lost.CompareAndSwap(false, true) {
					_ = r.Body.Close()
					return errors.New("synthetic lost commit response")
				}
				return nil
			}
			apiProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(502) }
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/api/"):
					currentAPI.Load().ServeHTTP(w, r)
				case strings.HasPrefix(r.URL.Path, "/xfer/"):
					currentXfer.Load().ServeHTTP(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(gateway.Close)
			work := t.TempDir()
			session := filepath.Join(work, "session.json")
			credentials := filepath.Join(work, "login.json")
			writeJSON(t, credentials, map[string]string{"name": "ada", "password": adminPassword, "code": e.fresh()})
			call := func(want int, args ...string) []byte {
				t.Helper()
				var out, stderr bytes.Buffer
				args = append(args, "--server", gateway.URL, "--allow-http", "--session-file", session)
				code := cli.Run(t.Context(), args, &out, &stderr)
				if code != want {
					t.Fatalf("CLI %s code=%d want=%d out=%s err=%s", args[0], code, want, out.String(), stderr.String())
				}
				if want != 0 {
					return stderr.Bytes()
				}
				return out.Bytes()
			}
			login := call(0, "login", "--credentials-file", credentials)
			if bytes.Contains(login, []byte(`"token"`)) || bytes.Contains(login, []byte(adminPassword)) {
				t.Fatal("credential appeared in CLI output")
			}
			call(0, "whoami")
			call(0, "types")
			projects := call(0, "project", "list", "--limit", "1")
			if !bytes.Contains(projects, []byte(e.project.ProjectID)) {
				t.Fatalf("visible project absent: %s", projects)
			}
			content := bytes.Repeat([]byte("synthetic CLI transfer\n"), 50000)
			if err := os.WriteFile(filepath.Join(work, "source.txt"), content, 0600); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(work, "push.json")
			state := filepath.Join(work, "push-state.json")
			in := client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "remote/sample", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "source.txt", Role: "source"}}, Rights: json.RawMessage(`{"usage":"production","license":"LicenseRef-Owned","sensitivity":"normal"}`)}}
			writeJSON(t, input, in)
			// 丢掉已提交响应；继续使用同一持久 state 和 operation 对账。
			call(3, "push", "--input", input, "--directory", work, "--state", state)
			if !lost.Load() {
				t.Fatal("fault did not occur after commit")
			}
			var saved client.PushState
			if err := client.LoadState(state, &saved); err != nil {
				t.Fatal(err)
			}
			if !saved.Committing || saved.OperationID == "" {
				t.Fatalf("no durable operation before response loss: %+v", saved)
			}
			// 核心关闭重开，网关 origin 不变；会话和回执均从真实五库/文件恢复。
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.Shutdown(closeCtx); err != nil {
				t.Fatal(err)
			}
			if err := a.Close(closeCtx); err != nil {
				t.Fatal(err)
			}
			cancel()
			a = reopenApplication(t, e)
			s = remoteServers(t, a, merged)
			apiURL, _ = url.Parse("http://" + s.Addresses.API)
			xferURL, _ = url.Parse("http://" + s.Addresses.Transfer)
			currentAPI.Store(httputil.NewSingleHostReverseProxy(apiURL))
			currentXfer.Store(httputil.NewSingleHostReverseProxy(xferURL))
			result := call(0, "push", "--input", input, "--directory", work, "--state", state)
			var committed catalog.VersionResult
			if err := json.Unmarshal(result, &committed); err != nil {
				t.Fatal(err)
			}
			if committed.VersionNumber != 1 || string(committed.OperationID) != saved.OperationID {
				t.Fatalf("retry identity: %+v", committed)
			}
			replay := call(0, "push", "--input", input, "--directory", work, "--state", state)
			if !bytes.Equal(result, replay) {
				t.Fatal("same-key replay changed result")
			}
			call(0, "show", "--ref", committed.URI, "--view", "full")
			pull := filepath.Join(work, "pulled")
			call(0, "pull", "--asset", string(committed.AssetID), "--version", string(committed.VersionID), "--directory", pull)
			got, err := os.ReadFile(filepath.Join(pull, "source.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if shaOf(got) != shaOf(content) {
				t.Fatal("CLI round trip digest differs")
			}
			if err = a.Sync(t.Context()); err != nil {
				t.Fatal(err)
			}
			search := call(0, "search", "--project", string(e.project.ProjectID), "--query", "sample")
			if !bytes.Contains(search, []byte(committed.AssetID)) {
				t.Fatalf("query missing committed asset: %s", search)
			}
			op := call(0, "operation", "--id", string(committed.OperationID))
			if !bytes.Contains(op, []byte(`"stage":"projected"`)) || bytes.Contains(op, []byte("response_summary")) {
				t.Fatalf("unsafe/incomplete operation: %s", op)
			}
			patch := filepath.Join(work, "patch.json")
			writeJSON(t, patch, map[string]string{"title": "updated title"})
			call(0, "metadata", "set", "--asset", string(committed.AssetID), "--input", patch, "--if-match", `"1"`, "--idempotency-key", "patch-title")
			failure := call(4, "metadata", "set", "--asset", string(committed.AssetID), "--input", patch, "--if-match", `"1"`, "--idempotency-key", "stale-title")
			if !bytes.Contains(failure, []byte(errcode.PreconditionFailed)) {
				t.Fatalf("old ETag: %s", failure)
			}
			var sessionSaved struct {
				Token string `json:"token"`
			}
			if err = client.LoadState(session, &sessionSaved); err != nil {
				t.Fatal(err)
			}
			c, err := client.New(client.Config{BaseURL: gateway.URL, AllowHTTP: true, SessionToken: sessionSaved.Token})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, err = c.Do(t.Context(), http.MethodPost, "/api/v1/uploads", map[string]any{"project_id": e.project.ProjectID, "files": []storage.FileSpec{{SHA256: shaOf([]byte("different")), Size: 9}}}, client.Options{IdempotencyKey: saved.CreateKey})
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) || apiErr.Body.Code != errcode.IdempotencyConflict {
				t.Fatalf("REST changed-body retry: %v", err)
			}
			_, err = c.Do(t.Context(), http.MethodPost, "/api/v1/projects", map[string]string{"key": "x", "summary": strings.Repeat("x", 3000)}, client.Options{IdempotencyKey: "limit"})
			if !errors.As(err, &apiErr) || apiErr.Body.Code != errcode.SchemaInvalid {
				t.Fatalf("JSON limit: %v", err)
			}
			for _, path := range []string{"/readyz", "/healthz", "/metrics"} {
				resp, err := http.Get(gateway.URL + path)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 404 {
					t.Fatalf("operations exposed: %s %d", path, resp.StatusCode)
				}
			}
			resp, err := http.Get("http://" + s.Addresses.Operations + "/readyz")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatal("operations not ready")
			}
			// 实时撤权后：已有会话、operation、项目列表均重新核对，不回放旧权限。
			e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, false)
			failure = call(1, "operation", "--id", string(committed.OperationID))
			if !bytes.Contains(failure, []byte(errcode.NotFound)) {
				t.Fatalf("revoked operation: %s", failure)
			}
			projects = call(0, "project", "list")
			if bytes.Contains(projects, []byte(e.project.ProjectID)) {
				t.Fatal("revoked project leaked")
			}
			call(0, "session", "end")
			call(5, "whoami")
		})
	}
}
