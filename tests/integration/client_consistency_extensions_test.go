package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/extensions/exttest"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// TEST-M2-11: executable CLI and stdio MCP obey the same explicitly installed
// command boundaries. These process observations apply to the tested platform;
// the one-shot host is not a sandbox.
func TestClientConsistencyExtensionBoundaries(t *testing.T) {
	bin := buildConsistencyCLI(t)
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	app := reopenApplication(t, e)
	f := &appFlow{flowEnv: &flowEnv{env: e, owner: e.login().Context}, app: app}
	dir := t.TempDir()
	pkg := exttest.Write(t, dir, exttest.Spec{ID: "org.example.clientmatrix", CLI: true, Mutate: func(doc map[string]any) {
		doc["contributes"].([]map[string]any)[0]["id"] = "org.example.clientmatrix.whoami"
	}})
	upload, inputs := f.uploadFiles(f.owner, readDir(t, dir))
	v, err := f.catalog.CommitVersion(t.Context(), catalog.VersionRequest{Who: f.owner, IdempotencyKey: f.key(), UploadID: upload, Slug: "plugins/clientmatrix", Content: catalog.ContentInput{AssetType: manifest.TypePlugin, Rights: rightsOwned(), Files: inputs, Metadata: map[string]any{"extension_id": pkg.Manifest.ID, "extension_version": pkg.Manifest.Version}}})
	if err != nil {
		t.Fatal(err)
	}
	// Pre-approved synthetic review fixture. This test does not certify review;
	// the real review path has TestM2ExtensionPackageReviewEnableCheckAndRevoke.
	if _, err = f.inst.DB(ownership.Ledger).ExecContext(t.Context(), `UPDATE ledger_version_states SET state='approved',effective_review_id=? WHERE version_id=?`, ids.New(), v.VersionID); err != nil {
		t.Fatal(err)
	}
	servers := remoteServers(t, app, true)
	login := e.login()
	h := newConsistencyClient(t, bin, "http://"+servers.Addresses.API, login.Token)
	work, registry := t.TempDir(), filepath.Join(t.TempDir(), "extensions.json")
	out, code := h.cli(t, "plugin", "import", "--input", writeTemp(t, map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID}), "--idempotency-key", "clientmatrix-import")
	if code != 0 || out["package_digest"] != string(pkg.Digest) {
		t.Fatal("static import", code, out)
	}
	t.Run("install_requires_server_enablement", func(t *testing.T) {
		out, code := h.cli(t, "ext", "install", "--package", dir, "--registry", registry)
		if code == 0 || out["error"] == nil {
			t.Fatal("unenabled package was installed", code, out)
		}
	})
	m := app.ExtensionManager
	raw, _ := json.Marshal(map[string]any{})
	req := extensions.EnableRequest{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Target: "cli", ScopeKind: "user", ScopeID: login.Context.PrincipalID, Config: raw, ConfigRevision: 1, Trust: extensions.TrustUnenforced, Reason: "synthetic client boundary validation"}
	action, err := m.EnableHumanAction(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	who, grant, child := f.grant(action)
	enabled, err := m.Enable(t.Context(), who, req, grant, child, f.id)
	if err != nil {
		t.Fatal(err)
	}
	if out, code = h.cli(t, "ext", "install", "--package", dir, "--registry", registry); code != 0 {
		t.Fatal("explicit install", code, out)
	}
	input := filepath.Join(work, "note.txt")
	if err = os.WriteFile(input, []byte("synthetic input"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(work, "unregistered-executed")
	fakePath := t.TempDir()
	for _, name := range []string{"whoami", "matrix-unregistered"} {
		for _, parent := range []string{fakePath, work} {
			exttest.CopyFile(t, exttest.Fixture(t), filepath.Join(parent, name+filepath.Ext(exttest.EntryPath())))
		}
	}
	h.env = []string{"PATH=" + fakePath + string(os.PathListSeparator) + os.Getenv("PATH"), "LANTAI_SESSION_TOKEN=" + login.Token, "CLIENT_TEST_SECRET=synthetic-private-value", "FIXTURE_HEARTBEAT=" + marker}
	h.directory = work
	projected := h.connectMCP(t, "--workspace", work, "--extensions-registry", registry)
	toolName := "ext_org_example_clientmatrix_whoami"
	t.Run("core_name_and_annotations_do_not_grant_authority", func(t *testing.T) {
		value, code := h.cli(t, "whoami")
		if code != 0 || value["principal"] == nil {
			t.Fatal("extension shadowed the core command", code, value)
		}
		tools, err := projected.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var found *mcp.Tool
		for _, tool := range tools.Tools {
			if tool.Name == toolName {
				found = tool
			}
		}
		if found == nil || found.Annotations == nil || found.Annotations.ReadOnlyHint {
			t.Fatal("local command was mis-annotated", found)
		}
		for _, name := range []string{"human_execute", "plugin_enable", "trash_purge", "shell"} {
			res, err := projected.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"readOnlyHint": true}})
			if err == nil && !res.IsError {
				t.Fatal("annotation granted a sensitive tool", name)
			}
		}
	})
	t.Run("unregistered_path_and_cwd_never_execute", func(t *testing.T) {
		if _, code := h.cli(t, "matrix-unregistered"); code == 0 {
			t.Fatal("unknown core command ran")
		}
		if _, code := h.cli(t, "ext", "run", "org.example.missing", "whoami", "--registry", registry, "--output", t.TempDir()); code == 0 {
			t.Fatal("unregistered extension command ran")
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("PATH/CWD executable ran", err)
		}
	})
	t.Run("verbatim_arguments_and_private_environment", func(t *testing.T) {
		shellMarker := filepath.Join(work, "shell-expanded")
		argv := []string{"inspect", "$(touch " + shellMarker + ")", "; touch " + shellMarker, "`touch " + shellMarker + "`", "$HOME", "space and 中文"}
		cliOutput := t.TempDir()
		args := []string{"ext", "run", pkg.Manifest.ID, "whoami", "--registry", registry, "--input-file", input, "--output", cliOutput}
		for _, arg := range argv {
			args = append(args, "--arg", arg)
		}
		cliValue, code := h.cli(t, args...)
		if code != 0 || cliValue["outcome"] != "completed" {
			t.Fatal("CLI extension execution", code, cliValue)
		}
		mcpOutput := filepath.Join(work, "mcp-out")
		if err := os.Mkdir(mcpOutput, 0o700); err != nil {
			t.Fatal(err)
		}
		res, err := projected.CallTool(t.Context(), &mcp.CallToolParams{Name: toolName, Arguments: map[string]any{"inputs": []string{"note.txt"}, "args": argv, "output": "mcp-out"}})
		if err != nil || res.IsError {
			t.Fatal("MCP extension execution", err)
		}
		mcpValue := consistencyJSON(t, mustJSON(res.StructuredContent))
		for _, field := range []string{"summary", "files", "outcome"} {
			if !bytes.Equal(mustJSON(cliValue[field]), mustJSON(mcpValue[field])) {
				t.Fatal("extension CLI/MCP disagree", field)
			}
		}
		summary := cliValue["summary"].(map[string]any)
		if !bytes.Equal(mustJSON(summary["args"]), mustJSON(argv)) || summary["private_working_directory"] != true {
			t.Fatal("arguments or working directory escaped the file protocol", summary)
		}
		for _, item := range summary["environment_names"].([]any) {
			name := item.(string)
			if name == "PATH" || name == "HOME" || strings.Contains(name, "TOKEN") || name == "CLIENT_TEST_SECRET" || name == "FIXTURE_HEARTBEAT" {
				t.Fatal("host environment leaked to the child", name)
			}
		}
		for _, dir := range []string{cliOutput, mcpOutput} {
			if b, err := os.ReadFile(filepath.Join(dir, "copies", "note.txt")); err != nil || string(b) != "SYNTHETIC INPUT" {
				t.Fatal("explicit input/output mismatch", err)
			}
		}
		for _, path := range []string{marker, shellMarker} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("a shell expression or inherited variable was executed", err)
			}
		}
	})
	assertRefused := func(t *testing.T, expected errcode.Code) {
		t.Helper()
		cliOutput := t.TempDir()
		value, code := h.cli(t, "ext", "run", pkg.Manifest.ID, "whoami", "--registry", registry, "--output", cliOutput)
		body, ok := value["error"].(map[string]any)
		if code == 0 || !ok || body["code"] != string(expected) {
			t.Fatal("CLI accepted forbidden execution", code, value)
		}
		output, err := os.MkdirTemp(work, "refused-")
		if err != nil {
			t.Fatal(err)
		}
		res, err := projected.CallTool(t.Context(), &mcp.CallToolParams{Name: toolName, Arguments: map[string]any{"inputs": []string{}, "args": []string{}, "output": filepath.Base(output)}})
		if err != nil || !res.IsError {
			t.Fatal("MCP accepted forbidden execution", err)
		}
		mcpValue := consistencyJSON(t, mustJSON(res.StructuredContent))
		if mcpValue["error"].(map[string]any)["code"] != string(expected) {
			t.Fatal("MCP changed the refusal code", mcpValue)
		}
		for _, dir := range []string{cliOutput, output} {
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("refused execution produced output", err)
			}
		}
	}
	t.Run("changed_entry_digest_is_rejected", func(t *testing.T) {
		entry := filepath.Join(dir, filepath.FromSlash(exttest.EntryPath()))
		original, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.WriteFile(entry, original, 0o755); err != nil {
				t.Error(err)
			}
		})
		if err := os.WriteFile(entry, append(slices.Clone(original), 0), 0o755); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, errcode.HashMismatch)
	})
	t.Run("revocation_rechecks_existing_projection", func(t *testing.T) {
		dreq := extensions.DisableRequest{EnablementID: enabled.Enablement.ID, Mode: "revoke", Reason: "synthetic validation"}
		action, err := m.DisableHumanAction(t.Context(), dreq)
		if err != nil {
			t.Fatal(err)
		}
		who, grant, child := f.grant(action)
		if _, err := m.Disable(t.Context(), who, dreq, grant, child, f.id); err != nil {
			t.Fatal(err)
		}
		assertRefused(t, errcode.ExtensionActivationStale)
	})
}
