package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
)

func TestManagementDiscoveryWithoutWorkspace(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/meta" {
			t.Errorf("unexpected discovery request %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_version":"v1","capabilities":["exact_read","uploads","management_batch_checks"]}`))
	}))
	defer remote.Close()
	c, err := client.New(client.Config{BaseURL: remote.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := New(t.Context(), Config{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "management-discovery", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	page, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range page.Tools {
		names[tool.Name] = true
	}
	if !names["management_list"] || !names["management_check"] {
		t.Fatal("remote-only discovery requires local workspace", names)
	}
	for _, name := range []string{"management_prepare", "management_save", "management_read", "management_read_chunk", "management_batch_create", "management_batch_revise"} {
		if names[name] {
			t.Fatal("file tool advertised without explicit workspace", name)
		}
	}
}
