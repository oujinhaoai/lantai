package mcpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func conflictToolSession(t *testing.T, workspace string, handler http.HandlerFunc) (*mcp.ClientSession, *client.Client) {
	t.Helper()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/meta" {
			_, _ = w.Write([]byte(`{"api_version":"v1","capabilities":[]}`))
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
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "local-conflict-test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, c
}

func requireLocalConflict(t *testing.T, result *mcp.CallToolResult, code errcode.Code) {
	t.Helper()
	if result == nil || !result.IsError {
		t.Fatalf("local conflict accepted: %+v", result)
	}
	b, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var envelope errcode.Envelope
	if err := json.Unmarshal(b, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != code || envelope.Error.Retryable || envelope.Error.RecoveryAction != errcode.ActionFixRequest || envelope.Error.OperationID != "" {
		t.Fatalf("incorrect local conflict recovery: %s", b)
	}
	if err := errcode.CheckBody(envelope.Error); err != nil {
		t.Fatal(err)
	}
}

func TestMCPPushChangedInputPreservesStateAndMakesNoRequest(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "input")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "sample.txt"), []byte("original input"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	cs, c := conflictToolSession(t, workspace, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Errorf("changed state reached REST: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	in := client.PushInput{ProjectID: string(ids.New()), Slug: "local/conflict", Content: client.ContentInput{AssetType: "doc", VersionNote: "original", Files: []client.InputFile{{Path: "sample.txt", Role: "source"}}, Rights: json.RawMessage(`{"usage":"production","license":"LicenseRef-Owned","sensitivity":"normal"}`)}}
	prepared, err := client.PreparePush(t.Context(), in, directory)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	statePath := filepath.Join(workspace, "state.json")
	if err := client.SaveState(statePath, client.PushState{Schema: "lantai.client-push/v1", Origin: c.Origin(), RequestHash: hex.EncodeToString(sum[:]), CreateKey: string(ids.New()), CommitKey: string(ids.New()), OperationID: string(ids.New()), Result: json.RawMessage(`{"preserved":true}`)}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	in = prepared
	in.Content.VersionNote = "changed"
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_push", Arguments: PushArgs{Directory: "input", State: "state.json", Input: in}})
	if err != nil {
		t.Fatal(err)
	}
	requireLocalConflict(t, result, errcode.IdempotencyConflict)
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(before, after) || requests.Load() != 0 {
		t.Fatalf("state changed or REST reached: err=%v requests=%d", err, requests.Load())
	}
}

func TestMCPPullOccupiedDestinationPreservesFileWithoutTransfer(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "output")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	before := []byte("existing content must survive")
	path := filepath.Join(directory, "sample.txt")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	expected := []byte("different remote content")
	sum := sha256.Sum256(expected)
	digest := hex.EncodeToString(sum[:])
	asset, version := string(ids.New()), string(ids.New())
	apiPath := "/api/v1/assets/" + asset + "/versions/" + version
	var manifests, grants, transfers atomic.Int32
	cs, _ := conflictToolSession(t, workspace, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPath:
			if r.Method != http.MethodGet {
				t.Errorf("manifest method: %s", r.Method)
			}
			manifests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"manifest": map[string]any{"content": map[string]any{"files": []client.InputFile{{Path: "sample.txt", Role: "source", SHA256: digest, Size: int64(len(expected))}}}}})
		case apiPath + "/read-grants":
			if r.Method != http.MethodPost {
				t.Errorf("read grant method: %s", r.Method)
			}
			grants.Add(1)
			_ = json.NewEncoder(w).Encode(client.DownloadGrant{URL: "http://" + r.Host + "/xfer/fixture", SHA256: digest, Size: int64(len(expected)), Path: "sample.txt"})
		default:
			transfers.Add(1)
			t.Errorf("occupied destination reached transfer: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_pull", Arguments: PullArgs{Directory: "output", AssetID: asset, VersionID: version, Purpose: "production"}})
	if err != nil {
		t.Fatal(err)
	}
	requireLocalConflict(t, result, errcode.PathConflict)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || manifests.Load() != 1 || grants.Load() != 1 || transfers.Load() != 0 {
		t.Fatalf("file changed or unexpected requests: err=%v manifest=%d grant=%d transfer=%d", err, manifests.Load(), grants.Load(), transfers.Load())
	}
	for _, suffix := range []string{".lantai-part", ".lantai-download.json"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("conflict created download state %s: %v", suffix, err)
		}
	}
}

func TestMCPPushInvalidStatePreservesOriginalBeforeREST(t *testing.T) {
	for _, kind := range []string{"schema", "origin", "create-key", "commit-key"} {
		t.Run(kind, func(t *testing.T) {
			workspace := t.TempDir()
			directory := filepath.Join(workspace, "input")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "sample.txt"), []byte("original input"), 0600); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			cs, c := conflictToolSession(t, workspace, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})
			in, err := client.PreparePush(t.Context(), client.PushInput{ProjectID: string(ids.New()), Slug: "local/conflict", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "sample.txt", Role: "source"}}, Rights: json.RawMessage(`{"usage":"production","license":"LicenseRef-Owned","sensitivity":"normal"}`)}}, directory)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			state := client.PushState{Schema: "lantai.client-push/v1", Origin: c.Origin(), RequestHash: hex.EncodeToString(sum[:]), CreateKey: string(ids.New()), CommitKey: string(ids.New())}
			switch kind {
			case "schema":
				state.Schema = "unknown"
			case "origin":
				state.Origin = "https://different.example"
			case "create-key":
				state.CreateKey = ""
			case "commit-key":
				state.CommitKey = ""
			}
			path := filepath.Join(workspace, "state.json")
			if err := client.SaveState(path, state); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_push", Arguments: PushArgs{Directory: "input", State: "state.json", Input: in}})
			if err != nil {
				t.Fatal(err)
			}
			requireLocalConflict(t, result, errcode.SchemaInvalid)
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) || requests.Load() != 0 {
				t.Fatalf("invalid state changed or REST reached: err=%v requests=%d", err, requests.Load())
			}
		})
	}
}
