package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type FileLinkArgs struct {
	AssetID   string `json:"asset_id"`
	VersionID string `json:"version_id"`
	Path      string `json:"path"`
	Purpose   string `json:"purpose"`
}

type BackupLinkArgs struct {
	JobID          string                     `json:"job_id"`
	Approval       client.BackupDownloadInput `json:"approval"`
	IdempotencyKey string                     `json:"idempotency_key"`
}

func (a *adapter) downloadTools(has func(string) bool) error {
	if has("read_grants") {
		schema, err := a.pullInputSchema()
		if err != nil {
			return err
		}
		properties := schema["properties"].(map[string]any)
		delete(properties, "directory")
		properties["path"] = map[string]any{"type": "string"}
		schema["required"] = []string{"asset_id", "version_id", "path", "purpose"}
		mcp.AddTool(a.s, &mcp.Tool{InputSchema: schema, Name: "resource_download_link", Description: "Get an expiring exact-file download URL, SHA256, size and required headers. Authenticate GET with the same session; keep the descriptor private. No file bytes are returned."}, func(ctx context.Context, _ *mcp.CallToolRequest, in FileLinkArgs) (*mcp.CallToolResult, any, error) {
			out, err := a.c.FileDownloadLink(ctx, in.AssetID, in.VersionID, in.Path, in.Purpose)
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
	if has("backup_management") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "backup_read", Description: "Read backup overview, one exact job, schedule, or bounded history. kind=history follows after/next_cursor until complete; changes are a live view, not a frozen snapshot. Requires a current human administrator session; Agent sessions remain forbidden. No backup mutations or human proof issuance.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			JobID string `json:"job_id,omitempty"`
			Kind  string `json:"kind,omitempty"`
			After string `json:"after,omitempty"`
			Limit int    `json:"limit,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			path := "/api/v1/admin/backups"
			if in.Kind != "history" && (in.After != "" || in.Limit != 0) {
				return readError("after and limit apply only to history")
			}
			if in.JobID != "" && in.Kind != "" {
				return readError("use job_id or kind, not both")
			}
			switch in.Kind {
			case "", "overview":
			case "schedule":
				path += "/schedule"
			case "history":
				if !has("bounded_backup_history") {
					return readError("bounded backup history requires an upgraded server")
				}
				if in.Limit == 0 {
					in.Limit = 10
				}
				if in.Limit < 1 || in.Limit > 20 || in.After != "" && !ids.ID(in.After).Valid() {
					return readError("history limit must be1..20 and after an original job ID")
				}
				path += "/history?" + url.Values{"completion": {"explicit"}, "limit": {fmt.Sprint(in.Limit)}, "cursor": {in.After}}.Encode()
			default:
				return readError("backup kind must be overview, schedule or history")
			}
			if in.JobID != "" {
				if !ids.ID(in.JobID).Valid() {
					return readError("backup job ULID required")
				}
				path += "/" + in.JobID
			}
			out, err := a.c.Do(ctx, http.MethodGet, path, nil, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		})
		mcp.AddTool(a.s, &mcp.Tool{Name: "backup_download_link", Description: "Redeem an explicit human approval for one exact backup digest to obtain a 15-minute download descriptor bound to the current normal human administrator session. First redemption uses the approval session; an already successful approval may be replayed by the same human with the same content/key under existing live authorization rules. Agent/delegated sessions are forbidden. Obtain and verify identity.backup_export outside MCP. Does not issue human proof, start backups, restore, or return bytes. Keep the temporary headers private."}, func(ctx context.Context, _ *mcp.CallToolRequest, in BackupLinkArgs) (*mcp.CallToolResult, any, error) {
			out, err := a.c.BackupDownloadLink(ctx, in.JobID, in.Approval, in.IdempotencyKey)
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
	return nil
}
