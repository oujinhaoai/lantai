package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// A normal Agent session exercises the new adapters against the real five-db
// application and file transport. Human setup is confined to this temporary
// synthetic fixture; no production identity, file or trust configuration is used.
func TestAgentResourceClients(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	principal, session := e.agent("resources@synthetic", identity.RoleOwner)
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
	origin := "http://" + servers.Addresses.API
	c, err := client.New(client.Config{BaseURL: origin, SessionToken: session.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	work := t.TempDir()
	source := []byte("synthetic Agent resource bytes\n")
	if err = os.WriteFile(filepath.Join(work, "选择素材.txt"), source, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(work, "download"), 0700); err != nil {
		t.Fatal(err)
	}
	server, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c, Workspace: work})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent-resource-acceptance", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args any, wantError bool) map[string]any {
		t.Helper()
		out, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || out.IsError != wantError {
			t.Fatalf("%s failed: %v %s", name, err, mustJSON(out))
		}
		return consistencyJSON(t, mustJSON(out.StructuredContent))
	}
	who := call("whoami", map[string]any{}, false)
	if !bytes.Contains(mustJSON(who), []byte(principal.ID)) || !bytes.Contains(mustJSON(who), []byte(authz.Agent)) {
		t.Fatal("fixture is not a normal Agent session", who)
	}
	call("project_list", map[string]any{"limit": 1}, false)
	call("resource_types", map[string]any{}, false)
	call("project_members", map[string]any{"project_id": e.project.ProjectID, "limit": 1}, false)
	in := client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "agent/coverage", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "选择素材.txt", Role: "source"}}, Rights: json.RawMessage(`{"usage":"production","license":"LicenseRef-Owned","sensitivity":"normal"}`)}, Describe: json.RawMessage(`{"title":"Agent coverage","tags":["exact","café"],"extra":{"retained":1234567890123}}`)}
	receipt := call("resource_push", map[string]any{"directory": ".", "state": "push-state.json", "input": in}, false)
	asset, version := receipt["asset_id"].(string), receipt["version_id"].(string)
	history := call("resource_versions", map[string]any{"asset_id": asset, "limit": 1}, false)
	if len(history["items"].([]any)) != 1 {
		t.Fatal("history missing committed version", history)
	}
	filePage := call("resource_files", map[string]any{"asset_id": asset, "version_id": version}, false)
	if filePage["complete"] != true || filePage["total_files"] != float64(1) {
		t.Fatal(filePage)
	}
	file := filePage["files"].([]any)[0].(map[string]any)
	if file["sha256"] != shaOf(source) || file["path"] != "选择素材.txt" || file["size"] != float64(len(source)) {
		t.Fatal("manifest byte evidence mismatch", file)
	}
	call("resource_download_link", map[string]any{"asset_id": asset, "version_id": version, "path": file["path"], "purpose": "archive_review"}, false)
	call("resource_pull", map[string]any{"asset_id": asset, "version_id": version, "directory": "download", "purpose": "archive_review"}, false)
	actual, err := os.ReadFile(filepath.Join(work, "download", file["path"].(string)))
	if err != nil || !bytes.Equal(actual, source) {
		t.Fatal("download byte verification failed", err)
	}
	current := call("resource_current", map[string]any{"asset_id": asset}, false)
	revision := current["description"].(map[string]any)["revision"].(float64)
	patch := map[string]any{"asset_id": asset, "if_match": fmt.Sprintf("\"%.0f\"", revision), "idempotency_key": "stable-metadata-change", "request": map[string]any{"title": "Agent updated", "profile": map[string]any{"schema": client.AssetProfileSchema, "business_state": "active", "characters": []string{"Alice"}}}}
	call("metadata_set", patch, false)
	call("metadata_set", patch, false)
	updated := call("resource_current", map[string]any{"asset_id": asset}, false)
	desc := updated["description"].(map[string]any)
	if desc["title"] != "Agent updated" || !bytes.Contains(mustJSON(updated), []byte("1234567890123")) {
		t.Fatal("conditional profile update lost unknown extra", updated)
	}
	patch["idempotency_key"] = "new-intent-stale-revision"
	conflict := call("metadata_set", patch, true)
	if !strings.Contains(string(mustJSON(conflict)), "PRECONDITION_FAILED") {
		t.Fatal(conflict)
	}
	call("resource_search", map[string]any{"project_id": e.project.ProjectID, "tags": []string{"exact", "cafe\u0301"}, "characters": []string{"Alice"}, "business_state": "active", "explicit_completion": true}, false)
	rights := call("rights_read", map[string]any{"asset_id": asset, "version_id": version}, false)
	if rights["evidence_revision"] == nil || rights["revision"] == nil {
		t.Fatal("current rights revisions missing", rights)
	}
	frozen := call("resource_read", map[string]any{"asset_id": asset, "version_id": version, "view": "full"}, false)
	digest := frozen["version"].(map[string]any)["manifest_digest"].(string)
	ref := rights["subject"]
	evidence := call("rights_evidence", map[string]any{"idempotency_key": "saved-source-evidence", "request": map[string]any{"subject": ref, "manifest_digest": digest, "expected_revision": rights["evidence_revision"], "evidence": map[string]any{"note": "Synthetic owned source evidence", "external_inputs": []any{}}}}, false)
	rights = call("rights_read", map[string]any{"asset_id": asset, "version_id": version}, false)
	restricted := call("rights_assert", map[string]any{"idempotency_key": "saved-rights-restriction", "request": map[string]any{"subject": ref, "manifest_digest": digest, "kind": "restrict", "expected_revision": rights["revision"], "fields": map[string]any{"noai": true}, "evidence_ids": []any{evidence["record_id"]}, "reason": "Synthetic monotonic no-AI restriction"}}, false)
	if restricted["operation_id"] == nil {
		t.Fatal(restricted)
	}
	call("rights_read", map[string]any{"asset_id": asset, "version_id": version}, false)
	// A foreign-instance permanent URI must fail before any resource read.
	foreign := ids.PermanentRef{InstanceID: ids.New(), AssetID: ids.ID(asset), VersionID: ids.ID(version)}
	foreignURI, _ := foreign.URI()
	call("resource_read", map[string]any{"ref": foreignURI}, true)
	// Compare the formal CLI history and current-rights entry points.
	sessionPath := filepath.Join(work, "cli-session.json")
	writeJSON(t, sessionPath, map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": session.Token})
	for _, args := range [][]string{{"versions", "--asset", asset}, {"rights", "show", "--asset", asset, "--version", version}} {
		var out, stderr bytes.Buffer
		argv := append(args, "--server", origin, "--allow-http", "--session-file", sessionPath)
		if exit := cli.Run(t.Context(), argv, &out, &stderr); exit != 0 {
			t.Fatal(exit, stderr.String())
		}
		if !bytes.Contains(out.Bytes(), []byte(version)) {
			t.Fatal("CLI lost exact version", out.String())
		}
	}
	e.setRole(principal.ID, identity.RoleOwner, false)
	denied := call("resource_files", map[string]any{"asset_id": asset, "version_id": version}, true)
	if !strings.Contains(string(mustJSON(denied)), "FORBIDDEN") && !strings.Contains(string(mustJSON(denied)), "NOT_FOUND") {
		t.Fatal("revoked Agent retained access", denied)
	}
}
