package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestResourceLargeEscapedJSON(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	_, session := e.agent("large-resource@synthetic", identity.RoleOwner)
	source := []byte("large manifest fixture")
	upload := e.upload(session.Context, source)
	app := reopenApplication(t, e)
	cfg := app.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: true}
	servers, err := app.StartHTTP(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = servers.Shutdown(ctx)
	}()
	origin := "http://" + servers.Addresses.API
	c, err := client.New(client.Config{BaseURL: origin, SessionToken: session.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	err = enc.Encode(map[string]any{"slug": "large/escaped", "content": map[string]any{"asset_type": "doc", "rights": rightsOwned(), "files": []any{map[string]any{"path": "content.txt", "role": "primary", "sha256": shaOf(source), "size": len(source)}}, "metadata": map[string]any{"extra": map[string]any{"blob": strings.Repeat("<", 2<<20)}}}})
	if err != nil || body.Len() > int(client.MaxJSONBytes) {
		t.Fatal("invalid fixture request", err, body.Len())
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/api/v1/uploads/"+string(upload)+"/commit", &body)
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "large-escaped-commit")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode >= 300 {
		t.Fatalf("real REST commit failed: %d %v %s", response.StatusCode, err, receipt)
	}
	var ids struct {
		Asset   string `json:"asset_id"`
		Version string `json:"version_id"`
	}
	if err = json.Unmarshal(receipt, &ids); err != nil || ids.Asset == "" || ids.Version == "" {
		t.Fatal("commit receipt", string(receipt), err)
	}
	path := "/api/v1/assets/" + ids.Asset + "/versions/" + ids.Version + "?view=full"
	// The ordinary JSON budget remains unchanged even for this valid resource.
	if _, err = c.Do(t.Context(), http.MethodGet, path, nil, client.Options{}); err == nil {
		t.Fatal("ordinary 8 MiB response budget bypassed")
	}
	server, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "large-resource-test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args any) map[string]any {
		t.Helper()
		r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || r.IsError {
			t.Fatalf("%s failed: %v %s", name, err, mustJSON(r))
		}
		return consistencyJSON(t, mustJSON(r.StructuredContent))
	}
	args := map[string]any{"kind": "version", "asset_id": ids.Asset, "version_id": ids.Version, "max_bytes": 1}
	first := call("resource_read_chunk", args)
	full, err := c.ReadResourceJSON(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(full.Body)
	digest := hex.EncodeToString(sum[:])
	if first["sha256"] != digest || first["total_bytes"] != float64(len(full.Body)) || len(full.Body) <= int(client.MaxJSONBytes) {
		t.Fatal("incorrect whole-object byte evidence", first)
	}
	firstByte, err := base64.StdEncoding.DecodeString(first["data"].(string))
	if err != nil || !bytes.Equal(firstByte, full.Body[:1]) {
		t.Fatal("first chunk", err)
	}
	args["offset"] = 1
	args["max_bytes"] = 8192
	args["expected_sha256"] = digest
	middle := call("resource_read_chunk", args)
	middleBytes, err := base64.StdEncoding.DecodeString(middle["data"].(string))
	if err != nil || !bytes.Equal(middleBytes, full.Body[1:8193]) || middle["sha256"] != digest || middle["next_offset"] != float64(8193) {
		t.Fatal("normal continuation chunk", err)
	}
	args["max_bytes"] = 1
	args["offset"] = len(full.Body) - 1
	args["expected_sha256"] = digest
	last := call("resource_read_chunk", args)
	lastByte, _ := base64.StdEncoding.DecodeString(last["data"].(string))
	if last["complete"] != true || !bytes.Equal(lastByte, full.Body[len(full.Body)-1:]) {
		t.Fatal("last chunk", last)
	}
	page := call("resource_files", map[string]any{"asset_id": ids.Asset, "version_id": ids.Version, "limit": 1})
	files := page["files"].([]any)
	if page["complete"] != true || page["total_files"] != float64(1) || len(files) != 1 || files[0].(map[string]any)["sha256"] != shaOf(source) {
		t.Fatal("incomplete file manifest", page)
	}

	t.Run("CLI_full_version", func(t *testing.T) {
		t.Setenv("LANTAI_SESSION_TOKEN", session.Token)
		output, err := os.CreateTemp(t.TempDir(), "full-*.json")
		if err != nil {
			t.Fatal(err)
		}
		defer output.Close()
		var stderr bytes.Buffer
		code := cli.Run(t.Context(), []string{"show", "--server", origin, "--allow-http", "--asset", ids.Asset, "--version", ids.Version, "--view", "full"}, output, &stderr)
		if code != 0 {
			t.Fatalf("full version CLI failed: %d %s", code, stderr.String())
		}
		if _, err = output.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		var value struct {
			Manifest struct {
				Content struct {
					Metadata struct {
						Extra struct {
							Blob string `json:"blob"`
						} `json:"extra"`
					} `json:"metadata"`
				} `json:"content"`
			} `json:"manifest"`
		}
		if err = json.NewDecoder(output).Decode(&value); err != nil || value.Manifest.Content.Metadata.Extra.Blob != strings.Repeat("<", 2<<20) {
			t.Fatal("redirected full JSON incomplete", err)
		}
	})
	t.Run("MCP_whole_version_guides_chunks", func(t *testing.T) {
		result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_read", Arguments: map[string]any{"asset_id": ids.Asset, "version_id": ids.Version, "view": "full"}})
		raw := string(mustJSON(result))
		if err != nil || !result.IsError || !strings.Contains(raw, "QUOTA_EXCEEDED") || !strings.Contains(raw, "resource_read_chunk") || strings.Contains(raw, "INTERNAL") {
			t.Fatalf("whole version did not guide bounded read: %v %s", err, raw)
		}
	})
	t.Run("workset_manifest", func(t *testing.T) {
		plan, err := c.InspectWorkset(t.Context(), client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: ids.Asset, VersionID: ids.Version, Destination: "selected"}}})
		if err != nil || plan.FileCount != 1 || len(plan.Entries) != 1 || plan.Entries[0].Files[0].SHA256 != shaOf(source) {
			t.Fatal("large exact manifest unavailable to workset", err)
		}
	})
	t.Run("derived_manifest", func(t *testing.T) {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		work := filepath.Join(root, "work")
		if err = os.Mkdir(work, 0700); err != nil {
			t.Fatal(err)
		}
		output, err := client.OpenPinnedPrivateOutput(filepath.Join(root, "comparison.json"))
		if err != nil {
			t.Fatal(err)
		}
		defer output.Close()
		ref := &client.FixedVersionRef{AssetID: ids.Asset, VersionID: ids.Version}
		receipt, err := c.DeriveTo(t.Context(), client.DerivationInput{Schema: client.DerivationInputSchema, Action: "compare", Purpose: "archive_review", Before: ref, After: ref}, work, output)
		if err != nil || len(receipt.Inputs) != 2 {
			t.Fatal("large exact manifest unavailable to derived comparison", err)
		}
	})
	// Make the current description large independently of the frozen manifest.
	var patch bytes.Buffer
	patchEncoder := json.NewEncoder(&patch)
	patchEncoder.SetEscapeHTML(false)
	if err = patchEncoder.Encode(map[string]any{"extra": map[string]any{"blob": strings.Repeat("<", 2<<20)}}); err != nil {
		t.Fatal(err)
	}
	current, err := c.Do(t.Context(), http.MethodGet, "/api/v1/assets/"+ids.Asset+"?view=brief", nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	patchRequest, _ := http.NewRequestWithContext(t.Context(), http.MethodPatch, origin+"/api/v1/assets/"+ids.Asset+"/metadata", &patch)
	patchRequest.Header.Set("Authorization", "Bearer "+session.Token)
	patchRequest.Header.Set("Content-Type", "application/json")
	patchRequest.Header.Set("Idempotency-Key", "large-current-description")
	patchRequest.Header.Set("If-Match", current.ETag)
	patchResponse, err := http.DefaultClient.Do(patchRequest)
	if err != nil {
		t.Fatal(err)
	}
	patchReceipt, err := io.ReadAll(io.LimitReader(patchResponse.Body, client.MaxResourceJSONBytes+1))
	patchResponse.Body.Close()
	if err != nil || patchResponse.StatusCode >= 300 {
		t.Fatalf("large current description patch failed: %d %v", patchResponse.StatusCode, err)
	}
	_ = patchReceipt
	t.Run("CLI_full_current", func(t *testing.T) {
		t.Setenv("LANTAI_SESSION_TOKEN", session.Token)
		for _, command := range [][]string{{"show", "--view", "full"}, {"metadata", "get"}} {
			var stdout, stderr bytes.Buffer
			args := append(command, "--server", origin, "--allow-http", "--asset", ids.Asset)
			if code := cli.Run(t.Context(), args, &stdout, &stderr); code != 0 {
				t.Fatalf("full current CLI failed: %d %s", code, stderr.String())
			}
			var value struct {
				Description struct {
					Extra struct {
						Blob string `json:"blob"`
					} `json:"extra"`
				} `json:"description"`
			}
			if err = json.Unmarshal(stdout.Bytes(), &value); err != nil || value.Description.Extra.Blob != strings.Repeat("<", 2<<20) {
				t.Fatal("full current JSON incomplete", err)
			}
		}
	})
	t.Run("MCP_whole_current_guides_chunks", func(t *testing.T) {
		result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "resource_current", Arguments: map[string]any{"asset_id": ids.Asset, "view": "full"}})
		raw := string(mustJSON(result))
		if err != nil || !result.IsError || !strings.Contains(raw, "QUOTA_EXCEEDED") || !strings.Contains(raw, "resource_read_chunk") || strings.Contains(raw, "INTERNAL") {
			t.Fatalf("whole current did not guide bounded read: %v %s", err, raw)
		}
	})
	t.Logf("real 2 MiB '<' commit; full JSON=%d bytes; first/last one-byte chunks and complete limit=1 file list verified; SHA256=%s", len(full.Body), digest)
}
