package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func (a *adapter) collaborationTools(has func(string) bool) error {
	for _, tool := range []struct{ name, path, cap, description string }{
		{"review_submit", "review-targets", "human_review", "Submit a fixed review target under existing authority and original key. Submission is not approval; native evidence and current rights remain required."},
		{"review_publish", "publications", "human_review", "Publish only against an already effective original review, current rights and fixed context revision. Model success, labels or QA reports cannot approve a target."},
		{"trash_own", "trash/own", "lifecycle", "Recycle the caller's own eligible resource under current role, quota and object-state checks, preserving the original key. Cannot purge, place holds or invoke human governance."},
		{"trash_restore", "trash/restore", "lifecycle", "Restore an eligible recycled resource using the exact trash entry and original key. Recheck current role, quota, purpose and bytes afterward; no historical grant is revived."},
	} {
		if has(tool.cap) {
			if err := a.postTool(tool.name, tool.path, tool.description); err != nil {
				return err
			}
		}
	}
	if has("milestones") {
		if err := a.commandTool("milestone_set", "projects/{project_id}/milestones", "Conditionally create or revise an authorized milestone with the original key and expected_revision. The owner identity port retains configuration; progress is read from current task facts, not a second task ledger.", http.MethodPost, []string{"project_id"}, false); err != nil {
			return err
		}
	}
	if has("lifecycle") {
		if err := a.bodyReadTool("trash_preview", "trash/preview", "Preview an authorized ordinary trash selector through current core policy. This performs no trash mutation and grants no purge/hold authority."); err != nil {
			return err
		}
	}
	if has("inbox") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "inbox_mark_read", Description: "Mark only the caller's authorized inbox through an observed sequence. Preserves the current principal/project scope and does not acknowledge unseen future events.", Annotations: &mcp.ToolAnnotations{IdempotentHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ProjectID string `json:"project_id"`
			Through   int64  `json:"through"`
		}) (*mcp.CallToolResult, any, error) {
			if !ids.ID(in.ProjectID).Valid() || in.Through < 0 {
				return readError("project ULID and nonnegative observed through required")
			}
			out, err := a.c.Do(ctx, http.MethodPost, "/api/v1/inbox/read", in, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
	if has("filtered_events") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "resync_read", Description: "Read the original authorized project snapshot after cursor expiry or replace_required. Accumulate every page, including empty continued pages, then replace the entire prior scope and replay events after the fixed replay_after. Scope changes/expired cursors require discarding partial snapshots.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ProjectID string `json:"project_id"`
			Cursor    string `json:"cursor,omitempty"`
			Limit     int    `json:"limit,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			if in.Limit == 0 {
				in.Limit = 10
			}
			if !ids.ID(in.ProjectID).Valid() || in.Limit < 1 || in.Limit > 20 {
				return readError("project ULID and limit 1-20 required")
			}
			q := url.Values{"project_id": {in.ProjectID}, "cursor": {in.Cursor}, "limit": {strconv.Itoa(in.Limit)}}
			out, err := a.c.Do(ctx, http.MethodGet, "/api/v1/resync?"+q.Encode(), nil, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
	a.collaborationReadTools(has)
	return nil
}

// Only fixed read-only POST routes use this helper; it does not add an
// artificial idempotency ledger to previews or permit arbitrary URLs.
func (a *adapter) bodyReadTool(name, path, description string) error {
	route := a.schema["paths"].(map[string]any)["/api/v1/"+path].(map[string]any)["post"].(map[string]any)
	request := route["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	input, err := a.localSchema(map[string]any{"type": "object", "additionalProperties": false, "required": []string{"request"}, "properties": map[string]any{"request": request}})
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	uri := "urn:lantai:mcp:" + name
	if err := compiler.AddResource(uri, input); err != nil {
		return err
	}
	validator, err := compiler.Compile(uri)
	if err != nil {
		return err
	}
	a.s.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: input, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if len(req.Params.Arguments) > MaxRequestBytes {
			return toolResult(client.Response{}, errcode.New(errcode.SchemaInvalid, "MCP request exceeds size limit"))
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil || validator.Validate(value) != nil {
			return toolResult(client.Response{}, errcode.New(errcode.SchemaInvalid, "arguments do not match the public REST contract"))
		}
		var in struct {
			Request json.RawMessage `json:"request"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
			return toolResult(client.Response{}, err)
		}
		out, err := a.c.Do(ctx, http.MethodPost, "/api/v1/"+path, in.Request, client.Options{})
		return toolResult(out, err)
	})
	return nil
}
