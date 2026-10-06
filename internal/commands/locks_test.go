package commands

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockOrderViolationsRejected(t *testing.T) {
	c := NewCoordinator()
	ctx, h, err := c.Acquire(t.Context(), Request{Barrier: ModeShared, Security: ModeShared, Assets: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	for name, req := range map[string]Request{
		"barrier again":   {Barrier: ModeShared},
		"security again":  {Security: ModeShared},
		"project after":   {Projects: []string{"p"}},
		"same asset":      {Assets: []string{"b"}},
		"earlier asset":   {Assets: []string{"a"}},
		"mixed backwards": {Assets: []string{"c"}, Projects: []string{"p"}},
	} {
		if _, _, err := c.Acquire(ctx, req); !errors.Is(err, ErrLockOrder) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// 向后嵌套允许：更大的 asset、task、blob。
	ctx2, h2, err := c.Acquire(ctx, Request{Assets: []string{"c"}, Tasks: []string{"t1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Acquire(ctx2, Request{Assets: []string{"d"}}); !errors.Is(err, ErrLockOrder) {
		t.Fatal("after taking a task lock, asset locks are out of order")
	}
	_, h3, err := c.Acquire(ctx2, Request{Blobs: []string{"ff"}})
	if err != nil {
		t.Fatal(err)
	}
	h3.Release()
	h2.Release()
	h2.Release() // 幂等
}

// 已持有的 security_guard 沿 ctx 传给嵌套调用；释放或换协调器后不再算持有。
func TestHoldsSecurityFollowsTheHeldChain(t *testing.T) {
	c := NewCoordinator()
	if HoldsSecurity(t.Context(), c) {
		t.Fatal("empty context holds nothing")
	}
	ctx, h, err := c.Acquire(t.Context(), Request{Security: ModeShared, Projects: []string{"p"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx2, h2, err := c.Acquire(ctx, Request{Tasks: []string{"t"}})
	if err != nil {
		t.Fatal(err)
	}
	if !HoldsSecurity(ctx, c) || !HoldsSecurity(ctx2, c) {
		t.Fatal("security guard must be visible to nested calls")
	}
	if HoldsSecurity(ctx2, NewCoordinator()) {
		t.Fatal("another coordinator's guard is not held")
	}
	_, h3, err := c.Acquire(t.Context(), Request{Projects: []string{"q"}})
	if err != nil {
		t.Fatal(err)
	}
	h2.Release()
	h.Release()
	h3.Release()
	if HoldsSecurity(ctx, c) || HoldsSecurity(ctx2, c) {
		t.Fatal("released guard still reported as held")
	}
	pctx, hp, err := c.Acquire(t.Context(), Request{Projects: []string{"p"}})
	if err != nil {
		t.Fatal(err)
	}
	defer hp.Release()
	if HoldsSecurity(pctx, c) {
		t.Fatal("a project lock alone is not the security guard")
	}
}

func TestKeysAreMutuallyExclusiveAndSorted(t *testing.T) {
	c := NewCoordinator()
	var inside atomic.Int32
	var maxInside atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 两组 goroutine 以相反顺序给出相同的键集合，排序保证不会死锁。
			req := Request{Assets: []string{"x", "y"}}
			if i%2 == 1 {
				req = Request{Assets: []string{"y", "x"}}
			}
			_, h, err := c.Acquire(context.Background(), req)
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			for {
				m := maxInside.Load()
				if n <= m || maxInside.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			h.Release()
		}()
	}
	wg.Wait()
	if maxInside.Load() != 1 {
		t.Fatalf("%d holders at once", maxInside.Load())
	}
	c.mu.Lock()
	leaked := len(c.keys)
	c.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("%d key locks leaked", leaked)
	}
}

func TestExclusiveWaitsForSharedAndIsNotStarved(t *testing.T) {
	c := NewCoordinator()
	_, reader, err := c.Acquire(t.Context(), Request{Security: ModeShared})
	if err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan struct{})
	go func() {
		_, w, err := c.Acquire(context.Background(), Request{Security: ModeExclusive})
		if err != nil {
			t.Error(err)
			return
		}
		close(writerDone)
		w.Release()
	}()
	time.Sleep(20 * time.Millisecond)
	// 写者排队后，新读者必须等写者完成，撤权不会被持续的提交饿死。
	lateReader := make(chan time.Time, 1)
	go func() {
		_, r, err := c.Acquire(context.Background(), Request{Security: ModeShared})
		if err != nil {
			t.Error(err)
			return
		}
		lateReader <- time.Now()
		r.Release()
	}()
	select {
	case <-writerDone:
		t.Fatal("writer must wait for the existing reader")
	case <-time.After(30 * time.Millisecond):
	}
	reader.Release()
	<-writerDone
	select {
	case <-lateReader:
	case <-time.After(time.Second):
		t.Fatal("late reader never proceeded")
	}
}

func TestAcquireHonoursContext(t *testing.T) {
	c := NewCoordinator()
	_, h, err := c.Acquire(t.Context(), Request{Barrier: ModeExclusive, Assets: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := c.Acquire(ctx, Request{Barrier: ModeShared}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("barrier wait: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel2()
	if _, _, err := c.Acquire(ctx2, Request{Assets: []string{"a"}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("key wait: %v", err)
	}
	h.Release()
	// 超时放弃的等待不应留下锁状态。
	_, h2, err := c.Acquire(t.Context(), Request{Barrier: ModeExclusive, Assets: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	h2.Release()
}
