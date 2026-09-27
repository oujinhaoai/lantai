package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

// testConfig 让测试不依赖临时盘至少有 1 GiB 可用；磁盘余量检查另有专门测试。
const testConfig = "contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"

func create(t *testing.T, home string, opts Options) *Instance {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	opts.Home = home
	inst, err := Create(t.Context(), CreateOptions{Options: opts, Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// createActive 初始化一个实例并关闭，返回数据根。
func createActive(t *testing.T, opts Options) string {
	t.Helper()
	home := t.TempDir()
	inst := create(t, home, opts)
	if err := inst.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	return home
}

func open(t *testing.T, home string, opts Options) *Instance {
	t.Helper()
	opts.Home = home
	inst, err := Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inst.Close(context.Background()) })
	return inst
}

func wantReason(t *testing.T, err error, code string) {
	t.Helper()
	if !HasReason(err, code) {
		t.Fatalf("want reason %s, got %v", code, err)
	}
}

func TestCreateOpenStartClose(t *testing.T) {
	home := t.TempDir()
	inst := create(t, home, Options{})
	if inst.State() != StateInitializing {
		t.Fatalf("state after create = %s", inst.State())
	}
	if _, _, err := inst.Gate().Acquire(t.Context(), commands.Request{}); errcode.CodeOf(err) != errcode.MaintenanceMode {
		t.Fatalf("writes must stay closed while initializing: %v", err)
	}
	m, err := ReadMarker(inst.Layout())
	if err != nil || m.State != MarkerInitializing || m.RecoveryEpoch != 1 {
		t.Fatalf("marker = %+v, %v", m, err)
	}
	if err := inst.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	inst = open(t, home, Options{})
	if inst.State() != StateStarting {
		t.Fatalf("state after open = %s (%+v)", inst.State(), inst.Readiness().Reasons)
	}
	if r := inst.Readiness(); r.Ready || !r.Live {
		t.Fatalf("not started yet: %+v", r)
	}
	if err := inst.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := inst.Readiness()
	if !r.Ready || r.InstanceID != m.InstanceID || r.DataFormatVersion != DataFormatVersion || len(r.Databases) != 5 {
		t.Fatalf("readiness = %+v", r)
	}
	for _, st := range r.Databases {
		if st.Pending() || st.InstanceID != m.InstanceID || len(st.Problems) > 0 {
			t.Fatalf("db status = %+v", st)
		}
	}
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
	epoch, _ := inst.RecoveryEpoch(t.Context())
	if epoch != 1 {
		t.Fatalf("recovery epoch = %d", epoch)
	}
	_, h, err := inst.Gate().Acquire(t.Context(), commands.Request{Security: commands.ModeShared})
	if err != nil {
		t.Fatal(err)
	}
	h.Release()
}

// 空实例只能初始化一次；标记存在而库缺失进入恢复诊断，初始化保持关闭。
func TestInitializeOnlyOnceAndMissingDatabase(t *testing.T) {
	home := createActive(t, Options{})
	if _, err := Create(t.Context(), CreateOptions{Options: Options{Home: home}}); !HasReason(err, CodeAlreadyInitialized) {
		t.Fatalf("second init = %v", err)
	}
	l, _ := NewLayout(home)
	if err := os.Rename(l.DBPath(ownership.Main), l.DBPath(ownership.Main)+".moved"); err != nil {
		t.Fatal(err)
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		os.Rename(l.DBPath(ownership.Main)+sfx, l.DBPath(ownership.Main)+".moved"+sfx)
	}
	_, err := Open(t.Context(), Options{Home: home})
	wantReason(t, err, CodeDatabaseMissing)
	if exists(l.DBPath(ownership.Main)) {
		t.Fatal("Open must not create an empty authoritative database")
	}
	if _, err := Create(t.Context(), CreateOptions{Options: Options{Home: home}}); !HasReason(err, CodeAlreadyInitialized) {
		t.Fatalf("init with missing database = %v", err)
	}
	rep, err := Inspect(t.Context(), home)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Compatible() || !hasProblem(rep, CodeDatabaseMissing) {
		t.Fatalf("doctor report = %+v", rep.Problems)
	}
	// 数据根锁已随失败的 Open 释放。
	lk, err := fsutil.TryLock(l.LockPath())
	if err != nil {
		t.Fatalf("lock leaked after failed open: %v", err)
	}
	lk.Unlock()
}

func hasProblem(r Report, code string) bool {
	for _, p := range r.Problems {
		if p.Code == code {
			return true
		}
	}
	return false
}

func TestRefusesUnmarkedDatabasesAndUninitialized(t *testing.T) {
	home := t.TempDir()
	if _, err := Open(t.Context(), Options{Home: home}); !HasReason(err, CodeNotInitialized) {
		t.Fatalf("open empty = %v", err)
	}
	l, _ := NewLayout(home)
	os.MkdirAll(l.DBDir(), 0o700)
	os.WriteFile(l.DBPath(ownership.Ledger), []byte("not empty"), 0o600)
	if _, err := Create(t.Context(), CreateOptions{Options: Options{Home: home}}); !HasReason(err, CodeUnmarkedData) {
		t.Fatalf("init over existing database files = %v", err)
	}
	if _, err := Open(t.Context(), Options{Home: home}); !HasReason(err, CodeUnmarkedData) {
		t.Fatalf("open unmarked data = %v", err)
	}
	if _, err := os.Stat(l.MarkerPath()); !os.IsNotExist(err) {
		t.Fatal("refused init must not write a marker")
	}
}

// 初始化中断后只能由 init 继续，不能被当作已初始化实例启动。
func TestResumeInterruptedInitialization(t *testing.T) {
	home := t.TempDir()
	inst := create(t, home, Options{})
	id := inst.InstanceID()
	inst.Close(t.Context()) // 模拟在身份初始化完成前退出
	if _, err := Open(t.Context(), Options{Home: home}); !HasReason(err, CodeInitIncomplete) {
		t.Fatalf("open unfinished init = %v", err)
	}
	inst = create(t, home, Options{})
	if inst.InstanceID() != id {
		t.Fatalf("resume changed instance id %s -> %s", id, inst.InstanceID())
	}
	if err := inst.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	inst.Close(t.Context())
	open(t, home, Options{})
}

func TestSecondInstanceIsRejected(t *testing.T) {
	home := createActive(t, Options{})
	first := open(t, home, Options{})
	if _, err := Open(t.Context(), Options{Home: home}); !HasReason(err, CodeInstanceLocked) {
		t.Fatalf("second writer = %v", err)
	}
	if _, err := Create(t.Context(), CreateOptions{Options: Options{Home: home}}); !HasReason(err, CodeInstanceLocked) {
		t.Fatalf("init while running = %v", err)
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := open(t, home, Options{})
	if second.State() != StateStarting {
		t.Fatalf("state = %s", second.State())
	}
}

// oldBuild 模拟较早的构建：每库只认识前 n 个迁移。
func oldBuild(n int) migrationSource {
	return func(db ownership.Database) []migrations.Migration {
		list := migrations.For(db)
		if len(list) > n {
			list = list[:n]
		}
		return list
	}
}

func appliedVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		rows.Scan(&v)
		out = append(out, v)
	}
	return out
}

// 迁移中断后实例保持维护状态；重跑只补未完成的版本，不重复也不伪造成功。
func TestInterruptedMigrationResumes(t *testing.T) {
	if migrations.Latest(ownership.Main) < 2 || migrations.Latest(ownership.Runtime) < 2 {
		t.Skip("needs at least two migrations in main and runtime")
	}
	home := createActive(t, Options{migrations: oldBuild(1)})

	inst := open(t, home, Options{})
	if inst.State() != StateBlocked || !hasReason(inst.Readiness().Reasons, CodeMigrationRequired) {
		t.Fatalf("new build over old data: %s %+v", inst.State(), inst.Readiness().Reasons)
	}
	if err := inst.Start(t.Context()); !HasReason(err, CodeMigrationRequired) {
		t.Fatalf("start before migrating = %v", err)
	}
	if inst.Readiness().Ready {
		t.Fatal("must not be ready before migrating")
	}
	inst.Close(t.Context())

	boom := errors.New("injected crash")
	inst = open(t, home, Options{beforeDB: func(db ownership.Database) error {
		if db == ownership.Runtime {
			return boom
		}
		return nil
	}})
	if _, err := inst.migrate(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("migrate = %v", err)
	}
	if got := appliedVersions(t, inst.DB(ownership.Main)); len(got) != migrations.Latest(ownership.Main) {
		t.Fatalf("main.db applied %v before the crash", got)
	}
	if got := appliedVersions(t, inst.DB(ownership.Runtime)); len(got) != 1 {
		t.Fatalf("runtime.db applied %v, crash was before it", got)
	}
	inst.Close(t.Context())

	inst = open(t, home, Options{})
	rs := inst.Readiness().Reasons
	if !hasReason(rs, CodeMigrationIncomplete) || !hasReason(rs, CodeMigrationRequired) {
		t.Fatalf("after interrupted migration: %+v", rs)
	}
	if err := inst.Start(t.Context()); err == nil {
		t.Fatal("must not start with an incomplete migration")
	}
	rep, err := inst.migrate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Applied[ownership.Main]) != 0 {
		t.Fatalf("main.db migrations must not be applied twice: %+v", rep.Applied)
	}
	if len(rep.Applied[ownership.Runtime]) == 0 {
		t.Fatalf("runtime.db migrations missing from the resumed run: %+v", rep.Applied)
	}
	for _, db := range migrations.Databases {
		want := migrations.Latest(db)
		if got := appliedVersions(t, inst.DB(db)); len(got) != want {
			t.Fatalf("%s applied %v, want %d versions", db, got, want)
		}
	}
	if m := inst.Marker(); m.Migration != nil || m.DataFormatVersion != DataFormatVersion {
		t.Fatalf("marker after migration = %+v", m)
	}
	if err := inst.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func hasReason(rs []Reason, code string) bool {
	for _, r := range rs {
		if r.Code == code {
			return true
		}
	}
	return false
}

func TestMigrateNoopAndRequiresUnstartedInstance(t *testing.T) {
	home := createActive(t, Options{})
	inst := open(t, home, Options{})
	rep, err := inst.migrate(t.Context())
	if err != nil || !rep.Noop {
		t.Fatalf("migrate up-to-date instance = %+v, %v", rep, err)
	}
	if inst.State() != StateStarting {
		t.Fatalf("state after noop migrate = %s", inst.State())
	}
	if err := inst.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := inst.migrate(t.Context()); err == nil {
		t.Fatal("migrating a running instance must be refused")
	}
}

// 旧构建打开由新构建迁移过的数据：拒绝，不降级。
func TestOlderBuildRefusesNewerSchema(t *testing.T) {
	if migrations.Latest(ownership.Main) < 2 {
		t.Skip("needs two migrations")
	}
	home := createActive(t, Options{})
	_, err := Open(t.Context(), Options{Home: home, migrations: oldBuild(1)})
	wantReason(t, err, CodeSchemaIncompatible)

	l, _ := NewLayout(home)
	m, _ := ReadMarker(l)
	m.DataFormatVersion = DataFormatVersion + 1
	if err := writeMarker(l, m); err != nil {
		t.Fatal(err)
	}
	_, err = Open(t.Context(), Options{Home: home})
	wantReason(t, err, CodeFormatNewer)
}

func TestDetectsForeignAndTamperedDatabases(t *testing.T) {
	homeA := createActive(t, Options{})
	homeB := createActive(t, Options{})
	la, _ := NewLayout(homeA)
	lb, _ := NewLayout(homeB)
	raw, err := os.ReadFile(lb.DBPath(ownership.Ledger))
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(la.DBPath(ownership.Ledger) + "-wal")
	os.Remove(la.DBPath(ownership.Ledger) + "-shm")
	if err := os.WriteFile(la.DBPath(ownership.Ledger), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(t.Context(), Options{Home: homeA})
	wantReason(t, err, CodeInstanceMismatch)

	// 已应用迁移的内容被改动，或出现未登记的表：拒绝启动。
	homeC := createActive(t, Options{})
	inst := open(t, homeC, Options{})
	if _, err := inst.DB(ownership.Main).ExecContext(t.Context(), `UPDATE schema_migrations SET checksum = 'sha256:00' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	inst.Close(t.Context())
	_, err = Open(t.Context(), Options{Home: homeC})
	wantReason(t, err, CodeSchemaIncompatible)

	homeD := createActive(t, Options{})
	inst = open(t, homeD, Options{})
	if _, err := inst.DB(ownership.Runtime).ExecContext(t.Context(), `CREATE TABLE scratch (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	inst.Close(t.Context())
	_, err = Open(t.Context(), Options{Home: homeD})
	wantReason(t, err, CodeSchemaIncompatible)
}

func TestIndexIsRecreatedWhenMissing(t *testing.T) {
	home := createActive(t, Options{})
	l, _ := NewLayout(home)
	for _, sfx := range []string{"", "-wal", "-shm"} {
		os.Remove(l.DBPath(ownership.Index) + sfx)
	}
	inst := open(t, home, Options{})
	r := inst.Readiness()
	if !hasReason(r.Notes, CodeIndexRecreated) || len(r.Reasons) > 0 {
		t.Fatalf("readiness = %+v", r)
	}
	if err := inst.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestDiskSpaceAndNetworkFileSystem(t *testing.T) {
	home := createActive(t, Options{})
	l, _ := NewLayout(home)
	cfg := "contract: lantai.config/v1\nstorage:\n  min_free_bytes: 9007199254740991\n"
	if err := os.WriteFile(l.ConfigPath(), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := open(t, home, Options{})
	if !hasReason(inst.Readiness().Reasons, CodeDiskSpaceLow) {
		t.Fatalf("reasons = %+v", inst.Readiness().Reasons)
	}
	if err := inst.Start(t.Context()); !HasReason(err, CodeDiskSpaceLow) {
		t.Fatalf("start with low disk = %v", err)
	}
	inst.Close(t.Context())
	os.WriteFile(l.ConfigPath(), []byte(testConfig), 0o644)

	remote := func(string) (fsutil.FSInfo, error) { return fsutil.FSInfo{Type: "nfs", Remote: true, Known: true}, nil }
	if _, err := Open(t.Context(), Options{Home: home, inspectFS: remote}); !HasReason(err, CodeNetworkFileSystem) {
		t.Fatalf("open on network fs = %v", err)
	}
	if _, err := Create(t.Context(), CreateOptions{Options: Options{Home: t.TempDir(), inspectFS: remote}}); !HasReason(err, CodeNetworkFileSystem) {
		t.Fatalf("init on network fs = %v", err)
	}
}

func TestBadConfigIsRejected(t *testing.T) {
	home := createActive(t, Options{})
	l, _ := NewLayout(home)
	os.WriteFile(l.ConfigPath(), []byte("contract: lantai.config/v1\nsecret: x\n"), 0o644)
	if _, err := Open(t.Context(), Options{Home: home}); err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("open with invalid config = %v", err)
	}
}

func TestStartHookFailureKeepsWritesClosed(t *testing.T) {
	home := createActive(t, Options{})
	inst := open(t, home, Options{})
	err := inst.Start(t.Context(), Hook{Name: "check", Run: func(context.Context) error { return errors.New("key missing") }})
	if err == nil {
		t.Fatal("start must fail")
	}
	r := inst.Readiness()
	if r.Ready || !hasReason(r.Reasons, CodeRecoveryFailed) {
		t.Fatalf("readiness = %+v", r)
	}
	if _, _, err := inst.Gate().Acquire(t.Context(), commands.Request{}); errcode.CodeOf(err) != errcode.MaintenanceMode {
		t.Fatalf("write after failed recovery = %v", err)
	}
}

// 维护屏障同时约束前台与后台写入：维护期间后台写入被拒，维护结束后继续。
func TestMaintenanceBarrierCoversBackgroundWriters(t *testing.T) {
	home := createActive(t, Options{})
	inst := open(t, home, Options{})
	var written, rejected atomic.Int64
	var inMaintenance atomic.Bool
	var duringMaintenance atomic.Int64
	inst.Go("writer", func(ctx context.Context) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			_, h, err := inst.Gate().Acquire(ctx, commands.Request{Security: commands.ModeShared})
			if err != nil {
				if errcode.CodeOf(err) == errcode.MaintenanceMode {
					rejected.Add(1)
					time.Sleep(time.Millisecond)
					continue
				}
				return err
			}
			if inMaintenance.Load() {
				duringMaintenance.Add(1)
			}
			written.Add(1)
			h.Release()
			time.Sleep(time.Millisecond)
		}
	})
	if err := inst.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return written.Load() > 5 })
	m, err := inst.Maintain(t.Context(), commands.ReasonMaintenance)
	if err != nil {
		t.Fatal(err)
	}
	inMaintenance.Store(true)
	if inst.Readiness().Ready || inst.State() != StateMaintenance {
		t.Fatal("instance must not report ready during maintenance")
	}
	before := rejected.Load()
	waitFor(t, func() bool { return rejected.Load() > before+5 })
	inMaintenance.Store(false)
	m.End()
	if duringMaintenance.Load() != 0 {
		t.Fatalf("%d background writes happened during maintenance", duringMaintenance.Load())
	}
	after := written.Load()
	waitFor(t, func() bool { return written.Load() > after+5 })
	if !inst.Readiness().Ready {
		t.Fatal("ready again after maintenance")
	}
	if err := inst.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// 优雅退出等待在途写入结束，之后释放数据根锁；退出期间新写入被拒。
func TestGracefulCloseDrainsInFlightWrites(t *testing.T) {
	home := createActive(t, Options{})
	inst := open(t, home, Options{})
	if err := inst.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, inflight, err := inst.Gate().Acquire(t.Context(), commands.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := inst.Close(context.Background()); err != nil {
			t.Error(err)
		}
		closed.Store(true)
	}()
	waitFor(t, func() bool { return inst.State() == StateStopping })
	if _, _, err := inst.Gate().Acquire(t.Context(), commands.Request{}); errcode.CodeOf(err) != errcode.MaintenanceMode {
		t.Fatalf("new write while stopping = %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if closed.Load() {
		t.Fatal("close must wait for the in-flight write")
	}
	inflight.Release()
	wg.Wait()
	if inst.State() != StateClosed || inst.DB(ownership.Main) != nil {
		t.Fatal("resources not released")
	}
	open(t, home, Options{}) // 锁已释放
}

// 在途写入没有排空时，Close 不关库、不释放数据根锁：另一个实例不能在旧写入
// 提交前取得写权；写入结束后再次 Close 才完成。
func TestCloseKeepsLockWhenWritesDoNotDrain(t *testing.T) {
	home := createActive(t, Options{})
	opts := Options{Home: home}
	inst, err := Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, inflight, err := inst.Gate().Acquire(t.Context(), commands.Request{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := inst.Close(ctx); err == nil || !strings.Contains(err.Error(), "did not drain") {
		t.Fatalf("close with stuck write = %v", err)
	}
	if inst.State() != StateStopping || inst.DB(ownership.Main) == nil {
		t.Fatalf("resources must be kept while a write is in flight: %s", inst.State())
	}
	if _, err := Open(t.Context(), opts); !HasReason(err, CodeInstanceLocked) {
		t.Fatalf("second instance while the first still has a write in flight = %v", err)
	}
	inflight.Release()
	if err := inst.Close(t.Context()); err != nil {
		t.Fatalf("close after the write finished = %v", err)
	}
	open(t, home, Options{})
}

// 启动钩子运行期间收到 Close：Close 等钩子结束，Start 之后不再开放写入。
func TestCloseDuringStartKeepsWritesClosed(t *testing.T) {
	home := createActive(t, Options{})
	inst, err := Open(t.Context(), Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	hookStarted, releaseHook := make(chan struct{}), make(chan struct{})
	startErr := make(chan error, 1)
	go func() {
		startErr <- inst.Start(context.Background(), Hook{Name: "slow", Run: func(context.Context) error {
			close(hookStarted)
			<-releaseHook
			return nil
		}})
	}()
	<-hookStarted
	closeErr := make(chan error, 1)
	go func() { closeErr <- inst.Close(context.Background()) }()
	waitFor(t, func() bool { return inst.State() == StateStopping })
	select {
	case err := <-closeErr:
		t.Fatalf("close finished while a startup step was running: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseHook)
	if err := <-startErr; err == nil {
		t.Fatal("start must fail when the instance was closed during startup")
	}
	if err := <-closeErr; err != nil {
		t.Fatal(err)
	}
	if open, _ := inst.Gate().State(); open || inst.State() != StateClosed {
		t.Fatalf("writes opened after close: open=%v state=%s", open, inst.State())
	}
	open(t, home, Options{})
}

// 较新构建开始、尚未完成的迁移，旧构建不能替它完成（否则标记会写成旧构建
// 不认识的格式）。
func TestIncompleteMigrationFromNewerBuild(t *testing.T) {
	home := createActive(t, Options{})
	l, _ := NewLayout(home)
	m, _ := ReadMarker(l)
	m.Migration = &MigrationRun{RunID: m.InstanceID, StartedAt: m.UpdatedAt, FromFormatVersion: DataFormatVersion, TargetFormatVersion: DataFormatVersion + 1}
	if err := writeMarker(l, m); err != nil {
		t.Fatal(err)
	}
	_, err := Open(t.Context(), Options{Home: home})
	wantReason(t, err, CodeFormatNewer)
	// 本构建自己的中断迁移可以续做，完成后标记写本构建的格式。
	m.Migration.TargetFormatVersion = DataFormatVersion
	if err := writeMarker(l, m); err != nil {
		t.Fatal(err)
	}
	inst := open(t, home, Options{})
	if _, err := inst.migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := inst.Marker(); got.Migration != nil || got.DataFormatVersion != DataFormatVersion {
		t.Fatalf("marker after resumed migration = %+v", got)
	}
}

func TestInspectHealthyInstance(t *testing.T) {
	home := createActive(t, Options{})
	rep, err := Inspect(t.Context(), home)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Compatible() || rep.Marker == nil || len(rep.Databases) != 5 {
		t.Fatalf("report = %+v", rep)
	}
	// 实例运行中也能诊断。
	inst := open(t, home, Options{})
	if rep, err = Inspect(t.Context(), home); err != nil || !rep.Compatible() {
		t.Fatalf("inspect while running = %+v %v", rep.Problems, err)
	}
	if rep.LockHolder == nil || rep.LockHolder.PID != os.Getpid() {
		t.Fatalf("lock holder = %+v", rep.LockHolder)
	}
	inst.Close(t.Context())
	empty, err := Inspect(t.Context(), filepath.Join(t.TempDir(), "none"))
	if err != nil || empty.Initialized || !hasProblem(empty, CodeNotInitialized) {
		t.Fatalf("empty report = %+v %v", empty, err)
	}
}
