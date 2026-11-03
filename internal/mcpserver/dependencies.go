package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func (a *adapter) dependencyTools() {
	mcp.AddTool(a.s, &mcp.Tool{Name: "dependency_analyze", Description: "Analyze only explicitly selected OBJ/MTL/ComfyUI text in directory from a private input_file {paths,comfy_input_root?}. Offline: no referenced files/URLs/scripts/models are loaded. Returns the full report as bounded base64 JSON chunks; every chunk re-reads selected files. Follow next_offset with original expected_sha256, reassemble and verify total_bytes/hash before parsing. analysis_complete covers supported statements only, not project readiness or missing files. No workset mapping or writes are automatic.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		InputFile      string `json:"input_file"`
		Directory      string `json:"directory"`
		Offset         int    `json:"offset,omitempty"`
		MaxBytes       int    `json:"max_bytes,omitempty"`
		ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		fail := func(err error) (*mcp.CallToolResult, any, error) {
			r, e := toolResult(client.Response{}, err)
			return r, nil, e
		}
		check := a.checkBatchWorkspace
		if err := check(); err != nil {
			return fail(err)
		}
		if in.MaxBytes == 0 {
			in.MaxBytes = 4096
		}
		if in.Offset < 0 || in.MaxBytes < 1 || in.MaxBytes > 8192 || in.Offset > 0 && in.ExpectedSHA256 == "" {
			return readError("invalid offset/chunk budget or missing expected_sha256")
		}
		input, err := a.localPath(in.InputFile, false)
		if err != nil {
			return fail(err)
		}
		raw, err := a.readBatchPrivate(input)
		if err != nil {
			return fail(err)
		}
		if err := canonjson.Validate(raw); err != nil {
			return readError("selection must be unambiguous JSON")
		}
		var selection client.DependencySelection
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&selection); err != nil {
			return readError("invalid explicit dependency selection")
		}
		_, err = a.localDirectory(in.Directory)
		if err != nil {
			return fail(err)
		}
		workspace, err := os.OpenRoot(a.workspace)
		if err != nil {
			return fail(err)
		}
		defer workspace.Close()
		identity, err := workspace.Stat(".")
		if err != nil || a.workspaceIdentity == nil || !os.SameFile(identity, a.workspaceIdentity) {
			return fail(errcode.New(errcode.PathConflict, "configured workspace replaced"))
		}
		report, err := client.AnalyzeDependencyWorkspace(ctx, selection, workspace, in.Directory)
		if err != nil {
			return fail(err)
		}
		if err := check(); err != nil {
			return fail(err)
		}
		raw, err = json.Marshal(report)
		if err != nil {
			return fail(err)
		}
		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		if in.ExpectedSHA256 != "" && digest != in.ExpectedSHA256 {
			return fail(errcode.New(errcode.PreconditionFailed, "selected bytes or mapping changed; replace all report chunks"))
		}
		if in.Offset > len(raw) {
			return readError("offset exceeds full analysis report")
		}
		end := min(in.Offset+in.MaxBytes, len(raw))
		page := map[string]any{"schema": report.Schema, "scope": report.Scope, "analysis_complete": report.Complete, "files": len(report.Files), "missing_selected_files": len(report.MissingSelectedFiles), "encoding": "base64", "data": base64.StdEncoding.EncodeToString(raw[in.Offset:end]), "sha256": digest, "total_bytes": len(raw), "offset": in.Offset, "complete": end == len(raw)}
		if end < len(raw) {
			page["next_offset"] = end
		}
		raw, _ = json.Marshal(page)
		r, e := toolResult(client.Response{Body: raw}, nil)
		return r, nil, e
	})
}
