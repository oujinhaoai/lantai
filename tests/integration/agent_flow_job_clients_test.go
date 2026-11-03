package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

func orchestrationSession(t *testing.T, e *env, name string, kind authz.PrincipalKind, roles ...identity.Role) identity.IssuedSession {
	t.Helper()
	result := e.sudo(&identity.RegisterPrincipal{Kind: kind, Name: name})
	var principal map[string]json.RawMessage
	if err := json.Unmarshal(result.Summary, &principal); err != nil {
		t.Fatal(err)
	}
	var id ids.ID
	if err := json.Unmarshal(principal["principal_id"], &id); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		e.setRole(id, role, true)
	}
	current, err := e.id.GetPrincipal(t.Context(), e.login().Context, id)
	if err != nil {
		t.Fatal(err)
	}
	scopes := []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize, identity.ScopeTask}
	if kind == authz.Worker {
		scopes = []identity.Scope{identity.ScopeWorker}
	}
	issued := e.sudo(&identity.IssueCredential{PrincipalID: id, ExpectedRevision: current.Revision, Scopes: scopes})
	session, err := e.id.ExchangeToken(t.Context(), issued.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	return session
}
func orchestrationCaller(t *testing.T, c *client.Client) func(string, any, any, bool) {
	t.Helper()
	server, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "flow-job-selftest", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return func(name string, args, out any, wantError bool) {
		t.Helper()
		result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError != wantError {
			t.Fatalf("%s: %v %s", name, err, mustJSON(result))
		}
		if out != nil {
			if err := json.Unmarshal(mustJSON(result.StructuredContent), out); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func TestAgentFlowAndWorkerJobActualMCP(t *testing.T) {
	release, err := extensions.CurrentReleaseDigest()
	if err != nil {
		t.Fatal(err)
	}
	// Existing accepted-config fixture is a precondition only. The business
	// commands, task-bound bytes, deterministic process and ledger facts are real.
	f := newFlowEnvConfig(t, release, jobs.ConfigDigest())
	agent := orchestrationSession(t, f.env, "flow-agent@synthetic", authz.Agent, identity.RoleOwner, identity.RoleContributor)
	worker := orchestrationSession(t, f.env, "worker:flow-check@synthetic", authz.Worker, identity.RoleChecker)
	app := reopenApplication(t, f.env)
	servers := assetRemoteServers(t, app)
	origin := "http://" + servers.Addresses.API
	connect := func(token string) *client.Client {
		t.Helper()
		c, err := client.New(client.Config{BaseURL: origin, SessionToken: token, AllowHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	c := connect(agent.Token)
	call := orchestrationCaller(t, c)
	workerCall := orchestrationCaller(t, connect(worker.Token))
	command := func(key string, request any) map[string]any {
		return map[string]any{"idempotency_key": key, "request": request}
	}
	start := workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: f.definition.Ref, ProfileRef: f.profile.Ref, Title: "Explicit MCP Flow", AcceptanceCriteria: []string{"actual fixed checked document"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "agent-flow-output", AssetType: "doc", CandidateCount: 1}}}
	var begun, replayed workflow.Result
	call("flow_start", command("original-flow-start", start), &begun, false)
	call("flow_start", command("original-flow-start", start), &replayed, false)
	if begun.FlowID != replayed.FlowID || begun.OperationID != replayed.OperationID {
		t.Fatal("Flow replay replaced identity")
	}
	var view workflow.FlowView
	read := func() { call("flow_read", map[string]any{"id": begun.FlowID}, &view, false) }
	read()
	control := workflow.FlowRef{ProjectID: f.project.ProjectID, FlowID: begun.FlowID, ExpectedRevision: view.Flow.Revision, Reason: "synthetic explicit pause"}
	call("flow_pause", command("original-flow-pause", control), nil, false)
	read()
	if view.Flow.State != "paused" {
		t.Fatal("Flow did not pause")
	}
	control.ExpectedRevision = view.Flow.Revision
	control.Reason = "synthetic explicit resume"
	call("flow_resume", command("original-flow-resume", control), nil, false)
	read()
	if view.Flow.State != "running" {
		t.Fatal("Flow did not resume")
	}
	dispatch := func(key string) {
		call("flow_dispatch", command(key, map[string]int{"limit": 100}), nil, false)
		read()
	}
	dispatch("explicit-produce-dispatch")
	findStep := func(key string) workflow.StepView {
		t.Helper()
		for _, step := range view.Steps {
			if step.StepRun.StepKey == key {
				return step
			}
		}
		t.Fatal("missing Flow step", key)
		return workflow.StepView{}
	}
	produce := findStep("produce")
	if len(produce.StepRun.TaskIDs) != 1 {
		t.Fatal("dispatch did not create exact producer task", produce)
	}
	taskID := produce.StepRun.TaskIDs[0]
	var task tasks.View
	readTask := func() { call("task_read", map[string]any{"id": taskID}, &task, false) }
	readTask()
	call("task_claim", command("flow-producer-claim", tasks.ClaimRequest{ProjectID: f.project.ProjectID, TaskID: taskID, SeatID: task.Seat.ID, ExpectedRevision: task.Seat.Revision}), nil, false)
	readTask()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "content.txt"), []byte("actual task-bound MCP Flow bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	binding := &catalog.TaskBinding{TaskID: taskID, AttemptID: task.Attempt.ID, LeaseFence: task.Attempt.Fence.LeaseFence}
	taskBinding, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	rights, err := json.Marshal(rightsOwned())
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Push(t.Context(), client.PushInput{Task: taskBinding, ProjectID: string(f.project.ProjectID), Slug: "agent-flow-output", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "content.txt", Role: "primary"}}, Rights: rights}}, directory, filepath.Join(directory, "../flow.push.json"), false)
	if err != nil {
		t.Fatal("task-bound Push", err)
	}
	var produced apiv1.CommitResult
	if err := result.Decode(&produced); err != nil {
		t.Fatal(err)
	}
	ref := ids.PermanentRef{InstanceID: ids.ID(produced.Ref.InstanceId), AssetID: ids.ID(produced.AssetId), VersionID: ids.ID(produced.VersionId)}
	readTask()
	call("task_submit", command("flow-producer-submit", tasks.SubmitRequest{AttemptRef: tasks.AttemptRef{ProjectID: f.project.ProjectID, TaskID: taskID, AttemptID: task.Attempt.ID, ExpectedRevision: task.Attempt.Revision, Fence: task.Attempt.Fence}, OutputRefs: []ids.PermanentRef{ref}}), nil, false)
	if err := app.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	dispatch("explicit-check-dispatch")
	var jobPage map[string]json.RawMessage
	call("job_list", map[string]any{"project_id": f.project.ProjectID, "limit": 20}, &jobPage, false)
	var listed []jobs.Job
	if err := json.Unmarshal(jobPage["items"], &listed); err != nil || len(listed) != 1 {
		t.Fatal("check job not created", len(listed), err)
	}
	job := listed[0]
	jobControl := jobs.Control{JobID: job.ID, ExpectedRevision: job.Revision}
	call("job_run", command("agent-cannot-be-worker", jobControl), nil, true)
	workerCall("job_run", command("missing-observation", jobControl), nil, true)
	observation := node.Observation{ProjectID: f.project.ProjectID, Capabilities: []string{"org.lantai.corecheck.manifest"}, Slots: 1}
	workerCall("node_observe", command("worker-observation", observation), nil, false)
	var completed, repeat jobs.Job
	workerCall("job_run", command("original-real-worker-run", jobControl), &completed, false)
	workerCall("job_run", command("original-real-worker-run", jobControl), &repeat, false)
	if completed.State != "succeeded" || completed.Attempt == nil || completed.Attempt.Verdict != execution.VerdictPass || len(completed.Evidence) == 0 || !completed.Attempt.Termination.Confirmed || repeat.Attempt.ID != completed.Attempt.ID || repeat.Attempts != 1 {
		t.Fatal("actual Job result or replay differs", completed, repeat)
	}
	workerCall("job_retry", command("successful-job-not-retryable", jobs.Control{JobID: completed.ID, ExpectedRevision: completed.Revision}), nil, true)
	dispatch("explicit-finished-check-sync")
	if findStep("check").StepRun.State != "completed" {
		t.Fatal("Flow did not consume actual Job receipt", view)
	}
	state, err := app.Ledger.VersionControl(t.Context(), ids.ID(produced.VersionId))
	if err != nil || state.ReviewState == "approved" {
		t.Fatal("successful process became human approval", state, err)
	}
	control.ExpectedRevision = view.Flow.Revision
	control.Reason = "synthetic explicit Flow cancellation"
	call("flow_cancel", command("original-flow-cancel", control), nil, false)
	read()
	if view.Flow.State != "cancelled" {
		t.Fatal("Flow cancellation not visible")
	}
	call("flow_resume", command("stale-cancelled-flow-resume", control), nil, true)
}
