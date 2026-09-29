package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/oujinhaoai/lantai/internal/client"
)

// The fixed route table exposes installed core services. It cannot dispatch
// arbitrary HTTP paths, local programs, SQL, or extension entry points.
var collaborationPosts = map[string]string{
	"task/create": "tasks", "task/claim": "tasks/claim", "task/renew": "tasks/renew", "task/release": "tasks/release", "task/submit": "tasks/submit", "task/block": "tasks/block", "task/handoff": "tasks/handoff", "task/assign": "tasks/assign", "task/answer": "tasks/answer", "task/cancel": "tasks/cancel", "task/reconcile": "tasks/reconcile", "task/complete": "tasks/complete", "task/rework": "tasks/rework",
	"flow/start": "flows", "flow/pause": "flows/pause", "flow/resume": "flows/resume", "flow/cancel": "flows/cancel", "flow/retry": "flows/retry", "flow/retry-command": "flows/retry-command", "flow/dispatch": "flows/dispatch",
	"run/start": "task-runs", "run/resume": "task-runs", "run/progress": "task-runs/progress", "run/tool": "task-runs/tool", "run/candidate": "task-runs/candidate", "run/checkpoint": "task-runs/checkpoint", "run/ask": "task-runs/ask", "run/answer": "task-runs/answer", "run/cancel": "task-runs/cancel", "run/reconcile": "task-runs/reconcile", "run/seal": "task-runs/seal",
	"job/run": "jobs/run", "job/cancel": "jobs/cancel", "job/retry": "jobs/retry", "job/reconcile": "jobs/reconcile", "node/observe": "nodes/observe",
	"rights/assert": "rights/assertions", "rights/cancel": "rights/cancel",
	"message/post": "messages", "review/submit": "review-targets", "review/publish": "publications", "evidence/append": "evidence", "inbox/read": "inbox/read",
	"trash/preview": "trash/preview", "trash/own": "trash/own", "trash/restore": "trash/restore",
	"human/prepare": "human/prepare", "human/execute": "human/execute", "human/rechallenge": "human/rechallenge",
}

func collaborationCommand(ctx context.Context, c *client.Client, command, action string, o options) (client.Response, error) {
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
