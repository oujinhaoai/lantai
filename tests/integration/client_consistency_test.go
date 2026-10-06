package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

// TEST-M2-11: the CLI and MCP adapter are separate executable processes. Each
// request is also sent by raw HTTP and the Python SDK to the same real core.
// Error request IDs vary; operation and recovery fields must survive every
// adapter. Opaque search cursors are compared by their continuation semantics.
// This is functional, not load, validation.
func TestClientConsistency(t *testing.T) {
	bin := buildConsistencyCLI(t)
	t.Run("common_capabilities", func(t *testing.T) {
		e := newEnv(t, storage.Config{}, transfer.Limits{})
		e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
		worker, session := e.agent("matrix-worker@node", identity.RoleContributor)
		worker, err := e.id.GetPrincipal(t.Context(), e.login().Context, worker.ID)
		if err != nil {
			t.Fatal(err)
		}
		credential := e.sudo(&identity.IssueCredential{PrincipalID: worker.ID, ExpectedRevision: worker.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeTask}})
		session, err = e.id.ExchangeToken(t.Context(), credential.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
		if err != nil {
			t.Fatal(err)
		}
		_, foreign := e.agent("matrix-foreign@node", "")
		var assetIDs, versionIDs []ids.ID
		for i := range 3 {
			v := e.ingest(e.admin.Context, fmt.Sprintf("matrix-%d", i), []byte(fmt.Sprintf("synthetic matrix %d", i)), *rightsOwned())
			assetIDs, versionIDs = append(assetIDs, v.AssetID), append(versionIDs, v.VersionID)
		}
		app := reopenApplication(t, e)
		var taskIDs []ids.ID
		for i := range 3 {
			r, err := app.Tasks.Create(t.Context(), e.login().Context, e.key(), tasks.CreateRequest{ProjectID: e.project.ProjectID, Type: "question", Title: fmt.Sprintf("matrix question %d", i), AcceptanceCriteria: []string{"record answer"}, Role: identity.RoleContributor})
			if err != nil {
				t.Fatal(err)
			}
			taskIDs = append(taskIDs, r.TaskID)
		}
		if err := app.Sync(t.Context()); err != nil {
			t.Fatal(err)
		}
		servers := remoteServers(t, app, true)
		origin := "http://" + servers.Addresses.API
		h := newConsistencyClient(t, bin, origin, session.Token)
		readTask := func(id ids.ID) consistencyRequest {
			return consistencyRequest{path: "/api/v1/tasks/" + string(id), cli: []string{"task", "show", "--id", string(id)}, tool: "task_read", args: map[string]any{"id": id}, python: "task", pythonArgs: []any{id}}
		}
		readVersion := consistencyRequest{path: fmt.Sprintf("/api/v1/assets/%s/versions/%s?view=brief", assetIDs[0], versionIDs[0]), cli: []string{"show", "--asset", string(assetIDs[0]), "--version", string(versionIDs[0])}, tool: "resource_read", args: map[string]any{"asset_id": assetIDs[0], "version_id": versionIDs[0]}, python: "version", pythonArgs: []any{assetIDs[0], versionIDs[0]}}
		t.Run("whoami", func(t *testing.T) {
			h.compare(t, consistencyRequest{path: "/api/v1/whoami", cli: []string{"whoami"}, tool: "whoami", python: "whoami"}, "")
		})
		t.Run("exact_version", func(t *testing.T) { h.compare(t, readVersion, "") })
		t.Run("task_detail", func(t *testing.T) { h.compare(t, readTask(taskIDs[0]), "") })
		t.Run("task_pagination", func(t *testing.T) {
			var after string
			var seen []string
			for range 4 {
				q := url.Values{"project_id": {string(e.project.ProjectID)}, "after": {after}, "limit": {"1"}}
				v := h.compare(t, consistencyRequest{path: "/api/v1/tasks?" + q.Encode(), cli: []string{"task", "list", "--project", string(e.project.ProjectID), "--cursor", after, "--limit", "1"}, tool: "task_list", args: map[string]any{"project_id": e.project.ProjectID, "after": after, "limit": 1}, python: "tasks", pythonArgs: []any{e.project.ProjectID}, pythonKw: map[string]any{"after": after, "limit": 1}}, "")
				items := v["items"].([]any)
				if len(items) == 0 {
					break
				}
				after = items[0].(map[string]any)["id"].(string)
				seen = append(seen, after)
			}
			want := []string{string(taskIDs[0]), string(taskIDs[1]), string(taskIDs[2])}
			slices.Sort(want)
			if !slices.Equal(seen, want) {
				t.Fatalf("pagination lost or duplicated tasks: %v", seen)
			}
		})
		t.Run("search_pagination", func(t *testing.T) {
			var cursor string
			seen := map[string]bool{}
			for range 4 {
				q := url.Values{"project_id": {string(e.project.ProjectID)}, "cursor": {cursor}, "limit": {"1"}, "view": {"brief"}}
				v := h.compare(t, consistencyRequest{cursor: true, path: "/api/v1/assets?" + q.Encode(), cli: []string{"search", "--project", string(e.project.ProjectID), "--cursor", cursor, "--limit", "1"}, tool: "resource_search", args: map[string]any{"project_id": e.project.ProjectID, "after": cursor, "limit": 1}, python: "search", pythonArgs: []any{e.project.ProjectID}, pythonKw: map[string]any{"cursor": cursor, "limit": 1}}, "")
				for _, item := range v["items"].([]any) {
					id := item.(map[string]any)["asset_id"].(string)
					if seen[id] {
						t.Fatal("duplicate search result", id)
					}
					seen[id] = true
				}
				cursor, _ = v["next_cursor"].(string)
				if cursor == "" {
					break
				}
			}
			if len(seen) != len(assetIDs) {
				t.Fatalf("search pagination returned %d of %d assets", len(seen), len(assetIDs))
			}
		})
		t.Run("not_found", func(t *testing.T) { h.compare(t, readTask(ids.New()), errcode.NotFound) })
		view, err := app.Tasks.Task(t.Context(), session.Context, taskIDs[0])
		if err != nil {
			t.Fatal(err)
		}
		claim := tasks.ClaimRequest{ProjectID: e.project.ProjectID, TaskID: view.Task.ID, SeatID: view.Seat.ID, ExpectedRevision: view.Seat.Revision}
		write := consistencyRequest{method: "POST", path: "/api/v1/tasks/claim", body: claim, key: "matrix-claim", cli: []string{"task", "claim"}, tool: "task_claim", python: "task_command", pythonArgs: []any{"claim", claim}, pythonKw: map[string]any{"key": "matrix-claim"}}
		t.Run("claim_and_cross_client_replay", func(t *testing.T) {
			h.compare(t, write, "")
			attempts, err := app.Tasks.Attempts(t.Context(), session.Context, taskIDs[0])
			if err != nil || len(attempts) != 1 {
				t.Fatalf("replay created a second attempt: count=%d error=%v", len(attempts), err)
			}
		})
		t.Run("same_key_different_request", func(t *testing.T) {
			changed := write
			other := claim
			other.ExpectedRevision++
			changed.body, changed.pythonArgs = other, []any{"claim", other}
			h.compare(t, changed, errcode.IdempotencyConflict)
		})
		t.Run("claim_conflict", func(t *testing.T) {
			other := write
			other.key, other.pythonKw = "matrix-claim-again", map[string]any{"key": "matrix-claim-again"}
			h.compare(t, other, errcode.TaskAlreadyClaimed)
		})
		t.Run("foreign_resource", func(t *testing.T) {
			other := newConsistencyClient(t, bin, origin, foreign.Token)
			other.compare(t, readVersion, errcode.NotFound)
		})
		t.Run("revocation_rechecks_existing_clients", func(t *testing.T) {
			e.setRole(worker.ID, identity.RoleContributor, false)
			h.compare(t, readVersion, errcode.NotFound)
			h.compare(t, readTask(taskIDs[0]), errcode.NotFound)
			other := write
			other.key, other.pythonKw = "matrix-revoked-claim", map[string]any{"key": "matrix-revoked-claim"}
			h.compare(t, other, errcode.NotFound)
		})
		t.Run("ended_session", func(t *testing.T) {
			if err := app.Identity.EndSession(t.Context(), session.Context, session.Context.SessionID); err != nil {
				t.Fatal(err)
			}
			h.compare(t, consistencyRequest{path: "/api/v1/whoami", cli: []string{"whoami"}, tool: "whoami", python: "whoami"}, errcode.TokenRevoked)
		})
	})
	t.Run("stdio_protocol_bounds_and_cancel", func(t *testing.T) { consistencyProtocol(t, bin) })
}

type consistencyRequest struct {
	method, path, key string
	cursor            bool
	body              any
	cli               []string
	tool              string
	args              map[string]any
	python            string
	pythonArgs        []any
	pythonKw          map[string]any
}

type consistencyClient struct {
	bin, origin, session, token string
	mcp                         *mcp.ClientSession
	env                         []string
	directory                   string
}

func buildConsistencyCLI(t *testing.T) string {
	t.Helper()
	name := "lantai"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	cmd := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", path, "./cmd/lantai")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build client executable: %v\n%s", err, out)
	}
	return path
}

func consistencyEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LANTAI_") && !strings.HasPrefix(kv, "PYTHONPATH=") {
			env = append(env, kv)
		}
	}
	return env
}

func newConsistencyClient(t *testing.T, bin, origin, token string) *consistencyClient {
	t.Helper()
	h := &consistencyClient{bin: bin, origin: origin, token: token, session: filepath.Join(t.TempDir(), "session.json")}
	writeJSON(t, h.session, map[string]string{"schema": "lantai.client-session/v1", "origin": origin, "token": token})
	h.mcp = h.connectMCP(t)
	init := h.mcp.InitializeResult()
	if init == nil || init.ProtocolVersion == "" || init.ServerInfo.Name != "lantai" {
		t.Fatal("stdio MCP did not negotiate the official server")
	}
	page, err := h.mcp.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range page.Tools {
		if strings.Contains(tool.Name, "human") || strings.Contains(tool.Name, "trash") || strings.Contains(tool.Name, "shell") || strings.Contains(tool.Name, "token") || strings.Contains(tool.Name, "enable") {
			t.Fatal("sensitive tool projected", tool.Name)
		}
	}
	return h
}

func (h *consistencyClient) connectMCP(t *testing.T, extra ...string) *mcp.ClientSession {
	t.Helper()
	args := append([]string{"mcp", "--server", h.origin, "--allow-http", "--session-file", h.session}, extra...)
	// CommandTransport owns process shutdown by closing stdin. A CommandContext
	// tied to t.Context would kill the child before cleanup can verify its exit.
	cmd := exec.Command(h.bin, args...)
	cmd.Env, cmd.Stderr = append(consistencyEnv(), h.env...), io.Discard
	cmd.Dir = h.directory
	s, err := mcp.NewClient(&mcp.Implementation{Name: "client-consistency", Version: "1"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal("stdio MCP connect", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error("stdio MCP close", err)
		}
	})
	return s
}

func consistencyJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("client response is not an object: %v", err)
	}
	return out
}

func (h *consistencyClient) cli(t *testing.T, args ...string) (map[string]any, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, append(args, "--server", h.origin, "--allow-http", "--session-file", h.session)...)
	cmd.Env = append(consistencyEnv(), h.env...)
	cmd.Dir = h.directory
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	raw := stdout.Bytes()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal("CLI process", err)
		}
		code, raw = exit.ExitCode(), stderr.Bytes()
	}
	if bytes.Contains(stdout.Bytes(), []byte(h.token)) || bytes.Contains(stderr.Bytes(), []byte(h.token)) {
		t.Fatal("CLI leaked the session credential")
	}
	if code == 2 && !json.Valid(raw) {
		// Local command syntax errors are usage text, not server errors.
		return map[string]any{"usage_error": string(raw)}, code
	}
	return consistencyJSON(t, raw), code
}

const consistencyPython = `import json, sys
from lantai import Client, APIError
spec=json.load(sys.stdin)
with open(spec["session"], encoding="utf-8") as f: saved=json.load(f)
c=Client(saved["origin"], saved["token"], allow_http=True)
try:
    value=getattr(c, spec["method"])(*(spec.get("args") or []), **(spec.get("kwargs") or {}))
    print(json.dumps(value, ensure_ascii=False))
except APIError as e:
    print(json.dumps({"error":e.body}, ensure_ascii=False))
`

func (h *consistencyClient) compare(t *testing.T, r consistencyRequest, expected errcode.Code) map[string]any {
	t.Helper()
	method := r.method
	if method == "" {
		method = "GET"
	}
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(mustJSON(r.body))
	}
	req, err := http.NewRequestWithContext(t.Context(), method, h.origin+r.path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", r.key)
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	want := consistencyJSON(t, raw)
	failed := resp.StatusCode >= 400
	if expected == "" && failed || expected != "" && (!failed || want["error"].(map[string]any)["code"] != string(expected)) {
		t.Fatalf("REST expected %s, got status=%d body=%s", expected, resp.StatusCode, raw)
	}
	args := slices.Clone(r.cli)
	if r.body != nil {
		args = append(args, "--input", writeTemp(t, r.body), "--idempotency-key", r.key)
	}
	cliValue, exit := h.cli(t, args...)
	if failed != (exit != 0) {
		t.Fatalf("CLI success disagrees with REST: exit=%d", exit)
	}
	if failed {
		wantExit := 1
		if resp.StatusCode == 401 {
			wantExit = 5
		} else if resp.StatusCode == 409 || resp.StatusCode == 412 {
			wantExit = 4
		}
		if exit != wantExit {
			t.Fatalf("CLI exit=%d, expected=%d", exit, wantExit)
		}
	}
	toolArgs := r.args
	if r.body != nil {
		toolArgs = map[string]any{"request": r.body, "idempotency_key": r.key}
	}
	out, err := h.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: r.tool, Arguments: toolArgs})
	if err != nil || out.IsError != failed {
		t.Fatalf("MCP success disagrees with REST: error=%v", err)
	}
	mcpValue := consistencyJSON(t, mustJSON(out.StructuredContent))
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("Python >=3.11 required", err)
	}
	cmd := exec.CommandContext(t.Context(), python, "-c", consistencyPython)
	sdk, err := filepath.Abs("../../sdk/python")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(consistencyEnv(), "PYTHONPATH="+sdk)
	cmd.Stdin = bytes.NewReader(mustJSON(map[string]any{"session": h.session, "method": r.python, "args": r.pythonArgs, "kwargs": r.pythonKw}))
	pyRaw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python SDK process: %v\n%s", err, pyRaw)
	}
	pyValue := consistencyJSON(t, pyRaw)
	values := map[string]map[string]any{"REST": want, "CLI": cliValue, "MCP": mcpValue, "Python": pyValue}
	if failed {
		for name, value := range values {
			body := value["error"].(map[string]any)
			requestID, _ := body["request_id"].(string)
			if requestID == "" {
				t.Fatal(name, "lost the machine error request ID")
			}
			delete(body, "request_id")
		}
	}
	var nextCursor any
	if r.cursor {
		h.compareCursorContinuations(t, r.path, values)
		nextCursor = want["next_cursor"]
		for _, value := range values {
			delete(value, "next_cursor")
		}
	}
	for name, got := range map[string]map[string]any{"CLI": cliValue, "MCP": mcpValue, "Python": pyValue} {
		if !bytes.Equal(mustJSON(want), mustJSON(got)) {
			t.Fatalf("%s differs from REST\nREST=%s\n%s=%s", name, mustJSON(want), name, mustJSON(got))
		}
	}
	if nextCursor != nil {
		want["next_cursor"] = nextCursor
	}
	t.Logf("REST/CLI/MCP/Python identical; error=%s CLI_exit=%d", expected, exit)
	return want
}

// Every adapter's randomly sealed cursor must continue to the same page when
// handed to REST. Subsequent compare calls hand REST's cursor to all adapters.
func (h *consistencyClient) compareCursorContinuations(t *testing.T, path string, values map[string]map[string]any) {
	t.Helper()
	first, _ := values["REST"]["next_cursor"].(string)
	var continuation []byte
	for _, name := range []string{"REST", "CLI", "MCP", "Python"} {
		cursor, _ := values[name]["next_cursor"].(string)
		if (cursor == "") != (first == "") {
			t.Fatalf("%s cursor presence differs from REST", name)
		}
		if cursor == "" {
			continue
		}
		u, err := url.Parse(h.origin + path)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("cursor", cursor)
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(t.Context(), "GET", u.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.token)
		transport := &http.Transport{}
		resp, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			transport.CloseIdleConnections()
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		transport.CloseIdleConnections()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s cursor is unusable: HTTP %d, %v", name, resp.StatusCode, err)
		}
		value := consistencyJSON(t, raw)
		next, _ := value["next_cursor"].(string)
		value["next_cursor"] = next != ""
		if name == "REST" {
			continuation = mustJSON(value)
		} else if !bytes.Equal(continuation, mustJSON(value)) {
			t.Fatalf("%s cursor continues to a different page", name)
		}
	}
}

func consistencyProtocol(t *testing.T, bin string) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/meta":
			_, _ = io.WriteString(w, `{"api_version":"v1","capabilities":["tasks","sessions"]}`)
		case "/api/v1/tasks/01ARZ3NDEKTSV4RRFFQ69G5FAV":
			close(started)
			<-r.Context().Done()
			close(cancelled)
		case "/api/v1/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{strings.Repeat("synthetic", mcpserver.MaxResultBytes)}})
		default:
			_, _ = io.WriteString(w, `{"principal":{"name":"synthetic"}}`)
		}
	}))
	defer remote.Close()
	h := newConsistencyClient(t, bin, remote.URL, "synthetic-protocol-token")
	t.Run("oversized_result", func(t *testing.T) {
		out, err := h.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "task_list", Arguments: map[string]any{"project_id": "01ARZ3NDEKTSV4RRFFQ69G5FAV"}})
		if err != nil || !out.IsError {
			t.Fatal("oversized result accepted", err)
		}
		value := consistencyJSON(t, mustJSON(out.StructuredContent))
		if value["error"].(map[string]any)["code"] != string(errcode.QuotaExceeded) || len(mustJSON(out)) > mcpserver.MaxResultBytes {
			t.Fatal("MCP result cap not enforced")
		}
	})
	t.Run("cancellation_reaches_rest", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := h.mcp.CallTool(ctx, &mcp.CallToolParams{Name: "task_read", Arguments: map[string]any{"id": "01ARZ3NDEKTSV4RRFFQ69G5FAV"}})
			done <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("REST call did not start")
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled call succeeded")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("MCP call did not cancel")
		}
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("REST call was not cancelled")
		}
		out, err := h.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "whoami"})
		if err != nil || out.IsError {
			t.Fatal("cancelling one request broke the stdio session", err)
		}
	})
}
