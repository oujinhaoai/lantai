package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// The fixed route table exposes installed core services. It cannot dispatch
// arbitrary HTTP paths, local programs, SQL, or extension entry points.
var collaborationPosts = map[string]string{
	"task/create": "tasks", "task/claim": "tasks/claim", "task/renew": "tasks/renew", "task/release": "tasks/release", "task/submit": "tasks/submit", "task/block": "tasks/block", "task/handoff": "tasks/handoff", "task/assign": "tasks/assign", "task/answer": "tasks/answer", "task/cancel": "tasks/cancel", "task/reconcile": "tasks/reconcile", "task/complete": "tasks/complete", "task/rework": "tasks/rework",
	"flow/start": "flows", "flow/pause": "flows/pause", "flow/resume": "flows/resume", "flow/cancel": "flows/cancel", "flow/retry": "flows/retry", "flow/retry-command": "flows/retry-command", "flow/dispatch": "flows/dispatch",
	"run/start": "task-runs", "run/resume": "task-runs", "run/progress": "task-runs/progress", "run/tool": "task-runs/tool", "run/candidate": "task-runs/candidate", "run/checkpoint": "task-runs/checkpoint", "run/ask": "task-runs/ask", "run/answer": "task-runs/answer", "run/cancel": "task-runs/cancel", "run/reconcile": "task-runs/reconcile", "run/seal": "task-runs/seal",
	"job/run": "jobs/run", "job/cancel": "jobs/cancel", "job/retry": "jobs/retry", "job/reconcile": "jobs/reconcile", "node/observe": "nodes/observe",
	"rights/evidence": "rights/evidence", "rights/assert": "rights/assertions", "rights/cancel": "rights/cancel",
	"message/post": "messages", "review/submit": "review-targets", "review/publish": "publications", "evidence/append": "evidence", "inbox/read": "inbox/read",
	"trash/preview": "trash/preview", "trash/own": "trash/own", "trash/restore": "trash/restore",
	"human/prepare": "human/prepare", "human/execute": "human/execute", "human/rechallenge": "human/rechallenge",
}

func collaborationCommand(ctx context.Context, c *client.Client, command, action string, o options) (client.Response, error) {
	if command == "milestone" {
		if !ids.ID(o.project).Valid() {
			return client.Response{}, usage("--project ULID is required")
		}
		path := "/api/v1/projects/" + o.project + "/milestones"
		switch action {
		case "", "list":
			query, err := snapshotQuery(o)
			if err != nil {
				return client.Response{}, err
			}
			return c.Do(ctx, http.MethodGet, path+query, nil, client.Options{})
		case "progress":
			if !ids.ID(o.id).Valid() {
				return client.Response{}, usage("--id milestone ULID is required")
			}
			return c.Do(ctx, http.MethodGet, path+"/"+o.id+"/progress", nil, client.Options{})
		case "set":
			if o.key == "" {
				return client.Response{}, usage("--idempotency-key is required; preserve it for retries")
			}
			body, err := input(o.input)
			if err != nil {
				return client.Response{}, err
			}
			return c.Do(ctx, http.MethodPost, path, body, client.Options{IdempotencyKey: o.key})
		default:
			return client.Response{}, usage("milestone requires list, set or progress")
		}
	}
	if path, ok := collaborationPosts[command+"/"+action]; ok {
		body, e := input(o.input)
		if e != nil {
			return client.Response{}, e
		}
		if command != "human" && command != "inbox" && !(command == "trash" && action == "preview") && o.key == "" {
			return client.Response{}, usage("--idempotency-key is required; preserve it for retries")
		}
		return c.Do(ctx, http.MethodPost, "/api/v1/"+path, body, client.Options{IdempotencyKey: o.key})
	}
	if command == "human" && action == "verify" {
		return identityCommand(ctx, c, "verify", o)
	}
	q := url.Values{"limit": {fmt.Sprint(o.limit)}}
	if o.project != "" {
		q.Set("project_id", o.project)
	}
	if o.cursor != "" {
		q.Set("after", o.cursor)
	}
	path := ""
	switch command {
	case "rights":
		if action == "show" {
			exact, err := readPath(o)
			if err != nil {
				return client.Response{}, err
			}
			return c.ReadResourceJSON(ctx, exact+"/rights")
		}
	case "task", "flow", "run", "job":
		base := map[string]string{"task": "tasks", "flow": "flows", "run": "task-runs", "job": "jobs"}[command]
		switch action {
		case "", "list":
			path = base
			if command == "run" {
				q.Set("task_id", o.id)
			}
		case "show":
			if o.id == "" {
				return client.Response{}, usage("--id is required")
			}
			path = base + "/" + segment(o.id)
		case "attempts":
			if command == "task" && o.id != "" {
				path = base + "/" + segment(o.id) + "/attempts"
			}
		case "capabilities":
			if command == "run" {
				path = "task-runs/capabilities"
			}
		}
	case "message":
		if action == "" || action == "list" {
			path = "messages"
			q.Set("kind", o.name)
			q.Set("id", o.id)
		}
	case "events":
		if action == "" || action == "list" {
			path = "events"
		}
	case "resync":
		if action == "" || action == "list" {
			path = "resync"
			q.Del("after")
			q.Set("cursor", o.cursor)
		}
	case "inbox":
		if action == "" || action == "list" {
			path = "inbox"
		}
	case "context":
		if action == "" || action == "show" {
			path = "context"
			q.Set("asset_type", o.assetType)
		}
	case "review":
		if action == "show" && o.id != "" {
			path = "review-targets/" + segment(o.id)
		}
	case "trash":
		if action == "show" && o.id != "" {
			path = "trash/" + segment(o.id)
		}
	case "human":
		if action == "items" && o.id != "" {
			path = "human/grants/" + segment(o.id)
		}
	}
	if path == "" {
		return client.Response{}, usage("unsupported " + strings.TrimSpace(command+" "+action) + " action or missing --id")
	}
	return c.Do(ctx, http.MethodGet, "/api/v1/"+path+"?"+q.Encode(), nil, client.Options{})
}

func snapshotQuery(o options) (string, error) {
	if o.pagination == "" {
		return "", nil
	}
	if o.pagination != "explicit" && o.pagination != "chunk" || o.offset < 0 || o.offset > 0 && o.expectedSHA256 == "" {
		return "", usage("pagination requires explicit/chunk, nonnegative offset and original digest for continuation")
	}
	q := url.Values{"pagination": {o.pagination}, "offset": {fmt.Sprint(o.offset)}, "expected_sha256": {o.expectedSHA256}}
	if o.pagination == "explicit" {
		if o.limit < 1 || o.limit > 20 {
			return "", usage("snapshot limit must be 1-20")
		}
		q.Set("limit", fmt.Sprint(o.limit))
	} else {
		if o.maxBytes < 1 || o.maxBytes > 8192 {
			return "", usage("snapshot max-bytes must be 1-8192")
		}
		q.Set("max_bytes", fmt.Sprint(o.maxBytes))
	}
	return "?" + q.Encode(), nil
}
