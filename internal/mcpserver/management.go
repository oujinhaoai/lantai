package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func managementResult(value any, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		r, e := toolResult(client.Response{}, err)
		return r, nil, e
	}
	raw, err := json.Marshal(value)
	r, e := toolResult(client.Response{Body: raw}, err)
	return r, nil, e
}

func (a *adapter) managementTools(has func(string) bool) {
	if has("exact_read") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "management_list", Description: "Discover currently authorized private management config records by exact project/kind tags. Follow next_cursor, including empty pages. Discovery is not acceptance.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ProjectID string `json:"project_id"`
			Kind      string `json:"kind"`
			After     string `json:"after,omitempty"`
			Limit     int    `json:"limit,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			if in.Limit == 0 {
				in.Limit = 10
			}
			out, err := a.c.ListManagementRecords(ctx, in.ProjectID, in.Kind, in.After, in.Limit)
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
	if has("exact_read") && a.workspace != "" {
		mcp.AddTool(a.s, &mcp.Tool{Name: "management_read", Description: "Read an exact management record into an existing relative workspace directory, verifying actual record.json bytes and its envelope scope. Returns the complete record when it fits; use management_read_chunk for large payloads. This cannot approve archived evidence.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			AssetID   string `json:"asset_id"`
			VersionID string `json:"version_id"`
			Directory string `json:"directory"`
		}) (*mcp.CallToolResult, any, error) {
			directory, err := a.localDirectory(in.Directory)
			if err != nil {
				return managementResult(nil, err)
			}
			out, err := a.c.ReadManagementRecord(ctx, in.AssetID, in.VersionID, directory)
			return managementResult(out, err)
		})
		mcp.AddTool(a.s, &mcp.Tool{Name: "management_read_chunk", Description: "Read bounded base64 chunks of exact, verified management record.json bytes. Every call rereads live authority and purpose before using the local copy. Continue with next_offset and original expected_sha256; verify total size/SHA before parsing. Cannot revive old epoch evidence.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			AssetID        string `json:"asset_id"`
			VersionID      string `json:"version_id"`
			Directory      string `json:"directory"`
			Offset         int    `json:"offset,omitempty"`
			MaxBytes       int    `json:"max_bytes,omitempty"`
			ExpectedSHA256 string `json:"expected_sha256,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			if in.MaxBytes == 0 {
				in.MaxBytes = 4096
			}
			if in.Offset < 0 || in.MaxBytes < 1 || in.MaxBytes > 8192 || in.Offset > 0 && in.ExpectedSHA256 == "" {
				return readError("invalid record chunk budget or missing original expected_sha256")
			}
			directory, err := a.localDirectory(in.Directory)
			if err != nil {
				return managementResult(nil, err)
			}
			out, err := a.c.ReadManagementRecord(ctx, in.AssetID, in.VersionID, directory)
			if err != nil {
				return managementResult(nil, err)
			}
			if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != out.SHA256 {
				return managementResult(nil, errcode.New(errcode.PreconditionFailed, "record bytes changed; replace prior chunks"))
			}
			if in.Offset > len(out.Data) {
				return readError("offset exceeds exact record bytes")
			}
			end := min(in.Offset+in.MaxBytes, len(out.Data))
			result := map[string]any{"asset_id": out.AssetID, "version_id": out.VersionID, "encoding": "base64", "data": base64.StdEncoding.EncodeToString(out.Data[in.Offset:end]), "offset": in.Offset, "total_bytes": out.Size, "sha256": out.SHA256, "complete": end == len(out.Data)}
			if end < len(out.Data) {
				result["next_offset"] = end
			}
			return managementResult(result, nil)
		})
	}
	if has("uploads") && has("exact_read") && a.workspace != "" {
		for _, name := range []string{"management_batch_create", "management_batch_revise"} {
			mcp.AddTool(a.s, &mcp.Tool{Name: name, Description: "Prepare a private migration-batch record from an explicit draft_file. Original import_plan/report, when present, account for every planned row; pending/unknown remain unresolved. Actor and instance must match this session; timestamps and batch ID are saved caller input. Revision reads exact original bytes and resets acceptance. Save the returned prepared_file with management_save/revise; only management_check can observe current acceptance."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
				DraftFile    string `json:"draft_file"`
				PreparedFile string `json:"prepared_file"`
				AssetID      string `json:"asset_id,omitempty"`
				VersionID    string `json:"version_id,omitempty"`
				Directory    string `json:"directory,omitempty"`
			}) (*mcp.CallToolResult, any, error) {
				source, err := a.localPath(in.DraftFile, false)
				if err != nil {
					return managementResult(nil, err)
				}
				output, err := a.localPath(in.PreparedFile, true)
				if err != nil {
					return managementResult(nil, err)
				}
				var draft client.MigrationBatchDraft
				if err = client.LoadState(source, &draft); err != nil {
					return managementResult(nil, errcode.New(errcode.SchemaInvalid, "draft_file must contain private explicit batch input"))
				}
				var previous *client.ManagementRecordRead
				if name == "management_batch_revise" {
					directory, err := a.localDirectory(in.Directory)
					if err != nil {
						return managementResult(nil, err)
					}
					record, err := a.c.ReadManagementRecord(ctx, in.AssetID, in.VersionID, directory)
					if err != nil {
						return managementResult(nil, err)
					}
					previous = &record
				} else if in.AssetID != "" || in.VersionID != "" || in.Directory != "" {
					return readError("new batch must not contain a revision baseline")
				}
				prepared, err := a.c.PrepareMigrationBatch(ctx, draft, previous)
				if err != nil {
					return managementResult(nil, err)
				}
				if err = client.WriteManagementPreparation(ctx, output, prepared); err != nil {
					return managementResult(nil, err)
				}
				return managementResult(map[string]any{"schema": prepared.Schema, "prepared_file": in.PreparedFile, "sha256": prepared.SHA256, "size": prepared.Size, "kind": "migration", "project_id": prepared.Spec.ProjectID, "complete": true}, nil)
			})
		}
		mcp.AddTool(a.s, &mcp.Tool{Name: "management_prepare", Description: "Prepare a management envelope from a private workspace spec_file and save immutable original input to prepared_file. Does not upload, create remote records or generate approval. Same input is reconstructable; changed existing preparation is a conflict."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			SpecFile     string `json:"spec_file"`
			PreparedFile string `json:"prepared_file"`
		}) (*mcp.CallToolResult, any, error) {
			source, err := a.localPath(in.SpecFile, false)
			if err != nil {
				return managementResult(nil, err)
			}
			output, err := a.localPath(in.PreparedFile, true)
			if err != nil {
				return managementResult(nil, err)
			}
			var spec client.ManagementRecordSpec
			if err = client.LoadState(source, &spec); err != nil {
				return managementResult(nil, errcode.New(errcode.SchemaInvalid, "spec_file must be a private valid management JSON input"))
			}
			prepared, err := a.c.PrepareManagementRecord(ctx, spec)
			if err != nil {
				return managementResult(nil, err)
			}
			if err = client.WriteManagementPreparation(ctx, output, prepared); err != nil {
				return managementResult(nil, err)
			}
			return managementResult(map[string]any{"schema": prepared.Schema, "prepared_file": in.PreparedFile, "sha256": prepared.SHA256, "size": prepared.Size, "kind": spec.Kind, "project_id": spec.ProjectID, "complete": true}, nil)
		})
		for _, name := range []string{"management_save", "management_revise"} {
			mcp.AddTool(a.s, &mcp.Tool{Name: name, Description: "Save original prepared management input using its exact project/instance/baseline and private persistent state. Preserve the same prepared_file and state on unknown results or description_pending; revision requires an explicit original asset/base. Does not approve evidence.", Annotations: &mcp.ToolAnnotations{IdempotentHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
				PreparedFile string `json:"prepared_file"`
				Directory    string `json:"directory"`
				State        string `json:"state"`
			}) (*mcp.CallToolResult, any, error) {
				source, err := a.localPath(in.PreparedFile, false)
				if err != nil {
					return managementResult(nil, err)
				}
				directory, err := a.localDirectory(in.Directory)
				if err != nil {
					return managementResult(nil, err)
				}
				state, err := a.localPath(in.State, true)
				if err != nil {
					return managementResult(nil, err)
				}
				var prepared client.PreparedManagementRecord
				if err = client.LoadState(source, &prepared); err != nil {
					return managementResult(nil, errcode.New(errcode.SchemaInvalid, "prepared_file must contain the original private management preparation"))
				}
				if name == "management_revise" && (prepared.Spec.AssetID == "" || prepared.Spec.BaseVersionID == "") {
					return readError("revision requires an explicitly prepared original asset and base version")
				}
				out, err := a.c.SaveManagementRecord(ctx, prepared, directory, state)
				r, err := toolResult(out, err)
				return r, nil, err
			})
		}
	}
	if has("management_batch_checks") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "management_check", Description: "Recompute acceptance of an exact current migration-batch record through the original core owner ports. Rechecks current permissions, rights, actual files, fixed worksets, restored/native/device evidence and human owner; stored checkboxes or historical checks cannot produce ready.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			AssetID   string `json:"asset_id"`
			VersionID string `json:"version_id"`
		}) (*mcp.CallToolResult, any, error) {
			out, err := a.c.CheckManagementBatch(ctx, in.AssetID, in.VersionID)
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
}
