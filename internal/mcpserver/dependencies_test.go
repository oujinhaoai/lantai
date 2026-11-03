package mcpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
)

func TestDependencyMCPChunksMatchCLIWithoutRemoteReads(t *testing.T) {
	work := t.TempDir()
	source := filepath.Join(work, "sources")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Repeat("call deliberately-unresolved\n", 500))
	if err := os.WriteFile(filepath.Join(source, "mesh.obj"), data, 0600); err != nil {
		t.Fatal(err)
	}
	selection := filepath.Join(work, "selection.json")
	if err := client.SaveState(selection, client.DependencySelection{Paths: []string{"mesh.obj"}}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if exit := cli.Run(t.Context(), []string{"asset", "dependencies", "--input", selection, "--directory", source, "--output", filepath.Join(work, "cli-report.json")}, &stdout, &stderr); exit != 0 {
		t.Fatal(exit, stderr.String())
	}
	var expected client.DependencyReport
	if err := json.Unmarshal(stdout.Bytes(), &expected); err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/meta" {
			t.Errorf("offline analysis fetched %s", r.URL)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_version":"v1","capabilities":["exact_read"]}`))
	}))
	defer remote.Close()
	c, err := client.New(client.Config{BaseURL: remote.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := New(t.Context(), Config{Client: c, Workspace: work})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "dependency-acceptance", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	args := map[string]any{"input_file": "selection.json", "directory": "sources", "max_bytes": 8192}
	var assembled []byte
	hash := ""
	for {
		r, page := resourceCall(t, cs, "dependency_analyze", args)
		if r.IsError {
			t.Fatal(page)
		}
		if page["analysis_complete"] != false || page["scope"] != "explicit-selected-text-files" {
			t.Fatal("unsupported statements treated as executable project", page)
		}
		part, err := base64.StdEncoding.DecodeString(page["data"].(string))
		if err != nil {
			t.Fatal(err)
		}
		assembled = append(assembled, part...)
		if hash == "" {
			hash = page["sha256"].(string)
		} else if hash != page["sha256"] {
			t.Fatal("chunk basis changed")
		}
		if page["complete"] == true {
			if len(assembled) != int(page["total_bytes"].(float64)) {
				t.Fatal("truncated analysis")
			}
			break
		}
		args["offset"], args["expected_sha256"] = page["next_offset"], hash
	}
	var actual client.DependencyReport
	if digest.Of(assembled).Hex() != hash {
		t.Fatal("assembled report hash mismatch")
	}
	if err := json.Unmarshal(assembled, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected, actual) {
		t.Fatal("CLI and MCP analysis differ")
	}
	if err := os.WriteFile(filepath.Join(source, "mesh.obj"), []byte("mtllib changed.mtl\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, changed := resourceCall(t, cs, "dependency_analyze", args)
	if !r.IsError || !strings.Contains(string(mustDependencyJSON(t, changed)), "PRECONDITION_FAILED") {
		t.Fatal("mixed changed analysis chunks", changed)
	}
	args["offset"] = 1
	delete(args, "expected_sha256")
	r, _ = resourceCall(t, cs, "dependency_analyze", args)
	if !r.IsError {
		t.Fatal("unbound continuation accepted")
	}
	args["offset"], args["directory"] = 0, "../outside"
	r, _ = resourceCall(t, cs, "dependency_analyze", args)
	if !r.IsError {
		t.Fatal("outside root accepted")
	}
}
func mustDependencyJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
