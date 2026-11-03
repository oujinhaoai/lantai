// Package mcpserver is a stdio client of the public REST API. It has no access
// to core services, database handles, HumanGrant issuance, or shell execution.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/api"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const MaxResultBytes = 64 << 10
const MaxRequestBytes = 256 << 10

// Contribution is a trusted assembly port for T09. It can alias only a core
// safe write command; it cannot introduce a URL, credentials or executable.
// Package approval/enablement remains with T09; stdio accepts no plugin config.
type Contribution struct {
	ExtensionID string
	Name        string
	CoreTool    string
}
type Config struct {
	Contributions []Contribution
	Client        *client.Client
	Workspace     string
	// ExtensionsRegistry projects explicitly installed local commands.
	ExtensionsRegistry string
}
type adapter struct {
	c                 *client.Client
	s                 *mcp.Server
	schema            map[string]any
	workspace         string
	instanceID        ids.ID
	workspaceIdentity os.FileInfo
}

func New(ctx context.Context, c Config) (*mcp.Server, error) {
	if c.Client == nil {
		return nil, errors.New("mcp: REST client required")
	}
	meta, e := c.Client.Do(ctx, http.MethodGet, "/api/v1/meta", nil, client.Options{})
	if e != nil {
		return nil, e
	}
	var supported struct {
		APIVersion   string   `json:"api_version"`
		InstanceID   ids.ID   `json:"instance_id"`
		Capabilities []string `json:"capabilities"`
	}
	if e = meta.Decode(&supported); e != nil {
		return nil, e
	}
	if supported.APIVersion != "v1" {
		return nil, errors.New("mcp: unsupported REST API version")
	}
	a := &adapter{c: c.Client, s: mcp.NewServer(&mcp.Implementation{Name: "lantai", Version: "0.2.0"}, nil), workspace: c.Workspace, instanceID: supported.InstanceID}
	if e = json.Unmarshal(api.OpenAPI, &a.schema); e != nil {
		return nil, e
	}
	has := func(cap string) bool {
		for _, s := range supported.Capabilities {
			if s == cap {
				return true
			}
		}
		return false
	}
	a.readTools(has)
	a.resourceTools(has)
	if e = a.uploadControlTools(has); e != nil {
		return nil, e
	}
	if has("conditional_metadata") {
		if e = a.commandTool("metadata_set", "assets/{asset_id}/metadata", "Conditionally revise current asset description/profile with exact quoted revision ETag and a caller-saved key. Unknown extra fields remain unchanged unless explicitly replaced; conflicts require rereading and explicit intent. This cannot change security defaults or frozen version content.", http.MethodPatch, []string{"asset_id"}, true); e != nil {
			return nil, e
		}
	}
	if has("current_version_rights") {
		for _, r := range []struct{ name, path, cap, description string }{
			{"rights_evidence", "rights/evidence", "current_version_rights", "Append source evidence through the formal rights evidence contract; QA evidence is a separate action. Preserve the original key and reread current rights revisions."},
			{"rights_assert", "rights/assertions", "current_version_rights", "Apply an authorized monotonic rights restriction/assertion. Server verifies evidence, revision and scope; this cannot relax rights or issue human proof."},
			{"rights_cancel", "rights/cancel", "current_version_rights", "Cancel an assertion only under existing ownership/policy rules. Reread current rights; this does not bypass human-only correction or release."},
		} {
			if e = a.postTool(r.name, r.path, r.description); e != nil {
				return nil, e
			}
		}
	}
	if e = a.collaborationTools(has); e != nil {
		return nil, e
	}
	if e = a.orchestrationTools(has); e != nil {
		return nil, e
	}
	a.worksetRecordDiscovery(has)
	if e = a.downloadTools(has); e != nil {
		return nil, e
	}
	writes := []struct{ name, path, cap, description string }{
		{"task_claim", "tasks/claim", "tasks", "Claim an existing authorized task seat; returns the server lease and fence."},
		{"task_renew", "tasks/renew", "tasks", "Renew the same current task attempt with its expected revision and fence."},
		{"task_release", "tasks/release", "tasks", "Release an existing task attempt with an explicit stop observation."},
		{"task_submit", "tasks/submit", "tasks", "Submit committed output references from the current task attempt. This does not approve or publish them."},
		{"task_block", "tasks/block", "tasks", "Record a task blocker without changing human approval."},
		{"task_handoff", "tasks/handoff", "tasks", "Record an explicit task handoff from the current attempt."},
		{"run_start", "task-runs", "manual_cli_execution", "Register a manual session or resume a persisted checkpoint. Never launches a model."},
		{"run_progress", "task-runs/progress", "manual_cli_execution", "Record cumulative observable usage and step progress; budgets cannot be reset."},
		{"run_tool", "task-runs/tool", "manual_cli_execution", "Persist a tool intent or its observed result. Does not execute the tool."},
		{"run_candidate", "task-runs/candidate", "manual_cli_execution", "Register an already committed candidate. Does not assign a passing check or approval."},
		{"run_checkpoint", "task-runs/checkpoint", "manual_cli_execution", "Save a stopped manual session checkpoint with fixed input and profile."},
		{"run_ask", "task-runs/ask", "manual_cli_execution", "Pause and request human clarification. Answers cannot raise budgets or approve assets."},
		{"run_seal", "task-runs/seal", "manual_cli_execution", "Seal an execution result. Execution success is not task completion or human acceptance."},
		{"qa_append", "evidence", "review_evidence", "Append a QA report through the ledger. Current task identity, subject and independent checker role are verified by the server; this cannot approve assets."},
		{"message_post", "messages", "discussions", "Append a discussion message or proposal with bounded anchors and mentions."},
	}
	for _, r := range writes {
		if has(r.cap) {
			if e = a.postTool(r.name, r.path, r.description); e != nil {
				return nil, e
			}
		}
	}
	if len(c.Contributions) > 32 {
		return nil, errors.New("mcp: too many contributions")
	}
	seen := map[string]bool{}
	for _, contribution := range c.Contributions {
		name, e := contributionName(contribution)
		if e != nil {
			return nil, e
		}
		if seen[name] {
			return nil, errors.New("mcp: duplicate contribution")
		}
		seen[name] = true
		found := false
		for _, r := range writes {
			if r.name == contribution.CoreTool && has(r.cap) {
				found = true
				if e = a.postTool(name, r.path, r.description); e != nil {
					return nil, e
				}
				break
			}
		}
		if !found {
			return nil, errors.New("mcp: contribution must select an enabled safe core command")
		}
	}
	if a.workspace != "" {
		if e = a.localTools(); e != nil {
			return nil, e
		}
		a.batchTools(has)
	}
	a.managementTools(has)
	if c.ExtensionsRegistry != "" && has("extension_cli_commands") {
		if e = a.extensionTools(ctx, c.ExtensionsRegistry); e != nil {
			return nil, e
		}
	}
	return a.s, nil
}
func toolResult(response client.Response, e error) (*mcp.CallToolResult, error) {
	if e != nil {
		code := errcode.Internal
		message := "REST request failed; inspect the original operation before retrying"
		var remote *client.APIError
		var body errcode.Body
		if errors.As(e, &remote) {
			body = remote.Body
		} else if local, ok := errcode.As(e); ok {
			body = local.Envelope("").Error
		} else {
			if errors.Is(e, context.Canceled) {
				message = "request cancelled; reconcile the original idempotency key"
			}
			body = errcode.New(code, message).Envelope("").Error
		}
		b, _ := json.Marshal(errcode.Envelope{Error: body})
		if len(b) > MaxResultBytes/3 {
			body.Message = "error detail exceeds MCP limit; inspect the original operation through REST"
			body.Details = nil
			b, _ = json.Marshal(errcode.Envelope{Error: body})
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: errcode.Envelope{Error: body}}, nil
	}
	if len(response.Body) > MaxResultBytes {
		return toolResult(client.Response{}, errcode.New(errcode.QuotaExceeded, "result exceeds MCP limit; use a smaller page or retrieve the exact object through REST"))
	}
	var value any
	if e = json.Unmarshal(response.Body, &value); e != nil {
		return toolResult(client.Response{}, e)
	}
	out := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(response.Body)}}, StructuredContent: value}
	if obj, ok := value.(map[string]any); ok {
		ref, ok := obj["ref"].(map[string]any)
		if !ok {
			// ExactVersionView wraps the public VersionView in "version";
			// commit receipts expose the same PermanentRef at the top level.
			if version, nested := obj["version"].(map[string]any); nested {
				ref, ok = version["ref"].(map[string]any)
			}
		}
		if ok {
			b, _ := json.Marshal(ref)
			var r ids.PermanentRef
			if json.Unmarshal(b, &r) == nil {
				if uri, e := r.URI(); e == nil {
					out.Content = append(out.Content, &mcp.ResourceLink{URI: uri, Name: "exact_version", Description: "Permanent version reference; use resource_read/resource_pull with its IDs. Authorization is rechecked on every read."})
				}
			}
		}
	}
	if notice := client.CommitNotice(response); notice != "" {
		out.Content = append(out.Content, &mcp.TextContent{Text: notice})
	}
	encoded, _ := json.Marshal(out)
	if len(encoded) > MaxResultBytes {
		return toolResult(client.Response{}, errcode.New(errcode.QuotaExceeded, "serialized result exceeds MCP limit; reduce page size or retrieve via REST"))
	}
	return out, nil
}
func (a *adapter) postTool(name, path, description string) error {
	return a.commandTool(name, path, description, http.MethodPost, nil, false)
}
func (a *adapter) commandTool(name, path, description, method string, pathParameters []string, conditional bool) error {
	paths := a.schema["paths"].(map[string]any)
	route := paths["/api/v1/"+path].(map[string]any)[strings.ToLower(method)].(map[string]any)
	request := route["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	root := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"request", "idempotency_key"}, "properties": map[string]any{"request": request, "idempotency_key": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$"}}}
	properties := root["properties"].(map[string]any)
	required := root["required"].([]string)
	for _, parameter := range pathParameters {
		properties[parameter] = map[string]any{"type": "string", "pattern": "^[0-7][0-9A-HJKMNP-TV-Z]{25}$"}
		required = append(required, parameter)
	}
	if conditional {
		properties["if_match"] = map[string]any{"type": "string", "pattern": `^"[0-9]+"$`}
		required = append(required, "if_match")
	}
	root["required"] = required
	input, e := a.localSchema(root)
	if e != nil {
		return e
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	uri := "https://lantai.invalid/mcp/" + name
	if e = compiler.AddResource(uri, input); e != nil {
		return e
	}
	validator, e := compiler.Compile(uri)
	if e != nil {
		return e
	}
	a.s.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: input, Annotations: &mcp.ToolAnnotations{IdempotentHint: path != "flows/dispatch"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if len(req.Params.Arguments) > MaxRequestBytes {
			return toolResult(client.Response{}, errcode.New(errcode.SchemaInvalid, "MCP request exceeds size limit"))
		}
		var value any
		d := json.NewDecoder(bytes.NewReader(req.Params.Arguments))
		d.UseNumber()
		if d.Decode(&value) != nil || validator.Validate(value) != nil {
			return toolResult(client.Response{}, errcode.New(errcode.SchemaInvalid, "arguments do not match the public REST contract"))
		}
		var in struct {
			Request json.RawMessage `json:"request"`
			Key     string          `json:"idempotency_key"`
			IfMatch string          `json:"if_match,omitempty"`
		}
		if e := json.Unmarshal(req.Params.Arguments, &in); e != nil {
			return toolResult(client.Response{}, e)
		}
		target := path
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(req.Params.Arguments, &fields)
		for _, parameter := range pathParameters {
			var id string
			_ = json.Unmarshal(fields[parameter], &id)
			if !ids.ID(id).Valid() {
				return toolResult(client.Response{}, errcode.New(errcode.SchemaInvalid, "invalid path identifier"))
			}
			target = strings.ReplaceAll(target, "{"+parameter+"}", id)
		}
		out, e := a.c.Do(ctx, method, "/api/v1/"+target, in.Request, client.Options{IdempotencyKey: in.Key, IfMatch: in.IfMatch})
		return toolResult(out, e)
	})
	return nil
}

// Only embedded component references are resolved. MCP never fetches a remote
// schema; each tool contains just its transitive public definitions.
func (a *adapter) localSchema(root map[string]any) (map[string]any, error) {
	defs := map[string]any{}
	source := a.schema["components"].(map[string]any)["schemas"].(map[string]any)
	var walk func(any) (any, error)
	walk = func(v any) (any, error) {
		switch x := v.(type) {
		case map[string]any:
			out := map[string]any{}
			for k, y := range x {
				if k == "$ref" {
					ref, ok := y.(string)
					if !ok {
						return nil, errors.New("invalid schema ref")
					}
					name, ok := strings.CutPrefix(ref, "#/components/schemas/")
					if !ok {
						return nil, fmt.Errorf("nonlocal schema reference")
					}
					if _, ok = defs[name]; !ok {
						doc, exists := source[name]
						if !exists {
							return nil, errors.New("missing schema component")
						}
						defs[name] = nil
						expanded, e := walk(doc)
						if e != nil {
							return nil, e
						}
						defs[name] = expanded
					}
					out[k] = "#/$defs/" + name
				} else {
					z, e := walk(y)
					if e != nil {
						return nil, e
					}
					out[k] = z
				}
			}
			return out, nil
		case []any:
			out := make([]any, len(x))
			for i, y := range x {
				z, e := walk(y)
				if e != nil {
					return nil, e
				}
				out[i] = z
			}
			return out, nil
		}
		return v, nil
	}
	out, e := walk(root)
	if e != nil {
		return nil, e
	}
	r := out.(map[string]any)
	r["$defs"] = defs
	b, e := json.Marshal(r)
	if e != nil {
		return nil, e
	}
	var normalized map[string]any
	e = json.Unmarshal(b, &normalized)
	return normalized, e
}

type ReadArgs struct {
	ExplicitCompletion bool     `json:"explicit_completion,omitempty" jsonschema:"Request server-derived complete and truncated fields; default preserves the legacy page shape."`
	Ref                string   `json:"ref,omitempty"`
	View               string   `json:"view,omitempty"`
	Tags               []string `json:"tags,omitempty"`
	Characters         []string `json:"characters,omitempty"`
	Source             string   `json:"source,omitempty"`
	BusinessState      string   `json:"business_state,omitempty"`
	LegacyVersion      string   `json:"legacy_version,omitempty"`

	ID        string `json:"id,omitempty"`
	Kind      string `json:"kind,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	AssetID   string `json:"asset_id,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Query     string `json:"query,omitempty"`
	AssetType string `json:"asset_type,omitempty"`
	After     string `json:"after,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

func (a *adapter) readTools(has func(string) bool) {
	if has("agent_operation_catalog") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "capability_list", Description: "Page the reviewed API/CLI/MCP operation inventory and explicit gaps/exceptions. This is build metadata, not a permission grant. Follow next_cursor until complete; installed services are separately advertised by meta.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			After string `json:"after,omitempty"`
			Limit int    `json:"limit,omitempty"`
		}) (*mcp.CallToolResult, any, error) {
			if in.Limit == 0 {
				in.Limit = 10
			}
			if in.Limit < 1 || in.Limit > 20 {
				return readError("capability limit must be 1-20")
			}
			q := url.Values{"cursor": {in.After}, "limit": {fmt.Sprint(in.Limit)}}
			out, err := a.c.Do(ctx, http.MethodGet, "/api/v1/capabilities?"+q.Encode(), nil, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
	for _, t := range []struct {
		name, path, cap string
		single          bool
	}{{"whoami", "whoami", "sessions", false}, {"message_read", "messages", "discussions", false}, {"resource_search", "assets", "search", false}, {"resource_read", "assets", "exact_read", true}, {"task_list", "tasks", "tasks", false}, {"task_read", "tasks", "tasks", true}, {"flow_read", "flows", "manual_flows", true}, {"run_list", "task-runs", "manual_cli_execution", false}, {"run_read", "task-runs", "manual_cli_execution", true}, {"job_read", "jobs", "official_check_jobs", true}, {"job_list", "jobs", "official_check_jobs", false}, {"flow_list", "flows", "manual_flows", false}, {"run_capabilities", "task-runs/capabilities", "manual_cli_execution", false}, {"context_read", "context", "effective_context", false}, {"inbox_read", "inbox", "inbox", false}, {"events_read", "events", "filtered_events", false}, {"operation_read", "operations", "operation_status", true}} {
		if !has(t.cap) {
			continue
		}
		mcp.AddTool(a.s, &mcp.Tool{Name: t.name, Description: "Read authorized " + t.path + " through REST. Results are bounded; follow cursors even on empty pages. CURSOR_EXPIRED requires replacing prior pages. resource_search supports exact NFC case-sensitive tag/profile AND filters; resource_read full returns a frozen manifest, and resource_files/resource_read_chunk provide bounded large-object reads.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadArgs) (*mcp.CallToolResult, any, error) {
			if in.Limit == 0 {
				in.Limit = 20
			}
			if in.Limit < 1 || in.Limit > 100 {
				return readError("limit must be 1-100")
			}
			path := "/api/v1/" + t.path
			if in.View == "" {
				in.View = "brief"
			}
			if in.View != "brief" && in.View != "full" {
				return readError("view must be brief or full")
			}
			q := url.Values{"limit": {fmt.Sprint(in.Limit)}, "view": {in.View}}
			if t.single && t.path == "assets" && in.Ref != "" {
				ref, err := ids.ParseURI(in.Ref)
				if err != nil || !a.instanceID.Valid() || ref.InstanceID != a.instanceID || in.AssetID != "" || in.VersionID != "" {
					return readError("reference must match the connected instance; use either ref or asset_id/version_id")
				}
				in.AssetID, in.VersionID = string(ref.AssetID), string(ref.VersionID)
			}
			if t.single {
				if t.path == "assets" {
					if in.AssetID == "" || in.VersionID == "" {
						return readError("exact asset_id and version_id required")
					}
					path += "/" + url.PathEscape(in.AssetID) + "/versions/" + url.PathEscape(in.VersionID)
				} else {
					if in.ID == "" {
						return readError("id required")
					}
					path += "/" + url.PathEscape(in.ID)
				}
			}
			for k, v := range map[string]string{"project_id": in.ProjectID, "task_id": in.TaskID, "q": in.Query, "asset_type": in.AssetType, "after": in.After, "kind": in.Kind} {
				if v != "" {
					q.Set(k, v)
				}
			}
			if t.path == "messages" {
				q.Set("id", in.ID)
			}
			if t.path == "assets" && !t.single {
				var err error
				q, err = client.AssetSearchQuery(client.AssetFilter{ProjectID: in.ProjectID, AssetType: in.AssetType, Text: in.Query, Tags: in.Tags, Characters: in.Characters, Source: in.Source, BusinessState: in.BusinessState, LegacyVersion: in.LegacyVersion}, in.After, in.Limit)
				if err != nil {
					return readError("invalid exact tag/profile filter")
				}
				q.Set("view", in.View)
			}
			if t.path == "assets" && !t.single && in.ExplicitCompletion {
				q.Set("completion", "explicit")
			}
			out, e := a.c.Do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, client.Options{})

			r, e := toolResult(out, e)
			return r, nil, e
		})
	}
}
func readError(message string) (*mcp.CallToolResult, any, error) {
	r, e := toolResult(client.Response{}, errcode.New(errcode.SchemaInvalid, message))
	return r, nil, e
}

func contributionName(c Contribution) (string, error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_-]*)+$`).MatchString(c.ExtensionID) || !regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`).MatchString(c.Name) {
		return "", errors.New("mcp: invalid contribution namespace")
	}
	name := "ext_" + strings.ReplaceAll(c.ExtensionID, ".", "_") + "_" + c.Name
	if len(name) > 128 {
		return "", errors.New("mcp: contribution name too long")
	}
	return name, nil
}
