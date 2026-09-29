package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func TestBoundedMachineErrorsAndResults(t *testing.T) {
	r, e := toolResult(client.Response{Body: []byte(strings.Repeat("x", MaxResultBytes+1))}, nil)
	if e != nil || !r.IsError {
		t.Fatal(r, e)
	}
	b, _ := json.Marshal(r.StructuredContent)
	if !strings.Contains(string(b), string(errcode.QuotaExceeded)) {
		t.Fatal(string(b))
	}
	remote := &client.APIError{Body: errcode.Body{Code: errcode.LeaseStale, Message: strings.Repeat("x", MaxResultBytes+1)}}
	r, e = toolResult(client.Response{}, remote)
	b, _ = json.Marshal(r.StructuredContent)
	if e != nil || !r.IsError || len(b) > MaxResultBytes || !strings.Contains(string(b), string(errcode.LeaseStale)) {
		t.Fatal(len(b), e)
	}
}
func TestLocalWorkspaceSymlinkAndTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	a := adapter{workspace: root}
	for _, p := range []string{"../escape", outside, "\\escape"} {
		if _, e := a.localPath(p, false); e == nil {
			t.Fatal(p)
		}
	}
	if e := os.Symlink(outside, filepath.Join(root, "escape")); e != nil {
		t.Skip("symlink unavailable", e)
	}
	if _, e := a.localPath("escape", false); e == nil {
		t.Fatal("symlink escape")
	}
	if e := os.Symlink(filepath.Join(outside, "state"), filepath.Join(root, "state")); e != nil {
		t.Fatal(e)
	}
	if _, e := a.localPath("state", true); e == nil {
		t.Fatal("state symlink")
	}
}

func TestProtocolCancellationAndContributionBoundary(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/meta" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"api_version":"v1","capabilities":["tasks"]}`))
			return
		}
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer remote.Close()
	c, e := client.New(client.Config{BaseURL: remote.URL, AllowHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	for _, core := range []string{"human_execute", "shell", "missing"} {
		if _, e = New(t.Context(), Config{Client: c, Contributions: []Contribution{{ExtensionID: "org.example.test", Name: "unsafe", CoreTool: core}}}); e == nil {
			t.Fatal("unsafe projection", core)
		}
	}
	s, e := New(t.Context(), Config{Client: c, Contributions: []Contribution{{ExtensionID: "org.example.test", Name: "claim", CoreTool: "task_claim"}}})
	if e != nil {
		t.Fatal(e)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, e := s.Connect(t.Context(), st, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ss.Close()
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "cancellation-test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer cs.Close()
	page, e := cs.ListTools(t.Context(), nil)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, tool := range page.Tools {
		if tool.Name == "ext_org_example_test_claim" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing safe projection")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "task_read", Arguments: map[string]any{"id": "01ARZ3NDEKTSV4RRFFQ69G5FAV"}})
		done <- e
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no REST call")
	}
	cancel()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("cancelled call succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("protocol call not cancelled")
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("REST call not cancelled")
	}
}
