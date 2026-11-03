package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestMetadataBatchCLIAndMCPPrivatePlansAndOriginalReplay(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	_, agent := e.agent("metadata-adapters@synthetic", identity.RoleOwner)
	v := e.ingest(agent.Context, "metadata-adapters/source", []byte("synthetic source"), *rightsOwned())
	a := reopenApplication(t, e)
	servers := assetRemoteServers(t, a)
	origin := "http://" + servers.Addresses.API
	c, err := client.New(client.Config{BaseURL: origin, SessionToken: agent.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	work := t.TempDir()
	selection := filepath.Join(work, "input.json")
	session := filepath.Join(work, "session.json")
	if err := client.SaveState(session, map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": agent.Token}); err != nil {
		t.Fatal(err)
	}
	const marker = "PRIVATE_METADATA_VALUE"
	in := client.MetadataBatchInput{Schema: client.MetadataBatchInputSchema, Items: []client.MetadataBatchItem{{AssetID: string(v.AssetID), Patch: json.RawMessage(`{"title":"PRIVATE_METADATA_VALUE"}`)}}}
	if err := client.SaveState(selection, in); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		argv := append([]string{"asset", "metadata"}, args...)
		argv = append(argv, "--server", origin, "--allow-http", "--session-file", session)
		var out, stderr bytes.Buffer
		if exit := cli.Run(t.Context(), argv, &out, &stderr); exit != 0 {
			t.Fatal(exit, stderr.String())
		}
		return out.Bytes()
	}
	var cliPlan client.MetadataBatchPlan
	if err := json.Unmarshal(run("plan", "--input", selection, "--output", filepath.Join(work, "cli-plan.json")), &cliPlan); err != nil {
		t.Fatal(err)
	}
	s, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c, Workspace: work})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "metadata-adapter-acceptance", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args any, wantError bool) map[string]any {
		t.Helper()
		r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || r.IsError != wantError {
			t.Fatal(name, err, string(mustJSON(r)))
		}
		if strings.Contains(string(mustJSON(r)), marker) {
			t.Fatal("private patch values leaked into bounded context")
		}
		return consistencyJSON(t, mustJSON(r.StructuredContent))
	}
	planArgs := map[string]any{"input_file": "input.json", "output_file": "mcp-plan.json"}
	planned := call("metadata_batch_plan", planArgs, false)
	if planned["plan_digest"] != cliPlan.Digest {
		t.Fatal("CLI/MCP plan mismatch", planned)
	}
	call("metadata_batch_plan", planArgs, false)
	result := call("metadata_batch_apply", map[string]any{"input_file": "mcp-plan.json", "output_file": "mcp-report.json", "state_file": "state.json"}, false)
	if result["complete"] != true || result["current_remote_verified"] != true {
		t.Fatal(result)
	}
	var report client.MetadataBatchReport
	if err := json.Unmarshal(run("apply", "--input", filepath.Join(work, "mcp-plan.json"), "--state", filepath.Join(work, "state.json"), "--output", filepath.Join(work, "cli-report.json")), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Complete || !report.CurrentRemoteVerified || report.Results[0].AcceptedRevision != cliPlan.Items[0].Revision+1 {
		t.Fatal("replay repeated a successful mutation", report)
	}
	history := call("metadata_batch_report_page", map[string]any{"file": "mcp-report.json", "limit": 1}, false)
	if history["current_remote_verified"] != false || history["recorded"].(map[string]any)["current_remote_verified"] != true {
		t.Fatal("saved receipt revived authority", history)
	}
	call("metadata_batch_report_page", map[string]any{"file": "session.json"}, true)
	call("metadata_batch_apply", map[string]any{"input_file": "mcp-plan.json", "output_file": "state.json", "state_file": "state.json"}, true)
}

func TestMetadataBatchCLIPlanFailureDoesNotPublishPartialPlan(t *testing.T) {
	instance, principal, project, first, second := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	var failSecond atomic.Bool
	failSecond.Store(true)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/meta":
			_ = json.NewEncoder(w).Encode(map[string]any{"instance_id": instance})
		case "/api/v1/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{"principal": map[string]any{"principal_id": principal}})
		case "/api/v1/assets/" + string(first), "/api/v1/assets/" + string(second):
			if strings.HasSuffix(r.URL.Path, string(second)) && failSecond.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"code":"STORAGE_UNAVAILABLE","message":"synthetic unavailable"}}`))
				return
			}
			asset := strings.TrimPrefix(r.URL.Path, "/api/v1/assets/")
			w.Header().Set("ETag", `"1"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"asset_id": asset, "description": map[string]any{"asset_id": asset, "project_id": project, "revision": 1}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer remote.Close()
	work := t.TempDir()
	selection, plan, session := filepath.Join(work, "selection.json"), filepath.Join(work, "plan.json"), filepath.Join(work, "session.json")
	if err := client.SaveState(session, map[string]any{"schema": "lantai.client-session/v1", "origin": remote.URL, "token": "synthetic-token"}); err != nil {
		t.Fatal(err)
	}
	if err := client.SaveState(selection, client.MetadataBatchInput{Schema: client.MetadataBatchInputSchema, Items: []client.MetadataBatchItem{{AssetID: string(first), Patch: json.RawMessage(`{"title":"first"}`)}, {AssetID: string(second), Patch: json.RawMessage(`{"title":"second"}`)}}}); err != nil {
		t.Fatal(err)
	}
	run := func(wantSuccess bool) {
		t.Helper()
		var out, stderr bytes.Buffer
		exit := cli.Run(t.Context(), []string{"asset", "metadata", "plan", "--input", selection, "--output", plan, "--server", remote.URL, "--allow-http", "--session-file", session}, &out, &stderr)
		if (exit == 0) != wantSuccess {
			t.Fatalf("CLI success=%v, want %v: %s", exit == 0, wantSuccess, stderr.String())
		}
	}
	run(false)
	if _, err := os.Stat(plan); !os.IsNotExist(err) {
		t.Fatalf("failed planning published a partial plan: %v", err)
	}
	failSecond.Store(false)
	run(true)
	original, err := client.ReadPrivate(plan)
	if err != nil {
		t.Fatal(err)
	}
	var complete client.MetadataBatchPlan
	if err := json.Unmarshal(original, &complete); err != nil || len(complete.Items) != 2 || complete.Digest == "" {
		t.Fatal("recovered planning did not publish both rows", complete, err)
	}
	failSecond.Store(true)
	run(false)
	after, err := client.ReadPrivate(plan)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("failed planning changed an existing original plan", err)
	}
	failSecond.Store(false)
	run(true)
}

func TestMetadataBatchCLIPlanPreservesOriginalIntentAndState(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	_, agent := e.agent("metadata-plan-original@synthetic", identity.RoleOwner)
	v := e.ingest(agent.Context, "metadata-plan-original/source", []byte("synthetic source"), *rightsOwned())
	servers := assetRemoteServers(t, reopenApplication(t, e))
	origin := "http://" + servers.Addresses.API
	work := t.TempDir()
	selection, plan, state, report, session := filepath.Join(work, "selection.json"), filepath.Join(work, "plan.json"), filepath.Join(work, "state.json"), filepath.Join(work, "report.json"), filepath.Join(work, "session.json")
	if err := client.SaveState(session, map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": agent.Token}); err != nil {
		t.Fatal(err)
	}
	writeSelection := func(title string) {
		t.Helper()
		patch, err := json.Marshal(map[string]string{"title": title})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.SaveState(selection, client.MetadataBatchInput{Schema: client.MetadataBatchInputSchema, Items: []client.MetadataBatchItem{{AssetID: string(v.AssetID), Patch: patch}}}); err != nil {
			t.Fatal(err)
		}
	}
	run := func(wantSuccess bool, args ...string) []byte {
		t.Helper()
		argv := append([]string{"asset", "metadata"}, args...)
		argv = append(argv, "--server", origin, "--allow-http", "--session-file", session)
		var out, stderr bytes.Buffer
		exit := cli.Run(t.Context(), argv, &out, &stderr)
		if (exit == 0) != wantSuccess {
			t.Fatalf("CLI success=%v, want %v: %s", exit == 0, wantSuccess, stderr.String())
		}
		return out.Bytes()
	}
	read := func(path string) []byte {
		t.Helper()
		raw, err := client.ReadPrivate(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	writeSelection("original intent")
	run(true, "plan", "--input", selection, "--output", plan)
	originalPlan := read(plan)
	// The same frozen bytes can resume without replacing the reviewed plan.
	run(true, "plan", "--input", selection, "--output", plan)
	if !bytes.Equal(read(plan), originalPlan) {
		t.Fatal("identical planning changed the original plan")
	}
	var accepted client.MetadataBatchReport
	if err := json.Unmarshal(run(true, "apply", "--input", plan, "--state", state, "--output", report), &accepted); err != nil {
		t.Fatal(err)
	}
	originalState := read(state)
	for _, title := range []string{"different intent", "original intent"} {
		// Both changed selection and unchanged selection at a newer remote
		// revision must preserve the original plan and its retry identities.
		writeSelection(title)
		run(false, "plan", "--input", selection, "--output", plan)
		if !bytes.Equal(read(plan), originalPlan) || !bytes.Equal(read(state), originalState) {
			t.Fatal("replanning replaced the original plan or state")
		}
	}
	var replay client.MetadataBatchReport
	if err := json.Unmarshal(run(true, "apply", "--input", plan, "--state", state, "--output", report), &replay); err != nil {
		t.Fatal(err)
	}
	if !accepted.Complete || !replay.Complete || !replay.CurrentRemoteVerified || replay.Results[0].AcceptedRevision != accepted.Results[0].AcceptedRevision || !bytes.Equal(read(state), originalState) {
		t.Fatal("preserved original intent could not resume without another revision", accepted, replay)
	}
}
