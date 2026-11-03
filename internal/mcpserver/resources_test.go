package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
)

const resourceTestAsset = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
const resourceTestVersion = "01ARZ3NDEKTSV4RRFFQ69G5FAW"

func resourceTestSession(t *testing.T, h http.HandlerFunc) *mcp.ClientSession {
	t.Helper()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/meta" {
			_, _ = io.WriteString(w, `{"api_version":"v1","capabilities":["projects","asset_types","exact_read","search","conditional_metadata","current_version_rights"]}`)
			return
		}
		h(w, r)
	}))
	t.Cleanup(remote.Close)
	c, err := client.New(client.Config{BaseURL: remote.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	server, err := New(t.Context(), Config{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "resource-test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
func resourceCall(t *testing.T, s *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	r, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxResultBytes {
		t.Fatal("unbounded result", len(raw), err)
	}
	raw, _ = json.Marshal(r.StructuredContent)
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return r, value
}

func TestResourceChunkCompleteChangedAndRevoked(t *testing.T) {
	body := []byte(`{"description":{"extra":{"large":"` + strings.Repeat("素材\\\"", 18000) + `"}}}`)
	if !json.Valid(body) {
		t.Fatal("invalid test JSON")
	}
	var state atomic.Int32
	s := resourceTestSession(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/assets/"+resourceTestAsset || r.URL.Query().Get("view") != "full" {
			t.Errorf("unexpected resource read %s", r.URL)
		}
		switch state.Load() {
		case 0:
			_, _ = w.Write(body)
		case 1:
			_, _ = io.WriteString(w, `{"description":{"revision":2}}`)
		case 2:
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"error":{"code":"FORBIDDEN","message":"revoked","retryable":false,"recovery_action":"none"}}`)
		}
	})
	args := map[string]any{"kind": "asset", "asset_id": resourceTestAsset, "max_bytes": 8192}
	var data []byte
	var digest string
	for {
		r, v := resourceCall(t, s, "resource_read_chunk", args)
		if r.IsError {
			t.Fatal(v)
		}
		chunk, err := base64.StdEncoding.DecodeString(v["data"].(string))
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, chunk...)
		if digest == "" {
			digest = v["sha256"].(string)
		} else if v["sha256"] != digest {
			t.Fatal("object changed")
		}
		if v["complete"] == true {
			if int(v["total_bytes"].(float64)) != len(data) {
				t.Fatal("size mismatch")
			}
			break
		}
		args["offset"] = v["next_offset"]
		args["expected_sha256"] = digest
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest || string(data) != string(body) {
		t.Fatal("full-object mismatch")
	}
	state.Store(1)
	r, v := resourceCall(t, s, "resource_read_chunk", args)
	if !r.IsError || !strings.Contains(fmt.Sprint(v), "PRECONDITION_FAILED") {
		t.Fatal(v)
	}
	state.Store(2)
	r, v = resourceCall(t, s, "resource_read_chunk", args)
	if !r.IsError || !strings.Contains(fmt.Sprint(v), "FORBIDDEN") {
		t.Fatal(v)
	}
}

func TestResourceFilesAdaptiveCompleteManifest(t *testing.T) {
	files := make([]map[string]any, 71)
	for i := range files {
		files[i] = map[string]any{"path": fmt.Sprintf("%03d-", i) + strings.Repeat("\\\"素材", 120), "role": "source", "sha256": strings.Repeat("a", 64), "size": i + 1}
	}
	s := resourceTestSession(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/assets/"+resourceTestAsset+"/versions/"+resourceTestVersion {
			t.Error(r.URL)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": map[string]any{"version_id": resourceTestVersion}, "manifest": map[string]any{"manifest_digest": strings.Repeat("b", 64), "content": map[string]any{"files": files}}})
	})
	offset := 0
	for {
		r, v := resourceCall(t, s, "resource_files", map[string]any{"asset_id": resourceTestAsset, "version_id": resourceTestVersion, "offset": offset, "limit": 20})
		if r.IsError {
			t.Fatal(v)
		}
		batch := v["files"].([]any)
		if len(batch) == 0 {
			t.Fatal("empty nonterminal page")
		}
		for _, raw := range batch {
			if raw.(map[string]any)["path"] != files[offset]["path"] {
				t.Fatal("file skipped or duplicated")
			}
			offset++
		}
		if v["complete"] == true {
			break
		}
		if int(v["next_offset"].(float64)) != offset {
			t.Fatal(v)
		}
	}
	if offset != len(files) {
		t.Fatal("incomplete files")
	}
}

func TestResourceDiscoveryFiltersAndConditionalMetadata(t *testing.T) {
	var calls atomic.Int32
	s := resourceTestSession(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch {
		case r.URL.Path == "/api/v1/assets" && r.Method == http.MethodGet:
			if !slices.Equal(r.URL.Query()["tag"], []string{"café", "state:ready"}) || r.URL.Query().Get("cursor") != "opaque" || r.URL.Query().Get("view") != "full" {
				t.Errorf("filter or cursor changed: %s", r.URL)
			}
			_, _ = io.WriteString(w, `{"items":[],"next_cursor":"continue-empty","state":{"generation":1,"high_water":0,"rebuild_start":0}}`)
		case r.URL.Path == "/api/v1/assets/"+resourceTestAsset+"/metadata":
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPatch || r.Header.Get("If-Match") != `"7"` || r.Header.Get("Idempotency-Key") != "saved-original-key" || !strings.Contains(string(body), "9007199254740993") {
				t.Errorf("conditional metadata changed: %s %s", r.Header, string(body))
			}
			w.WriteHeader(412)
			_, _ = io.WriteString(w, `{"error":{"code":"PRECONDITION_FAILED","message":"stale revision","retryable":false,"recovery_action":"refresh_state"}}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			_, _ = io.WriteString(w, `{}`)
		}
	})
	r, v := resourceCall(t, s, "resource_search", map[string]any{"tags": []string{"cafe\u0301", "state:ready", "café"}, "after": "opaque", "limit": 5, "view": "full"})
	if r.IsError || v["next_cursor"] != "continue-empty" {
		t.Fatal(v)
	}
	var patch any
	_ = json.Unmarshal([]byte(`{"extra":{"keep_number":9007199254740993}}`), &patch)
	// RawMessage retains the exact integer through the protocol and adapter.
	patch = json.RawMessage(`{"extra":{"keep_number":9007199254740993}}`)
	args := map[string]any{"asset_id": resourceTestAsset, "if_match": `"7"`, "idempotency_key": "saved-original-key", "request": patch}
	for i := 0; i < 2; i++ {
		r, v = resourceCall(t, s, "metadata_set", args)
		if !r.IsError || !strings.Contains(fmt.Sprint(v), "PRECONDITION_FAILED") {
			t.Fatal(v)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("unexpected retry or replacement", calls.Load())
	}
}

func TestResourceChunkCancellation(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	s := resourceTestSession(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(stopped) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "resource_read_chunk", Arguments: map[string]any{"kind": "asset", "asset_id": resourceTestAsset}})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("resource read did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled resource read succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MCP read did not cancel")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("REST request did not cancel")
	}

}

func TestResourceSearchExplicitCompletionMode(t *testing.T) {
	s := resourceTestSession(t, func(w http.ResponseWriter, r *http.Request) {
		page := map[string]any{"items": []any{}, "next_cursor": "continue-empty", "state": map[string]any{"generation": 1, "high_water": 0, "rebuild_start": 0}}
		if r.URL.Query().Get("completion") == "explicit" {
			page["complete"] = false
			page["truncated"] = false
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	for _, explicit := range []bool{false, true} {
		r, v := resourceCall(t, s, "resource_search", map[string]any{"explicit_completion": explicit, "limit": 1})
		if r.IsError {
			t.Fatal(v)
		}
		complete, present := v["complete"]
		if present != explicit || explicit && complete != false {
			t.Fatal("mode lost or empty page marked complete", v)
		}
	}
}
