package mcpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
)

func batchTestSession(t *testing.T, workspace string, handler http.HandlerFunc) *mcp.ClientSession {
	t.Helper()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/meta" {
			_, _ = w.Write([]byte(`{"api_version":"v1","capabilities":["uploads","commit_version","exact_read"]}`))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(remote.Close)
	c, err := client.New(client.Config{BaseURL: remote.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	s, err := New(t.Context(), Config{Client: c, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "batch-protocol", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func batchWrite(t *testing.T, path string, value any) {
	t.Helper()
	if err := client.SaveState(path, value); err != nil {
		t.Fatal(err)
	}
}

func TestBatchInputShapeFailsBeforeRemoteReadOrPublishing(t *testing.T) {
	for _, name := range []string{"import_plan", "workset_plan", "workset_pull", "asset_derive"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"inputs", "work", "reports", "artifacts"} {
				if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			batchWrite(t, filepath.Join(root, "inputs", "invalid.json"), map[string]any{"unknown_input_shape": "private-value-must-not-echo"})
			var remote atomic.Int32
			cs := batchTestSession(t, root, func(w http.ResponseWriter, r *http.Request) {
				remote.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})
			args := map[string]any{"input_file": "inputs/invalid.json", "directory": "work", "output_file": "reports/result.json"}
			if name == "workset_pull" {
				args["purpose"] = "archive_review"
			}
			if name == "asset_derive" {
				args["artifact_file"] = "artifacts/result.json"
			}
			result, body := resourceCall(t, cs, name, args)
			problem, _ := body["error"].(map[string]any)
			if !result.IsError || problem["code"] != "SCHEMA_INVALID" || problem["recovery_action"] != "fix_request" {
				t.Fatalf("invalid local shape needs input repair, not operation reconciliation: %v", body)
			}
			if strings.Contains(string(mustBatchJSON(t, body)), "private-value-must-not-echo") || remote.Load() != 0 {
				t.Fatal("invalid local input leaked contents or reached remote", body, remote.Load())
			}
			for _, path := range []string{"reports/result.json", "artifacts/result.json"} {
				if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
					t.Fatal("invalid input published output", path, err)
				}
			}
		})
	}
}

func TestBatchPlanNoRemoteWritesOriginalResumeAndChangedSource(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"source", "specs", "reports"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "source", "data.txt"), []byte("original synthetic bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	selection := client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{Key: "one", Directory: ".", Classification: "final", Push: client.PushInput{ProjectID: resourceTestAsset, Slug: "batch/unit", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "data.txt", Role: "source"}}, Rights: json.RawMessage(`{"usage":"production","license":"LicenseRef-Owned"}`)}}}}}
	batchWrite(t, filepath.Join(root, "specs", "import.json"), selection)
	var requests atomic.Int32
	cs := batchTestSession(t, root, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Errorf("plan/rejection reached remote %s %s", r.Method, r.URL)
		w.WriteHeader(500)
	})
	args := map[string]any{"input_file": "specs/import.json", "directory": "source", "output_file": "reports/plan.json"}
	r, first := resourceCall(t, cs, "import_plan", args)
	if r.IsError || first["planned"] != float64(1) || first["receipt_sha256"] == "" {
		t.Fatal(first)
	}
	original, err := client.ReadPrivate(filepath.Join(root, "reports", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, replay := resourceCall(t, cs, "import_plan", args)
	if r.IsError || replay["receipt_sha256"] != first["receipt_sha256"] {
		t.Fatal("same original input changed plan", replay)
	}
	if err := os.WriteFile(filepath.Join(root, "source", "data.txt"), []byte("changed synthetic bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	r, changed := resourceCall(t, cs, "import_plan", args)
	if !r.IsError || !strings.Contains(string(mustBatchJSON(t, changed)), "PATH_CONFLICT") {
		t.Fatal("changed original plan replaced", changed)
	}
	if got, err := client.ReadPrivate(filepath.Join(root, "reports", "plan.json")); err != nil || !bytes.Equal(got, original) {
		t.Fatal("original plan lost", err)
	}
	r, failed := resourceCall(t, cs, "import_apply", map[string]any{"input_file": "reports/plan.json", "directory": "source", "output_file": "reports/failed.json", "state_directory": "state"})
	if !r.IsError || failed["receipt"] == nil {
		t.Fatal("partial diagnostic receipt lost", failed)
	}
	if _, err := os.Stat(filepath.Join(root, "state")); !os.IsNotExist(err) {
		t.Fatal("changed source created replacement state", err)
	}
	if requests.Load() != 0 {
		t.Fatal("plan or changed input reached remote mutation", requests.Load())
	}
}

func mustBatchJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBatchReceiptMetadataExcludesNotesCredentialsAndSourceRoots(t *testing.T) {
	root := t.TempDir()
	marker := "COMPLETE_NOTE_OR_TOKEN_MUST_NOT_APPEAR"
	plan := client.ReferencePlan{Schema: client.ReferencePlanSchema, Digest: strings.Repeat("a", 64), SourceRoot: "/private/" + marker, Complete: true, Scope: "selected_notes", Notes: []client.ReferenceNotePlan{{Path: "note.md", Before: marker, After: marker, Diff: marker, BeforeSHA256: strings.Repeat("b", 64), AfterSHA256: strings.Repeat("c", 64), Edits: []client.ReferenceEdit{{Start: 0, End: 5, Before: marker, After: marker, Target: "asset.txt", FixedRef: "lantai://fixed", Syntax: "markdown"}}}}, Unresolved: []client.ReferenceIssue{{Note: "note.md", Target: "https://example.invalid/?token=" + marker, Reason: "protected target"}}}
	batchWrite(t, filepath.Join(root, "plan.json"), plan)
	batchWrite(t, filepath.Join(root, "session.json"), map[string]any{"schema": "lantai.client-session/v1", "token": marker})
	cs := batchTestSession(t, root, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("private receipt unexpectedly read server %s", r.URL)
		w.WriteHeader(500)
	})
	for _, section := range []string{"rows", "edits", "issues"} {
		args := map[string]any{"file": "plan.json", "section": section}
		if section == "edits" {
			args["key"] = "note.md"
		}
		r, p := resourceCall(t, cs, "batch_report_page", args)
		if r.IsError || bytes.Contains(mustBatchJSON(t, r), []byte(marker)) || p["current_remote_verified"] != false || p["historical_local_observation"] != true {
			t.Fatal("private payload leaked or became authority", section, p)
		}
	}
	r, _ := resourceCall(t, cs, "batch_report_page", map[string]any{"file": "session.json"})
	if !r.IsError || bytes.Contains(mustBatchJSON(t, r), []byte(marker)) {
		t.Fatal("credential document exposed")
	}
}

func TestBatchWorkspaceReplacementRejectsBeforeInputOrOutput(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	cs := batchTestSession(t, root, func(w http.ResponseWriter, r *http.Request) {
		t.Error("replaced workspace reached network")
		w.WriteHeader(500)
	})
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	batchWrite(t, filepath.Join(root, "input.json"), map[string]any{"schema": client.ImportInputSchema, "assets": []any{}})
	r, p := resourceCall(t, cs, "import_plan", map[string]any{"input_file": "input.json", "directory": ".", "output_file": "output.json"})
	if !r.IsError || !strings.Contains(string(mustBatchJSON(t, p)), "PATH_CONFLICT") {
		t.Fatal("workspace identity replacement accepted", p)
	}
	if _, err := os.Stat(filepath.Join(root, "output.json")); !os.IsNotExist(err) {
		t.Fatal("replacement received output", err)
	}
}

// filepath.Rel uses native Windows separators. Only internally derived paths
// are converted; backslashes supplied as tool input remain invalid.
func TestBatchPrivateNativePathsKeepToolPathBoundary(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "selected", "nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(nested, "input.json")
	batchWrite(t, input, map[string]string{"selected": "fixed"})
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	a := &adapter{workspace: root, workspaceIdentity: info}
	if got, err := a.workspaceStoredDirectory(nested); err != nil || got != nested {
		t.Fatal(got, err)
	}
	var value map[string]string
	if err := a.loadBatchPrivate(input, &value); err != nil || value["selected"] != "fixed" {
		t.Fatal(value, err)
	}
	for _, bad := range []string{`selected\nested\input.json`, "../outside.json", input} {
		if _, err := a.localPath(bad, false); err == nil {
			t.Fatalf("accepted non-tool path %q", bad)
		}
	}
}
