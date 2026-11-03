package integration

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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

func TestAgentBoundedAssetDerivationsActualBytes(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	principal, agent := e.agent("asset-derivations@synthetic", identity.RoleOwner)
	rights := rightsOwned()
	rights.RedistributeRaw = true
	before := e.ingest(agent.Context, "derivations/source", []byte("before bytes"), *rights)
	webp, err := os.ReadFile("../fixtures/asset-derivations/synthetic-red.webp")
	if err != nil {
		t.Fatal(err)
	}
	image := e.ingest(agent.Context, "derivations/image", webp, *rights)
	object := e.ingest(agent.Context, "derivations/obj", []byte("mtllib ../../outside.mtl\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"), *rights)
	app := reopenApplication(t, e)
	servers := assetRemoteServers(t, app)
	origin := "http://" + servers.Addresses.API
	c, err := client.New(client.Config{BaseURL: origin, SessionToken: agent.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	root := t.TempDir()
	for _, name := range []string{"specs", "reports", "artifacts", "source", "image-cache", "obj-cache", "compare-cache", "tar-cache"} {
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
	for path, value := range map[string]string{"content.txt": "after bytes", "added.txt": "new bytes"} {
		if err := os.WriteFile(filepath.Join(root, "source", path), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rightsRaw, _ := json.Marshal(rights)
	response, err := c.Push(t.Context(), client.PushInput{ProjectID: string(e.project.ProjectID), AssetID: string(before.AssetID), BaseVersionID: string(before.VersionID), Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "content.txt", Role: "source"}, {Path: "added.txt", Role: "source"}}, Rights: rightsRaw}}, filepath.Join(root, "source"), filepath.Join(root, "version.state.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	var after apiv1.CommitResult
	if err := response.Decode(&after); err != nil {
		t.Fatal(err)
	}
	s, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "derivation-selftest", Version: "1"}, nil).Connect(t.Context(), ct, nil)
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
	derive := func(key, directory string, input client.DerivationInput, wantError bool) map[string]any {
		t.Helper()
		write("specs/"+key+".json", input)
		return call("asset_derive", map[string]any{"input_file": "specs/" + key + ".json", "directory": directory, "artifact_file": "artifacts/" + key, "output_file": "reports/" + key + ".json"}, wantError)
	}
	imageSpec := client.DerivationInput{Schema: client.DerivationInputSchema, Action: "thumbnail", Purpose: "archive_review", Source: &client.DerivationSource{FixedVersionRef: client.FixedVersionRef{AssetID: string(image.AssetID), VersionID: string(image.VersionID)}, Path: "content.txt"}, Edge: 320}
	mcpImage := derive("image.png", "image-cache", imageSpec, false)
	if mcpImage["current_remote_verified"] != true || mcpImage["source_retirement_authorized"] != false {
		t.Fatal(mcpImage)
	}
	// Exact same input and private output rechecks live grants rather than replacing bytes.
	derive("image.png", "image-cache", imageSpec, false)
	write("specs/session.json", map[string]any{"schema": "lantai.client-session/v1", "origin": origin, "token": agent.Token})
	var out, stderr bytes.Buffer
	args := []string{"asset", "derive", "--input", filepath.Join(root, "specs/image.png.json"), "--directory", filepath.Join(root, "image-cache"), "--artifact", filepath.Join(root, "artifacts/cli.png"), "--output", filepath.Join(root, "reports/cli-image.json"), "--server", origin, "--allow-http", "--session-file", filepath.Join(root, "specs/session.json")}
	if exit := cli.Run(t.Context(), args, &out, &stderr); exit != 0 {
		t.Fatal(exit, stderr.String())
	}
	var cliReceipt, mcpReceipt client.DerivationReceipt
	if err := json.Unmarshal(out.Bytes(), &cliReceipt); err != nil {
		t.Fatal(err)
	}
	read("reports/image.png.json", &mcpReceipt)
	if cliReceipt.Artifact != mcpReceipt.Artifact {
		t.Fatal("CLI/MCP actual PNG differs", cliReceipt.Artifact, mcpReceipt.Artifact)
	}
	actualPNG, err := os.ReadFile(filepath.Join(root, "artifacts/image.png"))
	if err != nil || shaOf(actualPNG) != mcpReceipt.Artifact.SHA256 || int64(len(actualPNG)) != mcpReceipt.Artifact.Size {
		t.Fatal("artifact receipt not actual bytes", err)
	}
	objectSpec := client.DerivationInput{Schema: client.DerivationInputSchema, Action: "obj_wireframe", Purpose: "archive_review", Source: &client.DerivationSource{FixedVersionRef: client.FixedVersionRef{AssetID: string(object.AssetID), VersionID: string(object.VersionID)}, Path: "content.txt"}, Edge: 320}
	derive("object.svg", "obj-cache", objectSpec, false)
	svg, err := os.ReadFile(filepath.Join(root, "artifacts/object.svg"))
	if err != nil || !bytes.Contains(svg, []byte("<path")) || bytes.Contains(svg, []byte("outside")) {
		t.Fatal("OBJ source link loaded or embedded", err)
	}
	objectSpec.Action = "obj_structure"
	derive("object.json", "obj-cache", objectSpec, false)
	objectPage := call("batch_report_page", map[string]any{"file": "artifacts/object.json", "section": "edges", "limit": 2}, false)
	if objectPage["total_items"] != float64(3) || objectPage["complete"] != false || objectPage["current_remote_verified"] != false {
		t.Fatal("OBJ structure cannot be read in bounded pages", objectPage)
	}
	comparison := client.DerivationInput{Schema: client.DerivationInputSchema, Action: "compare", Purpose: "archive_review", Before: &client.FixedVersionRef{AssetID: string(before.AssetID), VersionID: string(before.VersionID)}, After: &client.FixedVersionRef{AssetID: after.AssetId, VersionID: after.VersionId}}
	derive("comparison.json", "compare-cache", comparison, false)
	var diff client.VersionComparison
	raw, err := os.ReadFile(filepath.Join(root, "artifacts/comparison.json"))
	if err != nil || json.Unmarshal(raw, &diff) != nil || len(diff.Files) != 2 || diff.Files[0].Kind != "added" || diff.Files[1].Kind != "changed" {
		t.Fatal(diff, err)
	}
	comparisonPage := call("batch_report_page", map[string]any{"file": "artifacts/comparison.json", "limit": 1}, false)
	if comparisonPage["total_items"] != float64(2) || comparisonPage["items"].([]any)[0].(map[string]any)["kind"] != "added" {
		t.Fatal("comparison artifact cannot be read", comparisonPage)
	}
	fieldsPage := call("batch_report_page", map[string]any{"file": "artifacts/comparison.json", "section": "fields", "limit": 20}, false)
	if bytes.Contains(mustJSON(fieldsPage), []byte("LicenseRef-Owned")) {
		t.Fatal("full raw fields leaked into metadata page")
	}
	workset := client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: after.AssetId, VersionID: after.VersionId, Destination: "工作集"}}}
	archive := client.DerivationInput{Schema: client.DerivationInputSchema, Action: "workset_tar", Purpose: "raw_export", Workset: &workset}
	if err := os.WriteFile(filepath.Join(root, "tar-cache/unselected.txt"), []byte("never export unselected file"), 0600); err != nil {
		t.Fatal(err)
	}
	derive("workset.tar", "tar-cache", archive, false)
	tarFile, err := os.Open(filepath.Join(root, "artifacts/workset.tar"))
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(tarFile)
	found := map[string]string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg || header.Mode != 0600 {
			t.Fatal("archive changed file kind/mode", header)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		found[header.Name] = string(data)
	}
	_ = tarFile.Close()
	if len(found) != 2 || found["工作集/content.txt"] != "after bytes" || found["工作集/added.txt"] != "new bytes" {
		t.Fatal("archive included unselected/wrong bytes", found)
	}
	page := call("batch_report_page", map[string]any{"file": "reports/workset.tar.json", "limit": 1}, false)
	if page["current_remote_verified"] != false || page["historical_local_observation"] != true {
		t.Fatal("historical derivation became current authority", page)
	}
	// No fallback from a rejected raw-export purpose to archive review.
	view := call("rights_read", map[string]any{"asset_id": after.AssetId, "version_id": after.VersionId}, false)
	evidence := call("rights_evidence", map[string]any{"idempotency_key": "derived-rights-evidence", "request": map[string]any{"subject": view["subject"], "manifest_digest": view["manifest_digest"], "expected_revision": view["evidence_revision"], "evidence": map[string]any{"note": "Synthetic raw export revoked", "external_inputs": []any{}}}}, false)
	view = call("rights_read", map[string]any{"asset_id": after.AssetId, "version_id": after.VersionId}, false)
	call("rights_assert", map[string]any{"idempotency_key": "derived-rights-restrict", "request": map[string]any{"subject": view["subject"], "manifest_digest": view["manifest_digest"], "expected_revision": view["revision"], "kind": "restrict", "fields": map[string]any{"redistribute_raw": false}, "evidence_ids": []any{evidence["record_id"]}, "reason": "Synthetic raw export revoked"}}, false)
	denied := derive("denied.tar", "tar-cache", archive, true)
	if !strings.Contains(string(mustJSON(denied)), "USE_RESTRICTED") {
		t.Fatal("raw export denied for unexpected reason", denied)
	}
	if _, err := os.Stat(filepath.Join(root, "artifacts/denied.tar")); !os.IsNotExist(err) {
		t.Fatal("denied raw export published artifact", err)
	}
	e.setRole(principal.ID, identity.RoleOwner, false)
	derive("revoked.png", "image-cache", imageSpec, true)
	if _, err := os.Stat(filepath.Join(root, "artifacts/revoked.png")); !os.IsNotExist(err) {
		t.Fatal("revoked source reused cached bytes", err)
	}
}
