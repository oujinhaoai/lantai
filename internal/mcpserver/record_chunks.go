package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Routes are fixed by the first-party adapter, never supplied as URLs by data.
func (a *adapter) recordChunkTool(has func(string) bool) {
	type route struct{ path, cap string }
	routes := map[string]route{"task": {"tasks/", "tasks"}, "task_attempts": {"tasks/", "tasks"}, "flow": {"flows/", "manual_flows"}, "run": {"task-runs/", "manual_cli_execution"}, "job": {"jobs/", "official_check_jobs"}}
	var kinds []string
	for _, kind := range []string{"task", "task_attempts", "flow", "run", "job"} {
		if has(routes[kind].cap) {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind", "id"}, "properties": map[string]any{
		"kind": map[string]any{"type": "string", "enum": kinds}, "id": map[string]any{"type": "string"},
		"offset": map[string]any{"type": "integer", "minimum": 0}, "max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": 8192},
		"expected_sha256": map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
	}}
	mcp.AddTool(a.s, &mcp.Tool{Name: "record_read_chunk", InputSchema: schema, Description: "Read an exact authorized task/attempt-history/Flow/Run/Job JSON record as bounded base64 byte chunks. Every chunk rereads current authority; nonzero offset requires the original sha256. Reassemble and verify sha256/total_bytes. A changed record requires discarding earlier chunks and restarting; this does not change operation, input, attempt or fence.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Kind           string `json:"kind"`
		ID             string `json:"id"`
		Offset         int    `json:"offset,omitempty"`
		MaxBytes       int    `json:"max_bytes,omitempty"`
		ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		target, ok := routes[in.Kind]
		if !ok || !has(target.cap) || !ids.ID(in.ID).Valid() {
			return readError("enabled record kind and exact ULID required")
		}
		if in.MaxBytes == 0 {
			in.MaxBytes = 4096
		}
		if in.Offset < 0 || in.MaxBytes < 1 || in.MaxBytes > 8192 || in.Offset > 0 && in.ExpectedSHA256 == "" {
			return readError("invalid chunk budget or missing expected_sha256 for continuation")
		}
		path := "/api/v1/" + target.path + in.ID
		if in.Kind == "task_attempts" {
			path += "/attempts"
		}
		out, err := a.c.Do(ctx, http.MethodGet, path, nil, client.Options{})
		if err != nil {
			r, e := toolResult(out, err)
			return r, nil, e
		}
		sum := sha256.Sum256(out.Body)
		digest := hex.EncodeToString(sum[:])
		if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != digest {
			r, e := toolResult(client.Response{}, errcode.New(errcode.PreconditionFailed, "record changed; discard earlier chunks and restart at offset zero"))
			return r, nil, e
		}
		if in.Offset > len(out.Body) {
			return readError("offset exceeds full record bytes")
		}
		end := min(in.Offset+in.MaxBytes, len(out.Body))
		page := map[string]any{"kind": in.Kind, "id": in.ID, "encoding": "base64", "data": base64.StdEncoding.EncodeToString(out.Body[in.Offset:end]), "offset": in.Offset, "total_bytes": len(out.Body), "sha256": digest, "complete": end == len(out.Body)}
		if end < len(out.Body) {
			page["next_offset"] = end
		}
		raw, _ := json.Marshal(page)
		r, e := toolResult(client.Response{Body: raw}, nil)
		return r, nil, e
	})
}
