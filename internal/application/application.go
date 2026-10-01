// Package application 组装一个进程内的真实领域模块。它不拥有业务表，不在
// 模块间共享 SQL 事务；HTTP 与本机服务生命周期复用同一个实例和授权通路。
package application

import (
	"cmp"
	"context"
	"errors"
	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
	"path/filepath"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type authority struct {
	*identity.Service
	epochs authz.EpochSource
}

func (a authority) RecoveryEpoch(ctx context.Context) (int64, error) {
	return a.epochs.RecoveryEpoch(ctx)
}

// Options 的服务配置来自实例；可注入的时钟和口令参数便于合成集成测试。
type Options struct {
	Instance operations.Options
	Identity identity.Config
	Storage  storage.Config
	// UploadSweepInterval 是 serve 清扫到期上传会话的间隔；默认 1 分钟。
	UploadSweepInterval time.Duration
}

type App struct {
	Instance   *operations.Instance
	Identity   *identity.Service
	Ledger     *ledger.Service
	Rights     *provenance.Service
	Storage    *storage.Service
	Catalog    *catalog.Service
	Events     *events.Store
	Query      *query.Service
	Extensions *extensions.Registry
	// ExtensionManager governs imported packages and serves the jobs host port.
	ExtensionManager *extensions.Manager
	// Scheduler runs due reminders/purges and GC; serve starts it only when
	// config lifecycle.scheduler is true.
	Scheduler     *operations.LifecycleScheduler
	Tasks         *tasks.Service
	Flows         *workflow.Service
	Execution     *ax.Service
	Jobs          *jobs.Service
	Nodes         *node.Service
	Evidence      *ledger.FileReviewSources
	Reviews       *ledger.Reviews
	Lifecycle     *ledger.Lifecycle
	Discussions   *ledger.DiscussionObjects
	Collaboration *query.Collaboration
	// coreMu 串行化 outbox 收录与查询追赶，tailMu 串行化收件箱与审计导出；
	// 完整同步（启动、备份、恢复）同时持有两者。运行期两段各自推进，慢的
	// 收件箱逐事件提交或审计导出不能拖住事件收录与查询投影。
	coreMu     sync.Mutex
	tailMu     sync.Mutex
	healthMu   sync.RWMutex
	coreFailed bool
	tailFailed bool
	sweepMu    sync.Mutex
	sweep      UploadSweepStats
}

// UploadSweepStats 是 serve 后台清扫到期上传会话的累计观测。
type UploadSweepStats struct {
	LastSuccess time.Time
	Failures    int64
	Expired     int64
}

// Open 在任何监听开放前检查身份、收录 outbox、重建或追平查询投影。
// 不迁移权威库；需要迁移的实例仍由显式 migrate 命令处理。
func Open(ctx context.Context, opts Options) (*App, error) { return open(ctx, opts, false) }

// OpenOffline assembles domain owners without opening the gate, listeners or
// background consumers. Local maintenance commands retain the instance lock.
func OpenOffline(ctx context.Context, opts Options) (*App, error) { return open(ctx, opts, true) }

func open(ctx context.Context, opts Options, offline bool) (_ *App, err error) {
	i, err := operations.Open(ctx, opts.Instance)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = errors.Join(err, i.Close(closeCtx))
		}
	}()
	k, err := masterkey.Load(i.Config().SecretsDir)
	if err != nil {
		return nil, err
	}
	a := &App{Instance: i}
	release, err := extensions.CurrentReleaseDigest()
	if err != nil {
		return nil, err
	}
	a.Extensions, err = extensions.New(extensions.Deps{DB: i.DB(ownership.Main), Gate: i.Gate(), ReleaseDigest: release})
	if err != nil {
		return nil, err
	}
	a.Identity, err = identity.New(identity.Deps{Main: i.DB(ownership.Main), Runtime: i.DB(ownership.Runtime), Gate: i.Gate(), Epochs: i, Clock: i.Clock(), IDs: i.IDs(), Key: k, InstanceID: i.InstanceID(), InstanceName: i.Marker().Name}, opts.Identity)
	if err != nil {
		return nil, err
	}
	a.Ledger, err = ledger.New(ledger.Deps{DB: i.DB(ownership.Ledger), Gate: i.Gate(), Authority: authority{a.Identity, i}, Clock: i.Clock(), IDs: i.IDs()})
	if err != nil {
		return nil, err
	}
	a.Rights, err = provenance.New(provenance.Deps{Producers: a.Extensions, DB: i.DB(ownership.Ledger), Gate: i.Gate(), Reader: a.Ledger, Authz: a.Identity, Clock: i.Clock(), IDs: i.IDs(), InstanceID: i.InstanceID()})
	if err != nil {
		return nil, err
	}
	grantKey, err := k.Derive("storage/read-grant/v1")
	if err != nil {
		return nil, err
	}
	opts.Storage = storageConfig(opts.Storage, i.Config())
	a.Storage, err = storage.New(storage.Deps{Runtime: i.DB(ownership.Runtime), Home: i.Layout().Home, Gate: i.Gate(), Clock: i.Clock(), IDs: i.IDs(), Authz: a.Identity, Reads: a.Identity, Ledger: a.Ledger, Rights: a.Rights, ReadGrantKey: grantKey, InstanceID: i.InstanceID()}, opts.Storage)
	if err != nil {
		return nil, err
	}
	a.Ledger.SetInstaller(a.Storage)
	a.Rights.SetFiles(a.Storage)
	a.Catalog, err = catalog.New(catalog.Deps{Producers: a.Extensions, Home: i.Layout().Home, Gate: i.Gate(), Storage: a.Storage, Ledger: a.Ledger, Authz: a.Identity, Rights: a.Rights, Clock: i.Clock(), IDs: i.IDs(), InstanceID: i.InstanceID()})
	if err != nil {
		return nil, err
	}
	a.Ledger.SetRevisionVerifier(a.Catalog)
	a.Ledger.SetAcceptanceVerifier(a.Catalog)
	a.Rights.SetCatalog(a.Catalog)
	a.Events = events.New(i.DB(ownership.Events), i.Clock(), i.Gate())
	a.Query, err = query.New(query.Deps{DB: i.DB(ownership.Index), Gate: i.Gate(), Reader: a.Ledger, Catalog: a.Catalog, Events: a.Events, Authz: a.Identity, Rights: a.Rights, InstanceID: i.InstanceID()})
	if err != nil {
		return nil, err
	}
	// Offline recovery keeps the same task/execution acceptance guards.
	if err = a.assembleCollaboration(); err != nil {
		return nil, err
	}
	if offline {
		return a, nil
	}
	if err = i.Start(ctx,
		operations.Hook{Name: "identity", Run: a.Identity.CheckStartup},
		operations.Hook{Name: "extensions", Run: a.Extensions.RegisterBuiltins},
		operations.Hook{Name: "recovery", Run: func(c context.Context) error { _, e := a.Recover(c); return e }},
		operations.Hook{Name: "outbox-query-audit", Run: func(c context.Context) error { return a.sync(c, true) }},
	); err != nil {
		return nil, err
	}
	if lc := i.Config().Lifecycle; lc.Scheduler {
		i.Go("lifecycle-scheduler", func(ctx context.Context) error {
			t := time.NewTicker(time.Duration(lc.IntervalSeconds) * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-t.C:
					_, _ = a.Scheduler.RunOnce(ctx) // Failures stay in job rows and retry with backoff.
				}
			}
		})
	}
	// 失败反映在 readiness；保留原操作并在下次恢复同一进度。
	i.Go("outbox-query", every(time.Second, func(ctx context.Context) { _ = a.syncCore(ctx, false) }))
	i.Go("inbox-audit", every(time.Second, func(ctx context.Context) { _ = a.syncTail(ctx) }))
	i.Go("upload-expiry", every(cmp.Or(opts.UploadSweepInterval, time.Minute), a.SweepUploads))
	return a, nil
}

// SweepUploads 关闭到期上传会话、删除其暂存并释放 upload pin，撤销未消费的
// 复用授权。它只处理未提交的暂存，不删除内容库原件或已提交内容，所以默认
// 运行，不受 lifecycle.scheduler 开关约束。维护期间写入口拒绝时本轮跳过，
// 不计为失败；暂存删除等其他失败计入失败数且不更新最近成功时间，下一轮重试。
func (a *App) SweepUploads(ctx context.Context) {
	started := a.Instance.Clock().Now() // 成功时间记本轮开始时刻：它只证明此前到期的会话已处理
	rep, err := a.Storage.SweepExpiredUploads(ctx)
	a.sweepMu.Lock()
	defer a.sweepMu.Unlock()
	a.sweep.Expired += int64(rep.Expired)
	switch {
	case err == nil:
		a.sweep.LastSuccess = started
	case errcode.CodeOf(err) != errcode.MaintenanceMode && ctx.Err() == nil:
		a.sweep.Failures++
	}
}

// UploadSweep 返回到期上传清扫的累计观测。
func (a *App) UploadSweep() UploadSweepStats {
	a.sweepMu.Lock()
	defer a.sweepMu.Unlock()
	return a.sweep
}

// every 在上一轮结束后按间隔重复执行，直到上下文取消。
func every(interval time.Duration, run func(context.Context)) func(context.Context) error {
	return func(ctx context.Context) error {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				run(ctx)
			}
		}
	}
}

func (a *App) Close(ctx context.Context) error { return a.Instance.Close(ctx) }

// Sync 每来源最多收录四批，再推进查询、收件箱和审计水位。持续写入不能让一个
// 来源饿住其他库或投影。没有自动确认备份或裁剪。
func (a *App) Sync(ctx context.Context) error { return a.sync(ctx, false) }

// sync 依次执行两段；持有两把锁，运行期的分段推进不会与之交错。
func (a *App) sync(ctx context.Context, startup bool) error {
	a.coreMu.Lock()
	defer a.coreMu.Unlock()
	a.tailMu.Lock()
	defer a.tailMu.Unlock()
	if err := a.syncCoreLocked(ctx, startup); err != nil {
		return err
	}
	return a.syncTailLocked(ctx)
}

func (a *App) syncCore(ctx context.Context, startup bool) error {
	a.coreMu.Lock()
	defer a.coreMu.Unlock()
	return a.syncCoreLocked(ctx, startup)
}

func (a *App) syncTail(ctx context.Context) error {
	a.tailMu.Lock()
	defer a.tailMu.Unlock()
	return a.syncTailLocked(ctx)
}

// syncCoreLocked 收录 outbox 并追平查询投影。
func (a *App) syncCoreLocked(ctx context.Context, startup bool) (err error) {
	defer func() { a.healthMu.Lock(); a.coreFailed = err != nil; a.healthMu.Unlock() }()
	for _, db := range []ownership.Database{ownership.Main, ownership.Ledger, ownership.Runtime} {
		src := commands.OutboxSource{Label: string(db), DB: a.Instance.DB(db), Gate: a.Instance.Gate()}
		// 启动尚无监听或后台业务写者，可安全排空现存 outbox；运行期有界轮转。
		for batch := 0; startup || batch < 4; batch++ {
			n, e := a.Events.Relay(ctx, src, 256)
			if e != nil {
				return e
			}
			if n < 256 {
				break
			}
		}
	}
	if _, err = a.Query.CatchUp(ctx); err != nil {
		if errcode.CodeOf(err) != errcode.CursorExpired {
			return err
		}
		if _, err = a.Query.Rebuild(ctx); err != nil {
			return err
		}
	}
	return nil
}

// syncTailLocked 推进收件箱消费与审计导出。
func (a *App) syncTailLocked(ctx context.Context) (err error) {
	defer func() { a.healthMu.Lock(); a.tailFailed = err != nil; a.healthMu.Unlock() }()
	if a.Collaboration != nil {
		if _, err = a.Collaboration.CatchUpInbox(ctx, 1000); err != nil {
			return err
		}
	}
	_, err = a.Events.ExportAudit(ctx, filepath.Join(a.Instance.Layout().Home, "audit"), 1000)
	return err
}

// Ready 额外包含事件/索引/审计的后台健康，不把暂时失败显示为就绪。
func (a *App) Ready() bool {
	a.healthMu.RLock()
	defer a.healthMu.RUnlock()
	return a.Instance.Readiness().Ready && !a.coreFailed && !a.tailFailed
}

// storageConfig 合并存储配置：调用方显式给出的值优先（测试用），其余取实例
// 配置，两者都未设置的由存储模块取默认值。
func storageConfig(c storage.Config, cfg operations.Config) storage.Config {
	if c.MinFreeBytes == 0 {
		c.MinFreeBytes = cfg.MinFreeBytes
	}
	u := cfg.Uploads
	c.MaxFileBytes = cmp.Or(c.MaxFileBytes, u.MaxFileBytes)
	c.MaxUploadBytes = cmp.Or(c.MaxUploadBytes, u.MaxUploadBytes)
	c.MaxUploadFiles = cmp.Or(c.MaxUploadFiles, u.MaxUploadFiles)
	c.UploadIdleTTL = cmp.Or(c.UploadIdleTTL, u.IdleExpiry)
	c.UploadMaxTTL = cmp.Or(c.UploadMaxTTL, u.AbsoluteExpiry)
	return c
}
