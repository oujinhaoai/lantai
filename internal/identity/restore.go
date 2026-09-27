package identity

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

// InvalidateForRestore closes every authentication path present in a backup.
// The caller first advances the instance recovery epoch and keeps the same
// exclusive maintenance barrier until the full restore has been reconciled.
// Main is authoritative; runtime cleanup is a separate, retryable transaction.
func (s *Service) InvalidateForRestore(ctx context.Context, runID ids.ID, epoch int64) error {
	if err := s.gate.RequireMaintenance(ctx); err != nil {
		return err
	}
	if !runID.Valid() || epoch < 2 || epoch > 9007199254740991 {
		return fixRequest("a restore run and advanced recovery epoch are required")
	}
	current, err := s.recoveryEpoch(ctx)
	if err != nil {
		return err
	}
	if current != epoch {
		return errcode.New(errcode.PreconditionFailed, "restore epoch is not current")
	}
	ctx, release, err := s.write(ctx, true)
	if err != nil {
		return err
	}
	defer release()
	var existing int64
	err = s.main.QueryRowContext(ctx, `SELECT recovery_epoch FROM identity_restore_runs WHERE run_id = ?`, runID).Scan(&existing)
	if err == nil && existing != epoch {
		return errcode.New(errcode.IdempotencyConflict, "restore run is bound to another epoch")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		var other ids.ID
		err = s.main.QueryRowContext(ctx, `SELECT run_id FROM identity_restore_runs WHERE recovery_epoch = ?`, epoch).Scan(&other)
		if err == nil {
			return errcode.New(errcode.IdempotencyConflict, "restore epoch is bound to another run")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Before disabling encrypted factors, prove that the separately restored
		// key actually opens them. A wrong key must not destroy the evidence.
		if err = s.CheckStartup(ctx); err != nil {
			return err
		}
		err = inTx(ctx, s.main, func(tx *sql.Tx) error {
			now := clock.Millis(s.now())
			_, err := tx.ExecContext(ctx, `UPDATE identity_principals SET auth_epoch = auth_epoch + 1, revision = revision + 1, updated_at = ?`, now)
			if err != nil {
				return err
			}
			statements := []struct {
				query string
				args  []any
			}{
				{`UPDATE identity_credentials SET revoked_at = ?, revoke_reason = 'instance_restored' WHERE revoked_at IS NULL`, []any{now}},
				{`DELETE FROM identity_passwords`, nil},
				{`UPDATE identity_totp_factors SET state = 'disabled', disabled_at = ? WHERE state IN ('enabled','pending')`, []any{now}},
				// Consumed recovery codes can continue an unfinished recovery; they
				// too must be invalidated, not just the unused codes.
				{`UPDATE identity_recovery_codes SET invalidated_at = ? WHERE invalidated_at IS NULL`, []any{now}},
				{`UPDATE identity_setup_codes SET invalidated_at = ? WHERE invalidated_at IS NULL`, []any{now}},
				{`UPDATE identity_human_accounts SET factor_state = 'setup_pending', pending_operation_id = ?, revision = revision + 1, updated_at = ?`, []any{runID, now}},
				{`UPDATE identity_human_grants SET state = 'revoked', revoked_at = ?, revoke_reason = 'instance_restored' WHERE state = 'bound'`, []any{now}},
				{`UPDATE identity_challenges SET state = 'expired' WHERE state IN ('pending','verified')`, nil},
				{`INSERT INTO identity_restore_runs(run_id,recovery_epoch,invalidated_at) VALUES(?,?,?)`, []any{runID, epoch, now}},
			}
			for _, statement := range statements {
				if _, err = tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
					return err
				}
			}
			if _, err = bumpPolicyRevision(ctx, tx); err != nil {
				return err
			}
			// The durable restore run is the audit record for this offline
			// maintenance step. No authenticated principal exists yet; do not
			// fabricate actor_id. The confirmed administrator reset emits its
			// ordinary identity event in its own transaction.
			return s.gate.RequireMaintenance(ctx)
		})
		if err != nil {
			return err
		}
	}
	// Never end a new administrator session when the same run is retried.
	// All sessions from a valid backup have an older recovery_epoch. Even if
	// cleanup fails, the new epoch and main auth_epoch already reject them.
	return inTx(ctx, s.runtime, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE identity_sessions SET ended_at = ?, end_reason = 'instance_restored' WHERE ended_at IS NULL AND recovery_epoch < ?`, clock.Millis(s.now()), epoch)
		if err != nil {
			return err
		}
		return s.gate.RequireMaintenance(ctx)
	})
}

// localResetOnly extends the existing offline reset solely for an offline
// restore that holds this instance's live exclusive maintenance context.
func (s *Service) localResetOnly(ctx context.Context) error {
	if err := s.localOnly(); err == nil {
		return nil
	}
	open, reason := s.gate.State()
	if open || reason != commands.ReasonRecovering {
		return ErrNotLocal
	}
	return s.gate.RequireMaintenance(ctx)
}

// recordRestoreAdministratorReset is called inside the existing offline reset
// transaction, after both the password and newly confirmed factor are saved.
func (s *Service) recordRestoreAdministratorReset(ctx context.Context, tx *sql.Tx, principal ids.ID, epoch int64, factor ids.ID) error {
	recovery, err := s.recoveryEpoch(ctx)
	if err != nil {
		return err
	}
	var runID ids.ID
	err = tx.QueryRowContext(ctx, `SELECT run_id FROM identity_restore_runs WHERE recovery_epoch = ?`, recovery).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Ordinary recover-admin, outside a restore run.
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_restore_admin_resets(run_id,principal_id,auth_epoch,factor_id,reset_at) VALUES(?,?,?,?,?)
		ON CONFLICT(run_id,principal_id) DO UPDATE SET auth_epoch=excluded.auth_epoch,factor_id=excluded.factor_id,reset_at=excluded.reset_at`, runID, principal, epoch, factor, clock.Millis(s.now()))
	return err
}

// VerifyRestoreAdministrator requires a completed offline password/TOTP reset
// after this exact run. Timestamps alone are not treated as proof of ordering.
func (s *Service) VerifyRestoreAdministrator(ctx context.Context, runID, principal ids.ID) error {
	if err := s.gate.RequireMaintenance(ctx); err != nil {
		return err
	}
	if !runID.Valid() || !principal.Valid() {
		return fixRequest("restore run and administrator are required")
	}
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return err
	}
	defer release()
	fail := func() error {
		return errcode.New(errcode.PreconditionFailed, "this restore requires a newly confirmed administrator password and authenticator")
	}
	var recovery, authEpoch int64
	var factorID ids.ID
	err = s.main.QueryRowContext(ctx, `SELECT r.recovery_epoch,a.auth_epoch,a.factor_id FROM identity_restore_runs r
		JOIN identity_restore_admin_resets a ON a.run_id=r.run_id WHERE r.run_id=? AND a.principal_id=?`, runID, principal).Scan(&recovery, &authEpoch, &factorID)
	if errors.Is(err, sql.ErrNoRows) {
		return fail()
	}
	if err != nil {
		return err
	}
	current, err := s.recoveryEpoch(ctx)
	if err != nil {
		return err
	}
	if current != recovery {
		return fail()
	}
	p, err := loadPrincipal(ctx, s.main, principal)
	if errors.Is(err, errNotFound) {
		return fail()
	}
	if err != nil {
		return err
	}
	admin, err := hasSystemRole(ctx, s.main, principal, RoleAdmin)
	if err != nil {
		return err
	}
	if p.Kind != authz.Human || p.State != StateActive || !admin || p.AuthEpoch != authEpoch {
		return fail()
	}
	acct, err := loadAccount(ctx, s.main, principal)
	if err != nil || acct.FactorState != factorEnrolled {
		return fail()
	}
	if _, err = loadPassword(ctx, s.main, principal); err != nil {
		return fail()
	}
	f, err := loadFactor(ctx, s.main, principal, factorEnabled)
	if err != nil || f.ID != factorID {
		return fail()
	}
	secret, err := s.openFactor(f)
	if err != nil {
		return err
	}
	valid := len(secret) == totp.SecretLen
	clear(secret)
	if !valid {
		return fail()
	}
	return s.gate.RequireMaintenance(ctx)
}
