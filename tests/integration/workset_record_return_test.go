package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestManagedWorksetReturnKeepsOldRecordUntilExplicitRevision(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	_, agent := e.agent("workset-records@synthetic", identity.RoleOwner)
	v1 := e.ingest(agent.Context, "workset-records/source", []byte("fixed original bytes"), *rightsOwned())
	a := reopenApplication(t, e)
	servers := assetRemoteServers(t, a)
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, SessionToken: agent.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	root := t.TempDir()
	directory := func(name string) string {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	source := directory("new-files")
	recordDir := directory("record-v1")
	readDir := directory("read-v1")
	secondRecordDir := directory("record-v2")
	readSecond := directory("read-v2")
	download := directory("returned-download")
	input := client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: string(v1.AssetID), VersionID: string(v1.VersionID), Destination: "scene"}}, Entrypoints: []string{"scene/content.txt"}}
	prepared, err := c.PrepareWorksetRecord(t.Context(), client.WorksetRecordSpec{ProjectID: string(e.project.ProjectID), Title: "Synthetic fixed workset", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	recordResponse, err := c.SaveManagementRecord(t.Context(), prepared, recordDir, filepath.Join(root, "record-v1.state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record apiv1.CommitResult
	if err := recordResponse.Decode(&record); err != nil {
		t.Fatal(err)
	}
	_, opened, err := c.ReadWorksetRecord(t.Context(), record.AssetId, record.VersionId, readDir)
	if err != nil || opened.Input.Entries[0].VersionID != string(v1.VersionID) {
		t.Fatal(opened, err)
	}
	data := []byte("explicit returned new version bytes\n")
	if err := os.WriteFile(filepath.Join(source, "content.txt"), data, 0600); err != nil {
		t.Fatal(err)
	}
	spec := client.WorksetReturnSpec{Entry: input.Entries[0], Files: []client.InputFile{{Path: "content.txt", Role: "primary"}}, Purpose: "production", Usage: "production", Note: "synthetic explicit return"}
	returning, err := c.PrepareWorksetReturn(t.Context(), spec, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "original-return.state.json")
	returned, err := c.ApplyWorksetReturn(t.Context(), returning, source, statePath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt apiv1.CommitResult
	if err := returned.Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.AssetId != string(v1.AssetID) || receipt.VersionId == string(v1.VersionID) || input.Entries[0].VersionID != string(v1.VersionID) {
		t.Fatal("return changed old fixed input", receipt, input)
	}
	var stateBefore client.PushState
	if err := client.LoadState(statePath, &stateBefore); err != nil {
		t.Fatal(err)
	}
	replayed, err := c.ApplyWorksetReturn(t.Context(), returning, source, statePath)
	if err != nil {
		t.Fatal(err)
	}
	var replay apiv1.CommitResult
	if err := replayed.Decode(&replay); err != nil {
		t.Fatal(err)
	}
	var stateAfter client.PushState
	if err := client.LoadState(statePath, &stateAfter); err != nil {
		t.Fatal(err)
	}
	if replay.VersionId != receipt.VersionId || stateBefore.CreateKey != stateAfter.CreateKey || stateBefore.CommitKey != stateAfter.CommitKey || stateBefore.UploadID != stateAfter.UploadID || stateBefore.OperationID != stateAfter.OperationID {
		t.Fatal("return replay replaced original operation")
	}
	if _, err := c.Pull(t.Context(), receipt.AssetId, receipt.VersionId, download, "production"); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(download, "content.txt"))
	if err != nil || !bytes.Equal(data, actual) {
		t.Fatal("returned actual bytes differ", err)
	}
	_, stillOld, err := c.ReadWorksetRecord(t.Context(), record.AssetId, record.VersionId, readDir)
	if err != nil || stillOld.Input.Entries[0].VersionID != string(v1.VersionID) {
		t.Fatal("return implicitly revised workset", stillOld, err)
	}
	adoption := client.WorksetAdoption{RecordAssetID: record.AssetId, RecordVersionID: record.VersionId, Return: returning, Receipt: receipt}
	newPrepared, err := c.PrepareWorksetAdoption(t.Context(), adoption, readDir)
	if err != nil {
		t.Fatal(err)
	}
	if newPrepared.Input.AssetID != record.AssetId || newPrepared.Input.BaseVersionID != record.VersionId {
		t.Fatal("adoption replaced original record identity")
	}
	newRecordResponse, err := c.SaveManagementRecord(t.Context(), newPrepared, secondRecordDir, filepath.Join(root, "record-v2.state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var newRecord apiv1.CommitResult
	if err := newRecordResponse.Decode(&newRecord); err != nil {
		t.Fatal(err)
	}
	_, revised, err := c.ReadWorksetRecord(t.Context(), newRecord.AssetId, newRecord.VersionId, readSecond)
	if err != nil || revised.Input.Entries[0].VersionID != receipt.VersionId {
		t.Fatal(revised, err)
	}
	_, oldAgain, err := c.ReadWorksetRecord(t.Context(), record.AssetId, record.VersionId, readDir)
	if err != nil || oldAgain.Input.Entries[0].VersionID != string(v1.VersionID) {
		t.Fatal("old record stopped being fixed", err)
	}
	pending := adoption
	yes := true
	pending.Receipt.DescriptionPending = &yes
	if _, err := c.PrepareWorksetAdoption(t.Context(), pending, readDir); err == nil {
		t.Fatal("pending initial metadata adopted")
	}
	if _, err := c.PrepareWorksetReturn(t.Context(), spec, source, nil); err == nil {
		t.Fatal("stale base accepted for a new return")
	}
	saved, err := client.ReadPrivate(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "content.txt"), []byte("changed after preparation"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyWorksetReturn(t.Context(), returning, source, statePath); err == nil {
		t.Fatal("changed bytes reused original return state")
	}
	afterFailure, err := client.ReadPrivate(statePath)
	if err != nil || !bytes.Equal(saved, afterFailure) {
		t.Fatal("failed return discarded original state", err)
	}
	// The stored payload matches the page's schema/input shape rather than a new
	// private workflow protocol. Its record.json is real downloaded config bytes.
	var envelope map[string]json.RawMessage
	raw, err := os.ReadFile(filepath.Join(readSecond, "record.json"))
	if err != nil || json.Unmarshal(raw, &envelope) != nil {
		t.Fatal(err)
	}
	var payload client.ManagedWorkset
	if err := json.Unmarshal(envelope["payload"], &payload); err != nil || payload.Schema != client.ManagedWorksetSchema {
		t.Fatal(payload, err)
	}
}

func TestManagedWorksetCLIAndMCPShareOriginalReturn(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	_, agent := e.agent("workset-adapters@synthetic", identity.RoleOwner)
	v1 := e.ingest(agent.Context, "workset-adapters/source", []byte("original"), *rightsOwned())
	a := reopenApplication(t, e)
	servers := assetRemoteServers(t, a)
	origin := "http://" + servers.Addresses.API
	c, err := client.New(client.Config{BaseURL: origin, SessionToken: agent.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	root := t.TempDir()
	for _, name := range []string{"specs", "reports", "source", "record", "read", "adopt"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name string, value any) {
		t.Helper()
		if err := client.SaveState(filepath.Join(root, name), value); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string, value any) {
		t.Helper()
		if err := client.LoadState(filepath.Join(root, name), value); err != nil {
			t.Fatal(err)
		}
	}
	write("specs/session.json", map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": agent.Token})
	run := func(want int, args ...string) []byte {
		t.Helper()
		var out, stderr bytes.Buffer
		argv := append(args, "--server", origin, "--allow-http", "--session-file", filepath.Join(root, "specs/session.json"))
		if exit := cli.Run(t.Context(), argv, &out, &stderr); exit != want {
			t.Fatalf("CLI %v exit%d want%d: %s", args, exit, want, stderr.String())
		}
		return out.Bytes()
	}
	server, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "workset-adapter-selftest", Version: "1"}, nil).Connect(t.Context(), ct, nil)
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
	entry := client.WorksetEntry{AssetID: string(v1.AssetID), VersionID: string(v1.VersionID), Destination: "scene"}
	spec := client.WorksetRecordSpec{ProjectID: string(e.project.ProjectID), Title: "Adapter fixed workset", Input: client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{entry}, Entrypoints: []string{"scene/content.txt"}}}
	write("specs/record.json", spec)
	var cliPrepared client.PreparedManagementRecord
	if err := json.Unmarshal(run(0, "asset", "workset", "record-prepare", "--input", filepath.Join(root, "specs/record.json"), "--output", filepath.Join(root, "reports/prepared.json")), &cliPrepared); err != nil {
		t.Fatal(err)
	}
	summary := call("workset_record_prepare", map[string]any{"input_file": "specs/record.json", "output_file": "reports/mcp-prepared.json"}, false)
	if summary["record_sha256"] != cliPrepared.SHA256 {
		t.Fatal("CLI/MCP record bytes differ", summary)
	}
	call("workset_record_save", map[string]any{"input_file": "reports/prepared.json", "directory": "record", "state_file": "record.state.json", "output_file": "reports/saved.json"}, false)
	var record apiv1.CommitResult
	read("reports/saved.json", &record)
	write("specs/ref.json", map[string]string{"asset_id": record.AssetId, "version_id": record.VersionId})
	run(0, "asset", "workset", "open", "--input", filepath.Join(root, "specs/ref.json"), "--directory", filepath.Join(root, "read"), "--output", filepath.Join(root, "reports/opened.json"))
	if err := a.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	listed := call("workset_record_list", map[string]any{"project_id": e.project.ProjectID, "limit": 1}, false)
	if len(listed["items"].([]any)) != 1 {
		t.Fatal(listed)
	}
	if err := os.WriteFile(filepath.Join(root, "source/content.txt"), []byte("actual adapter returned bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	returnSpec := client.WorksetReturnSpec{Entry: entry, Files: []client.InputFile{{Path: "content.txt", Role: "primary"}}, Purpose: "production", Usage: "production", Note: "explicit adapter return"}
	write("specs/return.json", returnSpec)
	prepArgs := map[string]any{"input_file": "specs/return.json", "directory": "source", "output_file": "reports/return.json"}
	first := call("workset_return_prepare", prepArgs, false)
	again := call("workset_return_prepare", prepArgs, false)
	if first["receipt_sha256"] != again["receipt_sha256"] {
		t.Fatal("preparation replaced original keys")
	}
	run(0, "asset", "workset", "return-apply", "--input", filepath.Join(root, "reports/return.json"), "--directory", filepath.Join(root, "source"), "--state", filepath.Join(root, "return.state.json"), "--output", filepath.Join(root, "reports/returned.json"))
	var receipt apiv1.CommitResult
	read("reports/returned.json", &receipt)
	replayed := call("workset_return_apply", map[string]any{"input_file": "reports/return.json", "directory": "source", "state_file": "return.state.json", "output_file": "reports/replayed.json"}, false)
	if replayed["version_id"] != receipt.VersionId || replayed["operation_id"] != receipt.OperationId {
		t.Fatal("cross-client replay created new return", replayed)
	}
	var prepared client.WorksetReturn
	read("reports/return.json", &prepared)
	write("specs/adoption.json", client.WorksetAdoption{RecordAssetID: record.AssetId, RecordVersionID: record.VersionId, Return: prepared, Receipt: receipt})
	call("workset_adopt_prepare", map[string]any{"input_file": "specs/adoption.json", "directory": "read", "output_file": "reports/adopted.json"}, false)
	run(0, "asset", "workset", "adopt-prepare", "--input", filepath.Join(root, "specs/adoption.json"), "--directory", filepath.Join(root, "read"), "--output", filepath.Join(root, "reports/cli-adopted.json"))
	var mcpAdopted, cliAdopted client.PreparedManagementRecord
	read("reports/adopted.json", &mcpAdopted)
	read("reports/cli-adopted.json", &cliAdopted)
	if mcpAdopted.SHA256 != cliAdopted.SHA256 {
		t.Fatal("cross-client adoption differs")
	}
	// Output/state overlap is rejected before a committed original state changes.
	stateBefore, err := client.ReadPrivate(filepath.Join(root, "return.state.json"))
	if err != nil {
		t.Fatal(err)
	}
	call("workset_return_apply", map[string]any{"input_file": "reports/return.json", "directory": "source", "state_file": "return.state.json", "output_file": "return.state.json"}, true)
	stateAfter, err := client.ReadPrivate(filepath.Join(root, "return.state.json"))
	if err != nil || !bytes.Equal(stateBefore, stateAfter) {
		t.Fatal("output overwrote original state", err)
	}
	// Tightening current source purpose revokes adoption of a saved receipt.
	rights := call("rights_read", map[string]any{"asset_id": entry.AssetID, "version_id": entry.VersionID}, false)
	evidence := call("rights_evidence", map[string]any{"idempotency_key": "workset-evidence", "request": map[string]any{"subject": rights["subject"], "manifest_digest": rights["manifest_digest"], "expected_revision": rights["evidence_revision"], "evidence": map[string]any{"note": "Synthetic tighter purpose", "external_inputs": []any{}}}}, false)
	rights = call("rights_read", map[string]any{"asset_id": entry.AssetID, "version_id": entry.VersionID}, false)
	call("rights_assert", map[string]any{"idempotency_key": "workset-restrict", "request": map[string]any{"subject": rights["subject"], "manifest_digest": rights["manifest_digest"], "expected_revision": rights["revision"], "kind": "restrict", "fields": map[string]any{"usage": "reference"}, "evidence_ids": []any{evidence["record_id"]}, "reason": "Synthetic production revoked"}}, false)
	call("workset_adopt_prepare", map[string]any{"input_file": "specs/adoption.json", "directory": "read", "output_file": "reports/denied-adoption.json"}, true)
	call("workset_return_apply", map[string]any{"input_file": "reports/return.json", "directory": "source", "state_file": "return.state.json", "output_file": "reports/denied-return.json"}, true)
	if _, err := os.Stat(filepath.Join(root, "reports/denied-adoption.json")); !os.IsNotExist(err) {
		t.Fatal("denied adoption emitted preparation", err)
	}
}
