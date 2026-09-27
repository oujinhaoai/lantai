package events

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

type fixture struct {
	ctx   context.Context
	db    *sql.DB
	clock *clock.Fake
	gate  *commands.Gate
	store *Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "events.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	gate := commands.NewGate(commands.NewCoordinator())
	gate.Open()
	return &fixture{ctx, db, clk, gate, New(db, clk, gate)}
}
func (f *fixture) record(t *testing.T, revision int64) commands.OutboxRecord {
	t.Helper()
	e, err := event.New(&ids.Generator{Clock: f.clock, Rand: rand.Reader}, f.clock, event.Params{EventType: "version.committed", SchemaVersion: 1, AggregateType: "version", AggregateID: ids.New(), AggregateRevision: revision, ActorID: ids.New(), ProjectID: ids.New(), OperationID: ids.New(), Payload: map[string]any{"revision": revision}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	return commands.OutboxRecord{Seq: revision, EventID: e.EventID, OperationID: e.OperationID, Envelope: b, OccurredAt: e.OccurredAt}
}
func (f *fixture) collect(t *testing.T, rs ...commands.OutboxRecord) []Entry {
	t.Helper()
	if _, err := f.store.Collect(f.ctx, rs); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.Read(f.ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return p.Entries
}
func (f *fixture) consumer(t *testing.T, name string) (*Consumer, *sql.DB) {
	t.Helper()
	db, err := sqlite.Open(f.ctx, filepath.Join(t.TempDir(), "target.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = EnsureConsumerSchema(f.ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE state (id INTEGER PRIMARY KEY, n INTEGER NOT NULL) STRICT; INSERT INTO state VALUES(1,0);`); err != nil {
		t.Fatal(err)
	}
	c, err := NewConsumer(db, name, f.clock, f.gate)
	if err != nil {
		t.Fatal(err)
	}
	return c, db
}

type fakeSource struct {
	name    string
	records []commands.OutboxRecord
	ackErr  error
	pulls   int
	acks    int
}

func (s *fakeSource) Name() string { return s.name }
func (s *fakeSource) Pull(_ context.Context, limit int) ([]commands.OutboxRecord, error) {
	s.pulls++
	return s.records[:min(limit, len(s.records))], nil
}
func (s *fakeSource) Ack(_ context.Context, got []ids.ID, _ time.Time) error {
	s.acks++
	if s.ackErr != nil {
		return s.ackErr
	}
	s.records = s.records[len(got):]
	return nil
}

func TestRelayRestartAfterCollectionBeforeSourceAck(t *testing.T) {
	f := newFixture(t)
	r := f.record(t, 9)
	src := &fakeSource{name: "ledger", records: []commands.OutboxRecord{r}, ackErr: errors.New("source temporarily unavailable")}
	if n, err := f.store.Relay(f.ctx, src, 10); n != 1 || err == nil {
		t.Fatalf("first relay: %d %v", n, err)
	}
	if _, err := f.store.Relay(f.ctx, src, 10); !errors.Is(err, ErrDeferred) {
		t.Fatalf("want deferred: %v", err)
	}
	if src.pulls != 1 {
		t.Fatal("backoff still pulled source")
	}
	f.store = New(f.db, f.clock, f.gate)
	f.clock.Advance(time.Second)
	src.ackErr = nil
	if n, err := f.store.Relay(f.ctx, src, 10); n != 0 || err != nil {
		t.Fatalf("retry: %d %v", n, err)
	}
	p, err := f.store.Read(f.ctx, 0, 10)
	if err != nil || len(p.Entries) != 1 || p.HighWater != 1 {
		t.Fatalf("duplicate collection: %+v %v", p, err)
	}
	other := &fakeSource{name: "runtime", records: []commands.OutboxRecord{f.record(t, 1)}}
	if _, err = f.store.Relay(f.ctx, other, 10); err != nil {
		t.Fatal(err)
	}
	p, err = f.store.Read(f.ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries[0].Envelope.AggregateRevision != 9 || p.Entries[1].Envelope.AggregateRevision != 1 {
		t.Fatal("global_seq was confused with business revision")
	}
	metrics, err := f.store.Metrics(f.ctx)
	if err != nil || metrics.RelayFailures != 0 {
		t.Fatalf("metrics: %+v %v", metrics, err)
	}
}

func TestCollectRejectsConflictingIdentityAndRollsBackBatch(t *testing.T) {
	f := newFixture(t)
	r := f.record(t, 1)
	f.collect(t, r)
	e, err := event.Parse(r.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	e.Payload = json.RawMessage(`{"changed":true}`)
	r.Envelope, err = e.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Collect(f.ctx, []commands.OutboxRecord{f.record(t, 2), r}); err == nil {
		t.Fatal("identity conflict accepted")
	}
	if high, err := f.store.HighWater(f.ctx); err != nil || high != 1 {
		t.Fatalf("batch did not roll back: %d %v", high, err)
	}
	bad := f.record(t, 3)
	bad.Envelope = append([]byte(" "), bad.Envelope...)
	if _, err = f.store.Collect(f.ctx, []commands.OutboxRecord{bad}); err == nil {
		t.Fatal("noncanonical source accepted")
	}
}

func TestConcurrentDuplicateCollection(t *testing.T) {
	f := newFixture(t)
	r := f.record(t, 1)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := f.store.Collect(f.ctx, []commands.OutboxRecord{r}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if high, err := f.store.HighWater(f.ctx); err != nil || high != 1 {
		t.Fatalf("high water: %d %v", high, err)
	}
}

func TestConsumerAtomicRollbackRetryAndDedup(t *testing.T) {
	f := newFixture(t)
	entry := f.collect(t, f.record(t, 1))[0]
	c, db := f.consumer(t, "projection")
	failing := func(ctx context.Context, tx *sql.Tx, _ Entry) ([]Command, error) {
		_, err := tx.ExecContext(ctx, `UPDATE state SET n=n+1`)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("after local mutation")
	}
	if _, err := c.Apply(f.ctx, entry, failing); err == nil {
		t.Fatal("wanted failure")
	}
	var n int
	if err := db.QueryRow(`SELECT n FROM state`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("state escaped rollback: %d %v", n, err)
	}
	if through, err := c.Offset(f.ctx); err != nil || through != 0 {
		t.Fatalf("offset escaped rollback: %d %v", through, err)
	}
	if _, err := c.Apply(f.ctx, entry, failing); !errors.Is(err, ErrDeferred) {
		t.Fatalf("missing backoff: %v", err)
	}
	f.clock.Advance(time.Second)
	handler := func(ctx context.Context, tx *sql.Tx, _ Entry) ([]Command, error) {
		_, err := tx.ExecContext(ctx, `UPDATE state SET n=n+1`)
		return nil, err
	}
	if ok, err := c.Apply(f.ctx, entry, handler); !ok || err != nil {
		t.Fatalf("apply: %v %v", ok, err)
	}
	c, _ = NewConsumer(db, "projection", f.clock, f.gate)
	if ok, err := c.Apply(f.ctx, entry, handler); ok || err != nil {
		t.Fatalf("restart dedup: %v %v", ok, err)
	}
	if err := db.QueryRow(`SELECT n FROM state`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate state: %d %v", n, err)
	}
	m, err := c.Metrics(f.ctx)
	if err != nil || m.Through != 1 || m.Failures != 0 {
		t.Fatalf("metrics: %+v %v", m, err)
	}
}

func TestConsumerFailureClassesAndSequence(t *testing.T) {
	for _, kind := range []FailureKind{SchemaIncompatible, MissingPrerequisite, BusinessConflict} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t)
			entries := f.collect(t, f.record(t, 1), f.record(t, 2))
			c, _ := f.consumer(t, "derived")
			handler := func(context.Context, *sql.Tx, Entry) ([]Command, error) {
				return nil, &Failure{kind, errors.New("cannot accept")}
			}
			if _, err := c.Apply(f.ctx, entries[0], handler); err == nil {
				t.Fatal("expected classified failure")
			}
			_, err := c.Apply(f.ctx, entries[0], handler)
			if permanent(kind) && !errors.Is(err, ErrBlocked) {
				t.Fatalf("expected block: %v", err)
			}
			if !permanent(kind) && !errors.Is(err, ErrDeferred) {
				t.Fatalf("expected defer: %v", err)
			}
			if _, err := c.Apply(f.ctx, entries[1], handler); err == nil {
				t.Fatal("skipped failed sequence")
			}
			if err := c.Retry(f.ctx, entries[0].Envelope.EventID); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Apply(f.ctx, entries[0], func(context.Context, *sql.Tx, Entry) ([]Command, error) { return nil, nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMaintenanceBarrierRejectsBackgroundWrites(t *testing.T) {
	f := newFixture(t)
	entry := f.collect(t, f.record(t, 1))[0]
	c, _ := f.consumer(t, "indexer")
	f.gate.Close(commands.ReasonMaintenance)
	checks := []func() error{
		func() error { _, err := f.store.Collect(f.ctx, []commands.OutboxRecord{f.record(t, 2)}); return err },
		func() error { _, err := f.store.ExportAudit(f.ctx, t.TempDir(), 10); return err },
		func() error { _, err := f.store.Prune(f.ctx); return err },
		func() error { return f.store.ConfirmBackup(f.ctx, 1) },
		func() error {
			_, err := c.Apply(f.ctx, entry, func(context.Context, *sql.Tx, Entry) ([]Command, error) { return nil, nil })
			return err
		},
	}
	for i, check := range checks {
		var ce *errcode.Error
		if err := check(); !errors.As(err, &ce) || ce.Code != errcode.MaintenanceMode {
			t.Fatalf("check %d bypassed maintenance: %v", i, err)
		}
	}
	if _, err := f.store.Read(f.ctx, 0, 10); err != nil {
		t.Fatalf("read blocked in maintenance: %v", err)
	}
}
