package integration

import (
	"bytes"
	"context"
	"encoding/base64"
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
	"github.com/oujinhaoai/lantai/internal/contract/managementrecord"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestAgentManagementClients(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	principal, session := e.agent("management@synthetic", identity.RoleOwner)
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
	for _, name := range []string{"source", "read", "revision", "revision-read"} {
		if err := os.Mkdir(filepath.Join(work, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	sessionPath := filepath.Join(work, "session.json")
	writeJSON(t, sessionPath, map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": session.Token})
	cliCall := func(args ...string) map[string]any {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args = append(args, "--server", origin, "--allow-http", "--session-file", sessionPath)
		if code := cli.Run(t.Context(), args, &stdout, &stderr); code != 0 {
			t.Fatal(code, stderr.String())
		}
		return consistencyJSON(t, stdout.Bytes())
	}
	spec := client.ManagementRecordSpec{ProjectID: string(e.project.ProjectID), Kind: "assistant", Title: "Synthetic management record", Payload: json.RawMessage(`{"original":{"operation_id":"original-operation","commit_key":"original-key"},"state":"unknown","extra":{"retained":42}}`)}
	specPath, preparedPath, state := filepath.Join(work, "spec.json"), filepath.Join(work, "prepared.json"), filepath.Join(work, "push-state.json")
	writeJSON(t, specPath, spec)
	cliCall("management", "prepare", "--input", specPath, "--output", preparedPath)
	var prepared client.PreparedManagementRecord
	if err := client.LoadState(preparedPath, &prepared); err != nil {
		t.Fatal(err)
	}
	first := cliCall("management", "save", "--input", preparedPath, "--directory", filepath.Join(work, "source"), "--state", state)
	replayed := cliCall("management", "save", "--input", preparedPath, "--directory", filepath.Join(work, "source"), "--state", state)
	if first["asset_id"] != replayed["asset_id"] || first["version_id"] != replayed["version_id"] {
		t.Fatal("resume created a second record", first, replayed)
	}
	asset, version := first["asset_id"].(string), first["version_id"].(string)
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "management-acceptance", Version: "1"}, nil).Connect(t.Context(), ct, nil)
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
	// Search is an asynchronous projection; explicitly advance the synthetic
	// fixture rather than assuming commit visibility implies index visibility.
	if err := app.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	list := call("management_list", map[string]any{"project_id": e.project.ProjectID, "kind": "assistant"}, false)
	if !bytes.Contains(mustJSON(list), []byte(asset)) {
		t.Fatal("record not discoverable", list)
	}
	read := call("management_read", map[string]any{"asset_id": asset, "version_id": version, "directory": "read"}, false)
	if read["sha256"] != prepared.SHA256 || read["size"] != float64(prepared.Size) || !bytes.Contains(mustJSON(read), []byte("original-key")) {
		t.Fatal("lost exact record evidence", read)
	}
	var actual []byte
	offset := 0
	for {
		chunk := call("management_read_chunk", map[string]any{"asset_id": asset, "version_id": version, "directory": "read", "offset": offset, "max_bytes": 31, "expected_sha256": prepared.SHA256}, false)
		part, err := base64.StdEncoding.DecodeString(chunk["data"].(string))
		if err != nil {
			t.Fatal(err)
		}
		actual = append(actual, part...)
		if chunk["complete"] == true {
			break
		}
		offset = int(chunk["next_offset"].(float64))
	}
	want, err := managementrecord.Render(prepared.Envelope)
	if err != nil || !bytes.Equal(actual, want) || shaOf(actual) != prepared.SHA256 {
		t.Fatal("record chunks lost bytes", err)
	}
	// Revisions bind the original record's kind/project and exact baseline.
	spec.AssetID, spec.BaseVersionID = asset, version
	spec.Payload = json.RawMessage(`{"original":{"operation_id":"original-operation","commit_key":"original-key"},"state":"reconciling"}`)
	writeJSON(t, filepath.Join(work, "revision-spec.json"), spec)
	call("management_prepare", map[string]any{"spec_file": "revision-spec.json", "prepared_file": "revision-prepared.json"}, false)
	revision := call("management_revise", map[string]any{"prepared_file": "revision-prepared.json", "directory": "revision", "state": "revision-state.json"}, false)
	if revision["asset_id"] != asset || revision["version_id"] == version {
		t.Fatal("revision lost original asset", revision)
	}
	cliCall("management", "read", "--asset", asset, "--version", revision["version_id"].(string), "--directory", filepath.Join(work, "revision-read"))
	spec.Kind = "migration"
	if _, err := c.PrepareManagementRecord(t.Context(), spec); err == nil {
		t.Fatal("cross-kind revision accepted")
	}
	// An oversized whole record is refused without truncation; bounded chunks
	// still return all exact bytes under current authority.
	for _, name := range []string{"large-source", "large-read"} {
		if err := os.Mkdir(filepath.Join(work, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	largeSpec := client.ManagementRecordSpec{ProjectID: string(e.project.ProjectID), Kind: "assistant", Title: "Large synthetic record", Payload: json.RawMessage(`{"large":"` + strings.Repeat("x", 80<<10) + `"}`)}
	largePrepared, err := c.PrepareManagementRecord(t.Context(), largeSpec)
	if err != nil {
		t.Fatal(err)
	}
	largeReceipt, err := c.SaveManagementRecord(t.Context(), largePrepared, filepath.Join(work, "large-source"), filepath.Join(work, "large-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var largeRef struct {
		AssetID   string `json:"asset_id"`
		VersionID string `json:"version_id"`
	}
	if err := largeReceipt.Decode(&largeRef); err != nil {
		t.Fatal(err)
	}
	call("management_read", map[string]any{"asset_id": largeRef.AssetID, "version_id": largeRef.VersionID, "directory": "large-read"}, true)
	var largeBytes []byte
	for offset := 0; ; {
		chunk := call("management_read_chunk", map[string]any{"asset_id": largeRef.AssetID, "version_id": largeRef.VersionID, "directory": "large-read", "offset": offset, "max_bytes": 8192, "expected_sha256": largePrepared.SHA256}, false)
		part, err := base64.StdEncoding.DecodeString(chunk["data"].(string))
		if err != nil {
			t.Fatal(err)
		}
		largeBytes = append(largeBytes, part...)
		if chunk["complete"] == true {
			break
		}
		offset = int(chunk["next_offset"].(float64))
	}
	if int64(len(largeBytes)) != largePrepared.Size || shaOf(largeBytes) != largePrepared.SHA256 {
		t.Fatal("large chunks lost bytes")
	}
	// A saved import report is historical input, including unresolved rows.
	// The builder must not count only the rows with successful receipts.
	importSource := t.TempDir()
	selection := client.ImportInput{Schema: client.ImportInputSchema}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("item-%d", i)
		dir := filepath.Join(importSource, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "record.json"), want, 0600); err != nil {
			t.Fatal(err)
		}
		input := prepared.Input
		input.Slug = "batch-source/" + name
		selection.Assets = append(selection.Assets, client.ImportAsset{Key: name, Directory: name, Classification: "unknown", Push: input})
	}
	plan, err := client.PrepareImport(t.Context(), selection, importSource)
	if err != nil {
		t.Fatal(err)
	}
	report, err := c.ApplyImport(t.Context(), plan, importSource, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a saved partial report, preserving original identity and keys.
	report.Complete = false
	report.Results[1].Status = "description_pending"
	for i, status := range []string{"unknown", "failed", "not_attempted"} {
		report.Results[i+2] = client.ImportResult{Key: report.Results[i+2].Key, Status: status}
	}
	checklist := map[string]bool{}
	for _, key := range []string{"excluded_backed_up", "working_inputs_backed_up", "entrypoints_opened", "new_results_captured", "fallback_ready", "selected_notes_reviewed"} {
		checklist[key] = false
	}
	batch := managementrecord.MigrationBatch{Schema: managementrecord.BatchContract, BatchID: "synthetic-batch-1", Revision: 1, ProjectID: e.project.ProjectID, InstanceID: prepared.Envelope.InstanceID, Title: "Synthetic migration", OwnerID: e.admin.Context.PrincipalID, LastAction: managementrecord.BatchAction{ActorID: principal.ID, At: e.clk.Now().UTC().Format(time.RFC3339Nano), Action: "create", Note: "Retain original inputs; incomplete report remains blocked."}, Stage: "inventory", Acceptance: "requested", SourceRetained: true, IntendedPurpose: "archive_review", ReferenceScope: "none", Evidence: managementrecord.BatchEvidence{Assets: []managementrecord.BatchAsset{}, Worksets: []managementrecord.Ref{}, Verifications: []managementrecord.Ref{}, Recovery: []managementrecord.Ref{}, ReferenceJobs: []managementrecord.BatchReferenceJob{}}, Checklist: checklist}
	draft := client.MigrationBatchDraft{Batch: batch, Plan: &plan, Report: &report}
	writeJSON(t, filepath.Join(work, "batch-draft.json"), draft)
	cliCall("management", "batch-create", "--input", filepath.Join(work, "batch-draft.json"), "--output", filepath.Join(work, "batch-prepared.json"))
	var batchPrepared client.PreparedManagementRecord
	if err := client.LoadState(filepath.Join(work, "batch-prepared.json"), &batchPrepared); err != nil {
		t.Fatal(err)
	}
	built, err := managementrecord.DecodeBatch(batchPrepared.Envelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if built.ImportSummary != (managementrecord.BatchSummary{Planned: 5, Committed: 1, DescriptionPending: 1, Unknown: 1, Failed: 1, NotAttempted: 1}) || len(built.Evidence.Assets) != 2 {
		t.Fatal("batch omitted unresolved planned rows", built)
	}
	for _, name := range []string{"batch-source", "batch-read", "batch-revised"} {
		if err := os.Mkdir(filepath.Join(work, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	batchReceipt := call("management_save", map[string]any{"prepared_file": "batch-prepared.json", "directory": "batch-source", "state": "batch-state.json"}, false)
	checked := call("management_check", map[string]any{"asset_id": batchReceipt["asset_id"], "version_id": batchReceipt["version_id"]}, false)
	if checked["acceptance"] != "blocked" {
		t.Fatal("incomplete batch incorrectly accepted", checked)
	}
	call("management_batch_revise", map[string]any{"draft_file": "batch-draft.json", "prepared_file": "batch-revised-prepared.json", "asset_id": batchReceipt["asset_id"], "version_id": batchReceipt["version_id"], "directory": "batch-read"}, false)
	var batchNext client.PreparedManagementRecord
	if err := client.LoadState(filepath.Join(work, "batch-revised-prepared.json"), &batchNext); err != nil {
		t.Fatal(err)
	}
	next, err := managementrecord.DecodeBatch(batchNext.Envelope.Payload)
	if err != nil || next.Revision != 2 || next.PreviousRef == nil || string(next.PreviousRef.VersionID) != batchReceipt["version_id"] || next.Acceptance != "not_requested" {
		t.Fatal("revision continuity or acceptance reset lost", next, err)
	}
	call("management_revise", map[string]any{"prepared_file": "batch-revised-prepared.json", "directory": "batch-revised", "state": "batch-revised-state.json"}, false)
	e.setRole(principal.ID, identity.RoleOwner, false)
	denied := call("management_read_chunk", map[string]any{"asset_id": asset, "version_id": version, "directory": "read", "offset": 31, "expected_sha256": prepared.SHA256}, true)
	if !strings.Contains(string(mustJSON(denied)), "FORBIDDEN") && !strings.Contains(string(mustJSON(denied)), "NOT_FOUND") {
		t.Fatal("local copy bypassed revoked authority", denied)
	}
}
