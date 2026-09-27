package taskstest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/tasks"
)

func read[T any](t *testing.T, name string) T {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("..", "..", "..", "..", "schemas", "examples", "tasks", "v1", name, "valid.json"))
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
func TestStaticSuccessConflictStaleAndImmutableReads(t *testing.T) {
	a := read[tasks.Attempt](t, "attempt")
	c := read[tasks.Command](t, "task-command")
	r := read[tasks.Receipt](t, "task-receipt")
	now, _ := clock.Parse("2026-09-27T08:01:00.000Z")
	f := Fixture{Task: read[tasks.Task](t, "task"), Seat: read[tasks.Seat](t, "seat"), Attempt: &a, Now: now, RecoveryEpoch: 1, Outcomes: map[ids.ID]Outcome{c.OperationID: {Receipt: r}}}
	s := New(f)
	ctx := context.Background()
	out, e := s.Execute(ctx, c)
	if e != nil || out.OperationID != c.OperationID || out.Revision != 2 {
		t.Fatalf("%+v %v", out, e)
	}
	got, e := s.ReadTask(ctx, f.Task.ID)
	if e != nil {
		t.Fatal(e)
	}
	got.SeatIDs[0] = ids.New()
	again, _ := s.ReadTask(ctx, f.Task.ID)
	if again.SeatIDs[0] != f.Seat.ID {
		t.Fatal("caller mutated fixture")
	}
	old := c
	ff := *c.Fence
	ff.LeaseFence++
	old.Fence = &ff
	old.RequestHash, _ = tasks.HashCommand(old)
	if _, e = s.Execute(ctx, old); errcode.CodeOf(e) != errcode.LeaseStale {
		t.Fatal(e)
	}
	old = c
	old.ExpectedRevision++
	old.RequestHash, _ = tasks.HashCommand(old)
	if _, e = s.Execute(ctx, old); errcode.CodeOf(e) != errcode.PreconditionFailed {
		t.Fatal(e)
	}
	// A preset committed receipt survives old write preconditions; caller separately asserts read authorization.
	f.Committed = map[ids.ID]tasks.Receipt{c.OperationID: r}
	f.Attempt.Revision = 3
	s = New(f)
	out, e = s.Execute(ctx, c)
	if e != nil || out.Revision != r.Revision {
		t.Fatalf("committed replay %v", e)
	}
	old = c
	old.OutputRefs = []ids.PermanentRef{}
	old.RequestHash, _ = tasks.HashCommand(old)
	if _, e = s.Execute(ctx, old); errcode.CodeOf(e) != errcode.IdempotencyConflict {
		t.Fatal(e)
	}
}
func TestStaticUnknownExecutionCannotRequeue(t *testing.T) {
	a := read[tasks.Attempt](t, "attempt")
	a.State = "reconciling"
	seat := read[tasks.Seat](t, "seat")
	seat.State = "reconciling"
	c := read[tasks.Command](t, "task-command")
	c.Type = "tasks.requeue"
	c.TargetID = seat.ID
	c.Fence = nil
	c.OutputRefs = nil
	c.RequestHash, _ = tasks.HashCommand(c)
	s := New(Fixture{Task: read[tasks.Task](t, "task"), Seat: seat, Attempt: &a, RecoveryEpoch: 1})
	if _, e := s.Execute(context.Background(), c); errcode.CodeOf(e) != errcode.OperationNeedsReconciliation {
		t.Fatal(e)
	}
}

func TestStaticReviewCompletionUsesAuthorityAndRevision(t *testing.T) {
	task := read[tasks.Task](t, "task")
	task.Type = "review"
	task.State = "todo"
	task.SeatIDs = []ids.ID{}
	c := read[tasks.Command](t, "task-command")
	c.Type = "tasks.complete"
	c.TargetID = task.ID
	c.Fence = nil
	c.OutputRefs = nil
	c.AuthorityOperationID = ids.New()
	c.RequestHash, _ = tasks.HashCommand(c)
	receipt := tasks.Receipt{Contract: "lantai.task-receipt/v1", OperationID: c.OperationID, RequestHash: c.RequestHash, TargetID: task.ID, Revision: 2, OutputRefs: []ids.PermanentRef{}}
	f := Fixture{Task: task, RecoveryEpoch: 1, Completion: tasks.CompletionEvidence{AuthorityOperationID: c.AuthorityOperationID, ReviewDecided: true}, Outcomes: map[ids.ID]Outcome{c.OperationID: {Receipt: receipt}}}
	if !tasks.TaskTransition(task.State, "done") {
		t.Fatal("type-specific completion missing from graph")
	}
	if _, err := New(f).Execute(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	f.Completion.ReviewDecided = false
	if _, err := New(f).Execute(context.Background(), c); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal(err)
	}
	f.Completion.ReviewDecided = true
	f.Task.Revision++
	if _, err := New(f).Execute(context.Background(), c); errcode.CodeOf(err) != errcode.PreconditionFailed {
		t.Fatal(err)
	}
}
