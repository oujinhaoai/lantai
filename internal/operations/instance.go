package operations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

// State 是实例的生命周期状态。只有 ready 开放写入。
type State string

const (
	// StateInitializing：Create 已建库，身份初始化尚未完成，只能 Activate 或 Close。
	StateInitializing State = "initializing"
	// StateBlocked：已打开，但有阻止开放写入的原因（待迁移、空间不足、恢复失败等）。
	StateBlocked State = "blocked"
	// StateStarting：已打开并通过检查，等待 Start 执行启动恢复。
	StateStarting State = "starting"
	// StateRecovering：正在执行启动恢复钩子。
	StateRecovering State = "recovering"
	// StateReady：开放写入。
	StateReady State = "ready"
	// StateMaintenance：维护屏障独占中（迁移、备份等），写入关闭。
	StateMaintenance State = "maintenance"
	// StateStopping：正在优雅退出。
	StateStopping State = "stopping"
	// StateClosed：已关闭，资源与数据根锁已释放。
	StateClosed State = "closed"
)

// 就绪与启动原因代码（小写下划线，供机器判断）。
const (
	CodeNotInitialized      = "not_initialized"
	CodeInitIncomplete      = "initialization_incomplete"
	CodeAlreadyInitialized  = "already_initialized"
	CodeUnmarkedData        = "unmarked_data"
	CodeInstanceLocked      = "instance_locked"
	CodeDatabaseMissing     = "database_missing"
	CodeInstanceMismatch    = "instance_mismatch"
	CodeBindingMissing      = "binding_missing"
	CodeSchemaIncompatible  = "schema_incompatible"
	CodeFormatNewer         = "format_newer"
	CodeMigrationRequired   = "migration_required"
	CodeMigrationIncomplete = "migration_incomplete"
	CodeNetworkFileSystem   = "network_filesystem"
	CodeFileSystemUnknown   = "filesystem_unverified"
	CodeDiskSpaceLow        = "disk_space_low"
	CodeRecoveryFailed      = "recovery_failed"
	CodeRestoreIncomplete   = "restore_incomplete"
	CodeIndexRecreated      = "index_recreated"
	CodeInvalidName         = "invalid_instance_name"
)

// Reason 是一条带代码的原因。
type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StartupError 汇总阻止打开、初始化或开放实例的原因。
type StartupError struct {
	Reasons []Reason
}

func (e *StartupError) Error() string {
	parts := make([]string, len(e.Reasons))
	for i, r := range e.Reasons {
		parts[i] = r.Code + ": " + r.Message
	}
	return "operations: " + strings.Join(parts, "; ")
}

// Has 报告是否包含某个原因代码。
func (e *StartupError) Has(code string) bool {
	return slices.ContainsFunc(e.Reasons, func(r Reason) bool { return r.Code == code })
}

// HasReason 报告 err 是否为包含 code 的 StartupError。
func HasReason(err error, code string) bool {
	var se *StartupError
	return errors.As(err, &se) && se.Has(code)
}

func startupErr(code, format string, args ...any) *StartupError {
	return &StartupError{Reasons: []Reason{{Code: code, Message: fmt.Sprintf(format, args...)}}}
}

// Options 是打开实例的参数。
type Options struct {
	// Home 是数据根目录。
	Home string
	// Clock 默认为系统时钟。
	Clock clock.Clock
	// IDs 默认为系统时钟与 crypto/rand。
	IDs *ids.Generator

	// 以下仅供包内测试替换。
	migrations migrationSource
	inspectFS  func(string) (fsutil.FSInfo, error)
	beforeDB   func(ownership.Database) error
}

// Hook 是 Start 时在开放写入前按顺序执行的启动恢复与核对步骤；任何一步
// 失败都让实例保持 blocked，不开放写入。
type Hook struct {
	Name string
	Run  func(ctx context.Context) error
}

// Instance 是一个已持有数据根写锁的实例。
type Instance struct {
	layout Layout
	cfg    Config
	clock  clock.Clock
	ids    *ids.Generator
	src    migrationSource
	fsInfo func(string) (fsutil.FSInfo, error)
	before func(ownership.Database) error

	lock  *fsutil.Lock
	dbs   map[ownership.Database]*sql.DB
	coord *commands.Coordinator
	gate  *commands.Gate

	mu       sync.Mutex
	marker   Marker
	state    State
	reasons  []Reason
	notes    []Reason
	statuses []DBStatus

	bgCtx      context.Context
	bgCancel   context.CancelFunc
	bgWG       sync.WaitGroup
	bgTasks    []bgTask
	bgErrs     []error
	started    bool
	backupOnce sync.Once
	backupLock chan struct{}
}

type bgTask struct {
	name string
	fn   func(context.Context) error
}

func newInstance(opts Options) (*Instance, error) {
	l, err := NewLayout(opts.Home)
	if err != nil {
		return nil, err
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.System{}
	}
	gen := opts.IDs
	if gen == nil {
		gen = &ids.Generator{Clock: clk, Rand: rand.Reader}
	}
	src := opts.migrations
	if src == nil {
		src = migrations.For
	}
	inspect := opts.inspectFS
	if inspect == nil {
		inspect = fsutil.Inspect
	}
	coord := commands.NewCoordinator()
	i := &Instance{
		layout: l, clock: clk, ids: gen, src: src, fsInfo: inspect, before: opts.beforeDB,
		dbs: map[ownership.Database]*sql.DB{}, coord: coord, gate: commands.NewGate(coord), state: StateBlocked,
	}
	i.bgCtx, i.bgCancel = context.WithCancel(context.Background())
	return i, nil
}

func (i *Instance) acquireLock() error {
	l, err := fsutil.TryLock(i.layout.LockPath())
	if errors.Is(err, fsutil.ErrLocked) {
		return startupErr(CodeInstanceLocked, "another instance holds the write lock on %s", i.layout.Home)
	}
	if err != nil {
		return err
	}
	i.lock = l
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func existingDBs(l Layout) []string {
	var out []string
	for _, db := range migrations.Databases {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if exists(l.DBPath(db) + suffix) {
				out = append(out, string(db)+".db"+suffix)
			}
		}
	}
	return out
}

// checkFileSystem 核对数据根与库目录（可能是另一个挂载点或符号链接）都在
// 本机文件系统上；不存在的路径跳过。
func (i *Instance) checkFileSystem(paths ...string) error {
	for _, p := range paths {
		if !exists(p) {
			continue
		}
		info, err := i.fsInfo(p)
		if err != nil {
			return fmt.Errorf("operations: inspect file system of %s: %w", p, err)
		}
		if info.Remote {
			return startupErr(CodeNetworkFileSystem, "%s is on a network file system (%s); the data root and databases must be on a local disk", p, info.Type)
		}
		if !info.Known && !slices.ContainsFunc(i.notes, func(r Reason) bool { return r.Code == CodeFileSystemUnknown }) {
			i.notes = append(i.notes, Reason{Code: CodeFileSystemUnknown,
				Message: fmt.Sprintf("file system type %q of %s could not be confirmed as local", info.Type, p)})
		}
	}
	return nil
}

// Open 打开已初始化的实例：取得数据根写锁，检查实例标记、文件系统、权威库、
// 实例绑定、迁移版本与兼容矩阵、磁盘余量。不自动迁移；有待迁移或空间不足
// 时返回处于 blocked 的实例，由调用方执行 Migrate 或处理后再 Start。实例
// 标记存在而权威库缺失、库属于其他实例、库版本比本构建新等情况返回
// *StartupError（恢复诊断），不创建空库，也不重新开放初始化。
func Open(ctx context.Context, opts Options) (_ *Instance, err error) {
	i, err := newInstance(opts)
	if err != nil {
		return nil, err
	}
	if err := i.acquireLock(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			i.releaseResources()
		}
	}()
	if i.cfg, err = LoadConfig(i.layout); err != nil {
		return nil, err
	}
	marker, err := ReadMarker(i.layout)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if present := existingDBs(i.layout); len(present) > 0 {
			return nil, startupErr(CodeUnmarkedData, "database files exist but the instance marker is missing (%s); restore the marker from backup", strings.Join(present, ", "))
		}
		return nil, startupErr(CodeNotInitialized, "%s has not been initialized; run lantai init", i.layout.Home)
	case err != nil:
		return nil, err
	}
	if marker.State == MarkerInitializing {
		return nil, startupErr(CodeInitIncomplete, "initialization of %s has not completed; run lantai init to resume", i.layout.Home)
	}
	i.marker = marker
	if err := i.checkFileSystem(i.layout.Home, i.layout.DBDir()); err != nil {
		return nil, err
	}
	var missing []string
	for _, db := range authoritative {
		if !exists(i.layout.DBPath(db)) {
			missing = append(missing, string(db)+".db")
		}
	}
	if len(missing) > 0 {
		return nil, startupErr(CodeDatabaseMissing, "the instance marker exists but %s is missing; restore from a complete backup (initialization stays closed)", strings.Join(missing, ", "))
	}
	if err := i.openDatabases(ctx); err != nil {
		return nil, err
	}
	if err := i.evaluate(ctx); err != nil {
		return nil, err
	}
	return i, nil
}

// CreateOptions 是初始化实例的参数。
type CreateOptions struct {
	Options
	// Name 为空时取配置中的实例名。
	Name string
}

var instanceNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Create 在空数据根上建立实例，或继续一次未完成的建立：取锁、写 initializing
// 标记、建立五库并应用全部迁移、把各库绑定到实例。已初始化（标记为 active）
// 或存在无标记的库文件时拒绝。返回的实例处于 initializing，写入保持关闭；
// 调用方完成身份初始化后调用 Activate。
func Create(ctx context.Context, opts CreateOptions) (_ *Instance, err error) {
	if opts.Home == "" {
		return nil, fmt.Errorf("operations: data root directory is not set")
	}
	if err := os.MkdirAll(opts.Home, 0o755); err != nil {
		return nil, fmt.Errorf("operations: create data root: %w", err)
	}
	i, err := newInstance(opts.Options)
	if err != nil {
		return nil, err
	}
	if err := i.acquireLock(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			i.releaseResources()
		}
	}()
	if i.cfg, err = LoadConfig(i.layout); err != nil {
		return nil, err
	}
	marker, err := ReadMarker(i.layout)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if present := existingDBs(i.layout); len(present) > 0 {
			return nil, startupErr(CodeUnmarkedData, "refusing to initialize: database files already exist (%s)", strings.Join(present, ", "))
		}
		name := opts.Name
		if name == "" {
			name = i.cfg.InstanceName
		}
		if !instanceNameRE.MatchString(name) {
			return nil, startupErr(CodeInvalidName, "instance name %q must match %s", name, instanceNameRE)
		}
		now := i.clock.Now()
		id, err := i.ids.New()
		if err != nil {
			return nil, err
		}
		marker = Marker{InstanceID: id, Name: name, State: MarkerInitializing, DataFormatVersion: DataFormatVersion,
			RecoveryEpoch: 1, CreatedAt: now, UpdatedAt: now}
		if err := i.checkFileSystem(i.layout.Home, i.layout.DBDir()); err != nil {
			return nil, err
		}
		if err := writeMarker(i.layout, marker); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case marker.State == MarkerActive:
		return nil, startupErr(CodeAlreadyInitialized, "%s is already initialized (instance %s); initialization is closed", i.layout.Home, marker.InstanceID)
	default:
		// initializing：继续上一次未完成的初始化。
		if err := i.checkFileSystem(i.layout.Home, i.layout.DBDir()); err != nil {
			return nil, err
		}
	}
	i.marker = marker
	if err := os.MkdirAll(i.layout.DBDir(), 0o700); err != nil {
		return nil, fmt.Errorf("operations: create database directory: %w", err)
	}
	if err := i.checkFileSystem(i.layout.DBDir()); err != nil {
		return nil, err
	}
	if err := i.openDatabases(ctx); err != nil {
		return nil, err
	}
	for _, db := range migrations.Databases {
		if _, err := migrateDB(ctx, i.dbs[db], db, i.src, i.clock); err != nil {
			return nil, err
		}
		if err := bindDB(ctx, i.dbs[db], db, marker.InstanceID, i.clock); err != nil {
			return nil, startupErr(CodeInstanceMismatch, "%v", err)
		}
	}
	if err := i.evaluate(ctx); err != nil {
		return nil, err
	}
	i.mu.Lock()
	i.state = StateInitializing
	i.mu.Unlock()
	i.gate.Close(commands.ReasonInitializing)
	return i, nil
}

// Activate 把 initializing 实例标记为已初始化；之后初始化永久关闭。
func (i *Instance) Activate(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.state != StateInitializing {
		return fmt.Errorf("operations: activate from state %s", i.state)
	}
	m := i.marker
	m.State = MarkerActive
	m.UpdatedAt = i.clock.Now()
	if err := writeMarker(i.layout, m); err != nil {
		return err
	}
	i.marker = m
	i.state = StateBlocked
	if len(i.reasons) == 0 {
		i.state = StateStarting
	}
	return nil
}

// openDatabases 打开五库；index 缺失时新建并应用其迁移（索引可重建）。
// 调用前已确认权威库存在（Open）或允许创建（Create）。
func (i *Instance) openDatabases(ctx context.Context) error {
	for _, db := range migrations.Databases {
		path := i.layout.DBPath(db)
		fresh := !exists(path)
		conn, err := sqlite.Open(ctx, path, sqlite.Options{})
		if err != nil {
			return fmt.Errorf("operations: open %s.db: %w", db, err)
		}
		i.dbs[db] = conn
		if fresh && db == ownership.Index && i.marker.State == MarkerActive {
			if _, err := migrateDB(ctx, conn, db, i.src, i.clock); err != nil {
				return err
			}
			if err := bindDB(ctx, conn, db, i.marker.InstanceID, i.clock); err != nil {
				return err
			}
			i.notes = append(i.notes, Reason{Code: CodeIndexRecreated,
				Message: "index.db was missing and has been recreated empty; rebuild the index before relying on queries"})
		}
	}
	return nil
}

// assess 核对各库与兼容矩阵：fatal 是无法安全继续的问题（恢复诊断），
// blocking 是可由迁移或运维处理、但在处理前不能开放写入的问题。
func (i *Instance) assess(ctx context.Context) (fatal, blocking []Reason, statuses []DBStatus, err error) {
	for _, db := range migrations.Databases {
		st, err := inspectDB(ctx, i.dbs[db], db, i.src)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("operations: inspect %s.db: %w", db, err)
		}
		statuses = append(statuses, st)
		switch {
		case len(st.Problems) > 0:
			fatal = append(fatal, Reason{CodeSchemaIncompatible, fmt.Sprintf("%s.db: %s", db, strings.Join(st.Problems, "; "))})
		case st.InstanceID == "":
			fatal = append(fatal, Reason{CodeBindingMissing, fmt.Sprintf("%s.db is not bound to any instance", db)})
		case st.InstanceID != i.marker.InstanceID:
			fatal = append(fatal, Reason{CodeInstanceMismatch, fmt.Sprintf("%s.db belongs to instance %s, not %s", db, st.InstanceID, i.marker.InstanceID)})
		case st.Pending():
			blocking = append(blocking, Reason{CodeMigrationRequired, fmt.Sprintf("%s.db is at version %d, this build requires %d", db, st.Applied, st.Latest)})
		}
	}
	switch {
	case i.marker.DataFormatVersion > DataFormatVersion:
		fatal = append(fatal, Reason{CodeFormatNewer, fmt.Sprintf("data format %d is newer than this build (%d); downgrade is not supported", i.marker.DataFormatVersion, DataFormatVersion)})
	case i.marker.DataFormatVersion < DataFormatVersion:
		blocking = append(blocking, Reason{CodeMigrationRequired, fmt.Sprintf("data format %d must be migrated to %d", i.marker.DataFormatVersion, DataFormatVersion)})
	}
	if run := i.marker.Migration; run != nil {
		if run.TargetFormatVersion > DataFormatVersion {
			fatal = append(fatal, Reason{CodeFormatNewer, fmt.Sprintf("an incomplete migration to data format %d was started by a newer build; finish it with that build",
				run.TargetFormatVersion)})
		} else {
			blocking = append(blocking, Reason{CodeMigrationIncomplete, fmt.Sprintf("migration run %s started at %s has not completed; run lantai migrate",
				run.RunID, clock.Format(run.StartedAt))})
		}
	}
	if i.marker.Restore != nil {
		blocking = append(blocking, Reason{CodeRestoreIncomplete, "backup restoration requires offline credential rotation and reconciliation before service can start"})
	}
	if r, ok := i.diskReason(); ok {
		blocking = append(blocking, r)
	}
	return fatal, blocking, statuses, nil
}

// evaluate 用于打开与初始化：fatal 问题返回 *StartupError，其余记为阻止原因。
func (i *Instance) evaluate(ctx context.Context) error {
	fatal, blocking, statuses, err := i.assess(ctx)
	if err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.statuses = statuses
	if len(fatal) > 0 {
		return &StartupError{Reasons: fatal}
	}
	i.setReasonsLocked(blocking)
	return nil
}

// reevaluate 用于已打开的实例（迁移之后等）：所有问题都记为阻止原因，
// 实例保持打开以便诊断；fatal 问题同时作为错误返回。
func (i *Instance) reevaluate(ctx context.Context) error {
	fatal, blocking, statuses, err := i.assess(ctx)
	if err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.statuses = statuses
	i.setReasonsLocked(append(fatal, blocking...))
	if len(fatal) > 0 {
		return &StartupError{Reasons: fatal}
	}
	return nil
}

func (i *Instance) setReasonsLocked(reasons []Reason) {
	i.reasons = reasons
	if len(reasons) > 0 {
		i.state = StateBlocked
	} else {
		i.state = StateStarting
	}
}

// diskReason 核对数据根与库目录所在文件系统的可用空间（取较小者）。
func (i *Instance) diskReason() (Reason, bool) {
	free, err := freeBytes(i.layout)
	if err != nil {
		return Reason{CodeDiskSpaceLow, fmt.Sprintf("free space could not be determined: %v", err)}, true
	}
	if free < i.cfg.MinFreeBytes {
		return Reason{CodeDiskSpaceLow, fmt.Sprintf("%d bytes free, at least %d required", free, i.cfg.MinFreeBytes)}, true
	}
	return Reason{}, false
}

func freeBytes(l Layout) (uint64, error) {
	free, err := fsutil.FreeBytes(l.Home)
	if err != nil {
		return 0, err
	}
	if exists(l.DBDir()) {
		db, err := fsutil.FreeBytes(l.DBDir())
		if err != nil {
			return 0, err
		}
		free = min(free, db)
	}
	return free, nil
}

// MigrationReport 记录一次迁移实际应用的版本。
type MigrationReport struct {
	RunID   ids.ID                       `json:"run_id,omitempty"`
	Noop    bool                         `json:"noop"`
	Applied map[ownership.Database][]int `json:"applied"`
	Before  map[ownership.Database]int   `json:"before"`
	After   map[ownership.Database]int   `json:"after"`
	Matrix  Matrix                       `json:"matrix"`
}

// Migrate 在维护屏障下依库顺序应用待执行的迁移。开始前把本次迁移写入实例
// 标记；中途失败时标记保留，实例保持维护状态，重跑从各库未完成的版本继续，
// 不重复已提交的迁移。全部库与格式满足兼容矩阵后才清除标记中的迁移记录。
// 迁移不自动生成备份：升级真实数据前先按运维流程完成 complete 备份。
func (i *Instance) migrate(ctx context.Context) (MigrationReport, error) {
	rep := MigrationReport{Applied: map[ownership.Database][]int{}, Before: map[ownership.Database]int{},
		After: map[ownership.Database]int{}, Matrix: i.src.matrix()}
	i.mu.Lock()
	switch i.state {
	case StateBlocked, StateStarting:
	default:
		st := i.state
		i.mu.Unlock()
		return rep, fmt.Errorf("operations: migrate requires an opened, not started instance (state %s)", st)
	}
	i.state = StateMaintenance
	i.mu.Unlock()
	_, held, err := i.gate.Maintain(ctx, commands.ReasonMigrating)
	if err != nil {
		i.reevaluate(ctx)
		return rep, err
	}
	defer held.Release()

	pending := i.marker.Migration != nil || i.marker.DataFormatVersion < DataFormatVersion
	for _, st := range i.statusesCopy() {
		rep.Before[st.Database] = st.Applied
		pending = pending || st.Pending()
	}
	if !pending {
		rep.Noop = true
		i.reevaluate(ctx)
		return rep, nil
	}
	m := i.marker
	switch {
	case m.Migration == nil:
		run, err := i.ids.New()
		if err != nil {
			i.reevaluate(ctx)
			return rep, err
		}
		m.Migration = &MigrationRun{RunID: run, StartedAt: i.clock.Now(), FromFormatVersion: m.DataFormatVersion, TargetFormatVersion: DataFormatVersion}
	case m.Migration.TargetFormatVersion > DataFormatVersion:
		// assess 已把它列为 fatal，这里只作防御：不替较新的构建完成它的迁移。
		i.reevaluate(ctx)
		return rep, fmt.Errorf("operations: migration run %s targets data format %d, newer than this build", m.Migration.RunID, m.Migration.TargetFormatVersion)
	default:
		// 由本构建继续一次较早构建开始的迁移：目标改为本构建的格式。
		run := *m.Migration
		run.TargetFormatVersion = DataFormatVersion
		m.Migration = &run
	}
	m.UpdatedAt = i.clock.Now()
	if err := writeMarker(i.layout, m); err != nil {
		i.reevaluate(ctx)
		return rep, err
	}
	i.setMarker(m)
	rep.RunID = m.Migration.RunID
	for _, db := range migrations.Databases {
		if i.before != nil {
			if err := i.before(db); err != nil {
				i.reevaluate(ctx)
				return rep, err
			}
		}
		applied, err := migrateDB(ctx, i.dbs[db], db, i.src, i.clock)
		if len(applied) > 0 {
			rep.Applied[db] = applied
		}
		if err != nil {
			i.reevaluate(ctx)
			return rep, err
		}
	}
	for _, db := range migrations.Databases {
		st, err := inspectDB(ctx, i.dbs[db], db, i.src)
		if err != nil {
			i.reevaluate(ctx)
			return rep, err
		}
		rep.After[db] = st.Applied
		if len(st.Problems) > 0 || st.Pending() {
			i.reevaluate(ctx)
			return rep, fmt.Errorf("operations: %s.db does not satisfy the compatibility matrix after migration", db)
		}
	}
	m.DataFormatVersion = DataFormatVersion
	m.Migration = nil
	m.UpdatedAt = i.clock.Now()
	if err := writeMarker(i.layout, m); err != nil {
		i.reevaluate(ctx)
		return rep, err
	}
	i.setMarker(m)
	if err := i.reevaluate(ctx); err != nil {
		return rep, err
	}
	return rep, nil
}

func (i *Instance) setMarker(m Marker) {
	i.mu.Lock()
	i.marker = m
	i.mu.Unlock()
}

func (i *Instance) statusesCopy() []DBStatus {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.statuses)
}

// Start 执行启动恢复钩子，全部成功且没有阻止原因时开放写入并启动后台任务。
// 钩子运行期间持有维护屏障独占锁（钩子直接操作各库，不经写入口），因此并发
// 的 Close 会等钩子结束；钩子之后若实例已进入停止，不再开放写入。阻止原因
// 存在时返回 *StartupError，实例保持 blocked；钩子失败同样保持 blocked 并
// 返回错误，不边恢复边接受写入。
func (i *Instance) Start(ctx context.Context, hooks ...Hook) error {
	i.mu.Lock()
	if i.state != StateStarting {
		reasons := slices.Clone(i.reasons)
		st := i.state
		i.mu.Unlock()
		if len(reasons) > 0 {
			return &StartupError{Reasons: reasons}
		}
		return fmt.Errorf("operations: start from state %s", st)
	}
	i.state = StateRecovering
	i.mu.Unlock()
	fail := func(r Reason, err error) error {
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.state == StateRecovering {
			i.reasons = append(i.reasons, r)
			i.state = StateBlocked
			i.gate.Close(commands.ReasonDiagnostic)
		}
		return err
	}
	lctx, held, err := i.gate.Maintain(ctx, commands.ReasonRecovering)
	if err != nil {
		return fail(Reason{CodeRecoveryFailed, err.Error()}, err)
	}
	defer held.Release()
	for _, h := range hooks {
		if err := h.Run(lctx); err != nil {
			return fail(Reason{CodeRecoveryFailed, fmt.Sprintf("%s: %v", h.Name, err)},
				fmt.Errorf("operations: startup step %s: %w", h.Name, err))
		}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.state != StateRecovering {
		return fmt.Errorf("operations: the instance entered %s during startup; writes stay closed", i.state)
	}
	if r, ok := i.diskReason(); ok {
		i.reasons = append(i.reasons, r)
		i.state = StateBlocked
		i.gate.Close(commands.ReasonDiagnostic)
		return &StartupError{Reasons: []Reason{r}}
	}
	i.state = StateReady
	i.started = true
	i.gate.Open()
	for _, t := range i.bgTasks {
		i.launch(t)
	}
	return nil
}

// Go 登记一个后台任务：实例就绪后启动（已就绪则立即启动），Close 时取消并
// 等待其结束。后台任务的每次写入都必须经 Gate().Acquire，维护与停止期间
// 与前台请求一样被拒绝。
func (i *Instance) Go(name string, fn func(ctx context.Context) error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	t := bgTask{name: name, fn: fn}
	i.bgTasks = append(i.bgTasks, t)
	if i.started && i.state != StateStopping && i.state != StateClosed {
		i.launch(t)
	}
}

func (i *Instance) launch(t bgTask) {
	i.bgWG.Add(1)
	go func() {
		defer i.bgWG.Done()
		if err := t.fn(i.bgCtx); err != nil && !errors.Is(err, context.Canceled) {
			i.mu.Lock()
			i.bgErrs = append(i.bgErrs, fmt.Errorf("background task %s: %w", t.name, err))
			i.mu.Unlock()
		}
	}()
}

// Maintenance 是一次持有维护屏障的维护窗口。
type Maintenance struct {
	i    *Instance
	ctx  context.Context
	held *commands.Held
	once sync.Once
}

// Context 返回持有屏障独占锁的上下文。
func (m *Maintenance) Context() context.Context { return m.ctx }

// End 释放维护屏障；实例没有其他阻止原因时恢复开放写入。重复调用无副作用。
func (m *Maintenance) End() {
	m.once.Do(func() {
		i := m.i
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.state == StateMaintenance {
			if len(i.reasons) == 0 {
				i.state = StateReady
				i.gate.Open()
			} else {
				i.state = StateBlocked
			}
		}
		m.held.Release()
	})
}

// Maintain 进入维护：先关闭写入，再等待全部在途写入（前台与后台）结束后
// 取得屏障独占锁。只允许从 ready 进入；ctx 取消时放弃并恢复开放写入。
func (i *Instance) Maintain(ctx context.Context, reason string) (*Maintenance, error) {
	i.mu.Lock()
	if i.state != StateReady {
		st := i.state
		i.mu.Unlock()
		return nil, fmt.Errorf("operations: maintenance requires a ready instance (state %s)", st)
	}
	i.state = StateMaintenance
	i.mu.Unlock()
	lctx, held, err := i.gate.Maintain(ctx, reason)
	if err != nil {
		i.mu.Lock()
		if i.state == StateMaintenance {
			i.state = StateReady
			i.gate.Open()
		}
		i.mu.Unlock()
		return nil, err
	}
	return &Maintenance{i: i, ctx: lctx, held: held}, nil
}

// Close 优雅退出：关闭写入、取消并等待后台任务、取得屏障独占锁等待在途写入
// （含启动钩子）结束，然后关闭五库并释放数据根写锁。ctx 到期而在途写入仍未
// 结束时，不关闭库、不释放数据根锁（否则另一个实例可能在旧写入提交前取得
// 写权），返回错误并保持 stopping；调用方可稍后再次 Close，或退出进程由操作
// 系统释放锁。已关闭时返回 nil。
func (i *Instance) Close(ctx context.Context) error {
	i.mu.Lock()
	if i.state == StateClosed {
		i.mu.Unlock()
		return nil
	}
	i.state = StateStopping
	i.mu.Unlock()
	i.gate.Close(commands.ReasonStopping)
	i.bgCancel()
	releaseBackup, err := i.lockBackup(ctx)
	if err != nil {
		return fmt.Errorf("operations: backup copy did not drain; data root remains locked: %w", err)
	}
	defer releaseBackup()
	var errs []error
	done := make(chan struct{})
	go func() {
		i.bgWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("operations: background tasks did not stop: %w", ctx.Err()))
	}
	_, held, err := i.gate.Maintain(ctx, commands.ReasonStopping)
	if err != nil {
		errs = append(errs, fmt.Errorf("operations: in-flight writes did not drain; databases stay open and the data root stays locked until they finish or the process exits: %w", err))
		return errors.Join(errs...)
	}
	defer held.Release()
	i.mu.Lock()
	errs = append(errs, i.bgErrs...)
	i.mu.Unlock()
	if err := i.releaseResources(); err != nil {
		errs = append(errs, err)
	}
	i.mu.Lock()
	i.state = StateClosed
	i.mu.Unlock()
	return errors.Join(errs...)
}

func (i *Instance) releaseResources() error {
	i.mu.Lock()
	dbs := i.dbs
	i.dbs = map[ownership.Database]*sql.DB{}
	i.mu.Unlock()
	var errs []error
	for _, db := range migrations.Databases {
		if conn := dbs[db]; conn != nil {
			if err := conn.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close %s.db: %w", db, err))
			}
		}
	}
	if err := i.lock.Unlock(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Readiness 是健康与就绪报告：Live 只表示进程与实例对象存活，Ready 表示可以
// 安全接收写入；不就绪时 Reasons 给出机器可读原因。
type Readiness struct {
	Live              bool       `json:"live"`
	Ready             bool       `json:"ready"`
	State             State      `json:"state"`
	Reasons           []Reason   `json:"reasons,omitempty"`
	Notes             []Reason   `json:"notes,omitempty"`
	InstanceID        ids.ID     `json:"instance_id"`
	DataFormatVersion int        `json:"data_format_version"`
	RecoveryEpoch     int64      `json:"recovery_epoch"`
	Databases         []DBStatus `json:"databases"`
	Matrix            Matrix     `json:"matrix"`
}

// Readiness 返回当前就绪状态。
func (i *Instance) Readiness() Readiness {
	i.mu.Lock()
	defer i.mu.Unlock()
	return Readiness{
		Live: i.state != StateClosed, Ready: i.state == StateReady, State: i.state,
		Reasons: slices.Clone(i.reasons), Notes: slices.Clone(i.notes),
		InstanceID: i.marker.InstanceID, DataFormatVersion: i.marker.DataFormatVersion,
		RecoveryEpoch: i.marker.RecoveryEpoch, Databases: slices.Clone(i.statuses), Matrix: i.src.matrix(),
	}
}

// State 返回当前生命周期状态。
func (i *Instance) State() State {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.state
}

// RecoveryEpoch 实现 authz.EpochSource：返回实例当前的恢复代次。
func (i *Instance) RecoveryEpoch(context.Context) (int64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.marker.RecoveryEpoch, nil
}

// InstanceID 返回实例 ID。
func (i *Instance) InstanceID() ids.ID {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.marker.InstanceID
}

// Marker 返回实例标记的副本。
func (i *Instance) Marker() Marker {
	i.mu.Lock()
	defer i.mu.Unlock()
	m := i.marker
	if m.Migration != nil {
		r := *m.Migration
		m.Migration = &r
	}
	if m.Restore != nil {
		r := *m.Restore
		m.Restore = &r
	}
	return m
}

// DB 返回某个库的连接；实例关闭后为 nil。
func (i *Instance) DB(name ownership.Database) *sql.DB {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.dbs[name]
}

// Gate 返回实例的写入口；所有写入（含后台任务）都必须经它取锁。
func (i *Instance) Gate() *commands.Gate { return i.gate }

// Layout 返回数据根布局。
func (i *Instance) Layout() Layout { return i.layout }

// Config 返回配置。
func (i *Instance) Config() Config { return i.cfg }

// Clock 返回实例使用的时钟。
func (i *Instance) Clock() clock.Clock { return i.clock }

// IDs 返回实例使用的 ID 生成器。
func (i *Instance) IDs() *ids.Generator { return i.ids }
