package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type ResourceFilesArgs struct {
	AssetID   string `json:"asset_id"`
	VersionID string `json:"version_id"`
	Offset    int    `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type ResourceChunkArgs struct {
	Kind           string `json:"kind"`
	AssetID        string `json:"asset_id"`
	VersionID      string `json:"version_id,omitempty"`
	Offset         int    `json:"offset,omitempty"`
	MaxBytes       int    `json:"max_bytes,omitempty"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
}

func exactResourcePath(asset, version string) (string, error) {
	if !ids.ID(asset).Valid() || !ids.ID(version).Valid() {
		return "", errcode.New(errcode.SchemaInvalid, "exact asset/version ULIDs required")
	}
	return "/api/v1/assets/" + asset + "/versions/" + version, nil
}

func (a *adapter) resourceTools(has func(string) bool) {
	if has("projects") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "project_list", Description: "List authorized projects. Follow next_cursor, including empty pages; extra metadata is omitted in brief view.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, a.resourceList("projects"))
		mcp.AddTool(a.s, &mcp.Tool{Name: "project_read", Description: "Read an authorized project by exact project key.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Key  string `json:"key"`
			View string `json:"view,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			if in.Key == "" {
				return readError("project key required")
			}
			if in.View == "" {
				in.View = "brief"
			}
			if in.View != "brief" && in.View != "full" {
				return readError("view must be brief or full")
			}
			out, err := a.c.Do(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(in.Key)+"?view="+in.View, nil, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		})

		mcp.AddTool(a.s, &mcp.Tool{Name: "project_members", Description: "Page current authorized project members with an exact membership revision. Continue using next_offset and expected_revision; changed membership requires replacing prior pages. This does not grant access.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ProjectID        string `json:"project_id"`
			Offset           int    `json:"offset,omitempty"`
			Limit            int    `json:"limit,omitempty"`
			ExpectedRevision *int64 `json:"expected_revision,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			if !ids.ID(in.ProjectID).Valid() {
				return readError("project ULID required")
			}
			if in.Limit == 0 {
				in.Limit = 10
			}
			if in.Offset < 0 || in.Limit < 1 || in.Limit > 20 || in.Offset > 0 && in.ExpectedRevision == nil {
				return readError("invalid membership page or missing expected_revision for continuation")
			}
			out, err := a.c.Do(ctx, http.MethodGet, "/api/v1/identity/projects/"+in.ProjectID+"/members", nil, client.Options{})
			if err != nil {
				r, e := toolResult(out, err)
				return r, nil, e
			}
			var value struct {
				Items    []json.RawMessage `json:"items"`
				Revision *int64            `json:"revision"`
			}
			if err = out.Decode(&value); err != nil || value.Items == nil || value.Revision == nil {
				return readError("server omitted current membership page/revision")
			}
			if in.ExpectedRevision != nil && *in.ExpectedRevision != *value.Revision {
				r, e := toolResult(client.Response{}, errcode.New(errcode.PreconditionFailed, "membership changed; replace prior pages"))
				return r, nil, e
			}
			if in.Offset > len(value.Items) {
				return readError("offset outside membership list")
			}
			end := min(in.Offset+in.Limit, len(value.Items))
			for {
				page := map[string]any{"project_id": in.ProjectID, "revision": *value.Revision, "items": value.Items[in.Offset:end], "offset": in.Offset, "total_items": len(value.Items), "complete": end == len(value.Items)}
				if end < len(value.Items) {
					page["next_offset"] = end
				}
				raw, _ := json.Marshal(page)
				r, e := toolResult(client.Response{Body: raw}, nil)
				if e != nil || !r.IsError || end <= in.Offset+1 {
					return r, nil, e
				}
				end--
			}
		})
	}
	if has("asset_types") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "resource_types", Description: "Discover the installed core asset type definitions.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Type string `json:"type,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			path := "/api/v1/types"
			if in.Type != "" {
				path += "/" + url.PathEscape(in.Type)
			}
			out, err := a.c.Do(ctx, http.MethodGet, path, nil, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
	if has("exact_read") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "resource_current", Description: "Read current asset description and latest reference. This is distinct from the immutable version manifest and current effective rights. Use resource_read_chunk for a large full object.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			AssetID string `json:"asset_id"`
			View    string `json:"view,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			if !ids.ID(in.AssetID).Valid() {
				return readError("asset ULID required")
			}
			if in.View == "" {
				in.View = "full"
			}
			if in.View != "brief" && in.View != "full" {
				return readError("view must be brief or full")
			}
			out, err := a.c.Do(ctx, http.MethodGet, "/api/v1/assets/"+in.AssetID+"?view="+in.View, nil, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		})
		mcp.AddTool(a.s, &mcp.Tool{Name: "resource_versions", Description: "List authorized historical committed versions of one exact asset. Follow next_cursor until absent; version history does not determine current rights.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, a.resourceList("versions"))
		mcp.AddTool(a.s, &mcp.Tool{Name: "resource_files", Description: "Page the complete immutable file manifest for an exact committed version, including paths, SHA256 and sizes. Follow next_offset until complete. Every page rechecks authorization; no file bytes or download grants are returned.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in ResourceFilesArgs) (*mcp.CallToolResult, any, error) {
			path, err := exactResourcePath(in.AssetID, in.VersionID)
			if err != nil {
				r, e := toolResult(client.Response{}, err)
				return r, nil, e
			}
			if in.Limit == 0 {
				in.Limit = 10
			}
			if in.Offset < 0 || in.Limit < 1 || in.Limit > 20 {
				return readError("offset must be nonnegative and limit 1-20")
			}
			out, err := a.c.Do(ctx, http.MethodGet, path+"?view=full", nil, client.Options{})
			if err != nil {
				r, e := toolResult(out, err)
				return r, nil, e
			}
			var value struct {
				Version  json.RawMessage `json:"version"`
				Manifest struct {
					Content struct {
						Files []apiv1.VersionManifestFile `json:"files"`
					} `json:"content"`
					ManifestDigest string `json:"manifest_digest"`
				} `json:"manifest"`
			}
			if err = out.Decode(&value); err != nil {
				r, e := toolResult(client.Response{}, err)
				return r, nil, e
			}
			files := value.Manifest.Content.Files
			if files == nil || in.Offset > len(files) {
				return readError("missing manifest or offset outside exact file list")
			}

			end := min(in.Offset+in.Limit, len(files))
			for {
				page := map[string]any{"version": value.Version, "manifest_digest": value.Manifest.ManifestDigest, "files": files[in.Offset:end], "offset": in.Offset, "total_files": len(files), "complete": end == len(files)}
				if end < len(files) {
					page["next_offset"] = end
				}
				raw, _ := json.Marshal(page)
				result, e := toolResult(client.Response{Body: raw}, nil)
				if e != nil || !result.IsError || end <= in.Offset+1 {
					return result, nil, e
				}
				end--
			}
		})
		mcp.AddTool(a.s, &mcp.Tool{Name: "resource_read_chunk", InputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"kind", "asset_id"},
			"properties": map[string]any{
				"kind":     map[string]any{"type": "string", "enum": []string{"asset", "version", "rights"}},
				"asset_id": map[string]any{"type": "string"}, "version_id": map[string]any{"type": "string"},
				"offset":          map[string]any{"type": "integer", "minimum": 0},
				"max_bytes":       map[string]any{"type": "integer", "minimum": 1, "maximum": 8192},
				"expected_sha256": map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
			}}, Description: "Read a full authorized asset/version/current-rights JSON object in exact base64 byte chunks (default 4096, maximum 8192 bytes). Save the returned sha256 and require it with every nonzero offset; changed objects fail and require replacing prior chunks. Reassemble, then verify total_bytes and sha256 before parsing. Every chunk performs a fresh authorized read. No private local file or arbitrary URL is accepted.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in ResourceChunkArgs) (*mcp.CallToolResult, any, error) {
			if !ids.ID(in.AssetID).Valid() {
				return readError("asset ULID required")
			}
			if in.MaxBytes == 0 {
				in.MaxBytes = 4096
			}
			if in.Offset < 0 || in.MaxBytes < 1 || in.MaxBytes > 8192 || in.Offset > 0 && in.ExpectedSHA256 == "" {
				return readError("offset/max_bytes invalid or expected_sha256 missing for continuation")
			}
			path := "/api/v1/assets/" + in.AssetID
			switch in.Kind {
			case "asset":
				path += "?view=full"
			case "version", "rights":
				var err error
				path, err = exactResourcePath(in.AssetID, in.VersionID)
				if err != nil {
					return readError("exact version ULID required")
				}
				if in.Kind == "rights" {
					path += "/rights"
				} else {
					path += "?view=full"
				}
			default:
				return readError("kind must be asset, version or rights")
			}
			out, err := a.c.Do(ctx, http.MethodGet, path, nil, client.Options{})
			if err != nil {
				r, e := toolResult(out, err)
				return r, nil, e
			}
			sum := sha256.Sum256(out.Body)
			digest := hex.EncodeToString(sum[:])
			if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != digest {
				r, e := toolResult(client.Response{}, errcode.New(errcode.PreconditionFailed, "object changed; discard earlier chunks and restart with offset zero"))
				return r, nil, e
			}
			if in.Offset > len(out.Body) {
				return readError("offset exceeds full object bytes")
			}
			end := min(in.Offset+in.MaxBytes, len(out.Body))
			page := map[string]any{"kind": in.Kind, "asset_id": in.AssetID, "version_id": in.VersionID, "encoding": "base64", "data": base64.StdEncoding.EncodeToString(out.Body[in.Offset:end]), "sha256": digest, "total_bytes": len(out.Body), "offset": in.Offset, "complete": end == len(out.Body)}
			if end < len(out.Body) {
				page["next_offset"] = end
			}
			raw, _ := json.Marshal(page)
			r, e := toolResult(client.Response{Body: raw}, nil)
			return r, nil, e
		})
	}
	if has("current_version_rights") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "rights_read", Description: "Read currently effective rights, assertions and evidence revisions for an exact version. Frozen manifest rights are historical declarations. Use resource_read_chunk kind rights for a large result.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			AssetID   string `json:"asset_id"`
			VersionID string `json:"version_id"`
		}) (*mcp.CallToolResult, any, error) {
			path, err := exactResourcePath(in.AssetID, in.VersionID)
			if err != nil {
				return readError("exact asset/version ULIDs required")
			}
			out, err := a.c.Do(ctx, http.MethodGet, path+"/rights", nil, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
}

func (a *adapter) resourceList(kind string) func(context.Context, *mcp.CallToolRequest, ReadArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ReadArgs) (*mcp.CallToolResult, any, error) {
		if in.Limit == 0 {
			in.Limit = 10
		}
		if in.Limit < 1 || in.Limit > 20 {
			return readError("limit must be 1-20")
		}
		if in.View == "" {
			in.View = "brief"
		}
		if in.View != "brief" && in.View != "full" {
			return readError("view must be brief or full")
		}
		path := "/api/v1/projects"
		if kind == "versions" {
			if !ids.ID(in.AssetID).Valid() {
				return readError("asset ULID required")
			}
			path = "/api/v1/assets/" + in.AssetID + "/versions"
		}
		q := url.Values{"cursor": {in.After}, "limit": {strconv.Itoa(in.Limit)}, "view": {in.View}}
		out, err := a.c.Do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, client.Options{})
		r, err := toolResult(out, err)
		return r, nil, err
	}
}
