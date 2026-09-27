package tasks_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/tasks"
)

func fixture[T any](t *testing.T, name string) T {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "examples", "tasks", "v1", name, "valid.json"))
	if e != nil {
		t.Fatal(e)
	}
	var w struct {
		Document T `json:"document"`
	}
	if e = json.Unmarshal(b, &w); e != nil {
		t.Fatal(e)
	}
	return w.Document
}
func checkCode(t *testing.T, err error, want errcode.Code) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if err == nil || errcode.CodeOf(err) != want {
		t.Fatalf("want %s got %v", want, err)
	}
}
func TestFinalAcceptanceUsesCurrentAttemptSessionRevisionAndTime(t *testing.T) {
	task := fixture[tasks.Task](t, "task")
	seat := fixture[tasks.Seat](t, "seat")
	a := fixture[tasks.Attempt](t, "attempt")
	base := fixture[tasks.Command](t, "task-command")
	now, _ := clock.Parse("2026-09-27T08:01:00.000Z")
	tests := []struct {
		name   string
		change func(*tasks.Command, *tasks.Attempt, *time.Time, *int64)
		code   errcode.Code
	}{
		{"current", func(*tasks.Command, *tasks.Attempt, *time.Time, *int64) {}, ""},
		{"old_revision", func(c *tasks.Command, _ *tasks.Attempt, _ *time.Time, _ *int64) { c.ExpectedRevision++ }, errcode.PreconditionFailed},
		{"old_attempt_same_session", func(c *tasks.Command, _ *tasks.Attempt, _ *time.Time, _ *int64) { c.Fence.AttemptID = ids.New() }, errcode.LeaseStale},
		{"old_fence", func(c *tasks.Command, _ *tasks.Attempt, _ *time.Time, _ *int64) { c.Fence.LeaseFence++ }, errcode.LeaseStale},
		{"other_session_same_principal", func(c *tasks.Command, _ *tasks.Attempt, _ *time.Time, _ *int64) { c.SessionID = ids.New() }, errcode.LeaseStale},
		{"restored_epoch_reuses_numeric_fence", func(_ *tasks.Command, _ *tasks.Attempt, _ *time.Time, e *int64) { *e = 2 }, errcode.LeaseStale},
		{"exact_expiry", func(_ *tasks.Command, a *tasks.Attempt, n *time.Time, _ *int64) { *n, _ = clock.Parse(a.ExpiresAt) }, errcode.LeaseStale},
		{"revoked_attempt", func(_ *tasks.Command, a *tasks.Attempt, _ *time.Time, _ *int64) { a.State = "reconciling" }, errcode.LeaseStale},
		{"no_server_time", func(_ *tasks.Command, _ *tasks.Attempt, n *time.Time, _ *int64) { *n = time.Time{} }, errcode.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			f := *base.Fence
			c.Fence = &f
			attempt := a
			n := now
			epoch := int64(1)
			tt.change(&c, &attempt, &n, &epoch)
			c.RequestHash, _ = tasks.HashCommand(c)
			checkCode(t, tasks.AcceptCommand(c, task, seat, attempt, n, epoch), tt.code)
		})
	}
}
func TestReclaimNeverInfersTerminationFromExpiry(t *testing.T) {
	task := fixture[tasks.Task](t, "task")
	task.State = "todo"
	seat := fixture[tasks.Seat](t, "seat")
	seat.State = "open"
	a := fixture[tasks.Attempt](t, "attempt")
	a.State = "reconciling"
	checkCode(t, tasks.CheckClaim(task, seat, &a, 1, 1), errcode.OperationNeedsReconciliation)
	a.Reconciliation.TerminationConfirmed = true
	a.Reconciliation.UnresolvedEffects = 1
	checkCode(t, tasks.CheckClaim(task, seat, &a, 1, 1), errcode.OperationNeedsReconciliation)
	a.Reconciliation.UnresolvedEffects = 0
	a.State = "expired"
	checkCode(t, tasks.CheckClaim(task, seat, &a, 1, 1), "")
	checkCode(t, tasks.CheckClaim(task, seat, nil, 1, 1), errcode.OperationNeedsReconciliation)
	next := a
	next.ID = ids.New()
	next.Fence.AttemptID = next.ID
	next.Fence.LeaseFence++
	next.State = "active"
	next.Reconciliation.TerminationConfirmed = false
	checkCode(t, tasks.CheckNewAttempt(seat, next), "")
	next.Fence.LeaseFence = seat.LastLeaseFence
	checkCode(t, tasks.CheckNewAttempt(seat, next), errcode.SchemaInvalid)
	task.Type = "review"
	checkCode(t, tasks.CheckClaim(task, seat, &a, 1, 1), errcode.InvalidStateTransition)
}
func TestSnapshotAndAttemptSemanticValidation(t *testing.T) {
	task := fixture[tasks.Task](t, "task")
	checkCode(t, task.Input.Validate(), "")
	task.Input.Refs[0].VersionID = ids.New()
	checkCode(t, task.Input.Validate(), errcode.SchemaInvalid)
	a := fixture[tasks.Attempt](t, "attempt")
	checkCode(t, a.Validate(), "")
	a.ExpiresAt = a.IssuedAt
	checkCode(t, a.Validate(), errcode.SchemaInvalid)
	a = fixture[tasks.Attempt](t, "attempt")
	a.Fence.AttemptID = ids.New()
	checkCode(t, a.Validate(), errcode.SchemaInvalid)
}
func TestTaskAndWorkflowAuthorityCannotBeReplacedByExecutionSuccess(t *testing.T) {
	task := fixture[tasks.Task](t, "task")
	task.State = "submitted"
	checkCode(t, tasks.CheckTaskCompletion(task, tasks.CompletionEvidence{}), errcode.InvalidStateTransition)
	checkCode(t, tasks.CheckTaskCompletion(task, tasks.CompletionEvidence{AuthorityOperationID: ids.New(), OutputsAccepted: true}), "")
	task.Type = "review"
	task.State = "todo"
	checkCode(t, tasks.CheckTaskCompletion(task, tasks.CompletionEvidence{AuthorityOperationID: ids.New(), ReviewDecided: true}), "")
	step := fixture[tasks.StepRun](t, "step-run")
	for _, kind := range []string{"task", "job", "gate", "review", "publish", "wait"} {
		step.Kind = kind
		checkCode(t, tasks.CheckStepCompletion(step, tasks.AdvanceEvidence{}), errcode.InvalidStateTransition)
	}
	step.Kind = "publish"
	checkCode(t, tasks.CheckStepCompletion(step, tasks.AdvanceEvidence{AuthorityOperationID: ids.New(), ReviewApproved: true}), errcode.InvalidStateTransition)
	checkCode(t, tasks.CheckStepCompletion(step, tasks.AdvanceEvidence{AuthorityOperationID: ids.New(), Published: true}), "")
}
func TestStateGraphsRequireReconciliationAndNewRounds(t *testing.T) {
	if tasks.TaskTransition("claimed", "todo") || tasks.TaskTransition("claimed", "done") || tasks.AttemptTransition("active", "cancelled") || tasks.AttemptTransition("expired", "active") || tasks.StepTransition("completed", "running") {
		t.Fatal("unsafe shortcut or historical round reuse")
	}
	if !tasks.TaskTransition("claimed", "reconciling") || !tasks.TaskTransition("reconciling", "todo") || !tasks.AttemptTransition("reconciling", "expired") || !tasks.FlowTransition("failed", "running") {
		t.Fatal("required recovery edge missing")
	}
	if tasks.SeatTransition("claimed", "open") || tasks.FlowTransition("completed", "running") || tasks.TaskTransition("unknown", "todo") {
		t.Fatal("invalid state edge")
	}
}

func TestFlowDefinitionCannotChangeInsideRun(t *testing.T) {
	f := fixture[tasks.Flow](t, "flow")
	s := fixture[tasks.StepRun](t, "step-run")
	checkCode(t, tasks.CheckStepBinding(f, s), "")
	s.Definition.Ref.VersionID = ids.New()
	checkCode(t, tasks.CheckStepBinding(f, s), errcode.SchemaInvalid)
}

func TestOtherOpenSeatMayBeClaimedWhileTaskAlreadyClaimed(t *testing.T) {
	task := fixture[tasks.Task](t, "task")
	seat := fixture[tasks.Seat](t, "seat")
	seat.ID = ids.New()
	seat.Ordinal = 2
	seat.State = "open"
	seat.LastLeaseFence = 0
	seat.CurrentAttemptID = ""
	task.SeatIDs = append(task.SeatIDs, seat.ID)
	checkCode(t, tasks.CheckClaim(task, seat, nil, 1, 1), "")
	seat.State = "claimed"
	seat.LastLeaseFence = 1
	seat.CurrentAttemptID = ids.New()
	checkCode(t, tasks.CheckClaim(task, seat, nil, 1, 1), errcode.TaskAlreadyClaimed)
}
