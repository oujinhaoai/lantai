package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
)

func (a *adapter) derivationTools(has func(string) bool) {
	if !has("exact_read") || a.workspace == "" {
		return
	}
	mcp.AddTool(a.s, &mcp.Tool{Name: "asset_derive", Description: "From explicit private input_file, derive a bounded exact-version file comparison, PNG image thumbnail/proxy, OBJ structure/SVG or fixed-workset TAR. Recheck current source grants/purpose and actual SHA bytes, even with cached files. Input purpose must be archive_review for previews/comparison or raw_export for TAR; never fallback. Saves immutable private artifact_file and receipt output_file outside directory. No upload, source edit, external material fetch, native app, human proof or permission change."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		InputFile    string `json:"input_file"`
		Directory    string `json:"directory"`
		ArtifactFile string `json:"artifact_file"`
		OutputFile   string `json:"output_file"`
	}) (*mcp.CallToolResult, any, error) {
		fail := func(err error) (*mcp.CallToolResult, any, error) {
			r, e := toolResult(client.Response{}, err)
			return r, nil, e
		}
		if err := a.checkBatchWorkspace(); err != nil {
			return fail(err)
		}
		input, err := a.localPath(in.InputFile, false)
		if err != nil {
			return fail(err)
		}
		directory, err := a.localDirectory(in.Directory)
		if err != nil {
			return fail(err)
		}
		artifactPath, err := a.localPath(in.ArtifactFile, true)
		if err != nil {
			return fail(err)
		}
		receiptPath, err := a.localPath(in.OutputFile, true)
		if err != nil {
			return fail(err)
		}
		artifact, err := client.OpenBatchPrivateOutput(artifactPath, input, directory, receiptPath)
		if err != nil {
			return fail(err)
		}
		defer artifact.Close()
		output, err := client.OpenBatchPrivateOutput(receiptPath, input, directory, artifactPath)
		if err != nil {
			return fail(err)
		}
		defer output.Close()
		var spec client.DerivationInput
		if err := a.loadBatchPrivate(input, &spec); err != nil {
			return fail(err)
		}
		receipt, err := a.c.DeriveTo(ctx, spec, directory, artifact)
		if err != nil {
			return fail(err)
		}
		if err := a.checkBatchWorkspace(); err != nil {
			return fail(err)
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			return fail(err)
		}
		if err := output.Write(append(raw, '\n')); err != nil {
			return fail(err)
		}
		summary := map[string]any{"schema": receipt.Schema, "tool": receipt.Tool, "action": receipt.Action, "purpose": receipt.Purpose, "parameters": receipt.Parameters, "input_versions": len(receipt.Inputs), "artifact": receipt.Artifact, "artifact_file": in.ArtifactFile, "receipt_file": in.OutputFile, "current_remote_verified": receipt.CurrentRemoteVerified, "completion_basis": receipt.CompletionBasis, "native_software_continuation_verified": false, "source_retirement_authorized": false}
		raw, err = json.Marshal(summary)
		r, e := toolResult(client.Response{Body: raw}, err)
		return r, nil, e
	})
}
