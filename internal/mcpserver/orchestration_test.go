package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
)

func TestOrchestrationToolsKeepRoleAndContributionBoundaries(t *testing.T) {
	var bodies, keys []string
	var mu sync.Mutex
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/meta" {
			_, _ = io.WriteString(w, `{"api_version":"v1","capabilities":["tasks","manual_flows","manual_cli_execution","official_check_jobs"]}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"error":{"code":"FORBIDDEN","message":"current role/scope required","retryable":false,"recovery_action":"none"}}`)
	}))
	defer remote.Close()
	c, err := client.New(client.Config{BaseURL: remote.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, core := range []string{"task_create", "flow_dispatch", "job_run", "run_answer"} {
		if _, err := New(t.Context(), Config{Client: c, Contributions: []Contribution{{ExtensionID: "org.example.test", Name: "admin", CoreTool: core}}}); err == nil {
			t.Fatal("new governance/worker tool silently entered contribution allowlist", core)
		}
	}
	server, err := New(t.Context(), Config{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "orchestration-test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	var names []string
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
		if tool.Name == "flow_dispatch" && tool.Annotations.IdempotentHint {
			t.Fatal("bounded dispatch pass is not an idempotent request receipt")
		}
	}
	for _, name := range []string{"task_create", "task_assign", "task_answer", "task_cancel", "task_complete", "task_rework", "task_reconcile", "task_attempts", "flow_start", "flow_list", "flow_pause", "flow_resume", "flow_cancel", "flow_retry", "flow_retry_command", "flow_dispatch", "job_list", "job_run", "job_cancel", "job_retry", "job_reconcile", "run_cancel", "run_reconcile", "run_capabilities", "node_observe"} {
		if !slices.Contains(names, name) {
			t.Fatal("missing first-party tool", name)
		}
	}
	if slices.Contains(names, "run_answer") || slices.Contains(names, "human_execute") {
		t.Fatal("human input exposed as an ordinary action")
	}
	args := map[string]any{"idempotency_key": "saved-original-task-key", "request": map[string]any{"project_id": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "type": "question", "title": "Synthetic coordination task", "acceptance_criteria": []string{"record answer"}, "role": "contributor", "checkouts": []any{}, "dependency_ids": []string{}, "distinct_from": []string{}, "expected_outputs": []any{}, "input_refs": []any{}, "priority": "P2"}}
	for i := 0; i < 2; i++ {
		out, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "task_create", Arguments: args})
		raw, _ := json.Marshal(out)
		if err != nil || !out.IsError || !strings.Contains(string(raw), "FORBIDDEN") {
			t.Fatal("permission denial was changed", err, string(raw))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] != "saved-original-task-key" || keys[1] != keys[0] || bodies[0] != bodies[1] {
		t.Fatal("key/input changed or implicit retry occurred", keys, bodies)
	}
}
