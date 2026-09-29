package extensions

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

type breakerPolicies map[string]int64

func (p breakerPolicies) ResolvePolicies(context.Context, ids.ID) (map[string]identity.PolicyValue, error) {
	out := map[string]identity.PolicyValue{}
	for k, v := range p {
		b, _ := json.Marshal(v)
		out[k] = identity.PolicyValue{Key: k, Value: b, Revision: 3}
	}
	return out, nil
}

type breakerCase struct {
	t   *testing.T
	m   *Manager
	clk *clock.Fake
	e   Enablement
}

func newBreakerCase(t *testing.T, policy breakerPolicies) *breakerCase {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.db")
	open := func() *Manager {
		db, err := sqlite.Open(t.Context(), path, sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return &Manager{d: ManagerDeps{Runtime: db, Policies: policy, InstanceID: ids.MustParse("01K00000000000000000000000")}}
	}
	m := open()
	for _, mig := range migrations.For(ownership.Runtime) {
		if _, err := m.d.Runtime.ExecContext(t.Context(), mig.SQL); err != nil {
			t.Fatal(err)
		}
	}
	c := &breakerCase{t: t, m: m, clk: clock.NewFake(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC))}
	m.d.Clock = c.clk
	c.e = Enablement{ID: ids.New(), Generation: 1, PackageDigest: digest.Of([]byte("pkg")), Entry: "bin/checker", Target: "server", ScopeKind: "instance"}
	return c
}

// call admits one invocation and optionally settles it.
func (c *breakerCase) call(outcome execution.InvocationOutcome) (ids.ID, error) {
	c.t.Helper()
	id := ids.New()
	if err := c.m.admit(c.t.Context(), c.e, id, ids.New(), "org.example.check", c.clk.Now().Add(time.Minute)); err != nil {
		return id, err
	}
	if outcome != "" {
		if err := c.m.settle(c.t.Context(), id, outcome); err != nil {
			c.t.Fatal(err)
		}
	}
	return id, nil
}

func (c *breakerCase) state() BreakerView {
	c.t.Helper()
	v, err := c.m.breakerView(c.t.Context(), c.m.breakerKey(c.e))
	if err != nil {
		c.t.Fatal(err)
	}
	return *v
}

func (c *breakerCase) mustFault(n int) {
	c.t.Helper()
	for range n {
		if _, err := c.call(execution.InvocationRuntimeFault); err != nil {
			c.t.Fatal(err)
		}
	}
}

func TestBreakerWindowCountingAndExclusions(t *testing.T) {
	c := newBreakerCase(t, breakerPolicies{})
	// Legal fail, refusal, cancellation and undispatched calls do not count.
	for _, o := range []execution.InvocationOutcome{execution.InvocationCompleted, execution.InvocationCancelled, execution.InvocationNotDispatched, execution.InvocationCompleted} {
		if _, err := c.call(o); err != nil {
			t.Fatal(err)
		}
	}
	c.mustFault(1)
	c.clk.Advance(600 * time.Second) // (now-600s, now]: the first fault leaves the window
	c.mustFault(3)
	// A duplicate callback (timeout, then late exit) is counted once.
	id, err := c.call(execution.InvocationRuntimeFault)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.m.settle(t.Context(), id, execution.InvocationRuntimeFault); err != nil {
		t.Fatal(err)
	}
	if s := c.state(); s.State != "closed" || s.WindowFaults != 4 {
		t.Fatalf("4th fault opened or miscounted: %+v", s)
	}
	// A closed-state success does not clear faults still inside the window.
	if _, err = c.call(execution.InvocationCompleted); err != nil {
		t.Fatal(err)
	}
	c.mustFault(1)
	s := c.state()
	if s.State != "open" || s.Epoch != 2 {
		t.Fatalf("5th fault did not open: %+v", s)
	}
	_, err = c.call("")
	mustBreaker(t, err)
}

func mustBreaker(t *testing.T, err error) {
	t.Helper()
	if errcode.CodeOf(err) != errcode.ExtensionBreakerOpen {
		t.Fatalf("want breaker open, got %v", err)
	}
}

func TestBreakerCooldownHalfOpenAndEpochs(t *testing.T) {
	c := newBreakerCase(t, breakerPolicies{})
	// An in-flight call from the closed epoch returns after the breaker opened.
	late, err := c.call("")
	if err != nil {
		t.Fatal(err)
	}
	c.mustFault(5)
	c.clk.Advance(900*time.Second - time.Millisecond)
	_, err = c.call("")
	mustBreaker(t, err)
	if err = c.m.settle(t.Context(), late, execution.InvocationCompleted); err != nil {
		t.Fatal(err)
	}
	if s := c.state(); s.State != "open" {
		t.Fatal("stale-epoch success closed the breaker", s)
	}
	c.clk.Advance(time.Millisecond)
	if s := c.state(); s.State != "half_open" || len(s.Probes) != 0 {
		t.Fatal("cooldown expiry must only make the key probeable", s)
	}
	probe, err := c.call("")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.call("")
	mustBreaker(t, err) // exactly one half-open slot
	// A cancelled probe with confirmed stop releases the slot but does not close.
	if err = c.m.settle(t.Context(), probe, execution.InvocationCancelled); err != nil {
		t.Fatal(err)
	}
	if s := c.state(); s.State != "half_open" || len(s.Probes) != 0 {
		t.Fatal(s)
	}
	// An unresolved probe keeps the slot; no concurrent replacement probe.
	probe, err = c.call(execution.InvocationUnresolved)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.call("")
	mustBreaker(t, err)
	// Restart: a new manager on the same database keeps cooldown and slot.
	restarted := &Manager{d: c.m.d}
	c.m = restarted
	_, err = c.call("")
	mustBreaker(t, err)
	// Reconciliation reports a runtime fault: reopen with a fresh cooldown.
	if err = c.m.Settle(t.Context(), probe, execution.InvocationRuntimeFault); err != nil {
		t.Fatal(err)
	}
	s := c.state()
	if s.State != "open" || s.Epoch != 3 {
		t.Fatal("probe fault must reopen with a new epoch", s)
	}
	c.clk.Advance(899 * time.Second)
	_, err = c.call("")
	mustBreaker(t, err)
	c.clk.Advance(time.Second)
	probe, err = c.call("")
	if err != nil {
		t.Fatal(err)
	}
	// A protocol-valid probe (a legal check fail is also "completed") closes
	// the breaker, resets the window and moves to a new epoch.
	if err = c.m.settle(t.Context(), probe, execution.InvocationCompleted); err != nil {
		t.Fatal(err)
	}
	if s = c.state(); s.State != "closed" || s.Epoch != 4 || s.WindowFaults != 0 {
		t.Fatal(s)
	}
	c.mustFault(4)
	if s = c.state(); s.State != "closed" {
		t.Fatal("old window leaked into the new epoch", s)
	}
}

func TestBreakerPolicyOverride(t *testing.T) {
	c := newBreakerCase(t, breakerPolicies{"plugins.breaker_failures": 2, "plugins.breaker_cooldown_seconds": 60, "plugins.breaker_half_open_max_calls": 2})
	c.mustFault(2)
	if s := c.state(); s.State != "open" {
		t.Fatal(s)
	}
	c.clk.Advance(time.Minute)
	for range 2 {
		if _, err := c.call(""); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.call("")
	mustBreaker(t, err)
}
