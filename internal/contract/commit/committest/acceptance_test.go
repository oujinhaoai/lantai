package committest

import (
	"context"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type verifyFunc func(context.Context, authz.Context, commit.Prepared) error

func (f verifyFunc) VerifyAcceptance(ctx context.Context, who authz.Context, p commit.Prepared) error {
	return f(ctx, who, p)
}

func TestAcceptanceHoldsSharedGuardAndDoesNotDeadlockReader(t *testing.T) {
	h := NewHarness(t)
	m := h.Ledger.(*Memory)
	gate := commands.NewGate(commands.NewCoordinator())
	gate.Open()
	m.SetGate(gate)
	a := newActor(h)
	req := a.newAsset(h, "asset/a", "body")
	p, err := m.Prepare(t.Context(), a.cmd(t, "key", req), req)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := h.Installer.Install(t.Context(), p.InstallRequest())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	m.SetAcceptanceVerifier(verifyFunc(func(ctx context.Context, who authz.Context, got commit.Prepared) error {
		calls++
		if got.OperationID != p.OperationID || who.PrincipalID != a.who.PrincipalID {
			t.Fatalf("wrong acceptance input: %+v", got)
		}
		// Reader 必须可重入；复验会读取别的来源版本。
		if _, err := m.Versions(ctx, "", 10); err != nil {
			return err
		}
		// 复验期间独占撤权不能越过 shared guard。
		deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, lock, err := gate.Coordinator().Acquire(deadline, commands.Request{Security: commands.ModeExclusive})
		if err == nil {
			lock.Release()
			t.Fatal("acceptance did not hold security guard")
		}
		return errcode.New(errcode.UseRestricted, "input rights changed")
	}))
	_, err = m.Commit(t.Context(), p.OperationID, a.who, proof)
	wantCode(t, err, errcode.UseRestricted)
	wantCode(t, readErr(h, p), errcode.NotFound)
	view, _ := m.Operation(t.Context(), p.OperationID)
	if view.Stage != commands.StageBlocked || (view.Reason == nil || view.Reason.Code != errcode.UseRestricted) {
		t.Fatalf("blocked view: %+v", view)
	}
	m.SetAcceptanceVerifier(verifyFunc(func(context.Context, authz.Context, commit.Prepared) error { calls++; return nil }))
	c, err := m.Commit(t.Context(), p.OperationID, a.who, proof)
	if err != nil {
		t.Fatal(err)
	}
	m.SetAcceptanceVerifier(verifyFunc(func(context.Context, authz.Context, commit.Prepared) error {
		t.Fatal("revalidated committed replay")
		return nil
	}))
	gate.Close(commands.ReasonMaintenance)
	again, err := m.Commit(t.Context(), p.OperationID, a.who, proof)
	if err != nil || again != c || calls != 2 {
		t.Fatalf("replay: %+v %v calls=%d", again, err, calls)
	}
	view, _ = m.Operation(t.Context(), p.OperationID)
	if view.Reason != nil {
		t.Fatalf("stale failure after recovery: %+v", view)
	}
}

func TestLookupPreparedPreservesReservationAndIdentity(t *testing.T) {
	h := NewHarness(t)
	a := newActor(h)
	req := a.newAsset(h, "asset/a", "a")
	cmd := a.cmd(t, "same", req)
	_, err := h.Ledger.LookupPrepared(t.Context(), cmd)
	wantCode(t, err, errcode.NotFound)
	p, err := h.Ledger.Prepare(t.Context(), cmd, req)
	if err != nil {
		t.Fatal(err)
	}
	retry := cmd
	retry.OperationID = ids.New()
	retry.SessionID = ids.New()
	got, err := h.Ledger.LookupPrepared(t.Context(), retry)
	if err != nil || got.OperationID != p.OperationID || got.ActorID != cmd.ActorID || got.SessionID != cmd.SessionID {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	got.Files[0].Path = "mutated"
	again, err := h.Ledger.LookupPrepared(t.Context(), cmd)
	if err != nil || again.Files[0].Path != p.Files[0].Path {
		t.Fatalf("lookup leaked mutable state: %+v %v", again, err)
	}
	retry.RequestHash = digest.Of([]byte("different"))
	_, err = h.Ledger.LookupPrepared(t.Context(), retry)
	wantCode(t, err, errcode.IdempotencyConflict)
	retry = cmd
	retry.ActorID = ids.New()
	_, err = h.Ledger.LookupPrepared(t.Context(), retry)
	wantCode(t, err, errcode.NotFound)
}
