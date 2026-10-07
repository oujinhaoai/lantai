package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func caller(kind authz.PrincipalKind) authz.Context {
	return authz.Context{PrincipalID: ids.New(), PrincipalKind: kind, SessionID: ids.New()}
}

func mustAdmit(t *testing.T, s *Scheduler, who authz.Context) *Ticket {
	t.Helper()
	tk, err := s.Admit(t.Context(), who)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// admitsWithin 报告在很短的等待内能否获准；用于判定“会被挡住”。
func admitsWithin(s *Scheduler, who authz.Context) (*Ticket, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	tk, err := s.Admit(ctx, who)
	return tk, err == nil
}

func TestClassComesFromTrustedContext(t *testing.T) {
	s := New(Limits{})
	human := mustAdmit(t, s, caller(authz.Human))
	agent := mustAdmit(t, s, caller(authz.Agent))
	delegated := caller(authz.Human)
	delegated.DelegatedBy = ids.New()
	proxy := mustAdmit(t, s, delegated)
	if human.Class() != authz.Interactive || agent.Class() != authz.Batch || proxy.Class() != authz.Batch {
		t.Fatalf("classes = %s %s %s", human.Class(), agent.Class(), proxy.Class())
	}
	if _, err := s.Admit(t.Context(), authz.Context{}); !errors.Is(err, ErrInvalidCaller) {
		t.Fatalf("empty caller: %v", err)
	}
}

func TestInteractiveKeepsReservedCapacity(t *testing.T) {
	s := New(Limits{InteractiveSlots: 2, BatchSlots: 4, BatchPerPrincipal: 2})
	a, b := caller(authz.Agent), caller(authz.Agent)
	held := []*Ticket{mustAdmit(t, s, a), mustAdmit(t, s, a), mustAdmit(t, s, b), mustAdmit(t, s, b)}
	if _, ok := admitsWithin(s, caller(authz.Agent)); ok {
		t.Fatal("batch pool should be full")
	}
	// 批量全满时，交互请求照样立即获准。
	h1, ok1 := admitsWithin(s, caller(authz.Human))
	h2, ok2 := admitsWithin(s, caller(authz.Human))
	if !ok1 || !ok2 {
		t.Fatal("interactive transfers must not wait for batch transfers")
	}
	if _, ok := admitsWithin(s, caller(authz.Human)); ok {
		t.Fatal("interactive pool is limited as well")
	}
	// 交互全满也不影响批量：释放一个批量名额后，排队的批量请求获准。
	held[0].Release()
	if tk, ok := admitsWithin(s, caller(authz.Agent)); !ok {
		t.Fatal("a released batch slot must go to a waiting batch transfer")
	} else {
		tk.Release()
	}
	h1.Release()
	h2.Release()
	for _, tk := range held[1:] {
		tk.Release()
	}
	if st := s.Stats(); st.ActiveBatch != 0 || st.ActiveInteractive != 0 {
		t.Fatalf("leaked slots: %+v", st)
	}
}

func TestPerPrincipalLimitKeepsOthersProgressing(t *testing.T) {
	s := New(Limits{BatchSlots: 4, BatchPerPrincipal: 2})
	greedy := caller(authz.Agent)
	a := mustAdmit(t, s, greedy)
	b := mustAdmit(t, s, greedy)
	if _, ok := admitsWithin(s, greedy); ok {
		t.Fatal("per-principal limit not enforced")
	}
	other := caller(authz.Agent)
	if tk, ok := admitsWithin(s, other); !ok {
		t.Fatal("another principal must still get a slot")
	} else {
		tk.Release()
	}
	a.Release()
	b.Release()
}

func TestWaitingBatchIsServedInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New(Limits{BatchSlots: 1, BatchPerPrincipal: 1})
		first := mustAdmit(t, s, caller(authz.Agent))
		var mu sync.Mutex
		var order []int
		var wg sync.WaitGroup
		for i := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tk, err := s.Admit(t.Context(), caller(authz.Agent))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				tk.Release()
			}()
			// The held slot makes each admission durably block in the queue.
			// Start the next caller only after that has happened; sleeps cannot
			// establish arrival order when the machine is busy.
			synctest.Wait()
			if waiting := s.Stats().WaitingBatch; waiting != i+1 {
				t.Fatalf("queued callers = %d, want %d", waiting, i+1)
			}
		}
		first.Release()
		wg.Wait()
		if len(order) != 3 || order[0] != 0 || order[1] != 1 || order[2] != 2 {
			t.Fatalf("admission order = %v, want FIFO", order)
		}
	})
}

func TestCancelledWaitDoesNotLeak(t *testing.T) {
	s := New(Limits{BatchSlots: 1, BatchPerPrincipal: 1})
	who := caller(authz.Agent)
	held := mustAdmit(t, s, who)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Admit(ctx, caller(authz.Agent))
		done <- err
	}()
	for s.Stats().WaitingBatch == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	held.Release()
	held.Release() // 重复释放无副作用
	st := s.Stats()
	if st.ActiveBatch != 0 || st.WaitingBatch != 0 {
		t.Fatalf("stats after cancel = %+v", st)
	}
	if tk, ok := admitsWithin(s, who); !ok {
		t.Fatal("slot leaked after cancellation")
	} else {
		tk.Release()
	}
	s.mu.Lock()
	n := len(s.principals)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("per-principal state leaked: %d", n)
	}
}

type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept time.Duration
}

func (f *fakeClock) clock() Clock {
	return Clock{
		Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f.mu.Lock()
			f.now = f.now.Add(d)
			f.slept += d
			f.mu.Unlock()
			return nil
		},
	}
}

func TestBatchBandwidthIsLimitedAndYields(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	s := NewWithClock(Limits{BatchBytesPerSecond: 1000, BatchBytesPerSecondWhileInteractive: 250}, fc.clock())
	tk := mustAdmit(t, s, caller(authz.Agent))
	data := bytes.Repeat([]byte("x"), 3000)
	n, err := io.Copy(io.Discard, tk.Reader(t.Context(), bytes.NewReader(data)))
	if err != nil || n != 3000 {
		t.Fatal(n, err)
	}
	// 满桶 1 秒的量免等待，其余 2000 字节按 1000 B/s 等待约 2 秒。
	if fc.slept < 1900*time.Millisecond || fc.slept > 2100*time.Millisecond {
		t.Fatalf("slept %s for 3000 bytes at 1000 B/s", fc.slept)
	}
	// 有交互传输时批量让出带宽：同样 1000 字节等待更久。
	human := mustAdmit(t, s, caller(authz.Human))
	before := fc.slept
	if _, err := io.Copy(io.Discard, tk.Reader(t.Context(), bytes.NewReader(data[:1000]))); err != nil {
		t.Fatal(err)
	}
	if waited := fc.slept - before; waited < 3900*time.Millisecond {
		t.Fatalf("batch did not yield to interactive traffic: waited %s for 1000 bytes", waited)
	}
	// 交互档本身不限速。
	before = fc.slept
	if _, err := io.Copy(io.Discard, human.Reader(t.Context(), bytes.NewReader(data))); err != nil {
		t.Fatal(err)
	}
	if fc.slept != before {
		t.Fatal("interactive transfers must not be throttled")
	}
	human.Release()
	tk.Release()
}

func TestReadSeekerKeepsSeek(t *testing.T) {
	s := New(Limits{BatchBytesPerSecond: 1 << 30})
	tk := mustAdmit(t, s, caller(authz.Agent))
	defer tk.Release()
	rs := tk.ReadSeeker(t.Context(), bytes.NewReader([]byte("0123456789")))
	if _, err := rs.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(rs)
	if string(rest) != "56789" {
		t.Fatalf("after seek: %q", rest)
	}
}
