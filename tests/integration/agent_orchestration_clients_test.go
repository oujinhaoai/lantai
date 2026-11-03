package integration

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/client"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/mcpserver"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"os"
)

func TestAgentOrchestrationClients(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	principal, _ := e.agent("orchestration@synthetic", identity.RoleOwner)
	e.setRole(principal.ID, identity.RoleContributor, true)
	p, err := e.id.GetPrincipal(t.Context(), e.login().Context, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	issued := e.sudo(&identity.IssueCredential{PrincipalID: p.ID, ExpectedRevision: p.Revision, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize, identity.ScopeTask}})
	session, err := e.id.ExchangeToken(t.Context(), issued.Secret, identity.SessionRequest{Channel: identity.ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	evidenceVersion := e.ingest(session.Context, "orchestration/reconciliation-evidence", []byte("synthetic controlled manual session stop evidence"), *rightsOwned())
	app := reopenApplication(t, e)
	cfg := app.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: true}
	servers, err := app.StartHTTP(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := servers.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, SessionToken: session.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	server, err := mcpserver.New(t.Context(), mcpserver.Config{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent-orchestration-acceptance", Version: "1"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args, out any, wantError bool) {
		t.Helper()
		r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || r.IsError != wantError {
			t.Fatalf("%s: %v %s", name, err, mustJSON(r))
		}
		if out != nil {
			if err = json.Unmarshal(mustJSON(r.StructuredContent), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var created, replayed tasks.Result
	create := tasks.CreateRequest{ProjectID: e.project.ProjectID, Type: "question", Title: "Agent MCP question", Priority: "P2", AcceptanceCriteria: []string{"record actual stop"}, Role: identity.RoleContributor}
	args := map[string]any{"idempotency_key": "original-create-task", "request": create}
	call("task_create", args, &created, false)
	call("task_create", args, &replayed, false)
	if created.TaskID != replayed.TaskID || created.OperationID != replayed.OperationID {
		t.Fatal("same-key task replay created new authority", created, replayed)
	}
	var page struct {
		Items      []tasks.Task `json:"items"`
		Complete   bool         `json:"complete"`
		NextCursor string       `json:"next_cursor"`
	}
	call("task_list", map[string]any{"project_id": e.project.ProjectID, "limit": 1}, &page, false)
	if len(page.Items) != 1 || page.Complete || page.NextCursor != string(created.TaskID) {
		t.Fatal("full page lost continuation", page)
	}
	call("task_list", map[string]any{"project_id": e.project.ProjectID, "limit": 1, "after": page.NextCursor}, &page, false)
	if len(page.Items) != 0 || !page.Complete {
		t.Fatal("terminal task page not explicit", page)
	}
	var view tasks.View
	read := func() { call("task_read", map[string]any{"id": created.TaskID}, &view, false) }
	read()
	var claimed tasks.Result
	call("task_claim", map[string]any{"idempotency_key": "original-claim-task", "request": tasks.ClaimRequest{ProjectID: e.project.ProjectID, TaskID: view.Task.ID, SeatID: view.Seat.ID, ExpectedRevision: view.Seat.Revision}}, &claimed, false)
	read()
	if view.Attempt == nil || view.Attempt.ID != claimed.Attempt.ID {
		t.Fatal("claim did not expose current attempt", view)
	}
	var history []tasks.Attempt
	call("task_attempts", map[string]any{"task_id": created.TaskID}, &history, false)
	if len(history) != 1 || history[0].ID != view.Attempt.ID {
		t.Fatal("attempt history mismatch", history)
	}
	// Reconstruct a current record without exceeding the MCP serialized budget.
	var bytes []byte
	var digest string
	offset := 0
	for {
		var chunk struct {
			Data     string `json:"data"`
			SHA      string `json:"sha256"`
			Bytes    int    `json:"total_bytes"`
			Next     int    `json:"next_offset"`
			Complete bool   `json:"complete"`
		}
		in := map[string]any{"kind": "task", "id": created.TaskID, "offset": offset, "max_bytes": 256}
		if digest != "" {
			in["expected_sha256"] = digest
		}
		call("record_read_chunk", in, &chunk, false)
		decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			t.Fatal(err)
		}
		bytes = append(bytes, decoded...)
		if digest == "" {
			digest = chunk.SHA
		} else if digest != chunk.SHA {
			t.Fatal("record changed without conflict")
		}
		if chunk.Complete {
			if len(bytes) != chunk.Bytes {
				t.Fatal("record size mismatch")
			}
			break
		}
		offset = chunk.Next
	}
	sum := sha256.Sum256(bytes)
	if hex.EncodeToString(sum[:]) != digest {
		t.Fatal("record digest mismatch")
	}
	// A real manual TaskRun keeps the original task attempt/fence. Cancellation
	// does not turn an unconfirmed stop into a safe replacement execution.
	rawStart, err := os.ReadFile("../../schemas/examples/agent-execution/v1/agent-request/start.json")
	if err != nil {
		t.Fatal(err)
	}
	var startFixture map[string]json.RawMessage
	if err := json.Unmarshal(rawStart, &startFixture); err != nil {
		t.Fatal(err)
	}
	var start ae.StartRequest
	if err := json.Unmarshal(startFixture["document"], &start); err != nil {
		t.Fatal(err)
	}
	start.OperationID, start.TaskRunID, start.BudgetID = ids.New(), ids.New(), ids.New()
	start.Fence = view.Attempt.Fence
	start.Fence.TaskID = view.Task.ID
	start.Input = view.Task.Input
	start.Activation = nil
	start.Profile.ActivationSnapshot = ae.ActivationSnapshot{}
	start.Profile.PlaybookRefs = []ids.PermanentRef{}
	start.Profile.RequiredCapabilities = []string{"manual_session"}
	start.ExecutionKey, _ = ae.ExecutionKey(start.TaskRunID, start.Fence.AttemptID)
	start.RequestHash, _ = ae.RequestHash(start)
	var run ax.Record
	call("run_start", map[string]any{"idempotency_key": "original-manual-run", "request": start}, &run, false)
	originalRun := run
	cancelRun := ax.CancelRequest{Mutation: ax.Mutation{RunID: run.Run.ID, ExpectedRevision: run.Run.Revision, Fence: start.Fence}, Reason: "synthetic explicit cancellation", Mode: execution.CancelRevokeOnly}
	call("run_cancel", map[string]any{"idempotency_key": "original-cancel-run", "request": cancelRun}, &run, false)
	if run.Run.Termination.Confirmed || run.Run.State == execution.RunCancelled {
		t.Fatal("unconfirmed manual execution was declared stopped", run.Run)
	}
	var cancelledReplay ax.Record
	call("run_cancel", map[string]any{"idempotency_key": "original-cancel-run", "request": cancelRun}, &cancelledReplay, false)
	if cancelledReplay.Run.ID != run.Run.ID || cancelledReplay.Run.Revision != run.Run.Revision {
		t.Fatal("same cancel replaced run state")
	}
	reconcileRun := ax.ReconcileRequest{Mutation: ax.Mutation{RunID: run.Run.ID, ExpectedRevision: run.Run.Revision, Fence: start.Fence}, Stopped: false, EvidenceRefs: []ids.PermanentRef{evidenceVersion.Ref}, Reason: "synthetic stop still unknown"}
	call("run_reconcile", map[string]any{"idempotency_key": "original-run-unknown", "request": reconcileRun}, &run, false)
	if run.Run.Termination.Confirmed || run.Run.State == execution.RunCancelled {
		t.Fatal("unknown manual side effect became stopped")
	}
	reconcileRun.ExpectedRevision = run.Run.Revision
	reconcileRun.Stopped = true
	reconcileRun.Reason = "synthetic manual session definitely stopped"
	call("run_reconcile", map[string]any{"idempotency_key": "original-run-stopped", "request": reconcileRun}, &run, false)
	if run.Run.State != execution.RunCancelled || !run.Run.Termination.Confirmed || run.Run.ID != originalRun.Run.ID || run.Run.CurrentAttemptID != originalRun.Run.CurrentAttemptID {
		t.Fatal("manual cancellation lost original identity", run.Run)
	}
	call("run_cancel", map[string]any{"idempotency_key": "stale-run-mutation", "request": cancelRun}, nil, true)
	read()
	original := *view.Attempt
	call("task_cancel", map[string]any{"idempotency_key": "original-cancel-task", "request": tasks.CancelRequest{TaskRef: tasks.TaskRef{ProjectID: e.project.ProjectID, TaskID: view.Task.ID, ExpectedRevision: view.Task.Revision}, Reason: "Synthetic explicit cancellation"}}, nil, false)
	read()
	if view.Task.State != "reconciling" || view.Attempt == nil || view.Attempt.State != "reconciling" {
		t.Fatal("cancel invented confirmed process stop", view)
	}
	var changed map[string]any
	call("record_read_chunk", map[string]any{"kind": "task", "id": created.TaskID, "offset": 256, "expected_sha256": digest}, &changed, true)
	if !strings.Contains(string(mustJSON(changed)), "PRECONDITION_FAILED") {
		t.Fatal(changed)
	}
	reconcile := tasks.ReconcileRequest{ProjectID: e.project.ProjectID, TaskID: view.Task.ID, AttemptID: view.Attempt.ID, ExpectedRevision: view.Attempt.Revision, TerminationConfirmed: false, UnresolvedEffects: 1, EvidenceRefs: []ids.ID{}, Reason: "Synthetic stop still unknown"}
	call("task_reconcile", map[string]any{"idempotency_key": "original-unknown-reconciliation", "request": reconcile}, nil, false)
	read()
	if view.Task.State != "reconciling" || view.Attempt.Reconciliation.TerminationConfirmed {
		t.Fatal("unknown side effect was treated as stopped", view)
	}
	reconcile.ExpectedRevision = view.Attempt.Revision
	reconcile.TerminationConfirmed = true
	reconcile.UnresolvedEffects = 0
	reconcile.Reason = "Synthetic execution confirmed stopped"
	call("task_reconcile", map[string]any{"idempotency_key": "original-confirmed-reconciliation", "request": reconcile}, nil, false)
	read()
	if view.Task.State != "cancelled" || view.Attempt.State != "cancelled" {
		t.Fatal("confirmed cancellation did not close exact attempt", view)
	}
	call("task_renew", map[string]any{"idempotency_key": "stale-renewal-rejected", "request": tasks.AttemptRef{ProjectID: e.project.ProjectID, TaskID: view.Task.ID, AttemptID: original.ID, ExpectedRevision: original.Revision, Fence: original.Fence}}, nil, true)
	call("run_capabilities", map[string]any{}, nil, false)
	call("flow_list", map[string]any{"project_id": e.project.ProjectID, "limit": 1}, nil, false)
	call("job_list", map[string]any{"project_id": e.project.ProjectID, "limit": 1}, nil, false)
}
