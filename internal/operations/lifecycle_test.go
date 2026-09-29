package operations

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

type epochSource struct{ epoch int64 }

func (e *epochSource) RecoveryEpoch(context.Context) (int64, error) { return e.epoch, nil }

// fakeOwners records what the scheduler asks the owners to do. RunDue calls
// the scheduler's own authority exactly like ledger.RunDueJob does.
type fakeOwners struct {
	mu        sync.Mutex
	s         *LifecycleScheduler
	gate      *commands.Gate
	due       []DueTrash
	fail      map[string]error
	runs      map[string][]ids.ID
	gc        []GCCandidate
	gcState   map[ids.ID]string
	collect   map[string]error
	retained  map[string]bool
	collected []string
}

func (f *fakeOwners) DueTrash(context.Context, time.Time, int) ([]DueTrash, error) {
	return f.due, nil
}
func (f *fakeOwners) RunDue(ctx context.Context, cmd commands.Context, job DueJob) error {
	if err := f.s.CheckLifecycleJob(ctx, cmd, cmd.CommandType, job.TrashID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[cmd.CommandType] = append(f.runs[cmd.CommandType], cmd.OperationID)
	if err := f.fail[cmd.CommandType]; err != nil {
		delete(f.fail, cmd.CommandType)
		return err
	}
	return nil
}
func (f *fakeOwners) GCCandidates(_ context.Context, after string, _ int) ([]GCCandidate, error) {
	out := []GCCandidate{}
	for _, c := range f.gc {
		if c.SHA256 > after {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeOwners) GCState(_ context.Context, op ids.ID) (string, error) { return f.gcState[op], nil }
func (f *fakeOwners) CollectBlob(_ context.Context, sha string) (bool, error) {
	if open, _ := f.gate.State(); !open {
		return false, errcode.New(errcode.MaintenanceMode, "")
	}
	if err := f.collect[sha]; err != nil {
		return false, err
	}
	if f.retained[sha] {
		return false, nil
	}
	f.collected = append(f.collected, sha)
	for _, c := range f.gc {
		if c.SHA256 == sha {
			f.gcState[c.OperationID] = "done"
		}
	}
	return true, nil
}
func (f *fakeOwners) PendingGC(context.Context) ([]string, error)    { return nil, nil }
func (f *fakeOwners) RetryFileFailures(context.Context) (int, error) { return 0, nil }

func newScheduler(t *testing.T) (*LifecycleScheduler, *fakeOwners, *clock.Fake, *epochSource, *commands.Gate) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "runtime.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, m := range migrations.For(ownership.Runtime) {
		if _, err = db.ExecContext(t.Context(), m.SQL); err != nil {
			t.Fatal(err)
		}
	}
	gate := commands.NewGate(commands.NewCoordinator())
	gate.Open()
	clk := clock.NewFake(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC))
	epochs := &epochSource{epoch: 1}
	owners := &fakeOwners{fail: map[string]error{}, runs: map[string][]ids.ID{}, gcState: map[ids.ID]string{}, collect: map[string]error{}, retained: map[string]bool{}}
	s, err := NewLifecycleScheduler(SchedulerDeps{Runtime: db, Gate: gate, Epochs: epochs, Ports: owners, Clock: clk, IDs: &ids.Generator{Clock: clk, Rand: rand.Reader}, Instance: ids.MustParse("01K00000000000000000000000")})
	if err != nil {
		t.Fatal(err)
	}
	owners.s, owners.gate = s, gate
	return s, owners, clk, epochs, gate
}

func TestSchedulerRegistersJobsBeforeOwnersRunThem(t *testing.T) {
	s, owners, clk, epochs, _ := newScheduler(t)
	ctx := t.Context()
	trash := ids.New()
	owners.due = []DueTrash{{ProjectID: ids.New(), TrashID: trash, Revision: 3, DueAt: clk.Now().Add(80 * time.Hour)}}
	if r, err := s.RunOnce(ctx); err != nil || r.Reminders != 0 {
		t.Fatal("reminder before the 72-hour boundary", r, err)
	}
	clk.Advance(8 * time.Hour) // exactly 72 hours before due
	owners.fail["ledger.purge_reminder"] = errors.New("synthetic crash after send")
	if r, err := s.RunOnce(ctx); err != nil || r.Failed != 1 {
		t.Fatal(r, err)
	}
	clk.Advance(time.Minute)
	if r, err := s.RunOnce(ctx); err != nil || r.Reminders != 1 {
		t.Fatal(r, err)
	}
	// The retry reused the registered operation instead of a new key.
	if runs := owners.runs["ledger.purge_reminder"]; len(runs) != 2 || runs[0] != runs[1] {
		t.Fatal("retry changed operation", runs)
	}
	owners.due[0].Reminded = true
	clk.Advance(72 * time.Hour)
	owners.fail["ledger.purge_due"] = errcode.New(errcode.TrashHeld, "")
	if r, err := s.RunOnce(ctx); err != nil || r.Superseded != 1 || r.Purges != 0 {
		t.Fatal("held entry must not be retried", r, err)
	}
	if r, err := s.RunOnce(ctx); err != nil || len(owners.runs["ledger.purge_due"]) != 1 {
		t.Fatal("superseded job reran", r, err)
	}
	// After Unhold the entry has a new revision: a new, separately registered job.
	owners.due[0].Revision = 4
	if r, err := s.RunOnce(ctx); err != nil || r.Purges != 1 {
		t.Fatal(r, err)
	}
	// A restored instance has a new epoch; old registrations cannot be replayed.
	cmd := commands.Context{OperationID: owners.runs["ledger.purge_due"][1], IdempotencyKey: "lifecycle-" + string(owners.runs["ledger.purge_due"][1]), CommandType: "ledger.purge_due", ActorID: s.instance, ProjectID: owners.due[0].ProjectID, RecoveryEpoch: 1}
	epochs.epoch = 2
	for name, mutate := range map[string]func(*commands.Context){
		"session":    func(c *commands.Context) { c.SessionID = ids.New() },
		"actor":      func(c *commands.Context) { c.ActorID = ids.New() },
		"kind":       func(c *commands.Context) { c.CommandType = "ledger.purge_reminder" },
		"unregister": func(c *commands.Context) { c.OperationID = ids.New() },
		"epoch":      func(c *commands.Context) { c.RecoveryEpoch = 2 },
	} {
		c := cmd
		mutate(&c)
		if err := s.CheckLifecycleJob(ctx, c, "ledger.purge_due", trash); errcode.CodeOf(err) != errcode.Forbidden {
			t.Fatal(name, "accepted", err)
		}
	}
}

func TestSchedulerGCWaitsRetainsAndCollectsOnce(t *testing.T) {
	s, owners, clk, _, gate := newScheduler(t)
	ctx := t.Context()
	young, pinned, old := GCCandidate{SHA256: "aa", OperationID: ids.New(), Since: clk.Now()}, GCCandidate{SHA256: "bb", OperationID: ids.New(), Since: clk.Now().Add(-25 * time.Hour)}, GCCandidate{SHA256: "cc", OperationID: ids.New(), Since: clk.Now().Add(-48 * time.Hour)}
	owners.gc = []GCCandidate{young, pinned, old}
	owners.retained["bb"] = true
	r, err := s.RunOnce(ctx)
	if err != nil || r.Deferred != 1 || r.Retained != 1 || r.Collected != 1 || len(owners.collected) != 1 || owners.collected[0] != "cc" {
		t.Fatal(r, owners.collected, err)
	}
	// A retained candidate is re-evaluated an hour later, never deleted by index.
	if r, err = s.RunOnce(ctx); err != nil || r.Retained != 0 || len(owners.collected) != 1 {
		t.Fatal(r, err)
	}
	clk.Advance(time.Hour)
	delete(owners.retained, "bb")
	owners.collect["bb"] = errcode.New(errcode.OperationNeedsReconciliation, "pending ledger operations prevent GC")
	if r, err = s.RunOnce(ctx); err != nil || r.Failed != 1 {
		t.Fatal(r, err)
	}
	clk.Advance(2 * time.Minute)
	delete(owners.collect, "bb")
	gate.Close(commands.ReasonMaintenance)
	if r, err = s.RunOnce(ctx); err != nil || r.Collected != 0 || len(owners.collected) != 1 {
		t.Fatal("maintenance barrier must defer collection", r, err)
	}
	gate.Open()
	clk.Advance(24 * time.Hour)
	if r, err = s.RunOnce(ctx); err != nil || r.Collected != 2 || len(owners.collected) != 3 {
		t.Fatal(r, owners.collected, err)
	}
	if r, err = s.RunOnce(ctx); err != nil || r.Collected != 0 {
		t.Fatal("finished candidates rescheduled", r, err)
	}
}
