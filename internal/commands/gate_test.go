package commands

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func maintenanceReason(t *testing.T, err error) string {
	t.Helper()
	e, ok := errcode.As(err)
	if !ok || e.Code != errcode.MaintenanceMode {
		t.Fatalf("want MAINTENANCE_MODE, got %v", err)
	}
	if len(e.Details) != 1 {
		t.Fatalf("details = %+v", e.Details)
	}
	return e.Details[0].Reason
}

func TestGateStartsClosedAndOpens(t *testing.T) {
	g := NewGate(NewCoordinator())
	if _, _, err := g.Acquire(t.Context(), Request{Security: ModeShared}); maintenanceReason(t, err) != ReasonStarting {
		t.Fatal("new gate must reject writes while starting")
	}
	g.Open()
	ctx, h, err := g.Acquire(t.Context(), Request{Security: ModeShared, Assets: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if ctx == nil || h == nil {
		t.Fatal("missing lock context")
	}
	h.Release()
	if _, _, err := g.Acquire(t.Context(), Request{Barrier: ModeExclusive}); !errors.Is(err, ErrExclusiveViaGate) {
		t.Fatalf("exclusive via Acquire = %v", err)
	}
}

// 维护先关闭写入，再等在途写入结束；等待期间到达的新写入立即被拒，
// 不会排在维护之后继续执行。
func TestGateMaintainWaitsForInFlightAndRejectsNew(t *testing.T) {
	g := NewGate(NewCoordinator())
	g.Open()
	_, inflight, err := g.Acquire(t.Context(), Request{Security: ModeShared})
	if err != nil {
		t.Fatal(err)
	}
	var entered atomic.Bool
	done := make(chan *Held)
	go func() {
		_, h, err := g.Maintain(context.Background(), ReasonMaintenance)
		if err != nil {
			t.Error(err)
		}
		entered.Store(true)
		done <- h
	}()
	// 等待维护开始关闭写入。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if open, _ := g.State(); !open || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, _, err := g.Acquire(t.Context(), Request{}); maintenanceReason(t, err) != ReasonMaintenance {
		t.Fatal("new write during maintenance must be rejected")
	}
	time.Sleep(20 * time.Millisecond)
	if entered.Load() {
		t.Fatal("maintenance must wait for the in-flight write")
	}
	inflight.Release()
	h := <-done
	if _, _, err := g.Acquire(t.Context(), Request{}); maintenanceReason(t, err) != ReasonMaintenance {
		t.Fatal("writes stay closed while maintenance holds the barrier")
	}
	h.Release()
	if _, _, err := g.Acquire(t.Context(), Request{}); maintenanceReason(t, err) != ReasonMaintenance {
		t.Fatal("releasing the barrier does not reopen writes by itself")
	}
	g.Open()
	_, h2, err := g.Acquire(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	h2.Release()
}

// 已通过第一次状态检查、排在维护者之后等锁的写入，即使维护结束、写入口
// 重新开放，也不能接着执行：写入不跨过维护窗口。
func TestGateWriteDoesNotStraddleMaintenance(t *testing.T) {
	g := NewGate(NewCoordinator())
	g.Open()
	_, inflight, err := g.Acquire(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	maintained := make(chan *Held)
	go func() {
		_, h, err := g.Maintain(context.Background(), ReasonMaintenance)
		if err != nil {
			t.Error(err)
		}
		maintained <- h
	}()
	// 维护者已关闭写入并在等在途写入；此时模拟一个在关闭前通过首检的写入：
	// 直接从协调器排队，再按 Acquire 的方式复核代次。
	waitClosed(t, g)
	gen := g.gen - 1 // 关闭前的代次
	result := make(chan error, 1)
	go func() {
		_, h, err := g.c.Acquire(context.Background(), Request{Barrier: ModeShared})
		if err == nil {
			_, err = g.check(gen, true)
			h.Release()
		}
		result <- err
	}()
	inflight.Release()
	h := <-maintained
	g.Open() // 维护结束，写入口重新开放
	h.Release()
	if reason := maintenanceReason(t, <-result); reason != ReasonMaintenance {
		t.Fatalf("reason = %s", reason)
	}
	// 维护结束后新到的写入正常。
	_, h2, err := g.Acquire(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	h2.Release()
}

func waitClosed(t *testing.T, g *Gate) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if open, _ := g.State(); !open {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("gate never closed")
		}
		time.Sleep(time.Millisecond)
	}
}

// 取锁后再次检查状态：排队期间写入口被关闭（停止）时拒绝。
func TestGateRechecksAfterWaiting(t *testing.T) {
	c := NewCoordinator()
	g := NewGate(c)
	g.Open()
	// 模拟维护者已持有独占锁，但还没关闭写入（Maintain 内部顺序的反面）。
	_, excl, err := c.Acquire(t.Context(), Request{Barrier: ModeExclusive})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, h, err := g.Acquire(context.Background(), Request{})
		if h != nil {
			h.Release()
		}
		result <- err
	}()
	time.Sleep(20 * time.Millisecond)
	g.Close(ReasonStopping)
	excl.Release()
	if reason := maintenanceReason(t, <-result); reason != ReasonStopping {
		t.Fatalf("reason = %s", reason)
	}
}

func TestGateMaintainHonoursCancel(t *testing.T) {
	g := NewGate(NewCoordinator())
	g.Open()
	_, inflight, err := g.Acquire(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer inflight.Release()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := g.Maintain(ctx, ReasonStopping); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Maintain with expired ctx = %v", err)
	}
	if open, reason := g.State(); open || reason != ReasonStopping {
		t.Fatalf("gate must stay closed after a failed Maintain: %v %s", open, reason)
	}
}

func TestGateRejectsBadReason(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("invalid reason must panic")
		}
	}()
	NewGate(NewCoordinator()).Close("Not Valid")
}
