package identity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

type recoveryBoundaryClock struct {
	base    clock.Clock
	blockAt int64
	calls   atomic.Int64
	entered chan struct{}
	resume  chan struct{}
}

func (c *recoveryBoundaryClock) Now() time.Time {
	if c.calls.Add(1) == c.blockAt {
		close(c.entered)
		<-c.resume
	}
	return c.base.Now()
}

func beginSelfRecovery(t *testing.T, f *fixture, code string) IssuedSession {
	t.Helper()
	r, err := f.svc.StartRecovery(t.Context(), RecoveryRequest{Name: "ada", Password: adminPassword, Code: code, Channel: ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 最终核验已经开始时，撤权必须等待该短提交退出；撤权成功后再开始的写入拒绝。
func TestRecoveryWritesSerializeWithSessionEnd(t *testing.T) {
	for _, name := range []string{"set_password", "enroll_factor"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			r := beginSelfRecovery(t, f, f.codes[0])
			blockAt := int64(1)
			mutate := func() error {
				_, err := f.svc.EnrollFactor(t.Context(), r.Context)
				return err
			}
			if name == "set_password" {
				blockAt = 2 // 慢哈希前的预检之外，第二次是锁内的最终核验。
				mutate = func() error { return f.svc.SetPassword(t.Context(), r.Context, "accepted before session ends") }
			}
			clk := &recoveryBoundaryClock{base: f.clk, blockAt: blockAt, entered: make(chan struct{}), resume: make(chan struct{})}
			f.svc.clock = clk
			var once sync.Once
			release := func() { once.Do(func() { close(clk.resume) }) }
			defer release()
			done := make(chan error, 1)
			go func() { done <- mutate() }()
			select {
			case <-clk.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("recovery write did not reach the final check")
			}
			waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			err := f.svc.EndSession(waitCtx, r.Context, r.Session.SessionID)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("session end bypassed an in-flight final acceptance: %v", err)
			}
			release()
			if err := <-done; err != nil {
				t.Fatalf("already accepted recovery write: %v", err)
			}
			if err := f.svc.EndSession(t.Context(), r.Context, r.Session.SessionID); err != nil {
				t.Fatal(err)
			}
			wantCode(t, mutate(), errcode.TokenRevoked)
		})
	}
}

func TestPasswordRevokedBeforeFinalAcceptance(t *testing.T) {
	f := newFixture(t)
	r := beginSelfRecovery(t, f, f.codes[0])
	clk := &recoveryBoundaryClock{base: f.clk, blockAt: 1, entered: make(chan struct{}), resume: make(chan struct{})}
	f.svc.clock = clk
	var once sync.Once
	release := func() { once.Do(func() { close(clk.resume) }) }
	defer release()
	done := make(chan error, 1)
	go func() { done <- f.svc.SetPassword(t.Context(), r.Context, "must never become the password") }()
	select {
	case <-clk.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("password change did not reach its preliminary check")
	}
	if err := f.svc.EndSession(t.Context(), r.Context, r.Session.SessionID); err != nil {
		t.Fatal(err)
	}
	release()
	wantCode(t, <-done, errcode.TokenRevoked)
	if _, _, valid, err := f.svc.verifyPassword(t.Context(), "ada", adminPassword); err != nil || !valid {
		t.Fatalf("revoked change replaced the existing password: valid=%v, err=%v", valid, err)
	}
}

func TestFactorConfirmationSurvivesRuntimeCleanupFailure(t *testing.T) {
	f := newFixture(t)
	r := beginSelfRecovery(t, f, f.codes[0])
	other := beginSelfRecovery(t, f, f.codes[0])
	e, err := f.svc.EnrollFactor(t.Context(), r.Context)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.runtime.ExecContext(t.Context(), `CREATE TRIGGER fail_recovery_cleanup BEFORE UPDATE OF ended_at ON identity_sessions
		BEGIN SELECT RAISE(ABORT, 'injected runtime cleanup failure'); END`); err != nil {
		t.Fatal(err)
	}
	codes, err := f.svc.ConfirmFactor(t.Context(), r.Context, f.now(e.raw))
	if err != nil || len(codes) != 10 {
		t.Fatalf("committed recovery must deliver its one-time codes: %d %v", len(codes), err)
	}
	for _, session := range []IssuedSession{r, other} {
		_, err := f.svc.VerifySession(t.Context(), session.Session.SessionID)
		wantCode(t, err, errcode.TokenRevoked)
	}
	if _, err := f.svc.ConfirmFactor(t.Context(), r.Context, f.now(e.raw)); !isCode(err, errcode.TokenRevoked) {
		t.Fatalf("completed factor confirmation replay: %v", err)
	}
	if _, err := f.svc.ReconcileRecoverySessions(t.Context()); err == nil {
		t.Fatal("cleanup must surface its retryable runtime error")
	}
	if _, err := f.svc.runtime.ExecContext(t.Context(), `DROP TRIGGER fail_recovery_cleanup`); err != nil {
		t.Fatal(err)
	}
	// 用已交付的新码开始另一恢复，清理不能误结束这个新 operation 的会话。
	current := beginSelfRecovery(t, f, codes[0])
	if count, err := f.svc.ReconcileRecoverySessions(t.Context()); err != nil || count != 2 {
		t.Fatalf("cleanup old recovery sessions: %d %v", count, err)
	}
	if _, err := f.svc.VerifySession(t.Context(), current.Session.SessionID); err != nil {
		t.Fatalf("cleanup ended the current recovery operation: %v", err)
	}
	if count, err := f.svc.ReconcileRecoverySessions(t.Context()); err != nil || count != 0 {
		t.Fatalf("cleanup replay: %d %v", count, err)
	}
}

func TestAdminFactorResetRequiresANewPassword(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin().Context
	alice, setup := f.register(admin, authz.Human, "alice")
	f.completeSetup("alice", setup, "alice original password")
	res := f.mustSudo(admin, f.adminSecret, &ResetHumanFactor{PrincipalID: alice.ID,
		ExpectedRevision: currentRev(t, f, alice.ID), Reason: "lost authenticator"})
	r, err := f.svc.StartSetup(t.Context(), SetupRequest{Name: "alice", Code: res.Secret, Channel: ChannelCLI})
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.svc.EnrollFactor(t.Context(), r.Context)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.ConfirmFactor(t.Context(), r.Context, f.now(e.raw))
	wantCode(t, err, errcode.InvalidStateTransition)
	if err := f.svc.SetPassword(t.Context(), r.Context, "alice replacement password"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ConfirmFactor(t.Context(), r.Context, f.now(e.raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Login(t.Context(), LoginRequest{Name: "alice", Password: "alice original password", Code: f.fresh(e.raw), Channel: ChannelCLI}); !isCode(err, errcode.AuthRequired) {
		t.Fatalf("old password remained valid after reset: %v", err)
	}
	f.login("alice", "alice replacement password", e.raw, ChannelCLI)
}

func TestFactorConfirmationRateLimitSurvivesReenrollment(t *testing.T) {
	f := newFixture(t)
	r := beginSelfRecovery(t, f, f.codes[0])
	if _, err := f.svc.EnrollFactor(t.Context(), r.Context); err != nil {
		t.Fatal(err)
	}
	const extra = 6
	done := make(chan error, f.svc.cfg.FailureLimit+extra)
	for range f.svc.cfg.FailureLimit + extra {
		go func() {
			_, err := f.svc.ConfirmFactor(t.Context(), r.Context, "invalid")
			done <- err
		}()
	}
	invalid, limited := 0, 0
	for range f.svc.cfg.FailureLimit + extra {
		switch err := <-done; errcode.CodeOf(err) {
		case errcode.HumanProofRequired:
			invalid++
		case errcode.RateLimited:
			limited++
		default:
			t.Fatalf("unexpected confirmation result: %v", err)
		}
	}
	if invalid != f.svc.cfg.FailureLimit || limited != extra {
		t.Fatalf("concurrent failures escaped the limit: invalid=%d limited=%d", invalid, limited)
	}
	e, err := f.svc.EnrollFactor(t.Context(), r.Context)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.ConfirmFactor(t.Context(), r.Context, f.now(e.raw))
	wantCode(t, err, errcode.RateLimited)
	f.clk.Advance(f.svc.cfg.FailureWindow + time.Millisecond)
	if _, err := f.svc.ConfirmFactor(t.Context(), r.Context, f.now(e.raw)); err != nil {
		t.Fatalf("confirmation did not recover after its rate window: %v", err)
	}
}

func TestSetupPasswordMigrationPreservesEnrolledAndSelfRecovery(t *testing.T) {
	f := newFixture(t)
	setup, recovering := ids.New(), ids.New()
	for id, state := range map[ids.ID]string{setup: factorSetupPending, recovering: factorResetPending} {
		if _, err := f.svc.main.ExecContext(t.Context(), `INSERT INTO identity_human_accounts
			(principal_id, factor_state, pending_operation_id, revision, updated_at) VALUES (?, ?, ?, 1, 1)`, id, state, ids.New()); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.main.ExecContext(t.Context(), `INSERT INTO identity_passwords (principal_id, params, salt, hash, updated_at)
			SELECT ?, params, salt, hash, updated_at FROM identity_passwords WHERE principal_id = ?`, id, f.admin.ID); err != nil {
			t.Fatal(err)
		}
	}
	var migration string
	for _, m := range migrations.For(ownership.Main) {
		if m.Name == "setup-password-reset" {
			migration = m.SQL
		}
	}
	if migration == "" {
		t.Fatal("setup password compatibility migration is not registered")
	}
	if _, err := f.svc.main.ExecContext(t.Context(), migration); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPassword(t.Context(), f.svc.main, setup); !errors.Is(err, errNotFound) {
		t.Fatalf("legacy setup password survived migration: %v", err)
	}
	for _, id := range []ids.ID{f.admin.ID, recovering} {
		if _, err := loadPassword(t.Context(), f.svc.main, id); err != nil {
			t.Fatalf("migration changed an enrolled or self-recovery password: %v", err)
		}
	}
}
