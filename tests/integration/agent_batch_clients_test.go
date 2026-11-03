package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// Real temporary five-db application, normal Agent session, original CLI/client
// state and actual bytes. This is adapter self-verification, not human approval.
func TestAgentBatchClients(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	principal, session := e.agent("batches@synthetic", identity.RoleOwner)
	app := reopenApplication(t, e)
	servers := remoteServers(t, app, true)
	origin := "http://" + servers.Addresses.API
	c, err := client.New(client.Config{BaseURL: origin, SessionToken: session.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	work := t.TempDir()
	for _, name := range []string{"source", "specs", "reports", "notes"} {
		if err := os.Mkdir(filepath.Join(work, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string, value any) {
		t.Helper()
		if err := client.SaveState(filepath.Join(work, name), value); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string, value any) {
		t.Helper()
		if err := client.LoadState(filepath.Join(work, name), value); err != nil {
			t.Fatal(err)
		}
	}
	assertBytes := func(name string, want []byte) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(work, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("bytes differ for %s: %v", name, err)
		}
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "batch-client-acceptance", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args any, wantError bool) map[string]any {
		t.Helper()
		out, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || out.IsError != wantError {
			t.Fatalf("%s: %v %s", name, err, mustJSON(out))
		}
		return consistencyJSON(t, mustJSON(out.StructuredContent))
	}
	write("specs/session.json", map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": session.Token})
	runCLI := func(args ...string) []byte {
		t.Helper()
		var out, stderr bytes.Buffer
		argv := append(args, "--server", origin, "--allow-http", "--session-file", filepath.Join(work, "specs/session.json"))
		if exit := cli.Run(t.Context(), argv, &out, &stderr); exit != 0 {
			t.Fatalf("CLI %v: %d %s", args, exit, stderr.String())
		}
		return out.Bytes()
	}
	data := []byte("synthetic original batch source\n")
	if err := os.WriteFile(filepath.Join(work, "source/data.txt"), data, 0600); err != nil {
		t.Fatal(err)
	}
	rights, err := json.Marshal(rightsOwned())
	if err != nil {
		t.Fatal(err)
	}
	input := client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{Key: "selected", Directory: ".", Classification: "final", Push: client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "batches/selected", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "data.txt", Role: "source"}}, Rights: rights}}}}}
	write("specs/import.json", input)
	planArgs := map[string]any{"input_file": "specs/import.json", "directory": "source", "output_file": "reports/plan.json"}
	planSummary := call("import_plan", planArgs, false)
	var cliPlan client.ImportPlan
	if err := json.Unmarshal(runCLI("asset", "import", "plan", "--input", filepath.Join(work, "specs/import.json"), "--directory", filepath.Join(work, "source"), "--output", filepath.Join(work, "reports/cli-plan.json")), &cliPlan); err != nil {
		t.Fatal(err)
	}
	if planSummary["plan_digest"] != cliPlan.Digest {
		t.Fatal("CLI/MCP original plan mismatch", planSummary)
	}
	call("import_plan", planArgs, false)
	// The real catalog is still empty after both offline plans.
	empty := call("resource_search", map[string]any{"project_id": e.project.ProjectID}, false)
	if len(empty["items"].([]any)) != 0 {
		t.Fatal("plan wrote remote assets", empty)
	}
	applyArgs := map[string]any{"input_file": "reports/plan.json", "directory": "source", "state_directory": "state", "output_file": "reports/applied.json"}
	apply := call("import_apply", applyArgs, false)
	if apply["complete"] != true || apply["current_remote_verified"] != false {
		t.Fatal("historical apply became current proof", apply)
	}
	var report client.ImportReport
	read("reports/applied.json", &report)
	var originalState client.PushState
	read("state/selected.push.json", &originalState)
	var replay client.ImportReport
	if err := json.Unmarshal(runCLI("asset", "import", "apply", "--input", filepath.Join(work, "reports/plan.json"), "--directory", filepath.Join(work, "source"), "--state", filepath.Join(work, "state"), "--output", filepath.Join(work, "reports/cli-applied.json")), &replay); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report, replay) {
		t.Fatal("CLI/MCP replay changed exact receipts", report, replay)
	}
	var replayState client.PushState
	read("state/selected.push.json", &replayState)
	if !reflect.DeepEqual(originalState, replayState) {
		t.Fatal("replay replaced original keys/state")
	}
	verifyArgs := map[string]any{"input_file": "reports/plan.json", "report_file": "reports/applied.json", "directory": "source", "state_directory": "state", "destination": "verified", "purpose": "archive_review", "output_file": "reports/verified.json"}
	verified := call("import_verify", verifyArgs, false)
	if verified["complete"] != true || verified["current_remote_verified"] != true {
		t.Fatal("fresh verification missing", verified)
	}
	assertBytes("verified/selected/data.txt", data)
	historical := call("batch_report_page", map[string]any{"file": "reports/verified.json"}, false)
	if historical["current_remote_verified"] != false || historical["historical_local_observation"] != true || historical["recorded"].(map[string]any)["current_remote_verified"] != true {
		t.Fatal("saved verification revived authority", historical)
	}
	row := report.Results[0]
	workset := client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: row.AssetID, VersionID: row.VersionID, Destination: "selected"}}, Entrypoints: []string{"selected/data.txt"}}
	write("specs/workset.json", workset)
	call("workset_plan", map[string]any{"input_file": "specs/workset.json", "directory": "working", "output_file": "reports/workset-plan.json"}, false)
	if _, err := os.Stat(filepath.Join(work, "working")); !os.IsNotExist(err) {
		t.Fatal("workset plan created directory", err)
	}
	call("workset_pull", map[string]any{"input_file": "specs/workset.json", "directory": "working", "output_file": "reports/workset-pull.json", "purpose": "archive_review"}, false)
	assertBytes("working/selected/data.txt", data)
	ref := ids.PermanentRef{InstanceID: app.Instance.InstanceID(), AssetID: ids.ID(row.AssetID), VersionID: ids.ID(row.VersionID)}
	uri, err := ref.URI()
	if err != nil {
		t.Fatal(err)
	}
	note := []byte("PRIVATE_SYNTHETIC_NOTE\n[Original](media/original.txt)\n")
	if err := os.WriteFile(filepath.Join(work, "notes/scene.md"), note, 0600); err != nil {
		t.Fatal(err)
	}
	write("specs/references.json", client.ReferenceInput{Schema: client.ReferenceInputSchema, Notes: []string{"scene.md"}, Mappings: []client.ReferenceMapping{{OldTarget: "media/original.txt", FixedRef: uri}}})
	call("references_plan", map[string]any{"input_file": "specs/references.json", "directory": "notes", "output_file": "reports/reference-plan.json"}, false)
	page := call("batch_report_page", map[string]any{"file": "reports/reference-plan.json", "section": "edits", "key": "scene.md"}, false)
	if bytes.Contains(mustJSON(page), []byte("PRIVATE_SYNTHETIC_NOTE")) || bytes.Contains(mustJSON(page), []byte(work)) {
		t.Fatal("note text or source root leaked", page)
	}
	call("references_copy", map[string]any{"input_file": "reports/reference-plan.json", "directory": "notes", "destination": "note-copy", "output_file": "reports/reference-copy.json"}, false)
	assertBytes("notes/scene.md", note)
	changed := bytes.ReplaceAll(note, []byte("media/original.txt"), []byte(uri))
	assertBytes("note-copy/scene.md", changed)
	// A real post-cutover committed version is retained in the explicit inventory.
	secondInput := input.Assets[0].Push
	secondInput.AssetID, secondInput.BaseVersionID, secondInput.Slug = row.AssetID, row.VersionID, ""
	secondInput.Content.VersionNote = "post-cutover synthetic result"
	prepared, err := client.PreparePush(t.Context(), secondInput, filepath.Join(work, "source"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Push(t.Context(), prepared, filepath.Join(work, "source"), filepath.Join(work, "specs/second.push.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	secondReceipt := consistencyJSON(t, second.Body)
	ref.VersionID = ids.ID(secondReceipt["version_id"].(string))
	secondURI, _ := ref.URI()
	write("specs/cutover.json", []string{secondURI})
	call("references_rollback", map[string]any{"input_file": "reports/reference-plan.json", "directory": "note-copy", "destination": "note-rollback", "post_cutover_refs_file": "specs/cutover.json", "output_file": "reports/reference-rollback.json"}, false)
	assertBytes("notes/scene.md", note)
	assertBytes("note-copy/scene.md", changed)
	assertBytes("note-rollback/scene.md", note)
	var rollback client.ReferenceReceipt
	read("reports/reference-rollback.json", &rollback)
	if !reflect.DeepEqual(rollback.PostCutoverRefs, []string{secondURI}) {
		t.Fatal("post-cutover inventory lost", rollback)
	}
	// Upload through original client state, commit through MCP, then resume the
	// original client state with exactly the same create/commit identities.
	prepared.AssetID, prepared.BaseVersionID, prepared.Slug = "", "", "batches/direct-commit"
	directState := filepath.Join(work, "specs/direct.push.json")
	if _, err := c.Push(t.Context(), prepared, filepath.Join(work, "source"), directState, true); err != nil {
		t.Fatal(err)
	}
	var before client.PushState
	read("specs/direct.push.json", &before)
	status := call("upload_status", map[string]any{"upload_id": before.UploadID, "limit": 1}, false)
	if bytes.Contains(mustJSON(status), []byte("part_url")) || status["complete"] != true {
		t.Fatal("upload status leaked grants", status)
	}
	call("upload_parts", map[string]any{"upload_id": before.UploadID, "sha256": shaOf(data), "limit": 1}, false)
	call("upload_check", map[string]any{"upload_id": before.UploadID, "request": map[string]any{"files": []any{map[string]any{"sha256": shaOf(data), "size": len(data)}}}}, false)
	var commitBody map[string]json.RawMessage
	if err := json.Unmarshal(mustJSON(prepared), &commitBody); err != nil {
		t.Fatal(err)
	}
	delete(commitBody, "project_id")
	committed := call("resource_commit", map[string]any{"upload_id": before.UploadID, "idempotency_key": before.CommitKey, "request": commitBody}, false)
	resumed, err := c.Push(t.Context(), prepared, filepath.Join(work, "source"), directState, false)
	if err != nil {
		t.Fatal(err)
	}
	var after client.PushState
	read("specs/direct.push.json", &after)
	if before.CreateKey != after.CreateKey || before.CommitKey != after.CommitKey || before.UploadID != after.UploadID || before.OperationID != after.OperationID || committed["version_id"] != consistencyJSON(t, resumed.Body)["version_id"] {
		t.Fatal("cross-client retry replaced state or receipt")
	}
	prepared.Slug = "batches/cancel-upload"
	if _, err := c.Push(t.Context(), prepared, filepath.Join(work, "source"), filepath.Join(work, "specs/cancel.push.json"), true); err != nil {
		t.Fatal(err)
	}
	var cancelState client.PushState
	read("specs/cancel.push.json", &cancelState)
	call("upload_cancel", map[string]any{"upload_id": cancelState.UploadID}, false)
	cancelled := call("upload_status", map[string]any{"upload_id": cancelState.UploadID}, false)
	if cancelled["state"] != "cancelled" {
		t.Fatal("original cancellation not reconciled", cancelled)
	}
	e.setRole(principal.ID, identity.RoleOwner, false)
	verifyArgs["output_file"] = "reports/revoked-verification.json"
	denied := call("import_verify", verifyArgs, true)
	if !strings.Contains(string(mustJSON(denied)), "FORBIDDEN") && !strings.Contains(string(mustJSON(denied)), "NOT_FOUND") {
		t.Fatal("revoked verification reused cached authority", denied)
	}
	var revoked client.ImportVerifyReport
	read("reports/revoked-verification.json", &revoked)
	if revoked.Complete || revoked.CurrentRemoteVerified {
		t.Fatal("revoked receipt claims current verification", revoked)
	}
	read("state/selected.push.json", &replayState)
	if !reflect.DeepEqual(originalState, replayState) {
		t.Fatal("revocation replaced original recovery state")
	}
}
