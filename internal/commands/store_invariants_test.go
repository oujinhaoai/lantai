package commands

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
)

func TestEveryOutboxEntryPointRejectsForeignOwnership(t *testing.T) {
	for _, entry := range []string{"execute", "complete", "append"} {
		for _, eventType := range []string{"principal.disabled", "unregistered.changed"} {
			t.Run(entry+"/"+eventType, func(t *testing.T) {
				f := newFixture(t, "ledger", "ledger.db")
				ctx := t.Context()
				cmd := f.cmd(t, "foreign-event", `{}`)
				valid := f.event(t, cmd.OperationID, f.gen.MustNew())
				foreign := f.event(t, cmd.OperationID, f.gen.MustNew())
				foreign.EventType = eventType
				events := []event.Envelope{valid, foreign}
				var err error
				if entry == "execute" {
					_, err = f.store.Execute(ctx, f.db, cmd, f.putItem("ghost", "value", events...))
				} else {
					if entry == "complete" {
						if _, err := f.store.Accept(ctx, f.db, cmd, StagePrepared, nil, nil); err != nil {
							t.Fatal(err)
						}
					}
					tx, txErr := f.db.BeginTx(ctx, nil)
					if txErr != nil {
						t.Fatal(txErr)
					}
					defer tx.Rollback()
					res, txErr := f.putItem("ghost", "value", events...)(ctx, tx)
					if txErr != nil {
						t.Fatal(txErr)
					}
					if entry == "complete" {
						err = f.store.Complete(ctx, tx, cmd.OperationID, StagePrepared, res)
					} else {
						err = f.store.AppendEvents(ctx, tx, cmd.OperationID, events)
					}
					var n int
					if txErr := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&n); txErr != nil || n != 0 {
						t.Fatalf("invalid batch partially appended before rollback: n=%d err=%v", n, txErr)
					}
					if txErr := tx.Rollback(); txErr != nil {
						t.Fatal(txErr)
					}
				}
				if err == nil || (eventType == "principal.disabled" && !errors.Is(err, ErrForeignCommand)) {
					t.Fatalf("event ownership check: %v", err)
				}
				for _, table := range []string{"ledger_items", "outbox"} {
					if count(t, f.db, `SELECT count(*) FROM `+table) != 0 {
						t.Fatalf("%s survived the rejected event batch", table)
					}
				}
				receipt, readErr := f.store.LookupReceipt(ctx, f.db, cmd.Key())
				if readErr != nil {
					t.Fatal(readErr)
				}
				if entry == "complete" {
					op, readErr := f.store.GetOperation(ctx, f.db, cmd.OperationID)
					if readErr != nil || op.Stage != StagePrepared || receipt == nil || receipt.Status != ReceiptInProgress {
						t.Fatalf("rejected completion changed control records: op=%+v receipt=%+v err=%v", op, receipt, readErr)
					}
				} else if receipt != nil {
					t.Fatal("rejected write created a receipt")
				}
			})
		}
	}
}

func TestResumedOperationClearsCurrentFailure(t *testing.T) {
	for _, target := range []Stage{StagePrepared, StageInstalled, StageCommitted} {
		t.Run(string(target), func(t *testing.T) {
			f := newFixture(t, "ledger", "ledger.db")
			ctx := t.Context()
			cmd := f.cmd(t, "resume", `{}`)
			if _, err := f.store.Accept(ctx, f.db, cmd, StagePrepared, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Advance(ctx, f.db, cmd.OperationID, StagePrepared, StageBlocked, Update{FailureCode: errcode.Forbidden}); err != nil {
				t.Fatal(err)
			}
			if target == StageCommitted {
				finishOperation(t, f, cmd, Result{Status: ReceiptSucceeded, ResponseCode: 201})
			} else if err := f.store.Advance(ctx, f.db, cmd.OperationID, StageBlocked, target, Update{}); err != nil {
				t.Fatal(err)
			}
			op, err := f.store.GetOperation(ctx, f.db, cmd.OperationID)
			if err != nil || op.Stage != target || op.FailureCode != "" {
				t.Fatalf("stale failure in operation: %+v %v", op, err)
			}
			v, err := f.store.View(ctx, f.db, cmd.OperationID)
			if err != nil || v.Reason != nil {
				t.Fatalf("stale reason in resumed view: %+v %v", v, err)
			}
			assertViewValid(t, v)
		})
	}
}

func TestFailedCompletionReplacesPreviousBlockReason(t *testing.T) {
	f := newFixture(t, "ledger", "ledger.db")
	cmd := f.cmd(t, "failed-resume", `{}`)
	if _, err := f.store.Accept(t.Context(), f.db, cmd, StagePrepared, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Advance(t.Context(), f.db, cmd.OperationID, StagePrepared, StageBlocked, Update{FailureCode: errcode.Forbidden}); err != nil {
		t.Fatal(err)
	}
	finishOperation(t, f, cmd, Result{Status: ReceiptFailed, FailureCode: errcode.HashMismatch})
	v, err := f.store.View(t.Context(), f.db, cmd.OperationID)
	if err != nil || v.Stage != StageFailed || v.Reason == nil || v.Reason.Code != errcode.HashMismatch {
		t.Fatalf("final failure reason: %+v %v", v, err)
	}
}

func finishOperation(t *testing.T, f *fixture, cmd Context, res Result) {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.store.Complete(t.Context(), tx, cmd.OperationID, StageBlocked, res); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
