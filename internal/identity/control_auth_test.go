package identity

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestBackupControlAuthenticationIsReadOnlyBoundAndPersistent(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin()
	dir := filepath.Join(t.TempDir(), "auth")
	if e := os.Chmod(filepath.Dir(dir), 0700); e != nil {
		t.Fatal(e)
	}
	f.svc.initControlAuth(dir)
	if p := f.svc.controlLedger.err.Load(); p != nil {
		t.Fatalf("ledger initialization: %v", *p)
	}
	scope := BackupControlScope{JobID: ids.New(), Attempt: 1}
	read := func() (BackupControlScope, bool) { return scope, true }
	m, e := f.inst.Maintain(t.Context(), "backup")
	if e != nil {
		t.Fatal(e)
	}
	for _, db := range []*sql.DB{f.svc.main, f.svc.runtime} {
		db.SetMaxOpenConns(1)
		if _, e = db.ExecContext(t.Context(), "PRAGMA query_only=ON"); e != nil {
			t.Fatal(e)
		}
	}
	defer func() {
		for _, db := range []*sql.DB{f.svc.main, f.svc.runtime} {
			_, _ = db.ExecContext(context.Background(), "PRAGMA query_only=OFF")
		}
		m.End()
	}()
	code := f.fresh(f.adminSecret)
	session, e := f.svc.LoginBackupControl(t.Context(), "ada", adminPassword, code, "source-1", read)
	if e != nil {
		t.Fatal(e)
	}
	if session.Token == "" || session.CSRFToken == "" || session.Scope != scope {
		t.Fatalf("session %+v", session)
	}
	use := func(v BackupControlSession) error {
		if v.Principal.ID != f.admin.ID {
			t.Fatal("wrong principal")
		}
		return nil
	}
	if e = f.svc.WithBackupControl(t.Context(), session.Token, "", false, read, use); e != nil {
		t.Fatal(e)
	}
	wantCode(t, f.svc.WithBackupControl(t.Context(), session.Token, "wrong", true, read, use), errcode.Forbidden)
	if e = f.svc.WithBackupControl(t.Context(), session.Token, session.CSRFToken, true, read, use); e != nil {
		t.Fatal(e)
	}
	if _, e = f.svc.AuthenticateToken(t.Context(), session.Token); e == nil {
		t.Fatal("control credential escaped into ordinary auth")
	}
	_, e = f.svc.LoginBackupControl(t.Context(), "ada", adminPassword, code, "source-1", read)
	wantCode(t, e, errcode.TOTPReplay)
	scope.Attempt++
	wantCode(t, f.svc.WithBackupControl(t.Context(), session.Token, "", false, read, use), errcode.TokenRevoked)
	scope.Attempt--
	// A fresh process/service retains replay protection but no control sessions.
	other := f.newService(f.key)
	other.initControlAuth(dir)
	wantCode(t, other.WithBackupControl(t.Context(), session.Token, "", false, read, use), errcode.TokenRevoked)
	_, e = other.LoginBackupControl(t.Context(), "ada", adminPassword, code, "source-1", read)
	wantCode(t, e, errcode.TOTPReplay)
	for _, db := range []*sql.DB{f.svc.main, f.svc.runtime} {
		_, _ = db.ExecContext(t.Context(), "PRAGMA query_only=OFF")
	}
	m.End()
	_, e = other.Login(t.Context(), LoginRequest{Name: "ada", Password: adminPassword, Code: code, Channel: ChannelCLI})
	wantCode(t, e, errcode.TOTPReplay)
	ch, e := other.CreateChallenge(t.Context(), admin.Context, &ApproveBackup{Kind: "start"})
	if e != nil {
		t.Fatal(e)
	}
	_, e = other.VerifyChallenge(t.Context(), admin.Context, ch.ChallengeID, code, "")
	wantCode(t, e, errcode.TOTPReplay)
	f.clk.Advance(10 * time.Minute)
	wantCode(t, f.svc.WithBackupControl(t.Context(), session.Token, "", false, read, use), errcode.TokenExpired)
}

func TestBackupControlFailuresSurviveRestartAndCannotRaceQuota(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "auth")
	if e := os.Chmod(filepath.Dir(dir), 0700); e != nil {
		t.Fatal(e)
	}
	f.svc.initControlAuth(dir)
	if p := f.svc.controlLedger.err.Load(); p != nil {
		t.Fatalf("ledger initialization: %v", *p)
	}
	scope := BackupControlScope{JobID: ids.New(), Attempt: 1}
	read := func() (BackupControlScope, bool) { return scope, true }
	m, e := f.inst.Maintain(t.Context(), "backup")
	if e != nil {
		t.Fatal(e)
	}
	defer m.End()
	var mu sync.Mutex
	codes := map[errcode.Code]int{}
	var wg sync.WaitGroup
	for range 14 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.svc.LoginBackupControl(t.Context(), "ada", "wrong password", "000000", "same-source", read)
			mu.Lock()
			codes[errcode.CodeOf(e)]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[errcode.AuthRequired] != 10 || codes[errcode.RateLimited] != 4 {
		t.Fatalf("rate counts %+v", codes)
	}
	other := f.newService(f.key)
	other.initControlAuth(dir)
	_, e = other.LoginBackupControl(t.Context(), "ada", adminPassword, f.fresh(f.adminSecret), "other-source", read)
	wantCode(t, e, errcode.RateLimited)
	m.End()
	_, e = other.Login(t.Context(), LoginRequest{Name: "ada", Password: adminPassword, Code: f.fresh(f.adminSecret), Channel: ChannelCLI, Source: "third-source"})
	wantCode(t, e, errcode.RateLimited)
	f.clk.Advance(11 * time.Minute)
	if _, e = other.Login(t.Context(), LoginRequest{Name: "ada", Password: adminPassword, Code: f.fresh(f.adminSecret), Channel: ChannelCLI, Source: "third-source"}); e != nil {
		t.Fatal(e)
	}
}

func TestBackupControlBadLedgerOnlyBlocksNewProofs(t *testing.T) {
	f := newFixture(t)
	session := f.adminLogin()
	dir := filepath.Join(t.TempDir(), "auth")
	if e := os.Chmod(filepath.Dir(dir), 0700); e != nil {
		t.Fatal(e)
	}
	f.svc.initControlAuth(dir)
	if p := f.svc.controlLedger.err.Load(); p != nil {
		t.Fatalf("ledger initialization: %v", *p)
	}
	if e := os.WriteFile(filepath.Join(dir, "state.json"), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	other := f.newService(f.key)
	other.initControlAuth(dir)
	if _, e := other.AuthenticateToken(t.Context(), session.Token); e != nil {
		t.Fatal("valid session must survive safety-ledger fault", e)
	}
	_, e := other.Login(t.Context(), LoginRequest{Name: "ada", Password: adminPassword, Code: f.fresh(f.adminSecret), Channel: ChannelCLI})
	wantCode(t, e, errcode.StorageUnavailable)
	other.controlLedger.fail(errors.New("disk fault"))
	ch, e := other.CreateChallenge(t.Context(), session.Context, &ApproveBackup{Kind: "start"})
	if e != nil {
		t.Fatal(e)
	}
	_, e = other.VerifyChallenge(t.Context(), session.Context, ch.ChallengeID, f.fresh(f.adminSecret), "")
	wantCode(t, e, errcode.StorageUnavailable)
	if d, e := other.Authorize(t.Context(), session.Context, ActBackupRead, authz.Resource{}); e != nil || !d.Allowed {
		t.Fatal("valid role check blocked", e)
	}
}

func TestBackupControlInitCrashAtomic(t *testing.T) {
	if stage := os.Getenv("LANTAI_TEST_CONTROL_INIT_CRASH"); stage != "" {
		f := newFixture(t)
		f.svc.controlInitFault = func(at string) error {
			if at == stage {
				os.Exit(31)
			}
			return nil
		}
		f.svc.initControlAuth(os.Getenv("LANTAI_TEST_CONTROL_AUTH_DIR"))
		t.Fatal("expected initialization crash")
	}
	for _, stage := range []string{"temporary_directory", "before_install"} {
		t.Run(stage, func(t *testing.T) {
			home := t.TempDir()
			if e := os.Chmod(home, 0700); e != nil {
				t.Fatal(e)
			}
			dir := filepath.Join(home, "backups", "control-auth")
			childtmp := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupControlInitCrashAtomic$")
			cmd.Env = append(os.Environ(), "LANTAI_TEST_CONTROL_INIT_CRASH="+stage, "LANTAI_TEST_CONTROL_AUTH_DIR="+dir, "TMPDIR="+childtmp)
			out, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 31 {
				t.Fatalf("child: %v %s", e, out)
			}
			if _, e = os.Stat(dir); !errors.Is(e, os.ErrNotExist) {
				t.Fatalf("partial final directory visible: %v", e)
			}
			f := newFixture(t)
			f.svc.initControlAuth(dir)
			if e = f.svc.controlLedger.problem(); e != nil {
				t.Fatalf("restart failed %v", e)
			}
			if _, e = os.Stat(filepath.Join(dir, "state.json")); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestBackupControlRejectsWritableParentAndLinkedLedger(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("POSIX boundary")
	}
	f := newFixture(t)
	home := t.TempDir()
	if e := os.Chmod(home, 0700); e != nil {
		t.Fatal(e)
	}
	parent := filepath.Join(home, "backups")
	if e := os.Mkdir(parent, 0770); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(parent, 0770); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(parent, "control-auth")
	f.svc.initControlAuth(dir)
	if e := f.svc.controlLedger.problem(); e == nil {
		t.Fatal("writable parent accepted")
	}
	if e := os.Chmod(parent, 0700); e != nil {
		t.Fatal(e)
	}
	f.svc.initControlAuth(dir)
	if e := f.svc.controlLedger.problem(); e != nil {
		t.Fatal(e)
	}
	if e := os.Link(filepath.Join(dir, "state.json"), filepath.Join(home, "linked-ledger")); e != nil {
		t.Fatal(e)
	}
	if e := f.svc.controlLedger.problem(); e == nil {
		t.Fatal("linked state file accepted")
	}
}

func TestBackupControlSlowHashDoesNotHoldProofOrSecurity(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "auth")
	if e := os.Chmod(filepath.Dir(dir), 0700); e != nil {
		t.Fatal(e)
	}
	f.svc.initControlAuth(dir)
	var active atomic.Bool
	active.Store(true)
	scope := BackupControlScope{JobID: ids.New(), Attempt: 1}
	read := func() (BackupControlScope, bool) { return scope, active.Load() }
	m, e := f.inst.Maintain(t.Context(), "backup")
	if e != nil {
		t.Fatal(e)
	}
	defer m.End()
	entered, resume := make(chan struct{}), make(chan struct{})
	f.svc.controlTestHook = func(stage string) {
		if stage == "before_hash" {
			close(entered)
			<-resume
		}
	}
	result := make(chan error, 1)
	code := f.fresh(f.adminSecret)
	go func() {
		_, e := f.svc.LoginBackupControl(t.Context(), "ada", adminPassword, code, "source", read)
		result <- e
	}()
	<-entered
	c, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, security, e := f.inst.Gate().Coordinator().Acquire(c, commands.Request{Security: commands.ModeExclusive})
	if e != nil {
		t.Fatal("slow hash holds security", e)
	}
	security.Release()
	done := make(chan struct{})
	go func() { release := f.svc.DrainBackupControl(); active.Store(false); release(); close(done) }()
	select {
	case <-done:
	case <-c.Done():
		t.Fatal("slow hash holds proof")
	}
	close(resume)
	wantCode(t, <-result, errcode.MaintenanceMode)
	if len(f.svc.controlLedger.state.Load().Failures) != 1 {
		t.Fatal("aborted proof lost reserved failure")
	}
}

func TestBackupControlFinalCommitIsDrainedBeforeResume(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "auth")
	if e := os.Chmod(filepath.Dir(dir), 0700); e != nil {
		t.Fatal(e)
	}
	f.svc.initControlAuth(dir)
	scope := BackupControlScope{JobID: ids.New(), Attempt: 1}
	read := func() (BackupControlScope, bool) { return scope, true }
	m, e := f.inst.Maintain(t.Context(), "backup")
	if e != nil {
		t.Fatal(e)
	}
	defer m.End()
	entered, resume := make(chan struct{}), make(chan struct{})
	f.svc.controlTestHook = func(stage string) {
		if stage == "before_commit" {
			close(entered)
			<-resume
		}
	}
	result := make(chan error, 1)
	code := f.fresh(f.adminSecret)
	go func() {
		_, e := f.svc.LoginBackupControl(t.Context(), "ada", adminPassword, code, "source", read)
		result <- e
	}()
	<-entered
	drained := make(chan struct{})
	go func() { release := f.svc.DrainBackupControl(); close(drained); release() }()
	select {
	case <-drained:
		t.Fatal("final authentication commit was not drained")
	case <-time.After(30 * time.Millisecond):
	}
	close(resume)
	if e = <-result; e != nil {
		t.Fatal(e)
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("proof fence did not finish")
	}
}

func TestBackupControlCounterWinsOverInFlightOrdinaryLogin(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "auth")
	if e := os.Chmod(filepath.Dir(dir), 0700); e != nil {
		t.Fatal(e)
	}
	f.svc.initControlAuth(dir)
	normalEntered, normalContinue := make(chan struct{}), make(chan struct{})
	f.svc.controlTestHook = func(stage string) {
		if stage == "ordinary_before_hash" {
			close(normalEntered)
			<-normalContinue
		}
	}
	code := f.fresh(f.adminSecret)
	ordinary := make(chan error, 1)
	go func() {
		_, e := f.svc.Login(t.Context(), LoginRequest{Name: "ada", Password: adminPassword, Code: code, Channel: ChannelCLI})
		ordinary <- e
	}()
	<-normalEntered
	// The ordinary request is already admitted and hashing outside all locks;
	// the domain write gate remains open until this in-flight request drains.
	scope := BackupControlScope{JobID: ids.New(), Attempt: 1}
	read := func() (BackupControlScope, bool) { return scope, true }
	if _, e := f.svc.LoginBackupControl(t.Context(), "ada", adminPassword, code, "source", read); e != nil {
		t.Fatal(e)
	}
	close(normalContinue)
	wantCode(t, <-ordinary, errcode.TOTPReplay)
}
