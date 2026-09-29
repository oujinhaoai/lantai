package extensions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// BreakerPolicy is the §11.2 configuration read from plugins.* policies.
type BreakerPolicy struct {
	Failures     int64 `json:"failures"`
	Window       int64 `json:"window_seconds"`
	Cooldown     int64 `json:"cooldown_seconds"`
	HalfOpenMax  int64 `json:"half_open_max_calls"`
	PolicyRevise int64 `json:"policy_revision"`
}

func (m *Manager) breakerPolicy(ctx context.Context) (BreakerPolicy, error) {
	p, err := m.d.Policies.ResolvePolicies(ctx, "")
	if err != nil {
		return BreakerPolicy{}, err
	}
	out := BreakerPolicy{Failures: 5, Window: 600, Cooldown: 900, HalfOpenMax: 1}
	for key, dst := range map[string]*int64{"plugins.breaker_failures": &out.Failures, "plugins.breaker_window_seconds": &out.Window, "plugins.breaker_cooldown_seconds": &out.Cooldown, "plugins.breaker_half_open_max_calls": &out.HalfOpenMax} {
		if v, ok := p[key]; ok {
			if err = json.Unmarshal(v.Value, dst); err != nil {
				return out, err
			}
			out.PolicyRevise = max(out.PolicyRevise, v.Revision)
		}
	}
	return out, nil
}

// breakerKey isolates one package digest/entry/target on this host node and
// enablement scope, so a fault elsewhere does not stop every deployment.
func (m *Manager) breakerKey(e Enablement) string {
	b, err := canonjson.CanonicalizeValue(map[string]any{"package_digest": e.PackageDigest, "entry": e.Entry, "target": e.Target, "host": m.d.InstanceID, "scope_kind": e.ScopeKind, "scope_id": e.ScopeID})
	if err != nil {
		return ""
	}
	return string(digest.Of(b))
}

type breakerRow struct {
	Key      string   `json:"breaker_key"`
	State    string   `json:"state"`
	Epoch    int64    `json:"epoch"`
	OpenedAt int64    `json:"opened_at"`
	Probes   []ids.ID `json:"probes"`
}

// Invocation is breaker/drain bookkeeping for one dispatched attempt.
type Invocation struct {
	ID           ids.ID `json:"invocation_id"`
	EnablementID ids.ID `json:"enablement_id"`
	Generation   int64  `json:"generation"`
	BreakerKey   string `json:"breaker_key"`
	Epoch        int64  `json:"breaker_epoch"`
	HalfOpen     bool   `json:"half_open"`
	State        string `json:"state"`
	AdmittedAt   int64  `json:"admitted_at"`
	Deadline     int64  `json:"deadline"`
	ProjectID    ids.ID `json:"project_id"`
	Contribution string `json:"contribution_id"`
	Outcome      string `json:"outcome,omitempty"`
	StaleEpoch   bool   `json:"stale_epoch,omitempty"`
	SettledAt    int64  `json:"settled_at,omitempty"`
}

func loadBreaker(ctx context.Context, tx *sql.Tx, key string) (breakerRow, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT record FROM extensions_breakers WHERE breaker_key=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return breakerRow{Key: key, State: "closed", Epoch: 1, Probes: []ids.ID{}}, nil
	}
	if err != nil {
		return breakerRow{}, err
	}
	var b breakerRow
	return b, json.Unmarshal([]byte(raw), &b)
}

func saveBreaker(ctx context.Context, tx *sql.Tx, b breakerRow) error {
	if b.Probes == nil {
		b.Probes = []ids.ID{}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO extensions_breakers(breaker_key,state,epoch,opened_at,record) VALUES(?,?,?,?,?) ON CONFLICT(breaker_key) DO UPDATE SET state=excluded.state,epoch=excluded.epoch,opened_at=excluded.opened_at,record=excluded.record`, b.Key, b.State, b.Epoch, b.OpenedAt, encode(b))
	return err
}

func saveInvocation(ctx context.Context, tx *sql.Tx, i Invocation) error {
	half := 0
	if i.HalfOpen {
		half = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO extensions_invocations(invocation_id,enablement_id,generation,breaker_key,breaker_epoch,half_open,state,admitted_at,deadline,record) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(invocation_id) DO UPDATE SET state=excluded.state,record=excluded.record`, i.ID, i.EnablementID, i.Generation, i.BreakerKey, i.Epoch, half, i.State, i.AdmittedAt, i.Deadline, encode(i))
	return err
}

func (m *Manager) invocation(ctx context.Context, id ids.ID) (Invocation, error) {
	var raw string
	err := m.d.Runtime.QueryRowContext(ctx, `SELECT record FROM extensions_invocations WHERE invocation_id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Invocation{}, errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return Invocation{}, err
	}
	var i Invocation
	return i, json.Unmarshal([]byte(raw), &i)
}

func (m *Manager) breakerTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	m.bmu.Lock()
	defer m.bmu.Unlock()
	tx, err := m.d.Runtime.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// admit atomically takes a closed-breaker dispatch or the single half-open
// slot. An open breaker inside its cooldown, or with every slot held, refuses
// new dispatch; cooldown expiry alone never starts any business work.
func (m *Manager) admit(ctx context.Context, e Enablement, invocation, project ids.ID, contribution string, deadline time.Time) error {
	policy, err := m.breakerPolicy(ctx)
	if err != nil {
		return err
	}
	now := m.d.Clock.Now()
	key := m.breakerKey(e)
	return m.breakerTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM extensions_invocations WHERE invocation_id=?`, invocation).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return failure(errcode.IdempotencyConflict, "invocation_already_admitted")
		}
		b, err := loadBreaker(ctx, tx, key)
		if err != nil {
			return err
		}
		inv := Invocation{ID: invocation, EnablementID: e.ID, Generation: e.Generation, BreakerKey: key, Epoch: b.Epoch, State: "dispatched", AdmittedAt: clock.Millis(now), Deadline: clock.Millis(deadline), ProjectID: project, Contribution: contribution}
		if b.State == "open" {
			if now.Before(clock.FromMillis(b.OpenedAt).Add(time.Duration(policy.Cooldown) * time.Second)) {
				return failure(errcode.ExtensionBreakerOpen, "cooldown")
			}
			if int64(len(b.Probes)) >= policy.HalfOpenMax {
				return failure(errcode.ExtensionBreakerOpen, "half_open_slot_held")
			}
			b.Probes = append(b.Probes, invocation)
			inv.HalfOpen = true
			if err = saveBreaker(ctx, tx, b); err != nil {
				return err
			}
		}
		return saveInvocation(ctx, tx, inv)
	})
}

// settle applies one outcome exactly once. Only runtime faults count; a legal
// fail, unsupported input, refusal, cancellation or undispatched call does not.
// A result from an older epoch is kept for audit and never moves the breaker.
func (m *Manager) settle(ctx context.Context, invocation ids.ID, outcome execution.InvocationOutcome) error {
	policy, err := m.breakerPolicy(ctx)
	if err != nil {
		return err
	}
	// Settlement is a background write: it holds the maintenance barrier.
	if m.d.Gate != nil {
		var h *commands.Held
		if ctx, h, err = m.d.Gate.Acquire(ctx, commands.Request{}); err != nil {
			return err
		}
		defer h.Release()
	}
	now := m.d.Clock.Now()
	return m.breakerTx(ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT record FROM extensions_invocations WHERE invocation_id=?`, invocation).Scan(&raw); err != nil {
			return err
		}
		var inv Invocation
		if err := json.Unmarshal([]byte(raw), &inv); err != nil {
			return err
		}
		if inv.State != "dispatched" && inv.State != "unresolved" {
			return nil // duplicate or late callback after a final settlement
		}
		inv.State = map[execution.InvocationOutcome]string{execution.InvocationCompleted: "completed", execution.InvocationRuntimeFault: "fault", execution.InvocationCancelled: "cancelled", execution.InvocationNotDispatched: "not_dispatched", execution.InvocationUnresolved: "unresolved"}[outcome]
		if inv.State == "" {
			return errors.New("extensions: unknown invocation outcome")
		}
		inv.Outcome, inv.SettledAt = string(outcome), clock.Millis(now)
		b, err := loadBreaker(ctx, tx, inv.BreakerKey)
		if err != nil {
			return err
		}
		if inv.Epoch != b.Epoch {
			inv.StaleEpoch = true
			return saveInvocation(ctx, tx, inv)
		}
		release := func() { b.Probes = slices.DeleteFunc(b.Probes, func(id ids.ID) bool { return id == inv.ID }) }
		switch outcome {
		case execution.InvocationRuntimeFault:
			if _, err = tx.ExecContext(ctx, `INSERT INTO extensions_breaker_faults(breaker_key,invocation_id,epoch,at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, inv.BreakerKey, inv.ID, inv.Epoch, clock.Millis(now)); err != nil {
				return err
			}
			if inv.HalfOpen {
				b.State, b.OpenedAt, b.Epoch, b.Probes = "open", clock.Millis(now), b.Epoch+1, []ids.ID{}
			} else if b.State == "closed" {
				var faults int64
				since := clock.Millis(now.Add(-time.Duration(policy.Window) * time.Second))
				if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM extensions_breaker_faults WHERE breaker_key=? AND epoch=? AND at>?`, inv.BreakerKey, b.Epoch, since).Scan(&faults); err != nil {
					return err
				}
				if faults >= policy.Failures {
					b.State, b.OpenedAt, b.Epoch, b.Probes = "open", clock.Millis(now), b.Epoch+1, []ids.ID{}
				}
			}
		case execution.InvocationCompleted:
			if inv.HalfOpen {
				// A protocol-valid probe (even a legal check fail) closes and resets.
				b.State, b.OpenedAt, b.Epoch, b.Probes = "closed", 0, b.Epoch+1, []ids.ID{}
			}
		case execution.InvocationCancelled, execution.InvocationNotDispatched:
			if inv.HalfOpen {
				release()
			}
		case execution.InvocationUnresolved:
			// Dispatched with unknown stop: keep the slot until reconciliation.
		}
		if err = saveBreaker(ctx, tx, b); err != nil {
			return err
		}
		return saveInvocation(ctx, tx, inv)
	})
}

// BreakerView is the diagnostic projection of one breaker key.
type BreakerView struct {
	Key          string   `json:"breaker_key"`
	State        string   `json:"state"`
	Epoch        int64    `json:"epoch"`
	OpenedAt     string   `json:"opened_at,omitempty"`
	RetryAt      string   `json:"retry_at,omitempty"`
	Probes       []ids.ID `json:"half_open_probes"`
	WindowFaults int64    `json:"window_faults"`
}

func (m *Manager) breakerView(ctx context.Context, key string) (*BreakerView, error) {
	policy, err := m.breakerPolicy(ctx)
	if err != nil {
		return nil, err
	}
	var out *BreakerView
	err = m.breakerTx(ctx, func(tx *sql.Tx) error {
		b, err := loadBreaker(ctx, tx, key)
		if err != nil {
			return err
		}
		now := m.d.Clock.Now()
		v := BreakerView{Key: key, State: b.State, Epoch: b.Epoch, Probes: b.Probes}
		if b.State == "open" {
			opened := clock.FromMillis(b.OpenedAt)
			v.OpenedAt, v.RetryAt = clock.Format(opened), clock.Format(opened.Add(time.Duration(policy.Cooldown)*time.Second))
			if !now.Before(opened.Add(time.Duration(policy.Cooldown) * time.Second)) {
				v.State = "half_open"
			}
		}
		since := clock.Millis(now.Add(-time.Duration(policy.Window) * time.Second))
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM extensions_breaker_faults WHERE breaker_key=? AND epoch=? AND at>?`, key, b.Epoch, since).Scan(&v.WindowFaults); err != nil {
			return err
		}
		out = &v
		return nil
	})
	return out, err
}
