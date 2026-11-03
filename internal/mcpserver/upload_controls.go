package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type uploadPageArgs struct {
	UploadID       string `json:"upload_id"`
	SHA256         string `json:"sha256,omitempty"`
	Offset         int    `json:"offset,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

func (a *adapter) uploadControlTools(has func(string) bool) error {
	if has("commit_version") {
		if err := a.commandTool("resource_commit", "uploads/{upload_id}/commit", "Commit the exact existing upload with its original saved idempotency key and frozen request. description_pending is already committed; keep original state/keys and reconcile before retry. Cannot approve or publish.", http.MethodPost, []string{"upload_id"}, false); err != nil {
			return err
		}
	}
	if !has("uploads") {
		return nil
	}
	for _, name := range []string{"upload_status", "upload_parts"} {
		mcp.AddTool(a.s, &mcp.Tool{Name: name, Description: "Read current authorized status of an original upload without transfer URLs or tokens. upload_status pages file metadata (max20); upload_parts pages received part indexes (max500) for an exact file SHA. Continue with next_offset and original status sha256; changed status requires replacing prior pages. Every page rechecks current authority.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in uploadPageArgs) (*mcp.CallToolResult, any, error) {
			if !ids.ID(in.UploadID).Valid() {
				return readError("original upload ULID required")
			}
			if in.Limit == 0 {
				if name == "upload_parts" {
					in.Limit = 100
				} else {
					in.Limit = 10
				}
			}
			maxLimit := 20
			if name == "upload_parts" {
				maxLimit = 500
			}
			if in.Offset < 0 || in.Limit < 1 || in.Limit > maxLimit || in.Offset > 0 && in.ExpectedSHA256 == "" {
				return readError("invalid upload page budget or missing expected_sha256")
			}
			out, err := a.c.Do(ctx, http.MethodGet, "/api/v1/uploads/"+in.UploadID, nil, client.Options{})
			if err != nil {
				r, e := toolResult(out, err)
				return r, nil, e
			}
			var upload client.Upload
			if err := out.Decode(&upload); err != nil {
				r, e := toolResult(client.Response{}, err)
				return r, nil, e
			}
			if upload.UploadID != in.UploadID {
				return readError("upload status identity mismatch")
			}
			// Transfer templates are deliberately excluded even from snapshot hash
			// input, so a refreshed grant cannot invalidate unchanged part status.
			type safeFile struct {
				SHA256        string `json:"sha256"`
				Size          int64  `json:"size"`
				PartSize      int64  `json:"part_size"`
				PartCount     int    `json:"part_count"`
				State         string `json:"state"`
				ReceivedParts []int  `json:"received_parts"`
			}
			files := make([]safeFile, 0, len(upload.Files))
			for _, f := range upload.Files {
				files = append(files, safeFile{f.SHA256, f.Size, f.PartSize, f.PartCount, f.State, f.ReceivedParts})
			}
			raw, _ := json.Marshal(struct {
				ID, Operation, State string
				Files                []safeFile
			}{upload.UploadID, upload.OperationID, upload.State, files})
			sum := sha256.Sum256(raw)
			hash := hex.EncodeToString(sum[:])
			if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != hash {
				r, e := toolResult(client.Response{}, errcode.New(errcode.PreconditionFailed, "upload status changed; replace prior pages"))
				return r, nil, e
			}
			page := map[string]any{"upload_id": upload.UploadID, "operation_id": upload.OperationID, "state": upload.State, "sha256": hash, "offset": in.Offset}
			if name == "upload_status" {
				if in.SHA256 != "" || in.Offset > len(files) {
					return readError("file status page requires no file selector and an offset within files")
				}
				end := min(in.Offset+in.Limit, len(files))
				items := make([]map[string]any, 0, end-in.Offset)
				for _, f := range files[in.Offset:end] {
					items = append(items, map[string]any{"sha256": f.SHA256, "size": f.Size, "part_size": f.PartSize, "part_count": f.PartCount, "state": f.State, "received_parts_count": len(f.ReceivedParts)})
				}
				page["items"], page["total_files"], page["complete"] = items, len(files), end == len(files)
				if end < len(files) {
					page["next_offset"] = end
				}
			} else {
				if !digest.ValidHex(in.SHA256) {
					return readError("exact uploaded file SHA256 required")
				}
				var parts []int
				found := false
				for _, f := range files {
					if f.SHA256 == in.SHA256 {
						parts = f.ReceivedParts
						found = true
						break
					}
				}
				if !found || in.Offset > len(parts) {
					return readError("unknown file or offset exceeds received parts")
				}
				if parts == nil {
					parts = []int{}
				}
				end := min(in.Offset+in.Limit, len(parts))
				page["file_sha256"], page["received_parts"], page["total_parts_received"], page["complete"] = in.SHA256, parts[in.Offset:end], len(parts), end == len(parts)
				if end < len(parts) {
					page["next_offset"] = end
				}
			}
			raw, _ = json.Marshal(page)
			r, e := toolResult(client.Response{Body: raw}, nil)
			return r, nil, e
		})
	}
	mcp.AddTool(a.s, &mcp.Tool{Name: "upload_cancel", Description: "Cancel an existing upload by its original upload identity. The ID is the retry identity; no new request key/state is created. Recheck the original upload/operation afterward, especially after an unknown response.", Annotations: &mcp.ToolAnnotations{IdempotentHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		UploadID string `json:"upload_id"`
	}) (*mcp.CallToolResult, any, error) {
		if !ids.ID(in.UploadID).Valid() {
			return readError("original upload ULID required")
		}
		out, err := a.c.Do(ctx, http.MethodDelete, "/api/v1/uploads/"+in.UploadID, nil, client.Options{})
		if err != nil {
			r, e := toolResult(out, err)
			return r, nil, e
		}
		raw, _ := json.Marshal(map[string]any{"upload_id": in.UploadID, "request_accepted": true, "recheck_required": true})
		r, e := toolResult(client.Response{Body: raw}, nil)
		return r, nil, e
	})
	mcp.AddTool(a.s, &mcp.Tool{Name: "upload_check", Description: "Check at most100 selected SHA/size blob grants against the exact existing upload. This returns granted/upload_required facts only; it does not upload, commit, mint human proof or replace original state.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		UploadID string                  `json:"upload_id"`
		Request  apiv1.CheckBlobsRequest `json:"request"`
	}) (*mcp.CallToolResult, any, error) {
		if !ids.ID(in.UploadID).Valid() || len(in.Request.Files) > 100 {
			return readError("original upload ULID and at most100 selected files required")
		}
		for _, f := range in.Request.Files {
			if !digest.ValidHex(f.Sha256) || f.Size < 0 {
				return readError("exact file SHA256 and nonnegative size required")
			}
		}
		out, err := a.c.Do(ctx, http.MethodPost, "/api/v1/uploads/"+in.UploadID+"/check", in.Request, client.Options{})
		if err != nil {
			r, e := toolResult(out, err)
			return r, nil, e
		}
		var result apiv1.BlobStatusPage
		if err := out.Decode(&result); err != nil {
			r, e := toolResult(client.Response{}, err)
			return r, nil, e
		}
		raw, _ := json.Marshal(result)
		r, e := toolResult(client.Response{Body: raw}, nil)
		return r, nil, e
	})
	return nil
}
