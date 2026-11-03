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
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type collaborationReadRoute struct {
	name, path, cap   string
	project, id, list bool
}

var collaborationReadRoutes = map[string]collaborationReadRoute{
	"review_target":         {"review_target_read", "review-targets/{id}", "human_review", false, true, false},
	"trash_entry":           {"trash_read", "trash/{id}", "lifecycle", true, true, false},
	"milestones":            {"milestone_list", "projects/{project_id}/milestones", "milestones", true, false, true},
	"milestone_progress":    {"milestone_progress", "projects/{project_id}/milestones/{id}/progress", "milestones", true, true, false},
	"extension_packages":    {"extension_packages", "extensions/packages", "extension_governance", false, false, true},
	"extension_enablements": {"extension_enablements", "extensions/enablements", "extension_governance", false, false, true},
	"extension_commands":    {"extension_commands", "extensions/commands", "extension_cli_commands", false, false, true},
}

func collaborationReadPath(route collaborationReadRoute, project, id string) (string, error) {
	if route.project && !ids.ID(project).Valid() || !route.project && project != "" || route.id && !ids.ID(id).Valid() || !route.id && id != "" {
		return "", errcode.New(errcode.SchemaInvalid, "exact required scope identifiers must match this record kind")
	}
	path := "/api/v1/" + strings.ReplaceAll(strings.ReplaceAll(route.path, "{id}", id), "{project_id}", project)
	if route.project && !strings.Contains(route.path, "{project_id}") {
		path += "?project_id=" + url.QueryEscape(project)
	}
	return path, nil
}

func (a *adapter) collaborationReadTools(has func(string) bool) {
	var kinds []string
	for _, kind := range []string{"review_target", "trash_entry", "milestones", "milestone_progress", "extension_packages", "extension_enablements", "extension_commands"} {
		route := collaborationReadRoutes[kind]
		if !has(route.cap) {
			continue
		}
		kinds = append(kinds, kind)
		if route.list {
			mcp.AddTool(a.s, &mcp.Tool{Name: route.name, Description: "Page the current authorized " + kind + " snapshot, binding continuation offsets to its full SHA256. Changed snapshots require replacing prior pages. Every page rechecks live policy; HumanOnly extension governance can still return FORBIDDEN. Use collaboration_read_chunk when one exact item exceeds the result budget.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
				ProjectID      string `json:"project_id,omitempty"`
				Offset         int    `json:"offset,omitempty"`
				Limit          int    `json:"limit,omitempty"`
				ExpectedSHA256 string `json:"expected_sha256,omitempty"`
			}) (*mcp.CallToolResult, any, error) {
				if in.Limit == 0 {
					in.Limit = 10
				}
				if in.Offset < 0 || in.Limit < 1 || in.Limit > 20 || in.Offset > 0 && in.ExpectedSHA256 == "" {
					return readError("invalid page budget or missing original expected_sha256")
				}
				path, err := collaborationReadPath(route, in.ProjectID, "")
				if err != nil {
					r, err := toolResult(client.Response{}, err)
					return r, nil, err
				}
				if has("bounded_collaboration_snapshots") {
					q := url.Values{"pagination": {"explicit"}, "offset": {strconv.Itoa(in.Offset)}, "limit": {strconv.Itoa(in.Limit)}, "expected_sha256": {in.ExpectedSHA256}}
					out, err := a.c.Do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, client.Options{})
					r, err := toolResult(out, err)
					return r, nil, err
				}
				out, err := a.c.Do(ctx, http.MethodGet, path, nil, client.Options{})
				if err != nil {
					r, err := toolResult(out, err)
					return r, nil, err
				}
				sum := sha256.Sum256(out.Body)
				digest := hex.EncodeToString(sum[:])
				if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != digest {
					r, err := toolResult(client.Response{}, errcode.New(errcode.PreconditionFailed, "snapshot changed; replace prior pages"))
					return r, nil, err
				}
				var snapshot struct {
					Items []json.RawMessage `json:"items"`
				}
				if err := out.Decode(&snapshot); err != nil || snapshot.Items == nil {
					return readError("server omitted complete snapshot items")
				}
				if in.Offset > len(snapshot.Items) {
					return readError("offset exceeds full snapshot")
				}
				end := min(in.Offset+in.Limit, len(snapshot.Items))
				for {
					page := map[string]any{"kind": kind, "project_id": in.ProjectID, "items": snapshot.Items[in.Offset:end], "offset": in.Offset, "total_items": len(snapshot.Items), "sha256": digest, "complete": end == len(snapshot.Items)}
					if end < len(snapshot.Items) {
						page["next_offset"] = end
					}
					raw, _ := json.Marshal(page)
					r, err := toolResult(client.Response{Body: raw}, nil)
					if err != nil || !r.IsError || end <= in.Offset+1 {
						return r, nil, err
					}
					end--
				}
			})
		} else {
			mcp.AddTool(a.s, &mcp.Tool{Name: route.name, Description: "Read exact authorized " + kind + " under current domain policy. Large records use collaboration_read_chunk; stored review/trash/milestone state cannot authorize a new action.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
				ProjectID string `json:"project_id,omitempty"`
				ID        string `json:"id"`
			}) (*mcp.CallToolResult, any, error) {
				path, err := collaborationReadPath(route, in.ProjectID, in.ID)
				if err != nil {
					r, err := toolResult(client.Response{}, err)
					return r, nil, err
				}
				out, err := a.c.Do(ctx, http.MethodGet, path, nil, client.Options{})
				r, err := toolResult(out, err)
				return r, nil, err
			})
		}
	}
	if len(kinds) == 0 {
		return
	}
	input := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"kind"}, "properties": map[string]any{"kind": map[string]any{"type": "string", "enum": kinds}, "project_id": map[string]any{"type": "string"}, "id": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 0}, "max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": 8192}, "expected_sha256": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}}}
	mcp.AddTool(a.s, &mcp.Tool{Name: "collaboration_read_chunk", InputSchema: input, Description: "Read only the listed authorized review/trash/milestone/extension objects as bounded base64 JSON chunks. Each call rechecks live authority. Nonzero offsets require original expected_sha256; changed snapshots require replacing prior chunks. Verify whole SHA256/total_bytes before parsing. No URL, HumanGrant or executable input exists.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		Kind           string `json:"kind"`
		ProjectID      string `json:"project_id,omitempty"`
		ID             string `json:"id,omitempty"`
		Offset         int    `json:"offset,omitempty"`
		MaxBytes       int    `json:"max_bytes,omitempty"`
		ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		route, ok := collaborationReadRoutes[in.Kind]
		if !ok || !has(route.cap) {
			return readError("enabled collaboration record kind required")
		}
		if in.MaxBytes == 0 {
			in.MaxBytes = 4096
		}
		if in.Offset < 0 || in.MaxBytes < 1 || in.MaxBytes > 8192 || in.Offset > 0 && in.ExpectedSHA256 == "" {
			return readError("invalid byte budget or missing expected_sha256")
		}
		path, err := collaborationReadPath(route, in.ProjectID, in.ID)
		if err != nil {
			r, err := toolResult(client.Response{}, err)
			return r, nil, err
		}
		if route.list && has("bounded_collaboration_snapshots") {
			q := url.Values{"pagination": {"chunk"}, "offset": {strconv.Itoa(in.Offset)}, "max_bytes": {strconv.Itoa(in.MaxBytes)}, "expected_sha256": {in.ExpectedSHA256}}
			out, err := a.c.Do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		}
		out, err := a.c.Do(ctx, http.MethodGet, path, nil, client.Options{})
		if err != nil {
			r, err := toolResult(out, err)
			return r, nil, err
		}
		sum := sha256.Sum256(out.Body)
		digest := hex.EncodeToString(sum[:])
		if in.ExpectedSHA256 != "" && in.ExpectedSHA256 != digest {
			r, err := toolResult(client.Response{}, errcode.New(errcode.PreconditionFailed, "object changed; replace prior chunks"))
			return r, nil, err
		}
		if in.Offset > len(out.Body) {
			return readError("offset exceeds full object")
		}
		end := min(in.Offset+in.MaxBytes, len(out.Body))
		page := map[string]any{"kind": in.Kind, "project_id": in.ProjectID, "id": in.ID, "offset": in.Offset, "encoding": "base64", "data": base64.StdEncoding.EncodeToString(out.Body[in.Offset:end]), "sha256": digest, "total_bytes": len(out.Body), "complete": end == len(out.Body)}
		if end < len(out.Body) {
			page["next_offset"] = end
		}
		raw, _ := json.Marshal(page)
		r, err := toolResult(client.Response{Body: raw}, nil)
		return r, nil, err
	})
}
