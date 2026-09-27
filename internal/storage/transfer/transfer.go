// Package transfer 是传输面的准入与带宽调度（T02.5）。
//
// 传输分两档：交互档（人本人的会话：看原件、拖动播放、单个下载）有保留的
// 并发，排在前面；批量档（Agent、执行器、委托会话的入藏与拉取）受每个主体
// 与全局的并发上限及总带宽限制，有交互流量时进一步让出带宽。档位只取自服务
// 端验证过的会话（authz.Context.TransferClass），客户端自报的优先级一律忽略。
//
// 两档使用互不借用的并发池：交互请求永远不等批量请求，批量请求也不会因为
// 持续的交互流量而永久饥饿。同档内按到达顺序（FIFO）准入；批量档先按主体
// 限额排队，再进入全局池，单个主体的大量请求不能占满全局名额。
//
// 本包只做准入与限速，监听、路由、超时与客户端连接池归 transport（T07）。
package transfer

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Limits 是调度参数；零值字段取 DefaultLimits 中的值。默认值只是起点，
// 在目标 NAS 上实测后再调（见设计中的传输分档）。
type Limits struct {
	// InteractiveSlots 是交互档保留的并发数。
	InteractiveSlots int
	// BatchSlots 是批量档的全局并发上限。
	BatchSlots int
	// BatchPerPrincipal 是同一主体的批量并发上限。
	BatchPerPrincipal int
	// BatchBytesPerSecond 是批量档的总带宽上限；0 表示不限。
	BatchBytesPerSecond int64
	// BatchBytesPerSecondWhileInteractive 是有交互传输进行时批量档的总带宽
	// 上限；0 表示取 BatchBytesPerSecond 的一半（BatchBytesPerSecond 为 0 时不限）。
	BatchBytesPerSecondWhileInteractive int64
}

// DefaultLimits 返回设计基线中的默认值：交互保留 4，批量全局 8、每主体 4。
func DefaultLimits() Limits {
	return Limits{InteractiveSlots: 4, BatchSlots: 8, BatchPerPrincipal: 4}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.InteractiveSlots <= 0 {
		l.InteractiveSlots = d.InteractiveSlots
	}
	if l.BatchSlots <= 0 {
		l.BatchSlots = d.BatchSlots
	}
	if l.BatchPerPrincipal <= 0 {
		l.BatchPerPrincipal = d.BatchPerPrincipal
	}
	if l.BatchPerPrincipal > l.BatchSlots {
		l.BatchPerPrincipal = l.BatchSlots
	}
	if l.BatchBytesPerSecondWhileInteractive <= 0 && l.BatchBytesPerSecond > 0 {
		l.BatchBytesPerSecondWhileInteractive = max(1, l.BatchBytesPerSecond/2)
	}
	return l
}

// Scheduler 是一个传输面的准入调度器，可并发使用。
type Scheduler struct {
	limits      Limits
	interactive *semaphore.Weighted
	batch       *semaphore.Weighted
	bucket      *bucket

	mu           sync.Mutex
	principals   map[ids.ID]*principalSlot
	active       map[authz.TransferClass]int
	waiting      map[authz.TransferClass]int
	admittedEver map[authz.TransferClass]int64
}

type principalSlot struct {
	sem  *semaphore.Weighted
	refs int
}

// Clock 让测试控制限速的时间；nil 字段使用系统时间。
type Clock struct {
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// New 创建调度器。
func New(l Limits) *Scheduler { return NewWithClock(l, Clock{}) }

// NewWithClock 创建使用给定时钟的调度器（测试用）。
func NewWithClock(l Limits, clk Clock) *Scheduler {
	l = l.withDefaults()
	if clk.Now == nil {
		clk.Now = time.Now
	}
	if clk.Sleep == nil {
		clk.Sleep = sleepCtx
	}
	s := &Scheduler{
		limits:       l,
		interactive:  semaphore.NewWeighted(int64(l.InteractiveSlots)),
		batch:        semaphore.NewWeighted(int64(l.BatchSlots)),
		principals:   map[ids.ID]*principalSlot{},
		active:       map[authz.TransferClass]int{},
		waiting:      map[authz.TransferClass]int{},
		admittedEver: map[authz.TransferClass]int64{},
	}
	s.bucket = &bucket{clk: clk, rate: s.batchRate}
	return s
}

// Limits 返回生效的参数。
func (s *Scheduler) Limits() Limits { return s.limits }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// batchRate 返回当前批量档的总带宽上限；0 表示不限。
func (s *Scheduler) batchRate() int64 {
	s.mu.Lock()
	interactive := s.active[authz.Interactive]
	s.mu.Unlock()
	if interactive > 0 && s.limits.BatchBytesPerSecondWhileInteractive > 0 {
		return s.limits.BatchBytesPerSecondWhileInteractive
	}
	return s.limits.BatchBytesPerSecond
}

// ErrInvalidCaller 表示调用者上下文不完整，无法定档。
var ErrInvalidCaller = errors.New("transfer: caller context has no principal")

// Ticket 是一次获准的传输；结束后必须 Release。
type Ticket struct {
	s         *Scheduler
	class     authz.TransferClass
	principal ids.ID
	once      sync.Once
}

// Class 返回获准的档位。
func (t *Ticket) Class() authz.TransferClass { return t.class }

// Admit 按调用者的可信档位排队准入，ctx 取消时放弃排队并返回其错误。
func (s *Scheduler) Admit(ctx context.Context, who authz.Context) (*Ticket, error) {
	if !who.PrincipalID.Valid() {
		return nil, ErrInvalidCaller
	}
	class := who.TransferClass()
	s.mu.Lock()
	s.waiting[class]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.waiting[class]--
		s.mu.Unlock()
	}()
	t := &Ticket{s: s, class: class, principal: who.PrincipalID}
	if class == authz.Interactive {
		if err := s.interactive.Acquire(ctx, 1); err != nil {
			return nil, err
		}
	} else {
		slot := s.principalSlot(who.PrincipalID)
		if err := slot.sem.Acquire(ctx, 1); err != nil {
			s.dropPrincipal(who.PrincipalID)
			return nil, err
		}
		if err := s.batch.Acquire(ctx, 1); err != nil {
			slot.sem.Release(1)
			s.dropPrincipal(who.PrincipalID)
			return nil, err
		}
	}
	s.mu.Lock()
	s.active[class]++
	s.admittedEver[class]++
	s.mu.Unlock()
	return t, nil
}

func (s *Scheduler) principalSlot(p ids.ID) *principalSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.principals[p]
	if slot == nil {
		slot = &principalSlot{sem: semaphore.NewWeighted(int64(s.limits.BatchPerPrincipal))}
		s.principals[p] = slot
	}
	slot.refs++
	return slot
}

func (s *Scheduler) dropPrincipal(p ids.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot := s.principals[p]; slot != nil {
		slot.refs--
		if slot.refs == 0 {
			delete(s.principals, p)
		}
	}
}

// Release 归还名额；重复调用无副作用。
func (t *Ticket) Release() {
	t.once.Do(func() {
		s := t.s
		if t.class == authz.Interactive {
			s.interactive.Release(1)
		} else {
			s.mu.Lock()
			slot := s.principals[t.principal]
			s.mu.Unlock()
			s.batch.Release(1)
			if slot != nil {
				slot.sem.Release(1)
			}
			s.dropPrincipal(t.principal)
		}
		s.mu.Lock()
		s.active[t.class]--
		s.mu.Unlock()
	})
}

// Stats 是调度器当前状态，供运维观测。
type Stats struct {
	ActiveInteractive  int   `json:"active_interactive"`
	ActiveBatch        int   `json:"active_batch"`
	WaitingInteractive int   `json:"waiting_interactive"`
	WaitingBatch       int   `json:"waiting_batch"`
	AdmittedBatch      int64 `json:"admitted_batch"`
	AdmittedInteract   int64 `json:"admitted_interactive"`
}

// Stats 返回当前计数。
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		ActiveInteractive: s.active[authz.Interactive], ActiveBatch: s.active[authz.Batch],
		WaitingInteractive: s.waiting[authz.Interactive], WaitingBatch: s.waiting[authz.Batch],
		AdmittedBatch: s.admittedEver[authz.Batch], AdmittedInteract: s.admittedEver[authz.Interactive],
	}
}

// Reader 返回受本票据档位限速的读取器：批量档按共享带宽上限限速，交互档不限。
func (t *Ticket) Reader(ctx context.Context, r io.Reader) io.Reader {
	if t.class == authz.Interactive {
		return r
	}
	return &limitedReader{ctx: ctx, r: r, b: t.s.bucket}
}

// ReadSeeker 与 Reader 相同，但保留 Seek（供 Range 下载）。
func (t *Ticket) ReadSeeker(ctx context.Context, rs io.ReadSeeker) io.ReadSeeker {
	if t.class == authz.Interactive {
		return rs
	}
	return &limitedReadSeeker{limitedReader: limitedReader{ctx: ctx, r: rs, b: t.s.bucket}, seeker: rs}
}

// chunk 是一次限速等待的最大字节数，避免大块读取造成突发。
const chunk = 256 << 10

type limitedReader struct {
	ctx context.Context
	r   io.Reader
	b   *bucket
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if len(p) > chunk {
		p = p[:chunk]
	}
	n, err := l.r.Read(p)
	if n > 0 {
		if werr := l.b.wait(l.ctx, int64(n)); werr != nil {
			return n, werr
		}
	}
	return n, err
}

type limitedReadSeeker struct {
	limitedReader
	seeker io.Seeker
}

func (l *limitedReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return l.seeker.Seek(offset, whence)
}

// bucket 是批量档共享的限速器（虚拟调度 / GCRA）：tat 是按当前速率把已放行
// 字节全部“发完”的理论时间；允许最多一秒的突发，超出部分按速率等待。速率
// 变化只影响之后放行的字节。
type bucket struct {
	clk  Clock
	rate func() int64

	mu  sync.Mutex
	tat time.Time
}

// burst 是允许的突发窗口。
const burst = time.Second

func (b *bucket) wait(ctx context.Context, n int64) error {
	rate := b.rate()
	if rate <= 0 {
		return nil
	}
	b.mu.Lock()
	now := b.clk.Now()
	tat := b.tat
	if floor := now.Add(-burst); tat.Before(floor) {
		tat = floor
	}
	tat = tat.Add(time.Duration(float64(n) / float64(rate) * float64(time.Second)))
	b.tat = tat
	b.mu.Unlock()
	if d := tat.Sub(now); d > 0 {
		return b.clk.Sleep(ctx, d)
	}
	return nil
}
