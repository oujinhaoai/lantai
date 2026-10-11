package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Reproduces the real minimal-scope client flow with an isolated actual core.
// A description warning is not a failed file commit or permission to reupload.
func TestIngestPartialResult(t *testing.T) {
	bin := buildConsistencyCLI(t)
	for _, withDescription := range []bool{false, true} {
		name := "derived_description"
		if withDescription {
			name = "requested_title"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, storage.Config{}, transfer.Limits{})
			agent, _ := e.agent("minimal-ingest@node", identity.RoleContributor)
			agent, err := e.id.GetPrincipal(t.Context(), e.login().Context, agent.ID)
			if err != nil {
				t.Fatal(err)
			}
			credential := e.sudo(&identity.IssueCredential{PrincipalID: agent.ID, ExpectedRevision: agent.Revision,
				Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeSelf}, Projects: []ids.ID{e.project.ProjectID}})
			session, err := e.id.ExchangeToken(t.Context(), credential.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
			if err != nil {
				t.Fatal(err)
			}
			app := reopenApplication(t, e)
			servers := remoteServers(t, app, true)
			h := newConsistencyClient(t, bin, "http://"+servers.Addresses.API, session.Token)
			work := t.TempDir()
			for _, directory := range []string{"input", "output"} {
				if err := os.Mkdir(filepath.Join(work, directory), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			content := []byte("synthetic minimal-scope partial-commit regression\n")
			if err := os.WriteFile(filepath.Join(work, "input", "sample.txt"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			in := client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "minimal-ingest",
				Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "sample.txt", Role: "source"}}, Rights: mustJSON(rightsOwned())}}
			if withDescription {
				in.Describe = json.RawMessage(`{"title":"Requested title"}`)
			}
			inputPath := filepath.Join(work, "input.json")
			writeJSON(t, inputPath, in)
			stdio := h.connectMCP(t, "--workspace", work)
			out, err := stdio.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_push", Arguments: mcpserver.PushArgs{Directory: "input", State: "push-state.json", Input: in}})
			if err != nil || out.IsError {
				t.Fatalf("committed partial result became a failed tool: %v %+v", err, out)
			}
			assertFileToolOutput(t, h, out, content)
			receipt := consistencyJSON(t, mustJSON(out.StructuredContent))
			if receipt["description_pending"] != true || receipt["description_error"] != "FORBIDDEN" || receipt["version_number"] != float64(1) {
				t.Fatalf("minimal scope did not preserve partial commit: %+v", receipt)
			}
			notice, link := false, false
			for _, item := range out.Content {
				if text, ok := item.(*mcp.TextContent); ok && strings.Contains(text.Text, "Files are committed; initial metadata is pending") {
					notice = strings.Contains(text.Text, "Do not start another upload") && strings.Contains(text.Text, "organize scope")
				}
				if resource, ok := item.(*mcp.ResourceLink); ok && resource.URI == receipt["uri"] {
					link = true
				}
			}
			if !notice || !link {
				t.Fatal("partial result lost the explanation or exact resource link")
			}
			// Reconcile through the real CLI with exactly the same input/state.
			cmd := exec.CommandContext(t.Context(), bin, "push", "--server", h.origin, "--allow-http", "--session-file", h.session,
				"--input", inputPath, "--directory", filepath.Join(work, "input"), "--state", filepath.Join(work, "push-state.json"))
			cmd.Env = append(consistencyEnv(), h.env...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("committed receipt must retain successful CLI exit: %v %s", err, stderr.String())
			}
			if !bytes.Equal(mustJSON(consistencyJSON(t, stdout.Bytes())), mustJSON(receipt)) || !strings.Contains(stderr.String(), "Files are committed; initial metadata is pending") {
				t.Fatal("CLI changed the stable receipt or omitted the warning")
			}
			if bytes.Contains(append(stdout.Bytes(), stderr.Bytes()...), []byte(h.token)) {
				t.Fatal("CLI exposed credentials")
			}
			for _, table := range []struct {
				db   ownership.Database
				name string
			}{
				{ownership.Ledger, "ledger_versions"}, {ownership.Runtime, "storage_uploads"},
			} {
				var count int
				if err := app.Instance.DB(table.db).QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table.name).Scan(&count); err != nil || count != 1 {
					t.Fatalf("same-state recovery created another %s: count=%d err=%v", table.name, count, err)
				}
			}
			for _, tool := range []string{"resource_read", "resource_pull"} {
				args := map[string]any{"asset_id": receipt["asset_id"], "version_id": receipt["version_id"]}
				if tool == "resource_pull" {
					args["directory"], args["purpose"] = "output", "production"
				}
				result, err := stdio.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
				if err != nil || result.IsError {
					t.Fatalf("committed exact version cannot %s: %v %+v", tool, err, result)
				}
			}
			got, err := os.ReadFile(filepath.Join(work, "output", "sample.txt"))
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("downloaded content changed: %v", err)
			}
		})
	}
}
