package identity

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
)

func restoreContext(t *testing.T, f *fixture) (context.Context, *commands.Held, *epochBox) {
	t.Helper()
	epochs := &epochBox{}
	epochs.v.Store(2)
	f.svc.epochs = epochs
	ctx, held, err := f.svc.gate.Maintain(t.Context(), commands.ReasonRecovering)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(held.Release)
	return ctx, held, epochs
}

func TestRestoreInvalidatesEveryOldAuthenticationPath(t *testing.T) {
	f := newFixture(t)
	adminSession := f.adminLogin()
	alice, setup := f.register(adminSession.Context, authz.Human, "alice")
	secret := f.completeSetup("alice", setup, "alice private password")
	aliceSession := f.login("alice", "alice private password", secret, ChannelCLI)
	_, pendingSetup := f.register(adminSession.Context, authz.Human, "bob")
	agent, _ := f.register(adminSession.Context, authz.Agent, "fixture@node")
	token := f.issueToken(adminSession.Context, agent, []Scope{ScopeRead})
	agentSession := f.exchange(token, SessionRequest{Channel: ChannelCLI})
	project := ids.New()
	f.grantRole(adminSession.Context, project, alice, RoleViewer)
	challenge, err := f.svc.CreateChallenge(t.Context(), adminSession.Context, &SetPolicy{Key: "lock.by_flow", Value: json.RawMessage(`true`)})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := f.svc.VerifyChallenge(t.Context(), adminSession.Context, challenge.ChallengeID, f.fresh(f.adminSecret), "")
	if err != nil {
		t.Fatal(err)
	}
	// A consumed code still authorizes continuation of its old reset operation;
	// restoration must invalidate these too, not only unused codes.
	if _, err = f.svc.main.Exec(`UPDATE identity_recovery_codes SET consumed_at=1,consumed_operation_id=? WHERE principal_id=?`, ids.New(), f.admin.ID); err != nil {
		t.Fatal(err)
	}
	ctx, _, epochs := restoreContext(t, f)
	run := ids.New()
	if err = f.svc.InvalidateForRestore(ctx, run, 2); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT count(*) FROM identity_passwords`,
		`SELECT count(*) FROM identity_totp_factors WHERE state IN ('enabled','pending')`,
		`SELECT count(*) FROM identity_credentials WHERE revoked_at IS NULL`,
		`SELECT count(*) FROM identity_recovery_codes WHERE invalidated_at IS NULL`,
		`SELECT count(*) FROM identity_setup_codes WHERE invalidated_at IS NULL`,
		`SELECT count(*) FROM identity_human_grants WHERE state='bound'`,
		`SELECT count(*) FROM identity_challenges WHERE state IN ('pending','verified')`,
	} {
		var n int
		if err = f.svc.main.QueryRow(query).Scan(&n); err != nil || n != 0 {
			t.Fatalf("old authentication state remains for %s: %d %v", query, n, err)
		}
	}
	var n int
	if err = f.svc.runtime.QueryRow(`SELECT count(*) FROM identity_sessions WHERE ended_at IS NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old sessions remain: %d %v", n, err)
	}
	for _, session := range []IssuedSession{adminSession, aliceSession, agentSession} {
		if _, err = f.svc.VerifySession(ctx, session.Session.SessionID); err == nil {
			t.Fatal("old session survived")
		}
	}
	if _, err = f.svc.ExchangeToken(ctx, token, SessionRequest{Channel: ChannelCLI}); err == nil {
		t.Fatal("old machine credential survived")
	}
	if _, err = f.svc.StartSetup(ctx, SetupRequest{Name: "bob", Code: pendingSetup, Channel: ChannelCLI}); err == nil {
		t.Fatal("old setup code survived")
	}
	if _, err = f.svc.StartRecovery(ctx, RecoveryRequest{Name: "ada", Password: adminPassword, Code: f.codes[0], Channel: ChannelCLI}); err == nil {
		t.Fatal("old recovery credentials survived")
	}
	if _, err = f.svc.Login(ctx, LoginRequest{Name: "alice", Password: "alice private password", Code: f.fresh(secret), Channel: ChannelCLI}); err == nil {
		t.Fatal("ordinary human's old password/TOTP survived")
	}
	var state string
	if err = f.svc.main.QueryRow(`SELECT state FROM identity_human_grants WHERE grant_id=?`, grant.GrantID).Scan(&state); err != nil || state != "revoked" {
		t.Fatalf("old grant: %s %v", state, err)
	}
	roles, err := projectRolesOf(ctx, f.svc.main, project, alice.ID)
	if err != nil || len(roles) != 1 || roles[0] != RoleViewer {
		t.Fatalf("identity/roles were destroyed: %v %v", roles, err)
	}
	wantCode(t, f.svc.VerifyRestoreAdministrator(ctx, run, f.admin.ID), errcode.PreconditionFailed)
	// Root swaps the separately stored key only after old factors have been
	// checked and disabled; the service is then reconstructed with the new key.
	key, err := masterkey.Generate(f.gen, f.clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = f.newService(key)
	f.svc.epochs = epochs
	reset, err := f.svc.BeginOfflineReset(ctx, "ada", "new recovery administrator password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reset.Confirm(ctx, f.now(reset.Enrollment.raw), "confirmed restore administrator"); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.VerifyRestoreAdministrator(ctx, run, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.svc.VerifyRestoreAdministrator(ctx, run, alice.ID), errcode.PreconditionFailed)
	if err = f.svc.InvalidateForRestore(ctx, run, 2); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.VerifyRestoreAdministrator(ctx, run, f.admin.ID); err != nil {
		t.Fatalf("retry destroyed new administrator: %v", err)
	}
	if _, err = f.svc.Login(ctx, LoginRequest{Name: "ada", Password: "new recovery administrator password", Code: f.fresh(reset.Enrollment.raw), Channel: ChannelCLI}); err != nil {
		t.Fatalf("new administrator cannot authenticate: %v", err)
	}
	if err = f.svc.InvalidateForRestore(ctx, run, 2); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.runtime.QueryRow(`SELECT count(*) FROM identity_sessions WHERE recovery_epoch=2 AND ended_at IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("retry ended new session: %d %v", n, err)
	}
	// Altered role/factor state cannot borrow the previously completed proof.
	if _, err = f.svc.main.Exec(`DELETE FROM identity_system_roles WHERE principal_id=?`, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.svc.VerifyRestoreAdministrator(ctx, run, f.admin.ID), errcode.PreconditionFailed)
}

func TestRestoreRetriesAfterRuntimeFailureWithoutRepeatingInvalidation(t *testing.T) {
	f := newFixture(t)
	old := f.adminLogin()
	before, err := loadPrincipal(t.Context(), f.svc.main, f.admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _, _ := restoreContext(t, f)
	run := ids.New()
	if _, err = f.svc.runtime.Exec(`CREATE TRIGGER fail_restore_cleanup BEFORE UPDATE OF ended_at ON identity_sessions BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.InvalidateForRestore(ctx, run, 2); err == nil {
		t.Fatal("runtime failure was hidden")
	}
	if _, err = f.svc.VerifySession(ctx, old.Session.SessionID); err == nil {
		t.Fatal("main revocation was not authoritative")
	}
	if _, err = f.svc.runtime.Exec(`DROP TRIGGER fail_restore_cleanup`); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.InvalidateForRestore(ctx, run, 2); err != nil {
		t.Fatal(err)
	}
	after, err := loadPrincipal(ctx, f.svc.main, f.admin.ID)
	if err != nil || after.AuthEpoch != before.AuthEpoch+1 {
		t.Fatalf("retry repeated main invalidation: %v %v", after.AuthEpoch, err)
	}
	var n int
	if err = f.svc.main.QueryRow(`SELECT count(*) FROM identity_restore_runs WHERE run_id=?`, run).Scan(&n); err != nil || n != 1 {
		t.Fatalf("restore record duplicated: %d %v", n, err)
	}
}

func TestRestoreInvalidationIsAtomicAndRequiresCorrectKey(t *testing.T) {
	f := newFixture(t)
	ctx, _, _ := restoreContext(t, f)
	run := ids.New()
	before, err := loadPrincipal(ctx, f.svc.main, f.admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := masterkey.Generate(f.gen, f.clk, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.key = wrong
	if err = f.svc.InvalidateForRestore(ctx, run, 2); err == nil {
		t.Fatal("wrong backup key was accepted")
	}
	f.svc.key = f.key
	if _, err = f.svc.main.Exec(`CREATE TRIGGER fail_restore_run BEFORE INSERT ON identity_restore_runs BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.InvalidateForRestore(ctx, run, 2); err == nil {
		t.Fatal("main write failure was hidden")
	}
	after, err := loadPrincipal(ctx, f.svc.main, f.admin.ID)
	if err != nil || after.AuthEpoch != before.AuthEpoch {
		t.Fatal("main invalidation partially committed")
	}
	if _, err = loadPassword(ctx, f.svc.main, f.admin.ID); err != nil {
		t.Fatal("password deleted despite rollback", err)
	}
	if _, err = loadFactor(ctx, f.svc.main, f.admin.ID, factorEnabled); err != nil {
		t.Fatal("factor disabled despite rollback", err)
	}
}

func TestRestoreRunAndMaintenanceBindings(t *testing.T) {
	f := newFixture(t)
	run := ids.New()
	wantCode(t, f.svc.InvalidateForRestore(t.Context(), run, 2), errcode.MaintenanceMode)
	ctx, held, epochs := restoreContext(t, f)
	wantCode(t, f.svc.InvalidateForRestore(ctx, run, 3), errcode.PreconditionFailed)
	if err := f.svc.InvalidateForRestore(ctx, run, 2); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.svc.InvalidateForRestore(ctx, ids.New(), 2), errcode.IdempotencyConflict)
	epochs.v.Store(3)
	wantCode(t, f.svc.InvalidateForRestore(ctx, run, 3), errcode.IdempotencyConflict)
	epochs.v.Store(2)
	held.Release()
	wantCode(t, f.svc.InvalidateForRestore(ctx, run, 2), errcode.MaintenanceMode)
	wantCode(t, f.svc.VerifyRestoreAdministrator(ctx, run, f.admin.ID), errcode.MaintenanceMode)
	if _, err := f.svc.BeginOfflineReset(ctx, "ada", "new recovery password"); !isCode(err, errcode.MaintenanceMode) {
		t.Fatalf("released maintenance reset: %v", err)
	}
}

func TestRestoreAdministratorProofDoesNotRelyOnTimestamps(t *testing.T) {
	f := newFixture(t)
	ctx, _, _ := restoreContext(t, f)
	reset, err := f.svc.BeginOfflineReset(ctx, "ada", "password before invalidation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reset.Confirm(ctx, f.now(reset.Enrollment.raw), "reset before restore invalidation"); err != nil {
		t.Fatal(err)
	}
	run := ids.New()
	if err = f.svc.InvalidateForRestore(ctx, run, 2); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.svc.VerifyRestoreAdministrator(ctx, run, f.admin.ID), errcode.PreconditionFailed)
	reset, err = f.svc.BeginOfflineReset(ctx, "ada", "password after invalidation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reset.Confirm(ctx, f.now(reset.Enrollment.raw), "reset after restore invalidation"); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.VerifyRestoreAdministrator(ctx, run, f.admin.ID); err != nil {
		t.Fatal(err)
	}
	var invalidated, confirmed int64
	if err = f.svc.main.QueryRow(`SELECT invalidated_at FROM identity_restore_runs WHERE run_id=?`, run).Scan(&invalidated); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.main.QueryRow(`SELECT reset_at FROM identity_restore_admin_resets WHERE run_id=?`, run).Scan(&confirmed); err != nil || invalidated != confirmed {
		t.Fatal("fixture must keep an identical timestamp across both actions", err)
	}
}

func TestRestoreAdministratorProofCommitsWithFactors(t *testing.T) {
	f := newFixture(t)
	ctx, _, _ := restoreContext(t, f)
	run := ids.New()
	if err := f.svc.InvalidateForRestore(ctx, run, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.main.Exec(`CREATE TRIGGER fail_reset_proof BEFORE INSERT ON identity_restore_admin_resets BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	reset, err := f.svc.BeginOfflineReset(ctx, "ada", "password after invalidation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reset.Confirm(ctx, f.now(reset.Enrollment.raw), "new factor confirmation interrupted"); err == nil {
		t.Fatal("proof storage failure was hidden")
	}
	if _, err = loadPassword(ctx, f.svc.main, f.admin.ID); err != errNotFound {
		t.Fatalf("password committed without reset proof: %v", err)
	}
	if _, err = loadFactor(ctx, f.svc.main, f.admin.ID, factorEnabled); err != errNotFound {
		t.Fatalf("factor committed without reset proof: %v", err)
	}
	wantCode(t, f.svc.VerifyRestoreAdministrator(ctx, run, f.admin.ID), errcode.PreconditionFailed)
}
