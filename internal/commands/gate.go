package commands

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

// Gate 是实例维护屏障的写入口（T08.1）。前台命令与后台写入者都必须经
// Acquire 进入写路径：实例未开放写入（启动、迁移、恢复、维护、停止、诊断）
// 时立即返回 MAINTENANCE_MODE，而不是排队等待；开放时先取屏障共享锁，再按
// 统一顺序取请求中的其余锁。维护经 Maintain 先关闭写入、再取屏障独占锁，
// 等在途写入全部结束后才开始。
//
// 只读请求不经过 Gate；下载开始等最终读取检查只取 security_guard 读锁。
type Gate struct {
	c *Coordinator

	mu     sync.Mutex
	open   bool
	reason string
	// gen 在每次关闭写入时递增；取锁前后代次不同说明中间经历过维护或停止，
	// 这次写入即使在维护结束后拿到锁也拒绝，写入不会跨过维护窗口。
	gen        uint64
	lastReason string
}

// 关闭写入的常用原因，写入 MAINTENANCE_MODE 错误的 details.reason。
const (
	ReasonInitializing = "initializing"
	ReasonStarting     = "starting"
	ReasonRecovering   = "recovering"
	ReasonMaintenance  = "maintenance"
	ReasonMigrating    = "migrating"
	ReasonStopping     = "stopping"
	ReasonDiagnostic   = "diagnostic"
)

var reasonRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// NewGate 创建关闭状态（starting）的写入口；实例完成启动检查后调用 Open。
func NewGate(c *Coordinator) *Gate {
	return &Gate{c: c, reason: ReasonStarting}
}

// Coordinator 返回底层协调器；取锁时仍须经 Gate，避免绕过维护屏障。
func (g *Gate) Coordinator() *Coordinator { return g.c }

// Stats returns aggregate lock contention without resource names.
func (g *Gate) Stats() LockStats { return g.c.Stats() }

// ErrExclusiveViaGate 表示试图经 Acquire 取屏障独占锁；维护必须用 Maintain。
var ErrExclusiveViaGate = errors.New("commands: exclusive barrier must be taken with Gate.Maintain")

// Acquire 在实例开放写入时取屏障共享锁与 req 中的其余锁，返回携带锁集合的
// ctx；未开放时返回 MAINTENANCE_MODE。取锁后再确认一次：等待期间实例进入过
// 维护或停止（写入口代次变化）时释放并拒绝，即使维护已经结束也不接着写。
func (g *Gate) Acquire(ctx context.Context, req Request) (context.Context, *Held, error) {
	if req.Barrier == ModeExclusive {
		return ctx, nil, ErrExclusiveViaGate
	}
	// A trusted startup/recovery hook may call normal domain writes while its
	// parent owns this gate's exclusive barrier. Do not reacquire that barrier.
	if RequireMaintenance(ctx, g.c) == nil {
		req.Barrier = ModeNone
		lctx, h, err := g.c.Acquire(ctx, req)
		if err != nil {
			return ctx, nil, err
		}
		if err = RequireMaintenance(lctx, g.c); err != nil {
			h.Release()
			return ctx, nil, err
		}
		return lctx, h, nil
	}
	gen, err := g.check(0, false)
	if err != nil {
		return ctx, nil, err
	}
	req.Barrier = ModeShared
	lctx, h, err := g.c.Acquire(ctx, req)
	if err != nil {
		return ctx, nil, err
	}
	if _, err := g.check(gen, true); err != nil {
		h.Release()
		return ctx, nil, err
	}
	return lctx, h, nil
}

// RequireMaintenance binds maintenance checks to this gate's coordinator.
func (g *Gate) RequireMaintenance(ctx context.Context) error { return RequireMaintenance(ctx, g.c) }

// HoldsSecurity reports whether ctx carries a live security_guard of this gate.
func (g *Gate) HoldsSecurity(ctx context.Context) bool { return HoldsSecurity(ctx, g.c) }

func (g *Gate) check(gen uint64, compare bool) (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case !g.open:
		return 0, maintenanceErr(g.reason)
	case compare && g.gen != gen:
		return 0, maintenanceErr(g.lastReason)
	}
	return g.gen, nil
}

func maintenanceErr(reason string) error {
	return errcode.New(errcode.MaintenanceMode, "the instance is not accepting writes").
		WithDetails(errcode.Detail{Reason: reason})
}

// State 报告是否开放写入及关闭原因。
func (g *Gate) State() (open bool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open, g.reason
}

// Open 开放写入。
func (g *Gate) Open() {
	g.mu.Lock()
	g.open, g.reason = true, ""
	g.mu.Unlock()
}

// Close 关闭写入：之后的 Acquire 立即返回 MAINTENANCE_MODE；已持锁的在途写入
// 不受影响，需要等待它们结束时用 Maintain。
func (g *Gate) Close(reason string) {
	if !reasonRE.MatchString(reason) {
		panic(fmt.Sprintf("commands: invalid gate reason %q", reason))
	}
	g.mu.Lock()
	g.open, g.reason, g.lastReason = false, reason, reason
	g.gen++
	g.mu.Unlock()
}

// Maintain 关闭写入并取得屏障独占锁：返回时所有在途写入都已结束，新的写入
// 一律被拒。释放返回的 Held 后写入仍保持关闭，由调用方确认实例可写后再
// Open。ctx 取消时放弃等待并返回错误，写入保持关闭。
func (g *Gate) Maintain(ctx context.Context, reason string) (context.Context, *Held, error) {
	g.Close(reason)
	return g.c.Acquire(ctx, Request{Barrier: ModeExclusive})
}
