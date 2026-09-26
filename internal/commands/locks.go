package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"golang.org/x/sync/semaphore"
)

// 锁层级即全局取锁顺序：实例维护屏障 → security_guard → project/namespace
// → asset → task/attempt → blob。同一层按键排序。任何一次取锁都一次性给出
// 完整集合；已持有锁时只能继续取更靠后的键，禁止反向嵌套。锁内不做上传或
// 远程网络调用。这些锁只在单个核心进程内有效，多进程写入需要另行设计。
const (
	levelBarrier = iota
	levelSecurity
	levelProject
	levelAsset
	levelTask
	levelBlob
)

// Mode 是读写锁模式。
type Mode int

const (
	// ModeNone 不取该锁。
	ModeNone Mode = iota
	// ModeShared 共享：业务提交取屏障共享、security_guard 读。
	ModeShared
	// ModeExclusive 独占：维护/备份取屏障独占；撤权、角色/策略变更、
	// 限制激活、锁定、审定撤销、敏感授权撤销取 security_guard 写。
	ModeExclusive
)

// Request 描述一次需要的全部锁。
type Request struct {
	Barrier    Mode
	Security   Mode
	Projects   []string
	Namespaces []string // 形如 "<project_id>/<normalized_slug>"
	Assets     []string
	Tasks      []string // 任务或 attempt ID
	Blobs      []string // sha256 十六进制
}

type lockKey struct {
	level int
	key   string
}

func (a lockKey) less(b lockKey) bool {
	if a.level != b.level {
		return a.level < b.level
	}
	return a.key < b.key
}

func (r Request) keys() []lockKey {
	var ks []lockKey
	add := func(level int, prefix string, vals []string) {
		for _, v := range vals {
			ks = append(ks, lockKey{level, prefix + v})
		}
	}
	add(levelProject, "project:", r.Projects)
	add(levelProject, "namespace:", r.Namespaces)
	add(levelAsset, "asset:", r.Assets)
	add(levelTask, "task:", r.Tasks)
	add(levelBlob, "blob:", r.Blobs)
	sort.Slice(ks, func(i, j int) bool { return ks[i].less(ks[j]) })
	return slices.CompactFunc(ks, func(a, b lockKey) bool { return a == b })
}

// ErrLockOrder 表示已持有锁时试图取顺序更靠前（或相同）的锁。
var ErrLockOrder = errors.New("commands: lock acquisition violates the global lock order")

// Coordinator 在单个核心进程内按统一顺序协调锁。
type Coordinator struct {
	barrier  *semaphore.Weighted
	security *semaphore.Weighted

	mu   sync.Mutex
	keys map[string]*keyLock
}

type keyLock struct {
	sem  chan struct{}
	refs int
}

// rwWeight 让独占模式占满全部权重；semaphore 按 FIFO 服务，等待中的写者
// 会阻止后到的读者，撤权与维护不会被持续的业务请求饿死。
const rwWeight = 1 << 30

// NewCoordinator 创建协调器。
func NewCoordinator() *Coordinator {
	return &Coordinator{
		barrier:  semaphore.NewWeighted(rwWeight),
		security: semaphore.NewWeighted(rwWeight),
		keys:     map[string]*keyLock{},
	}
}

type heldKey struct{}

// Held 是一次成功取得的锁集合。
type Held struct {
	c        *Coordinator
	barrier  Mode
	security Mode
	keys     []lockKey
	max      lockKey
	parent   *Held
	once     sync.Once
}

// Acquire 按全局顺序取得 req 中的全部锁，等待期间遵守 ctx 取消。
// 返回的 ctx 携带已持有集合，用它继续取锁时会检查顺序。顺序检查只沿 ctx
// 传递：持有锁时必须用返回的 ctx 继续取锁；改用其他 ctx 会绕过检查并可能
// 与反向取锁的调用方死锁（只能等 ctx 超时解开）。
func (c *Coordinator) Acquire(ctx context.Context, req Request) (context.Context, *Held, error) {
	parent, _ := ctx.Value(heldKey{}).(*Held)
	keys := req.keys()
	if err := checkOrder(parent, req, keys); err != nil {
		return ctx, nil, err
	}
	h := &Held{c: c, parent: parent}
	if parent != nil {
		h.max = parent.max
	} else {
		h.max = lockKey{level: -1}
	}
	release := func() { h.release() }
	if req.Barrier != ModeNone {
		if err := c.barrier.Acquire(ctx, weight(req.Barrier)); err != nil {
			return ctx, nil, err
		}
		h.barrier = req.Barrier
		h.max = lockKey{level: levelBarrier}
	}
	if req.Security != ModeNone {
		if err := c.security.Acquire(ctx, weight(req.Security)); err != nil {
			release()
			return ctx, nil, err
		}
		h.security = req.Security
		h.max = lockKey{level: levelSecurity}
	}
	for _, k := range keys {
		if err := c.lockKey(ctx, k.key); err != nil {
			release()
			return ctx, nil, err
		}
		h.keys = append(h.keys, k)
		h.max = k
	}
	return context.WithValue(ctx, heldKey{}, h), h, nil
}

func checkOrder(parent *Held, req Request, keys []lockKey) error {
	if parent == nil {
		return nil
	}
	first := lockKey{level: 1 << 30}
	switch {
	case req.Barrier != ModeNone:
		first = lockKey{level: levelBarrier}
	case req.Security != ModeNone:
		first = lockKey{level: levelSecurity}
	case len(keys) > 0:
		first = keys[0]
	}
	if !parent.max.less(first) {
		return fmt.Errorf("%w: holding %s:%q, requested %s:%q", ErrLockOrder,
			levelName(parent.max.level), parent.max.key, levelName(first.level), first.key)
	}
	return nil
}

func levelName(l int) string {
	switch l {
	case levelBarrier:
		return "barrier"
	case levelSecurity:
		return "security_guard"
	case levelProject:
		return "project"
	case levelAsset:
		return "asset"
	case levelTask:
		return "task"
	case levelBlob:
		return "blob"
	}
	return "none"
}

func weight(m Mode) int64 {
	if m == ModeExclusive {
		return rwWeight
	}
	return 1
}

func (c *Coordinator) lockKey(ctx context.Context, key string) error {
	c.mu.Lock()
	kl := c.keys[key]
	if kl == nil {
		kl = &keyLock{sem: make(chan struct{}, 1)}
		c.keys[key] = kl
	}
	kl.refs++
	c.mu.Unlock()
	select {
	case kl.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		c.dropRef(key, kl)
		return ctx.Err()
	}
}

func (c *Coordinator) dropRef(key string, kl *keyLock) {
	c.mu.Lock()
	kl.refs--
	if kl.refs == 0 {
		delete(c.keys, key)
	}
	c.mu.Unlock()
}

func (c *Coordinator) unlockKey(key string) {
	c.mu.Lock()
	kl := c.keys[key]
	c.mu.Unlock()
	if kl == nil {
		panic("commands: unlock of unheld key " + key)
	}
	<-kl.sem
	c.dropRef(key, kl)
}

// Release 按取锁的逆序释放；重复调用无副作用。
func (h *Held) Release() { h.release() }

func (h *Held) release() {
	h.once.Do(func() {
		for i := len(h.keys) - 1; i >= 0; i-- {
			h.c.unlockKey(h.keys[i].key)
		}
		if h.security != ModeNone {
			h.c.security.Release(weight(h.security))
		}
		if h.barrier != ModeNone {
			h.c.barrier.Release(weight(h.barrier))
		}
	})
}
