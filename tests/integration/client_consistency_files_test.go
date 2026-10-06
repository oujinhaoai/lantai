package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// TEST-M2-11: metadata and tool output carry exact references; file bytes travel
// through authorized HTTP transfers into explicit destinations. Python's minimal
// download API does not implement the Go client's resumable working-copy state.
func TestClientConsistencyFiles(t *testing.T) {
	bin := buildConsistencyCLI(t)
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	worker, session := e.agent("file-matrix@node", identity.RoleContributor)
	content := bytes.Repeat([]byte("synthetic file matrix content\n"), 8192)
	v := e.ingest(e.admin.Context, "file-matrix", content, *rightsOwned())
	app := reopenApplication(t, e)
	servers := remoteServers(t, app, true)
	h := newConsistencyClient(t, bin, "http://"+servers.Addresses.API, session.Token)
	work := t.TempDir()
	stdio := h.connectMCP(t, "--workspace", work)
	versionPath := "/api/v1/assets/" + string(v.AssetID) + "/versions/" + string(v.VersionID)
	full, status := consistencyFileHTTP(t, h, "GET", versionPath+"?view=full", nil)
	if status != 200 {
		t.Fatal("exact version fixture is not readable", status)
	}
	var prior client.DownloadGrant
	readGrant := func(t *testing.T) client.DownloadGrant {
		t.Helper()
		raw, status := consistencyFileHTTP(t, h, "POST", versionPath+"/read-grants", map[string]string{"path": "content.txt", "purpose": "archive_review"})
		var g client.DownloadGrant
		if status != 201 || json.Unmarshal(raw, &g) != nil || g.Path != "content.txt" || g.Size != int64(len(content)) || g.SHA256 != shaOf(content) {
			t.Fatal("read grant differs from the exact file", status)
		}
		return g
	}
	pull := func(t *testing.T, directory string) *mcp.CallToolResult {
		t.Helper()
		out, err := stdio.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_pull", Arguments: map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID, "purpose": "archive_review", "directory": directory}})
		if err != nil {
			t.Fatal("stdio pull", err)
		}
		return out
	}
	t.Run("exact_resource_link_and_bounded_metadata", func(t *testing.T) {
		out, err := stdio.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_read", Arguments: map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID}})
		if err != nil || out.IsError {
			t.Fatal("stdio exact resource", err)
		}
		want, err := v.Ref.URI()
		if err != nil {
			t.Fatal(err)
		}
		links := 0
		for _, item := range out.Content {
			switch value := item.(type) {
			case *mcp.ResourceLink:
				links++
				if value.URI != want {
					t.Fatal("MCP link lost the exact resource identity")
				}
			case *mcp.TextContent:
			default:
				t.Fatal("MCP inlined file content", item)
			}
		}
		if links != 1 {
			t.Fatal("exact version link missing or duplicated", links)
		}
		assertFileToolOutput(t, h, out, content)
		tools, err := stdio.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, tool := range tools.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		t.Logf("MCP protocol=%s tool_names=%v", stdio.InitializeResult().ProtocolVersion, names)
	})
	t.Run("four_clients_install_identical_bytes", func(t *testing.T) {
		prior = readGrant(t)
		raw, status := consistencyFileHTTP(t, h, "GET", prior.URL, nil)
		if status != 200 || !bytes.Equal(raw, content) {
			t.Fatal("REST exact transfer mismatch", status)
		}
		for _, adapter := range []string{"REST", "CLI", "MCP", "Python"} {
			directory := filepath.Join(work, adapter)
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			switch adapter {
			case "REST":
				if err := os.WriteFile(filepath.Join(directory, "content.txt"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			case "CLI":
				value, code := h.cli(t, "pull", "--asset", string(v.AssetID), "--version", string(v.VersionID), "--purpose", "archive_review", "--directory", directory)
				if code != 0 || !bytes.Equal(mustJSON(value), mustJSON(consistencyJSON(t, full))) {
					t.Fatal("CLI exact transfer or manifest mismatch", code)
				}
			case "MCP":
				out := pull(t, adapter)
				if out.IsError {
					t.Fatal("MCP exact transfer refused")
				}
				value := consistencyJSON(t, mustJSON(out.StructuredContent))
				if value["asset_id"] != string(v.AssetID) || value["version_id"] != string(v.VersionID) || value["directory"] != adapter {
					t.Fatal("MCP pull summary lost the exact destination or identity")
				}
				assertFileToolOutput(t, h, out, content)
			case "Python":
				value := consistencyFilePython(t, h, string(v.AssetID), string(v.VersionID), directory)
				if value["sha256"] != shaOf(content) || value["size"] != float64(len(content)) {
					t.Fatal("Python exact transfer mismatch", value)
				}
			}
			got, err := os.ReadFile(filepath.Join(directory, "content.txt"))
			if err != nil || !bytes.Equal(got, content) {
				t.Fatal(adapter, "installed different bytes", err)
			}
			t.Logf("%s size=%d sha256=%s", adapter, len(got), shaOf(got))
		}
	})
	t.Run("different_existing_destination_is_preserved", func(t *testing.T) {
		for _, adapter := range []string{"CLI", "MCP", "Python"} {
			directory := filepath.Join(work, "existing-"+adapter)
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "content.txt")
			original := []byte("existing synthetic user file")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			switch adapter {
			case "CLI":
				if _, code := h.cli(t, "pull", "--asset", string(v.AssetID), "--version", string(v.VersionID), "--purpose", "archive_review", "--directory", directory); code == 0 {
					t.Fatal("CLI replaced a different existing file")
				}
			case "MCP":
				if !pull(t, filepath.Base(directory)).IsError {
					t.Fatal("MCP replaced a different existing file")
				}
			case "Python":
				if consistencyFilePython(t, h, string(v.AssetID), string(v.VersionID), directory)["local_error"] != "FileExistsError" {
					t.Fatal("Python did not refuse a different existing file")
				}
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
				t.Fatal(adapter, "changed the existing file", err)
			}
		}
	})
	t.Run("workspace_destination_boundaries", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(work, "outside-link")); err != nil {
			t.Fatal("create boundary fixture", err)
		}
		for _, directory := range []string{"../outside", outside, "outside-link"} {
			out := pull(t, directory)
			value := consistencyJSON(t, mustJSON(out.StructuredContent))
			if !out.IsError || value["error"].(map[string]any)["code"] != string(errcode.SchemaInvalid) {
				t.Fatal("MCP accepted a destination outside its workspace")
			}
		}
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
			t.Fatal("workspace escape wrote outside files", err)
		}
	})
	t.Run("revocation_rechecks_clients_and_prior_transfer_url", func(t *testing.T) {
		e.setRole(worker.ID, identity.RoleContributor, false)
		raw, status := consistencyFileHTTP(t, h, "GET", versionPath+"?view=full", nil)
		if status != 404 {
			t.Fatal("revoked identity still sees the exact version", status)
		}
		want := consistencyJSON(t, raw)
		delete(want["error"].(map[string]any), "request_id")
		for _, adapter := range []string{"CLI", "MCP", "Python"} {
			directory := filepath.Join(work, "revoked-"+adapter)
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			switch adapter {
			case "CLI":
				var code int
				value, code = h.cli(t, "pull", "--asset", string(v.AssetID), "--version", string(v.VersionID), "--purpose", "archive_review", "--directory", directory)
				if code != 1 {
					t.Fatal("CLI revoked transfer exit code", code)
				}
			case "MCP":
				out := pull(t, filepath.Base(directory))
				if !out.IsError {
					t.Fatal("MCP accepted a revoked transfer")
				}
				value = consistencyJSON(t, mustJSON(out.StructuredContent))
			case "Python":
				value = consistencyFilePython(t, h, string(v.AssetID), string(v.VersionID), directory)
			}
			body, ok := value["error"].(map[string]any)
			requestID, _ := body["request_id"].(string)
			if !ok || requestID == "" {
				t.Fatal(adapter, "lost the server error")
			}
			delete(body, "request_id")
			if !bytes.Equal(mustJSON(value), mustJSON(want)) {
				t.Fatal(adapter, "changed the revoked transfer error")
			}
			if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
				t.Fatal(adapter, "revoked transfer wrote files", err)
			}
		}
		if prior.URL == "" {
			t.Fatal("no prior transfer grant was exercised")
		}
		raw, status = consistencyFileHTTP(t, h, "GET", prior.URL, nil)
		if status != 403 && status != 404 {
			t.Fatal("old signed URL bypassed current authorization", status)
		}
		code := consistencyJSON(t, raw)["error"].(map[string]any)["code"]
		if code != string(errcode.NotFound) && code != string(errcode.Forbidden) {
			t.Fatal("prior URL failed for an unexpected reason", code)
		}
		t.Logf("REST/CLI/MCP/Python error=%s; old transfer URL error=%s", errcode.NotFound, code)
	})
}

func assertFileToolOutput(t *testing.T, h *consistencyClient, out *mcp.CallToolResult, content []byte) {
	t.Helper()
	raw := mustJSON(out)
	marker := bytes.SplitN(content, []byte("\n"), 2)[0]
	if len(raw) > mcpserver.MaxResultBytes || bytes.Contains(raw, marker) || bytes.Contains(raw, []byte(h.token)) || bytes.Contains(raw, []byte("/xfer/")) || bytes.Contains(raw, []byte("sig=")) {
		t.Fatal("MCP exposed file bytes, credentials or transfer URLs")
	}
}

func consistencyFileHTTP(t *testing.T, h *consistencyClient, method, path string, body any) ([]byte, int) {
	t.Helper()
	base, err := url.Parse(h.origin)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := url.Parse(path)
	if err != nil {
		t.Fatal("invalid authorized path")
	}
	target := base.ResolveReference(rel)
	if target.Scheme != base.Scheme || target.Host != base.Host {
		t.Fatal("transfer left the API origin")
	}
	var input io.Reader
	if body != nil {
		input = bytes.NewReader(mustJSON(body))
	}
	req, err := http.NewRequestWithContext(t.Context(), method, target.String(), input)
	if err != nil {
		t.Fatal("HTTP request construction")
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal("HTTP transfer failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("HTTP transfer read failed")
	}
	return raw, response.StatusCode
}

func consistencyFilePython(t *testing.T, h *consistencyClient, asset, version, directory string) map[string]any {
	t.Helper()
	const script = `import hashlib, json, sys
from lantai import Client, APIError
spec=json.load(sys.stdin)
with open(spec["session"], encoding="utf-8") as f: saved=json.load(f)
c=Client(saved["origin"], saved["token"], allow_http=True)
try:
    p=c.download_file(spec["asset"], spec["version"], "content.txt", spec["directory"])
    print(json.dumps({"sha256":hashlib.sha256(p.read_bytes()).hexdigest(), "size":p.stat().st_size}))
except APIError as e:
    print(json.dumps({"error":e.body}))
except (OSError, ValueError) as e:
    print(json.dumps({"local_error":type(e).__name__}))
`
	sdk, err := filepath.Abs("../../sdk/python")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "python3", "-c", script)
	cmd.Env = append(consistencyEnv(), "PYTHONPATH="+sdk)
	cmd.Stdin = bytes.NewReader(mustJSON(map[string]string{"session": h.session, "asset": asset, "version": version, "directory": directory}))
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal("Python SDK file process failed", err)
	}
	if strings.Contains(string(raw), h.token) {
		t.Fatal("Python SDK exposed the session credential")
	}
	return consistencyJSON(t, raw)
}
