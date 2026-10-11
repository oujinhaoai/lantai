package mcpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/api"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func localPathPurposeFixture(t *testing.T) (PushArgs, PullArgs, string, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/local-path-purpose.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Push           PushArgs `json:"push"`
		Pull           PullArgs `json:"pull"`
		InvalidPurpose string   `json:"invalid_purpose"`
		Sample         string   `json:"sample"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Push, fixture.Pull, fixture.InvalidPurpose, []byte(fixture.Sample)
}

func TestMCPLocalDirectoryErrorsBeforeREST(t *testing.T) {
	for _, scenario := range []string{"missing-state-parent", "state-parent-is-file", "missing-input", "input-is-file"} {
		t.Run(scenario, func(t *testing.T) {
			push, _, _, sample := localPathPurposeFixture(t)
			workspace := t.TempDir()
			input := filepath.Join(workspace, push.Directory)
			if err := os.Mkdir(input, 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(input, "sample.txt")
			if err := os.WriteFile(source, sample, 0600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "state-parent-is-file":
				if err := os.WriteFile(filepath.Join(workspace, ".lantai"), sample, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-input":
				push.Directory = "missing"
			case "input-is-file":
				push.Directory = "input/sample.txt"
				push.State = "state.json"
			}
			var requests atomic.Int32
			cs, _ := conflictToolSession(t, workspace, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})
			for range 2 {
				result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_push", Arguments: push})
				if err != nil {
					t.Fatal(err)
				}
				requireLocalConflict(t, result, errcode.SchemaInvalid)
				raw, _ := json.Marshal(result.StructuredContent)
				if !bytes.Contains(raw, []byte("local")) || bytes.Contains(raw, []byte("REST")) || bytes.Contains(raw, []byte(workspace)) {
					t.Fatalf("misleading or private path error: %s", raw)
				}
			}
			after, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(after, sample) || requests.Load() != 0 {
				t.Fatalf("source changed or REST reached: err=%v requests=%d", err, requests.Load())
			}
			if scenario != "state-parent-is-file" {
				if _, err := os.Stat(filepath.Join(workspace, ".lantai")); !os.IsNotExist(err) {
					t.Fatalf("created missing state parent: %v", err)
				}
			} else if after, err := os.ReadFile(filepath.Join(workspace, ".lantai")); err != nil || !bytes.Equal(after, sample) {
				t.Fatalf("state parent file changed: %v", err)
			}
		})
	}
}

func TestMCPLocalPathRetainsExtensionFileInputs(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workspace, "note.txt")
	if err := os.WriteFile(want, []byte("synthetic extension input"), 0600); err != nil {
		t.Fatal(err)
	}
	a := adapter{workspace: workspace}
	if got, err := a.localPath("note.txt", false); err != nil || got != want {
		t.Fatalf("valid extension input rejected: %q %v", got, err)
	}
	if _, err := a.localDirectory("note.txt"); err == nil {
		t.Fatal("file accepted as a push/pull directory")
	}
}

func TestMCPPullPurposeContractAndTransfer(t *testing.T) {
	_, pull, invalidPurpose, sample := localPathPurposeFixture(t)
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, pull.Directory), 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(sample)
	digest := hex.EncodeToString(sum[:])
	apiPath := "/api/v1/assets/" + pull.AssetID + "/versions/" + pull.VersionID
	var requests atomic.Int32
	cs, _ := conflictToolSession(t, workspace, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case apiPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"manifest": map[string]any{"content": map[string]any{"files": []client.InputFile{{Path: "sample.txt", Role: "primary", SHA256: digest, Size: int64(len(sample))}}}}})
		case apiPath + "/read-grants":
			var in map[string]string
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["purpose"] != pull.Purpose || in["path"] != "sample.txt" {
				t.Errorf("grant purpose/path changed: %v %+v", err, in)
			}
			_ = json.NewEncoder(w).Encode(client.DownloadGrant{URL: "http://" + r.Host + "/xfer/fixture", SHA256: digest, Size: int64(len(sample)), Path: "sample.txt"})
		case "/xfer/fixture":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(sample)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	for _, tool := range list.Tools {
		if tool.Name == "resource_pull" {
			raw, _ := json.Marshal(tool.InputSchema)
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
		}
	}
	if schema == nil {
		t.Fatal("resource_pull missing")
	}
	purpose := schema["properties"].(map[string]any)["purpose"].(map[string]any)
	var contract map[string]any
	if err := json.Unmarshal(api.OpenAPI, &contract); err != nil {
		t.Fatal(err)
	}
	want := contract["components"].(map[string]any)["schemas"].(map[string]any)["ReadGrantRequest"].(map[string]any)["properties"].(map[string]any)["purpose"].(map[string]any)
	if !reflect.DeepEqual(purpose["enum"], want["enum"]) || purpose["type"] != want["type"] || purpose["description"] == nil {
		t.Errorf("purpose contract lost: advertised=%v public=%v", purpose, want)
	}
	for _, value := range []string{invalidPurpose, ""} {
		invalid := pull
		invalid.Purpose = value
		result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_pull", Arguments: invalid})
		if err != nil || result == nil || !result.IsError {
			t.Fatalf("invalid purpose accepted: err=%v result=%+v", err, result)
		}
		if requests.Load() != 0 {
			t.Fatalf("invalid purpose reached REST: %d requests", requests.Load())
		}
	}
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_pull", Arguments: pull})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("valid production pull rejected: err=%v result=%+v", err, result)
	}
	after, err := os.ReadFile(filepath.Join(workspace, pull.Directory, "sample.txt"))
	if err != nil || !bytes.Equal(after, sample) || requests.Load() != 3 {
		t.Fatalf("download mismatch: err=%v requests=%d", err, requests.Load())
	}
	// Local directory errors remain actionable even when the purpose is valid.
	pull.Directory = "missing"
	result, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_pull", Arguments: pull})
	if err != nil {
		t.Fatal(err)
	}
	requireLocalConflict(t, result, errcode.SchemaInvalid)
	if requests.Load() != 3 || strings.Contains(string(mustJSONLocal(t, result)), "REST request failed") {
		t.Fatal("missing pull directory reached REST or claimed a remote failure")
	}
}

func mustJSONLocal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
