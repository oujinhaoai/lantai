package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Exercises the advertised contract and actual stdio calls against real storage.
// Derived from the original failing TEST-M2-11 object-input reproducer.
func TestMCPPushInputContract(t *testing.T) {
	bin := buildConsistencyCLI(t)
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	session := e.login()
	app := reopenApplication(t, e)
	servers := remoteServers(t, app, true)
	work := t.TempDir()
	if err := os.Mkdir(filepath.Join(work, "Unicode-é"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "Unicode-é", "样本.txt"), []byte("synthetic minimal MCP push\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := newConsistencyClient(t, bin, "http://"+servers.Addresses.API, session.Token)
	h.mcp = h.connectMCP(t, "--workspace", work)
	ev := &m211Evidence{t: t, dir: os.Getenv("LANTAI_M211_EVIDENCE")}
	if ev.dir != "" {
		if err := os.MkdirAll(ev.dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	tools, err := h.mcp.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ev.save("MCP-tool-list", tools)
	// Validate rejected inputs before any state file, upload, receipt or version
	// can be created. The real SDK must reject them at its advertised boundary.
	snapshot := func(t *testing.T) []int {
		t.Helper()
		var counts []int
		for _, item := range []struct {
			db    ownership.Database
			table string
		}{
			{ownership.Runtime, "storage_uploads"}, {ownership.Runtime, "command_receipts"},
			{ownership.Ledger, "ledger_versions"}, {ownership.Ledger, "command_receipts"},
		} {
			var count int
			if err := app.Instance.DB(item.db).QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+item.table).Scan(&count); err != nil {
				t.Fatal(err)
			}
			counts = append(counts, count)
		}
		return counts
	}
	validRights := map[string]any{"usage": "production", "license": "LicenseRef-Owned", "sensitivity": "normal"}
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"rights_array", "rights", []any{1, 2}}, {"rights_string", "rights", "license"},
		{"rights_number", "rights", 1}, {"rights_boolean", "rights", true},
		{"rights_empty", "rights", map[string]any{}},
		{"rights_missing_license", "rights", map[string]any{"usage": "production", "sensitivity": "normal"}},
		{"rights_bad_usage", "rights", map[string]any{"usage": "unsafe", "license": "MIT", "sensitivity": "normal"}},
		{"rights_bad_sensitivity", "rights", map[string]any{"usage": "production", "license": "MIT", "sensitivity": "unsafe"}},
		{"rights_bad_bool", "rights", map[string]any{"usage": "production", "license": "MIT", "sensitivity": "normal", "noai": "false"}},
		{"rights_unknown_field", "rights", map[string]any{"usage": "production", "license": "MIT", "sensitivity": "normal", "unknown": true}},
		{"metadata_array", "metadata", []any{}}, {"producer_array", "producer", []any{}},
		{"uses_object", "uses", map[string]any{}},
		{"describe_array", "describe", []any{}}, {"task_array", "task", []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := map[string]any{"asset_type": "doc", "files": []any{map[string]any{"path": "样本.txt", "role": "source"}}, "rights": validRights}
			input := map[string]any{"project_id": string(e.project.ProjectID), "slug": "m211/invalid", "content": content}
			if tc.field == "describe" || tc.field == "task" {
				input[tc.field] = tc.value
			} else {
				content[tc.field] = tc.value
			}
			state := "invalid-" + tc.name + ".json"
			args := map[string]any{"directory": "Unicode-é", "state": state, "input": input}
			before := snapshot(t)
			out, err := h.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_push", Arguments: args})
			if err != nil || out == nil || !out.IsError || !strings.Contains(string(mustJSON(out)), "validating") {
				t.Fatalf("expected advertised input rejection: %v %s", err, mustJSON(out))
			}
			after := snapshot(t)
			if !bytes.Equal(mustJSON(before), mustJSON(after)) {
				t.Fatalf("rejection caused side effects: %v -> %v", before, after)
			}
			if _, err := os.Stat(filepath.Join(work, state)); !os.IsNotExist(err) {
				t.Fatal("rejected input created state", err)
			}
			ev.save(tc.name, map[string]any{"request": args, "response": out, "before": before, "after": after})
		})
	}
	input := client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "m211/minimal-schema", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "样本.txt", Role: "source"}}, Rights: json.RawMessage(`{"usage":"production","license":"LicenseRef-Owned","sensitivity":"normal"}`), Metadata: json.RawMessage(`{"extra":{"note":"kept"}}`), Uses: json.RawMessage(`[]`)}, Describe: json.RawMessage(`{"title":"MCP 对象上传","tags":null,"summary":null}`)}
	var countBefore, countAfter int
	if err = app.Instance.DB(ownership.Runtime).QueryRowContext(t.Context(), "SELECT COUNT(*) FROM storage_uploads").Scan(&countBefore); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"input": input, "directory": "Unicode-é", "state": "state.json"}
	out, err := h.mcp.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_push", Arguments: args})
	if err != nil {
		t.Fatal("MCP protocol failure", err)
	}
	if err = app.Instance.DB(ownership.Runtime).QueryRowContext(t.Context(), "SELECT COUNT(*) FROM storage_uploads").Scan(&countAfter); err != nil {
		t.Fatal(err)
	}
	ev.save("minimal-repro", map[string]any{"request": args, "response": out, "upload_count_before": countBefore, "upload_count_after": countAfter})
	if out.IsError || countAfter != countBefore+1 {
		t.Fatalf("normal PushInput rights object rejected before upload (%d -> %d); original MCP response=%s", countBefore, countAfter, mustJSON(out))
	}
	result := consistencyJSON(t, mustJSON(out.StructuredContent))
	readRights := func(t *testing.T, result map[string]any) any {
		t.Helper()
		path := "/api/v1/assets/" + result["asset_id"].(string) + "/versions/" + result["version_id"].(string) + "?view=full"
		raw, status := m211HTTP(t, h, "GET", path, "", nil, nil)
		if status != 200 {
			t.Fatal("read persisted manifest", status, string(raw))
		}
		full := consistencyJSON(t, raw)
		return full["manifest"].(map[string]any)["content"].(map[string]any)["rights"]
	}
	want := map[string]any{"usage": "production", "license": "LicenseRef-Owned", "sensitivity": "normal", "noai": false, "redistribute_raw": false}
	if !bytes.Equal(mustJSON(readRights(t, result)), mustJSON(want)) {
		t.Fatal("persisted rights differ from declared rights")
	}
	for _, tc := range []struct {
		name   string
		rights json.RawMessage
	}{{"omitted", nil}, {"null", json.RawMessage(`null`)}} {
		t.Run("inherit_rights_"+tc.name, func(t *testing.T) {
			in := input
			in.AssetID, in.BaseVersionID, in.Slug = result["asset_id"].(string), result["version_id"].(string), ""
			in.Content.Rights = tc.rights
			in.Describe = nil
			in.Task = json.RawMessage(`null`)
			in.Content.Metadata = json.RawMessage(`null`)
			in.Content.Producer = json.RawMessage(`null`)
			in.Content.Uses = json.RawMessage(`null`)
			v, failed := m211Call(t, h, "resource_push", map[string]any{"directory": "Unicode-é", "state": "inherit-" + tc.name + ".json", "input": in})
			if failed {
				t.Fatal("omitted/null rights no longer inherited", v)
			}
			if !bytes.Equal(mustJSON(readRights(t, v)), mustJSON(want)) {
				t.Fatal("inherited rights changed")
			}
			result = v
		})
	}
	// Syntactically valid input still goes through domain validation. Missing
	// rights on a NEW asset retains the existing server rejection semantics.
	for _, tc := range []struct {
		name   string
		rights json.RawMessage
	}{{"omitted", nil}, {"null", json.RawMessage(`null`)}} {
		t.Run("new_asset_requires_rights_"+tc.name, func(t *testing.T) {
			in := input
			in.Slug, in.Content.Rights = "m211/missing-rights-"+tc.name, tc.rights
			before := snapshot(t)
			v, failed := m211Call(t, h, "resource_push", map[string]any{"directory": "Unicode-é", "state": "missing-rights-" + tc.name + ".json", "input": in})
			if !failed {
				t.Fatal("new asset accepted without rights")
			}
			m211AssertError(t, v, errcode.SchemaInvalid)
			after := snapshot(t)
			if after[2] != before[2] || after[3] != before[3] {
				t.Fatal("domain rejection committed a version/receipt")
			}
		})
	}
}
