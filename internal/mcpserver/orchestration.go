package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Additional administrative/worker commands are deliberately separate from the
// small extension-contribution allowlist. Installed extensions cannot alias
// these commands merely because the first-party MCP adapter supports them.
func (a *adapter) orchestrationTools(has func(string) bool) error {
	a.recordChunkTool(has)
	for _, t := range []struct{ name, path, cap, description string }{
		{"task_create", "tasks", "tasks", "Create an authorized task with fixed input and saved idempotency key. Does not claim its seat or approve its output."},
		{"task_assign", "tasks/assign", "tasks", "Conditionally assign an existing task under current coordinator permissions. Preserve round, expected revision and original key."},
		{"task_answer", "tasks/answer", "tasks", "Answer an authorized task blocker by exact recorded message. This is task coordination, not human input for a TaskRun and not approval."},
		{"task_cancel", "tasks/cancel", "tasks", "Cancel an existing task and revoke execution acceptance. Unknown process stop remains reconciling; do not claim a replacement attempt."},
		{"task_reconcile", "tasks/reconcile", "tasks", "Reconcile the exact current task attempt using observed stop state and original fence. Unknown side effects must not be replayed."},
		{"task_complete", "tasks/complete", "tasks", "Complete a task only when its original domain acceptance rules hold. Execution success cannot substitute for review or publication."},
		{"task_rework", "tasks/rework", "tasks", "Open explicit task rework under current revision and authority; does not revive an old attempt or approval."},
		{"flow_start", "flows", "manual_flows", "Start a fixed supported business Flow under existing permissions. Does not enable automatic triggers or managed model execution."},
		{"flow_pause", "flows/pause", "manual_flows", "Pause the exact current Flow revision using the original request key."},
		{"flow_resume", "flows/resume", "manual_flows", "Resume an explicitly paused Flow after current authorization and revision checks."},
		{"flow_cancel", "flows/cancel", "manual_flows", "Cancel a Flow; downstream task/job cancellation and unknown stop state remain visible for reconciliation."},
		{"flow_retry", "flows/retry", "manual_flows", "Explicitly retry an eligible Flow step with fixed input and expected revision. No blind retry of unknown external effects."},
		{"flow_retry_command", "flows/retry-command", "manual_flows", "Reconcile/retry one persisted Flow command under its existing operation identity; do not replace keys or fences."},
		{"flow_dispatch", "flows/dispatch", "manual_flows", "Explicitly dispatch already persisted authorized Flow commands and synchronize jobs with a bounded limit. Each command retains its stable operation; read current Flow commands after an unknown response before requesting another dispatch pass."},
		{"run_cancel", "task-runs/cancel", "manual_cli_execution", "Revoke execution acceptance and request cancellation of an exact TaskRun. Unconfirmed stop stays cancelling/needs_reconciliation."},
		{"run_reconcile", "task-runs/reconcile", "manual_cli_execution", "Reconcile an existing TaskRun from observable state, preserving input, attempt, checkpoint, budget and recovery epoch."},
		{"job_run", "jobs/run", "official_check_jobs", "Explicitly run a supported deterministic Job using existing worker permissions and fixed input. An Agent cannot obtain worker scope by self-reporting node capability."},
		{"job_cancel", "jobs/cancel", "official_check_jobs", "Cancel an exact Job under current authority. Unknown stop must be reconciled before retry."},
		{"job_retry", "jobs/retry", "official_check_jobs", "Explicitly retry an eligible deterministic Job with the original fixed input and revision/fence checks."},
		{"job_reconcile", "jobs/reconcile", "official_check_jobs", "Reconcile the current Job attempt using actual stop/result observations. Never treat an unknown side effect as failure safe to repeat."},
		{"node_observe", "nodes/observe", "official_check_jobs", "Record an authorized node observation under existing node scope. Observation is not worker authorization, capability proof or human approval."},
	} {
		if has(t.cap) {
			if err := a.postTool(t.name, t.path, t.description); err != nil {
				return err
			}
		}
	}
	if has("tasks") {
		mcp.AddTool(a.s, &mcp.Tool{Name: "task_attempts", Description: "Read authorized attempt/fence history of one exact task, including stopped and reconciling attempts.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			TaskID string `json:"task_id"`
		}) (*mcp.CallToolResult, any, error) {
			if !ids.ID(in.TaskID).Valid() {
				return readError("task ULID required")
			}
			out, err := a.c.Do(ctx, http.MethodGet, "/api/v1/tasks/"+in.TaskID+"/attempts", nil, client.Options{})
			r, err := toolResult(out, err)
			return r, nil, err
		})
	}
	return nil
}
