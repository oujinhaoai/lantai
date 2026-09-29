package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/api"
	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestRemoteM2TaskExecutionAndMCP(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	e.setRole(e.admin.Context.PrincipalID, identity.RoleContributor, true)
	asset := e.ingest(e.admin.Context, "remote-synthetic", []byte("synthetic remote bytes"), *rightsOwned())
	app := reopenApplication(t, e)
	cfg := app.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: true}
	servers, err := app.StartHTTP(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := servers.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	login := e.login()
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, SessionToken: login.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	post := func(path string, in, out any) {
		t.Helper()
		r, err := c.Do(t.Context(), http.MethodPost, "/api/v1/"+path, in, client.Options{IdempotencyKey: e.key()})
		if err != nil {
			t.Fatal(path, err)
		}
		if out != nil {
			if err = r.Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var created tasks.Result
	post("tasks", tasks.CreateRequest{ProjectID: e.project.ProjectID, Type: "question", Title: "remote question", AcceptanceCriteria: []string{"record answer"}, Role: identity.RoleContributor}, &created)
	r, err := c.Do(t.Context(), http.MethodGet, "/api/v1/tasks/"+string(created.TaskID), nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var v tasks.View
	if err = r.Decode(&v); err != nil {
		t.Fatal(err)
	}
	validatePublic(t, "M2TasksView", r.Body)
	var claimed tasks.Result
	post("tasks/claim", tasks.ClaimRequest{ProjectID: e.project.ProjectID, TaskID: v.Task.ID, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision}, &claimed)
	raw, err := os.ReadFile("../../schemas/examples/agent-execution/v1/agent-request/start.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Document ae.StartRequest `json:"document"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	in := fixture.Document
	in.OperationID = ids.New()
	in.TaskRunID = ids.New()
	in.BudgetID = ids.New()
	in.Fence = claimed.Attempt.Fence
	in.Fence.TaskID = v.Task.ID
	in.Input = v.Task.Input
	in.Activation = nil
	in.Profile.ActivationSnapshot = ae.ActivationSnapshot{}
	in.Profile.PlaybookRefs = []ids.PermanentRef{}
	in.Profile.RequiredCapabilities = []string{"manual_session"}
	in.ExecutionKey, _ = ae.ExecutionKey(in.TaskRunID, in.Fence.AttemptID)
	in.RequestHash, _ = ae.RequestHash(in)
	var run ax.Record
	post("task-runs", in, &run)
	post("task-runs/progress", ax.ProgressRequest{Mutation: mut(run, in.Fence), Usage: ae.BudgetUsage{ModelCalls: 1}}, &run)
	r, err = c.Do(t.Context(), http.MethodGet, "/api/v1/task-runs/"+string(run.Run.ID), nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	validatePublic(t, "M2AgentexecutionRecord", r.Body)
	post("messages", map[string]any{"target": map[string]any{"project_id": e.project.ProjectID, "kind": "task", "id": v.Task.ID}, "kind": "question", "text": "clarify the output", "anchors": []any{}, "mentions": []any{}}, nil)
	if err = app.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	r, err = c.Do(t.Context(), http.MethodGet, "/api/v1/events?project_id="+string(e.project.ProjectID)+"&after=0&limit=100", nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	validatePublic(t, "M2QueryEventPage", r.Body)
	// The remote CLI uses the same session and API; it cannot open the server DB.
	dir := t.TempDir()
	session := filepath.Join(dir, "session.json")
	writeJSON(t, session, map[string]string{"schema": "lantai.client-session/v1", "origin": c.Origin(), "token": login.Token})
	var stdout, stderr bytes.Buffer
	code := cli.Run(t.Context(), []string{"run", "show", "--id", string(run.Run.ID), "--server", c.Origin(), "--allow-http", "--session-file", session}, &stdout, &stderr)
	if code != 0 || !bytes.Contains(stdout.Bytes(), []byte(run.Run.ID)) {
		t.Fatal(code, stderr.String())
	}
	// The Python adapter talks to the same live application and sees the same
	// version, task and error contracts as REST/CLI/MCP.
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("Python >=3.11 required for SDK integration", err)
	}
	script := `import os
from lantai import Client, APIError
c=Client(os.environ["TEST_ORIGIN"],os.environ["TEST_SESSION"],allow_http=True)
assert c.meta()["api_version"]=="v1"
assert c.whoami()
assert c.task(os.environ["TEST_TASK"])["task"]["id"]==os.environ["TEST_TASK"]
assert c.tasks(os.environ["TEST_PROJECT"],limit=1)["items"]
p=c.download_file(os.environ["TEST_ASSET"],os.environ["TEST_VERSION"],"content.txt",os.environ["TEST_OUTPUT"])
assert p.read_bytes()==b"synthetic remote bytes"
try: c.task("00000000000000000000000000")
except APIError as e: assert e.code=="NOT_FOUND" and not e.retryable
else: raise AssertionError("expected API error")
`
	cmd := exec.CommandContext(t.Context(), python, "-c", script)
	sdkPath, _ := filepath.Abs("../../sdk/python")
	cmd.Env = append(os.Environ(), "PYTHONPATH="+sdkPath, "TEST_ORIGIN="+c.Origin(), "TEST_SESSION="+login.Token, "TEST_TASK="+string(v.Task.ID), "TEST_PROJECT="+string(e.project.ProjectID), "TEST_ASSET="+string(asset.AssetID), "TEST_VERSION="+string(asset.VersionID), "TEST_OUTPUT="+t.TempDir())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	// Human preparation freezes the exact domain request on the server. A
	// mutable request cannot be injected at grant execution.
	var prepared struct {
		Challenge identity.Challenge `json:"challenge"`
	}
	post("human/prepare", map[string]any{"items": []any{map[string]any{"kind": "control", "request": ledger.ControlMutation{Action: "ledger.disable_version", ProjectID: e.project.ProjectID, Kind: "version", ID: asset.VersionID, ExpectedRevision: 1, Reason: "synthetic review"}}}}, &prepared)
	var grant identity.Grant
	post("identity/challenges/"+string(prepared.Challenge.ChallengeID)+"/verify", map[string]string{"code": e.fresh()}, &grant)
	gr, err := c.Do(t.Context(), http.MethodGet, "/api/v1/human/grants/"+string(grant.GrantID), nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var items []identity.HumanGrantItem
	if err = gr.Decode(&items); err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	_, err = c.Do(t.Context(), http.MethodPost, "/api/v1/human/execute", map[string]any{"grant_id": grant.GrantID, "operation_id": items[0].OperationID, "request": map[string]string{"action": "ledger.enable_version"}}, client.Options{})
	if err == nil {
		t.Fatal("mutable human request accepted")
	}
	post("human/execute", map[string]any{"grant_id": grant.GrantID, "operation_id": items[0].OperationID}, nil)
	post("human/execute", map[string]any{"grant_id": grant.GrantID, "operation_id": items[0].OperationID}, nil)
	state, err := app.Ledger.VersionControl(t.Context(), asset.VersionID)
	if err != nil || state.Availability != "disabled" {
		t.Fatal(state, err)
	}
	// A real newline framed stdio transport, official SDK negotiation and tool calls.
	server, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c, Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	sr, cw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cr, sw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ss, err := server.Connect(t.Context(), &mcp.IOTransport{Reader: sr, Writer: sw, MaxLineLength: mcpserver.MaxRequestBytes}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	mc := mcp.NewClient(&mcp.Implementation{Name: "integration", Version: "1"}, nil)
	cs, err := mc.Connect(t.Context(), &mcp.IOTransport{Reader: cr, Writer: cw, MaxLineLength: 2 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(t.Context(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
		if strings.Contains(tool.Name, "human") || strings.Contains(tool.Name, "trash") || strings.Contains(tool.Name, "shell") || strings.Contains(tool.Name, "token") {
			t.Fatal("unsafe tool", tool.Name)
		}
	}
	for _, name := range []string{"task_read", "run_progress", "message_post", "resource_push", "resource_pull"} {
		if !names[name] {
			t.Fatal("missing real tool", name)
		}
	}
	out, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "task_read", Arguments: map[string]any{"id": v.Task.ID}})
	if err != nil || out.IsError || out.StructuredContent == nil {
		t.Fatal(out, err)
	}
	out, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_pull", Arguments: map[string]any{"directory": "../escape", "asset_id": ids.New(), "version_id": ids.New(), "purpose": "archive_review"}})
	if err != nil || !out.IsError {
		t.Fatal("workspace escape", out, err)
	}
	out, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "run_progress", Arguments: map[string]any{"idempotency_key": e.key(), "request": ax.ProgressRequest{Mutation: mut(run, in.Fence), Usage: ae.BudgetUsage{ModelCalls: 2}}}})
	if err != nil || out.IsError {
		t.Fatal(out, err)
	}
	// A recorded execution result cannot close its task.
	var latest ax.Record
	r, err = c.Do(t.Context(), http.MethodGet, "/api/v1/task-runs/"+string(run.Run.ID), nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Decode(&latest); err != nil {
		t.Fatal(err)
	}
	post("task-runs/seal", ax.SealRequest{Mutation: mut(latest, in.Fence), Outcome: execution.RunExecutionSucceeded, Stopped: true, CandidateIDs: []ids.ID{}, Limitations: []string{"manual observation"}}, &latest)
	current, err := app.Tasks.Task(t.Context(), login.Context, v.Task.ID)
	if err != nil || current.Task.State != "claimed" {
		t.Fatal(current, err)
	}
}
func validatePublic(t *testing.T, name string, body []byte) {
	t.Helper()
	var doc, value any
	if err := json.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("https://api.example.test/spec", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("https://api.example.test/spec#/components/schemas/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Validate(value); err != nil {
		t.Fatal(name, err)
	}
}
