package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestPendingCommandSurvivesUnknownResultWithSameOperation(t *testing.T) {
	for _, lostCheckpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost_response", true: "lost_local_checkpoint"}[lostCheckpoint], func(t *testing.T) {
			f := newFixture(t)
			entry := f.collect(t, f.record(t, 1))[0]
			c, db := f.consumer(t, "followup")
			handler := func(ctx context.Context, tx *sql.Tx, _ Entry) ([]Command, error) {
				if _, err := tx.ExecContext(ctx, `UPDATE state SET n=n+1`); err != nil {
					return nil, err
				}
				return []Command{{"register", "version.register", json.RawMessage(`{"value":1}`)}}, nil
			}
			if _, err := c.Apply(f.ctx, entry, handler); err != nil {
				t.Fatal(err)
			}
			m, err := c.Metrics(f.ctx)
			if err != nil || m.Through != 1 || m.Pending != 1 {
				t.Fatalf("intent not committed with offset: %+v %v", m, err)
			}
			calls, effects := 0, 0
			var operation ids.ID
			execute := func(ctx context.Context, p Pending) (json.RawMessage, error) {
				// 目标可独立取得同一实例屏障：调用时未持消费事务或屏障。
				_, h, err := f.gate.Acquire(ctx, commands.Request{})
				if err != nil {
					return nil, err
				}
				h.Release()
				calls++
				if operation == "" {
					operation = p.OperationID
					effects++
				} else if operation != p.OperationID {
					t.Fatal("retry changed child operation")
				}
				if operation == entry.Envelope.OperationID {
					t.Fatal("child reused parent ID")
				}
				if calls == 1 {
					if lostCheckpoint {
						f.gate.Close(commands.ReasonMaintenance)
					} else {
						return nil, errors.New("response lost after downstream committed")
					}
				}
				return json.RawMessage(`{"status":"succeeded"}`), nil
			}
			if _, err = c.Dispatch(f.ctx, 10, execute); err == nil {
				t.Fatal("wanted first response/checkpoint failure")
			}
			f.gate.Open()
			if n, err := c.Dispatch(f.ctx, 10, execute); err != nil || n != 0 || calls != 1 {
				t.Fatalf("did not defer retry: %d %d %v", n, calls, err)
			}
			f.clock.Advance(time.Second)
			c, _ = NewConsumer(db, "followup", f.clock, f.gate)
			if n, err := c.Dispatch(f.ctx, 10, execute); err != nil || n != 1 {
				t.Fatalf("restart dispatch: %d %v", n, err)
			}
			if effects != 1 || calls != 2 {
				t.Fatalf("downstream duplicate effect: %d effects / %d calls", effects, calls)
			}
			var status, receipt string
			if err = db.QueryRow(`SELECT status,receipt FROM pending_commands`).Scan(&status, &receipt); err != nil || status != "succeeded" || receipt != `{"status":"succeeded"}` {
				t.Fatalf("receipt missing: %s %s %v", status, receipt, err)
			}
			if n, err := c.Dispatch(f.ctx, 10, execute); err != nil || n != 0 {
				t.Fatalf("completed command repeated: %d %v", n, err)
			}
		})
	}
}

func TestPendingConflictRequiresExplicitRetry(t *testing.T) {
	f := newFixture(t)
	entry := f.collect(t, f.record(t, 1))[0]
	c, db := f.consumer(t, "dependent")
	if _, err := c.Apply(f.ctx, entry, func(context.Context, *sql.Tx, Entry) ([]Command, error) {
		return []Command{{"update", "asset.update", json.RawMessage(`{}`)}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var id ids.ID
	execute := func(_ context.Context, p Pending) (json.RawMessage, error) {
		id = p.OperationID
		return nil, &Failure{BusinessConflict, errors.New("stale target revision")}
	}
	if _, err := c.Dispatch(f.ctx, 1, execute); err == nil {
		t.Fatal("expected conflict")
	}
	if m, err := c.Metrics(f.ctx); err != nil || m.BlockedCommands != 1 {
		t.Fatalf("not blocked: %+v %v", m, err)
	}
	f.clock.Advance(24 * time.Hour)
	if n, err := c.Dispatch(f.ctx, 1, execute); err != nil || n != 0 {
		t.Fatalf("blocked command retried automatically: %d %v", n, err)
	}
	if err := c.RetryCommand(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Dispatch(f.ctx, 1, func(_ context.Context, p Pending) (json.RawMessage, error) {
		if p.OperationID != id {
			t.Fatal("explicit retry changed identity")
		}
		return json.RawMessage(`{}`), nil
	}); err != nil || n != 1 {
		t.Fatalf("explicit retry: %d %v", n, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM pending_commands`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry recreated command: %d %v", count, err)
	}
}

func TestInvalidPendingIntentRollsBackConsumerState(t *testing.T) {
	f := newFixture(t)
	entry := f.collect(t, f.record(t, 1))[0]
	c, db := f.consumer(t, "intent")
	if _, err := c.Apply(f.ctx, entry, func(ctx context.Context, tx *sql.Tx, _ Entry) ([]Command, error) {
		if _, err := tx.ExecContext(ctx, `UPDATE state SET n=8`); err != nil {
			return nil, err
		}
		return []Command{{"invalid", "asset.update", json.RawMessage(`[]`)}}, nil
	}); err == nil {
		t.Fatal("nonobject intent accepted")
	}
	var n int
	if err := db.QueryRow(`SELECT n FROM state`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("invalid intent committed local state: %d %v", n, err)
	}
	if offset, err := c.Offset(f.ctx); err != nil || offset != 0 {
		t.Fatalf("invalid intent advanced offset: %d %v", offset, err)
	}
	if _, err := c.Apply(f.ctx, entry, func(context.Context, *sql.Tx, Entry) ([]Command, error) { return nil, nil }); !errors.Is(err, ErrBlocked) {
		t.Fatalf("invalid intent must require repair, not retry forever: %v", err)
	}
}

func TestPendingCommandTypeUsesSharedCommandGrammar(t *testing.T) {
	for _, typ := range []string{"missing_domain", "asset.patch-title", "asset.more.parts"} {
		t.Run(typ, func(t *testing.T) {
			f := newFixture(t)
			entry := f.collect(t, f.record(t, 1))[0]
			c, _ := f.consumer(t, "validation")
			_, err := c.Apply(f.ctx, entry, func(context.Context, *sql.Tx, Entry) ([]Command, error) {
				return []Command{{"update", typ, json.RawMessage(`{}`)}}, nil
			})
			if kindOf(err) != SchemaIncompatible {
				t.Fatalf("invalid command type was not blocked: %v", err)
			}
			if offset, err := c.Offset(f.ctx); err != nil || offset != 0 {
				t.Fatalf("invalid intent advanced offset: %d %v", offset, err)
			}
		})
	}
}
