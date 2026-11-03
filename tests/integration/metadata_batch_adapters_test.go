package integration

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
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
