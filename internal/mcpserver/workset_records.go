package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
)

func (a *adapter) worksetRecordDiscovery(has func(string) bool) {
	if !has("search") {
		return
	}
	mcp.AddTool(a.s, &mcp.Tool{Name: "workset_record_list", Description: "List authorized private workset config records for an explicit project, at most20 per page. Follow next_cursor; open an exact version separately. This metadata list is not proof of file availability or permission to execute a project.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProjectID string `json:"project_id"`
		After     string `json:"after,omitempty"`
		Limit     int    `json:"limit,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		if in.Limit == 0 {
			in.Limit = 10
		}
		if in.Limit < 1 || in.Limit > 20 {
			return readError("workset record list limit must be1..20")
		}
		out, err := a.c.ListManagementRecords(ctx, in.ProjectID, "workset", in.After, in.Limit)
		r, e := toolResult(out, err)
		return r, nil, e
	})
}

type worksetRecordArgs struct {
	InputFile    string `json:"input_file"`
	OutputFile   string `json:"output_file"`
	Directory    string `json:"directory,omitempty"`
	StateFile    string `json:"state_file,omitempty"`
	OriginalFile string `json:"original_file,omitempty"`
}

func (a *adapter) worksetRecordTools() {
	for _, action := range []string{"workset_record_prepare", "workset_record_save", "workset_record_open", "workset_return_prepare", "workset_return_apply", "workset_adopt_prepare"} {
		mcp.AddTool(a.s, &mcp.Tool{Name: action, Description: "Operate exact fixed-workset records or return preparations from private input_file/output_file in the explicit workspace. Prepare/adopt only freeze private input; save/apply perform original Push with original state_file. Return never revises old workset references. Explicit adoption verifies actual core return identity/files/current purpose and prepares a new record version; save separately. Source changes, stale base, description_pending or tightened purpose cannot silently pass. No native app runs, human proof or permission expansion.", Annotations: &mcp.ToolAnnotations{IdempotentHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in worksetRecordArgs) (*mcp.CallToolResult, any, error) {
			return a.executeWorksetRecord(ctx, action, in)
		})
	}
}
func (a *adapter) executeWorksetRecord(ctx context.Context, action string, in worksetRecordArgs) (*mcp.CallToolResult, any, error) {
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
	outputPath, err := a.localPath(in.OutputFile, true)
	if err != nil {
		return fail(err)
	}
	protected := []string{input}
	directory, state := "", ""
	if action != "workset_record_prepare" {
		directory, err = a.localDirectory(in.Directory)
		if err != nil {
			return fail(err)
		}
		protected = append(protected, directory)
	}
	if action == "workset_record_save" || action == "workset_return_apply" {
		state, err = a.localPath(in.StateFile, true)
		if err != nil {
			return fail(err)
		}
		protected = append(protected, state)
	}
	output, err := client.OpenBatchPrivateOutput(outputPath, protected...)
	if err != nil {
		return fail(err)
	}
	defer output.Close()
	var value any
	summary := map[string]any{}
	immutable := action == "workset_record_prepare" || action == "workset_return_prepare" || action == "workset_adopt_prepare"
	load := func(path string, value any) error { return a.loadBatchPrivate(path, value) }
	switch action {
	case "workset_record_prepare":
		var spec client.WorksetRecordSpec
		if err := load(input, &spec); err != nil {
			return fail(err)
		}
		var prepared client.PreparedManagementRecord
		prepared, err = a.c.PrepareWorksetRecord(ctx, spec)
		value = prepared
		if err == nil {
			summary["schema"], summary["record_sha256"], summary["record_bytes"], summary["entries"] = prepared.Schema, prepared.SHA256, prepared.Size, len(spec.Input.Entries)
		}
	case "workset_record_save":
		var prepared client.PreparedManagementRecord
		if err := load(input, &prepared); err != nil {
			return fail(err)
		}
		if prepared.Spec.Kind != "workset" {
			return readError("preparation must be an exact workset record")
		}
		var response client.Response
		response, err = a.c.SaveManagementRecord(ctx, prepared, directory, state)
		value = json.RawMessage(response.Body)
	case "workset_record_open":
		var ref struct {
			AssetID   string `json:"asset_id"`
			VersionID string `json:"version_id"`
		}
		if err := load(input, &ref); err != nil {
			return fail(err)
		}
		var record client.ManagementRecordRead
		var stored client.ManagedWorkset
		record, stored, err = a.c.ReadWorksetRecord(ctx, ref.AssetID, ref.VersionID, directory)
		value = stored
		if err == nil {
			summary["schema"], summary["asset_id"], summary["version_id"], summary["record_sha256"], summary["entries"], summary["verification_scope"] = stored.Schema, ref.AssetID, ref.VersionID, record.SHA256, len(stored.Input.Entries), "record_bytes_and_current_fixed_manifest_metadata"
		}
	case "workset_return_prepare":
		var spec client.WorksetReturnSpec
		if err := load(input, &spec); err != nil {
			return fail(err)
		}
		originalPath := outputPath
		if in.OriginalFile != "" {
			originalPath, err = a.localPath(in.OriginalFile, false)
			if err != nil {
				return fail(err)
			}
		}
		var original *client.WorksetReturn
		if _, err := os.Stat(originalPath); err == nil {
			var prior client.WorksetReturn
			if err := load(originalPath, &prior); err != nil {
				return fail(err)
			}
			original = &prior
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		var prepared client.WorksetReturn
		prepared, err = a.c.PrepareWorksetReturn(ctx, spec, directory, original)
		value = prepared
		if err == nil {
			summary["schema"], summary["asset_id"], summary["base_version_id"], summary["files"], summary["purpose"] = prepared.Schema, prepared.Entry.AssetID, prepared.Entry.VersionID, len(prepared.Input.Content.Files), prepared.Purpose
		}
	case "workset_return_apply":
		var prepared client.WorksetReturn
		if err := load(input, &prepared); err != nil {
			return fail(err)
		}
		var response client.Response
		response, err = a.c.ApplyWorksetReturn(ctx, prepared, directory, state)
		value = json.RawMessage(response.Body)
	case "workset_adopt_prepare":
		var spec client.WorksetAdoption
		if err := load(input, &spec); err != nil {
			return fail(err)
		}
		var prepared client.PreparedManagementRecord
		prepared, err = a.c.PrepareWorksetAdoption(ctx, spec, directory)
		value = prepared
		if err == nil {
			summary["schema"], summary["record_asset_id"], summary["base_record_version_id"], summary["record_sha256"], summary["record_bytes"] = prepared.Schema, prepared.Spec.AssetID, prepared.Spec.BaseVersionID, prepared.SHA256, prepared.Size
		}
	}
	if err != nil {
		return fail(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fail(err)
	}
	raw = append(raw, '\n')
	if err := a.checkBatchWorkspace(); err != nil {
		return fail(err)
	}
	if immutable {
		err = output.WriteOriginal(raw)
	} else {
		err = output.Write(raw)
	}
	if err != nil {
		return fail(err)
	}
	if action == "workset_record_save" || action == "workset_return_apply" {
		// Commit receipts contain fixed identities only and retain the existing
		// description_pending notice through the shared result formatter.
		var receipt map[string]any
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return fail(err)
		}
		summary = receipt
	}
	sum := sha256.Sum256(raw)
	summary["receipt_file"], summary["receipt_sha256"], summary["receipt_bytes"] = in.OutputFile, hex.EncodeToString(sum[:]), len(raw)
	raw, _ = json.Marshal(summary)
	r, e := toolResult(client.Response{Body: raw}, nil)
	return r, nil, e
}
