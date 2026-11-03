package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/api"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
)

func allOperations(t *testing.T) []api.AgentOperation {
	t.Helper()
	var out []api.AgentOperation
	after := ""
	cursors := map[string]bool{}
	digest := sha256.Sum256(api.AgentOperations)
	for {
		p, err := api.AgentOperationList(after, 20)
		if err != nil {
			t.Fatal(err)
		}
		if p.CatalogSHA256 != hex.EncodeToString(digest[:]) {
			t.Fatal("catalog digest changed during traversal")
		}
		encoded, _ := json.Marshal(p)
		// StructuredContent and text are both serialized by MCP.
		if len(encoded)*2+2048 > mcpserver.MaxResultBytes {
			t.Fatal("unbounded capability page")
		}
		out = append(out, p.Items...)
		if p.Complete {
			if p.NextCursor != "" {
				t.Fatal("complete page has continuation")
			}
			return out
		}
		if p.NextCursor == "" || cursors[p.NextCursor] {
			t.Fatal("non-advancing capability cursor")
		}
		cursors[p.NextCursor] = true
		after = p.NextCursor
	}
}

func TestAgentOperationPages(t *testing.T) {
	all := allOperations(t)
	if len(all) < 100 {
		t.Fatal("incomplete public inventory")
	}
	for _, tc := range []struct {
		after string
		limit int
	}{{"unknown", 10}, {"", 0}, {"", 21}} {
		if _, err := api.AgentOperationList(tc.after, tc.limit); err == nil {
			t.Fatal(tc)
		}
	}
	digest := sha256.Sum256(api.AgentOperations)
	cursor := func(digest, after string) string {
		raw, _ := json.Marshal(map[string]string{"catalog_sha256": digest, "after": after})
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	last, err := api.AgentOperationList(cursor(hex.EncodeToString(digest[:]), all[len(all)-1].ID), 10)
	if err != nil || !last.Complete || len(last.Items) != 0 {
		t.Fatal(last, err)
	}
	if _, err := api.AgentOperationList(cursor(strings.Repeat("0", 64), all[0].ID), 10); !errors.Is(err, api.ErrAgentOperationCursorExpired) {
		t.Fatal("stale catalog cursor accepted", err)
	}
}

func validateOperations(ops []api.AgentOperation, spec map[string]any, roots, tools []string) error {
	routes, seen, mappedTools, mappedRoots := map[string]string{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for path, raw := range spec["paths"].(map[string]any) {
		for method, raw := range raw.(map[string]any) {
			if !slices.Contains([]string{"get", "post", "put", "patch", "delete"}, method) {
				continue
			}
			routes[raw.(map[string]any)["operationId"].(string)] = strings.ToUpper(method) + " " + path
		}
	}
	previous := ""
	for _, op := range ops {
		if op.ID <= previous || seen[op.ID] {
			return fmt.Errorf("duplicate or unsorted operation %s", op.ID)
		}
		previous, seen[op.ID] = op.ID, true
		if op.Route != "" && routes[op.ID] != op.Route {
			return fmt.Errorf("route mismatch %s", op.ID)
		}
		if !slices.Contains([]string{"cli_mcp", "cli_only", "mcp_only", "api_only", "web_only", "unimplemented", "human_only", "local_only", "not_applicable"}, op.Classification) {
			return fmt.Errorf("unknown classification %s", op.ID)
		}
		if op.Classification != "cli_mcp" && op.Reason == "" {
			return fmt.Errorf("missing gap/exception reason %s", op.ID)
		}
		if op.Classification == "cli_mcp" && (len(op.CLI) == 0 || len(op.MCP) == 0) {
			return fmt.Errorf("false adapter coverage %s", op.ID)
		}
		if slices.Contains([]string{"api_only", "web_only", "unimplemented", "not_applicable"}, op.Classification) && (len(op.CLI) > 0 || len(op.MCP) > 0) {
			return fmt.Errorf("inconsistent gap %s", op.ID)
		}
		for _, cmd := range op.CLI {
			root := strings.Fields(cmd)[0]
			if !slices.Contains(roots, root) {
				return fmt.Errorf("unregistered CLI %s", cmd)
			}
			mappedRoots[root] = true
		}
		for _, tool := range op.MCP {
			if !slices.Contains(tools, tool) {
				return fmt.Errorf("unregistered MCP %s", tool)
			}
			mappedTools[tool] = true
		}
	}
	for id := range routes {
		if !seen[id] {
			return fmt.Errorf("unmapped API %s", id)
		}
	}
	for _, tool := range tools {
		if !mappedTools[tool] {
			return fmt.Errorf("unmapped MCP %s", tool)
		}
	}
	for _, root := range roots {
		if !mappedRoots[root] {
			return fmt.Errorf("unmapped CLI %s", root)
		}
	}
	return nil
}

func TestAgentOperationRegisteredAdapters(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_version":"v1","capabilities":["agent_operation_catalog","sessions","projects","asset_types","uploads","commit_version","exact_read","read_grants","conditional_metadata","search","operation_status","backup_management","tasks","manual_flows","manual_cli_execution","official_check_jobs","review_evidence","human_review","effective_context","discussions","filtered_events","inbox","milestones","lifecycle","current_version_rights"]}`))
	}))
	defer remote.Close()
	c, err := client.New(client.Config{BaseURL: remote.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c, Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "coverage-test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	var tools []string
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools = append(tools, tool.Name)
	}
	var spec map[string]any
	if err = json.Unmarshal(api.OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	ops := allOperations(t)
	if err = validateOperations(ops, spec, cli.Names(), tools); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing_api", "unknown_tool", "missing_reason", "extra_tool"} {
		t.Run(mode, func(t *testing.T) {
			broken := slices.Clone(ops)
			extra := slices.Clone(tools)
			switch mode {
			case "missing_api":
				for i, op := range broken {
					if op.Route != "" {
						broken = append(broken[:i], broken[i+1:]...)
						break
					}
				}
			case "unknown_tool":
				broken[0].MCP = []string{"arbitrary_http"}
			case "missing_reason":
				for i, op := range broken {
					if op.Classification != "cli_mcp" {
						broken[i].Reason = ""
						break
					}
				}
			case "extra_tool":
				extra = append(extra, "unsafe_unmapped_tool")
			}
			if validateOperations(broken, spec, cli.Names(), extra) == nil {
				t.Fatal("coverage drift accepted")
			}
		})
	}
}

// Inspect the actual registration calls, including conditional service routes.
// A new undocumented route therefore fails even before its OpenAPI is added.
func TestAgentOperationHTTPRegistrations(t *testing.T) {
	files, err := filepath.Glob("../internal/transport/httpapi/*.go")
	if err != nil {
		t.Fatal(err)
	}
	var parsed []*ast.File
	constants := map[string]ast.Expr{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, f)
		ast.Inspect(f, func(n ast.Node) bool {
			if v, ok := n.(*ast.ValueSpec); ok && len(v.Names) == 1 && len(v.Values) == 1 {
				constants[v.Names[0].Name] = v.Values[0]
			}
			return true
		})
	}
	var stringValue func(ast.Expr) string
	stringValue = func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.BasicLit:
			s, _ := strconv.Unquote(v.Value)
			return s
		case *ast.BinaryExpr:
			if v.Op == token.ADD {
				return stringValue(v.X) + stringValue(v.Y)
			}
		case *ast.Ident:
			if value, ok := constants[v.Name]; ok {
				return stringValue(value)
			}
		}
		return ""
	}
	known := map[string]bool{}
	for _, op := range allOperations(t) {
		known[op.Route] = true
	}
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || !slices.Contains([]string{"route", "backupReadRoute"}, sel.Sel.Name) {
				return true
			}
			if len(c.Args) < 2 {
				t.Fatal("invalid registration")
			}
			pattern := stringValue(c.Args[1])
			if pattern == "" || !known[pattern] {
				t.Errorf("unmapped HTTP registration %q", pattern)
			}
			return true
		})
	}
	// Keep accidental fixture/private paths out of the public inventory.
	if strings.Contains(string(api.AgentOperations), os.TempDir()+string(os.PathSeparator)) {
		t.Fatal("private path in catalog")
	}
}

// Exercise direct commands, not just their root names. A typo such as a new
// top-level command accidentally nested under project must fail this gate.
func TestAgentOperationDirectCLIRoutes(t *testing.T) {
	t.Setenv("LANTAI_SESSION_TOKEN", "synthetic-session")
	inputPath := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(inputPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	skip := []string{"push", "pull", "download-link", "commit"}
	for _, op := range allOperations(t) {
		if op.Route == "" || op.Classification == "human_only" || op.ID == "exchangeSession" {
			continue
		}
		for _, command := range op.CLI {
			root := strings.Fields(command)[0]
			if slices.Contains(skip, root) || root == "ext" || root == "asset" {
				continue
			}
			t.Run(op.ID+"/"+command, func(t *testing.T) {
				var method, path string
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					method, path = r.Method, r.URL.Path
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				}))
				defer remote.Close()
				name := "test-project"
				if op.ID == "getAssetType" {
					name = "doc"
				}
				args := append(strings.Fields(command), "--server", remote.URL, "--allow-http", "--asset", id, "--id", id, "--project", id, "--upload", id, "--input", inputPath, "--idempotency-key", "stable-test-key", "--if-match", `"1"`)
				if strings.Contains(op.Route, "{version_id}") {
					args = append(args, "--version", id)
				}
				if op.ID == "getProject" || op.ID == "getAssetType" {
					args = append(args, "--name", name)
				}
				var stdout, stderr bytes.Buffer
				if code := cli.Run(t.Context(), args, &stdout, &stderr); code != 0 {
					t.Fatalf("registered direct CLI failed (%d): %s", code, stderr.String())
				}
				expected := strings.ReplaceAll(op.Route, "{key}", name)
				expected = strings.ReplaceAll(expected, "{type}", name)
				for strings.Contains(expected, "{") {
					a := strings.Index(expected, "{")
					b := strings.Index(expected[a:], "}") + a
					expected = expected[:a] + id + expected[b+1:]
				}
				if method+" "+path != expected {
					t.Fatalf("CLI routes to %s %s; catalog declares %s", method, path, expected)
				}
			})
		}
	}
}
