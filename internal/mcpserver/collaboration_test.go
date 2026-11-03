package mcpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func collaborationSession(t *testing.T, handle http.HandlerFunc, extraCapabilities ...string) *mcp.ClientSession {
	t.Helper()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/meta" {
			capabilities := append([]string{"milestones", "lifecycle", "human_review", "inbox", "filtered_events", "extension_governance", "extension_cli_commands"}, extraCapabilities...)
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "v1", "capabilities": capabilities})
			return
		}
		handle(w, r)
	}))
	t.Cleanup(remote.Close)
	c, err := client.New(client.Config{BaseURL: remote.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	s, err := New(t.Context(), Config{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "collaboration-protocol", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestCollaborationUsesAdvertisedServerPagesAndChunks(t *testing.T) {
	const project = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	digest := strings.Repeat("a", 64)
	cs := collaborationSession(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/"+project+"/milestones" || r.Method != http.MethodGet {
			t.Errorf("unexpected snapshot path %s", r.URL)
			w.WriteHeader(500)
			return
		}
		switch r.URL.Query().Get("pagination") {
		case "explicit":
			if r.URL.Query().Get("limit") != "1" {
				t.Errorf("lost item budget %s", r.URL)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "one"}}, "offset": 0, "next_offset": 1, "sha256": digest, "total_items": 1000000, "complete": false})
		case "chunk":
			if r.URL.Query().Get("offset") != "0" || r.URL.Query().Get("max_bytes") != "8" || r.URL.Query().Get("expected_sha256") != digest {
				t.Errorf("lost byte continuation %s", r.URL)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "data": base64.StdEncoding.EncodeToString([]byte(`{"items"`)), "offset": 0, "next_offset": 8, "sha256": digest, "total_bytes": 9 << 20, "complete": false})
		default:
			t.Error("adapter requested whole large snapshot")
			w.WriteHeader(500)
		}
	}, "bounded_collaboration_snapshots")
	page := collaborationCall(t, cs, "milestone_list", map[string]any{"project_id": project, "limit": 1}, false)
	if page["total_items"] != float64(1000000) || page["complete"] != false {
		t.Fatal(page)
	}
	chunk := collaborationCall(t, cs, "collaboration_read_chunk", map[string]any{"kind": "milestones", "project_id": project, "max_bytes": 8, "expected_sha256": digest}, false)
	if chunk["total_bytes"] != float64(9<<20) || chunk["next_offset"] != float64(8) {
		t.Fatal(chunk)
	}
}

func TestCollaborationGovernanceDiscoveryOnlyUsesGETAndPreservesDenial(t *testing.T) {
	var reads atomic.Int32
	cs := collaborationSession(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/extensions/enablements" && r.URL.Path != "/api/v1/extensions/packages" {
			t.Errorf("governance discovery attempted mutation %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		reads.Add(1)
		w.WriteHeader(403)
		_ = json.NewEncoder(w).Encode(errcode.New(errcode.Forbidden, "original HumanOnly governance").Envelope("synthetic"))
	})
	for _, name := range []string{"extension_enablements", "extension_packages"} {
		p := collaborationCall(t, cs, name, map[string]any{}, true)
		if !strings.Contains(string(mustCollaborationJSON(t, p)), "FORBIDDEN") {
			t.Fatal(p)
		}
	}
	p := collaborationCall(t, cs, "collaboration_read_chunk", map[string]any{"kind": "extension_enablements"}, true)
	if !strings.Contains(string(mustCollaborationJSON(t, p)), "FORBIDDEN") || reads.Load() != 3 {
		t.Fatal(p, reads.Load())
	}
}

func collaborationCall(t *testing.T, cs *mcp.ClientSession, name string, args any, wantError bool) map[string]any {
	t.Helper()
	r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || r.IsError != wantError {
		t.Fatalf("%s: %v %+v", name, err, r)
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxResultBytes {
		t.Fatal("unbounded complete serialized MCP result", len(raw), err)
	}
	raw, err = json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCollaborationSnapshotBoundsChangeAndRevocation(t *testing.T) {
	const project = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	var changed, denied atomic.Bool
	snapshot := func() []byte {
		raw, _ := json.Marshal(map[string]any{"items": []any{map[string]any{"id": "one", "payload": strings.Repeat("x", 20<<10)}, map[string]any{"id": "two", "payload": strings.Repeat("y", 20<<10)}, map[string]any{"id": "three", "changed": changed.Load(), "payload": strings.Repeat("z", 20<<10)}}})
		return raw
	}
	cs := collaborationSession(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/"+project+"/milestones" {
			t.Errorf("unexpected path %s %s", r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		if denied.Load() {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":{"code":"FORBIDDEN","message":"revoked","retryable":false,"recovery_action":"none"}}`))
			return
		}
		_, _ = w.Write(snapshot())
	})
	page := collaborationCall(t, cs, "milestone_list", map[string]any{"project_id": project, "limit": 3}, false)
	if len(page["items"].([]any)) != 1 || page["complete"] != false || page["next_offset"] != float64(1) || page["total_items"] != float64(3) {
		t.Fatal("snapshot truncated without accurate continuation", page)
	}
	digest := page["sha256"].(string)
	for _, offset := range []int{1, 2} {
		p := collaborationCall(t, cs, "milestone_list", map[string]any{"project_id": project, "offset": offset, "limit": 1, "expected_sha256": digest}, false)
		if len(p["items"].([]any)) != 1 || p["complete"] != (offset == 2) {
			t.Fatal(p)
		}
	}
	var all []byte
	for offset := 0; ; {
		p := collaborationCall(t, cs, "collaboration_read_chunk", map[string]any{"kind": "milestones", "project_id": project, "offset": offset, "max_bytes": 8192, "expected_sha256": digest}, false)
		part, err := base64.StdEncoding.DecodeString(p["data"].(string))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, part...)
		if p["complete"] == true {
			break
		}
		offset = int(p["next_offset"].(float64))
	}
	if !bytes.Equal(all, snapshot()) {
		t.Fatal("snapshot chunks lost full JSON bytes")
	}
	changed.Store(true)
	p := collaborationCall(t, cs, "milestone_list", map[string]any{"project_id": project, "offset": 1, "expected_sha256": digest}, true)
	if !strings.Contains(string(mustCollaborationJSON(t, p)), "PRECONDITION_FAILED") {
		t.Fatal(p)
	}
	denied.Store(true)
	p = collaborationCall(t, cs, "collaboration_read_chunk", map[string]any{"kind": "milestones", "project_id": project, "offset": 8192, "expected_sha256": digest}, true)
	if !strings.Contains(string(mustCollaborationJSON(t, p)), "FORBIDDEN") {
		t.Fatal(p)
	}
}

func mustCollaborationJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCollaborationResyncEmptyContinuationAndFixedRoutes(t *testing.T) {
	const project = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	var calls atomic.Int32
	cs := collaborationSession(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/api/v1/resync":
			if r.Method != http.MethodGet || r.URL.Query().Get("cursor") != "original-cursor" || r.URL.Query().Get("project_id") != project {
				t.Errorf("resync scope changed %s", r.URL)
			}
			_, _ = w.Write([]byte(`{"objects":[],"next_cursor":"continue-empty","replace":true,"replay_after":123}`))
		case "/api/v1/projects/" + project + "/milestones":
			if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != "original-key" {
				t.Errorf("conditional milestone changed %s %s", r.Method, r.Header)
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body["expected_revision"]) != "7" {
				t.Errorf("revision lost %s %v", body["expected_revision"], err)
			}
			w.WriteHeader(412)
			_, _ = w.Write([]byte(`{"error":{"code":"PRECONDITION_FAILED","message":"stale","retryable":false,"recovery_action":"refresh_state"}}`))
		default:
			t.Errorf("unexpected route %s", r.URL)
			w.WriteHeader(500)
		}
	})
	p := collaborationCall(t, cs, "resync_read", map[string]any{"project_id": project, "cursor": "original-cursor", "limit": 1}, false)
	if p["next_cursor"] != "continue-empty" || p["replace"] != true || p["replay_after"] != float64(123) {
		t.Fatal(p)
	}
	// The original schema rejects incomplete milestone input before HTTP.
	collaborationCall(t, cs, "milestone_set", map[string]any{"project_id": project, "idempotency_key": "original-key", "request": map[string]any{}}, true)
	if calls.Load() != 1 {
		t.Fatal("invalid input reached HTTP")
	}
	args := map[string]any{"project_id": project, "idempotency_key": "original-key", "request": map[string]any{"expected_revision": 7, "milestone": map[string]any{"project_id": project, "name": "Synthetic", "owner_id": project, "task_ids": []any{}}}}
	collaborationCall(t, cs, "milestone_set", args, true)
	collaborationCall(t, cs, "milestone_set", args, true)
	if calls.Load() != 3 {
		t.Fatal("adapter replaced original request", calls.Load())
	}
}
