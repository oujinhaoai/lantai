package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/password"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

// Enrollment 是一次 TOTP 登记需要当场展示给本人的内容。种子只在这里出现
// 一次，不写日志、事件或回执；本人用验证器扫描或手工输入后，以当场验证码
// 确认才启用。
type Enrollment struct {
	// Secret 是 Base32 种子，供手工输入。
	Secret string
	// URI 是 otpauth:// 登记地址。
	URI string
	raw []byte
}

func (s *Service) enrollment(account string, secret []byte) Enrollment {
	return Enrollment{Secret: totp.EncodeSecret(secret), URI: totp.URI(s.issuer, account, secret), raw: secret}
}

type factorRow struct {
	ID          ids.ID
	PrincipalID ids.ID
	KeyID       ids.ID
	Sealed      []byte
	State       string
	LastCounter int64
}

const (
	factorPending  = "pending"
	factorEnabled  = "enabled"
	factorDisabled = "disabled"
)

func loadFactor(ctx context.Context, q commands.DBTX, principal ids.ID, state string) (factorRow, error) {
	var f factorRow
	err := q.QueryRowContext(ctx, `SELECT factor_id, principal_id, key_id, sealed_secret, state, last_accepted_counter
		FROM identity_totp_factors WHERE principal_id = ? AND state = ?`, principal, state).
		Scan(&f.ID, &f.PrincipalID, &f.KeyID, &f.Sealed, &f.State, &f.LastCounter)
	if errors.Is(err, sql.ErrNoRows) {
		return f, errNotFound
	}
	return f, err
}

func factorAAD(factorID, principal ids.ID) []byte {
	return []byte("lantai.totp-secret/v1\x00" + string(factorID) + "\x00" + string(principal))
}

func (s *Service) openFactor(f factorRow) ([]byte, error) {
	if f.KeyID != s.key.ID() {
		return nil, fmt.Errorf("identity: factor %s was sealed with key %s, the loaded master key is %s", f.ID, f.KeyID, s.key.ID())
	}
	return s.key.Open(f.Sealed, factorAAD(f.ID, f.PrincipalID))
}

// checkFactorTx 在调用方事务中用已启用因子校验动态码；成功时推进
// last_accepted_counter，使同一时间步不能再成功一次（包括用于别的挑战）。
func (s *Service) checkFactorTx(ctx context.Context, tx *sql.Tx, principal ids.ID, code string) (totp.Result, error) {
	f, err := loadFactor(ctx, tx, principal, factorEnabled)
	if errors.Is(err, errNotFound) {
		return totp.Invalid, nil
	}
	if err != nil {
		return totp.Invalid, err
	}
	secret, err := s.openFactor(f)
	if err != nil {
		return totp.Invalid, err
	}
	res, counter := totp.Verify(secret, code, s.now(), f.LastCounter)
	if res != totp.Accepted {
		return res, nil
	}
	out, err := tx.ExecContext(ctx, `UPDATE identity_totp_factors SET last_accepted_counter = ?
		WHERE factor_id = ? AND last_accepted_counter = ?`, counter, f.ID, f.LastCounter)
	if err != nil {
		return totp.Invalid, err
	}
	if n, _ := out.RowsAffected(); n != 1 {
		return totp.Replayed, nil
	}
	return totp.Accepted, nil
}

// insertFactor 保存一个新因子（加密种子）。
func (s *Service) insertFactor(ctx context.Context, tx *sql.Tx, principal ids.ID, secret []byte, state string, lastCounter int64) (ids.ID, error) {
	id, err := s.newID()
	if err != nil {
		return "", err
	}
	sealed, err := s.key.Seal(secret, factorAAD(id, principal))
	if err != nil {
		return "", err
	}
	now := clock.Millis(s.now())
	var enabledAt any
	if state == factorEnabled {
		enabledAt = now
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_totp_factors (factor_id, principal_id, key_id, sealed_secret,
		algorithm, digits, period, state, last_accepted_counter, created_at, enabled_at)
		VALUES (?, ?, ?, ?, 'SHA1', 6, 30, ?, ?, ?, ?)`, id, principal, s.key.ID(), sealed, state, lastCounter, now, enabledAt)
	return id, err
}

// disableFactors 停用主体的全部已启用与待确认因子。
func (s *Service) disableFactors(ctx context.Context, tx *sql.Tx, principal ids.ID) error {
	_, err := tx.ExecContext(ctx, `UPDATE identity_totp_factors SET state = 'disabled', disabled_at = ?
		WHERE principal_id = ? AND state IN ('enabled', 'pending')`, clock.Millis(s.now()), principal)
	return err
}

// issueRecoveryCodes 作废旧恢复码并生成新的一批，返回明文（只展示一次）。
func (s *Service) issueRecoveryCodes(ctx context.Context, tx *sql.Tx, principal ids.ID) ([]string, error) {
	if err := s.invalidateRecoveryCodes(ctx, tx, principal); err != nil {
		return nil, err
	}
	batch, err := s.newID()
	if err != nil {
		return nil, err
	}
	now := clock.Millis(s.now())
	codes := make([]string, 0, s.cfg.RecoveryCodes)
	for range s.cfg.RecoveryCodes {
		code, err := newCode()
		if err != nil {
			return nil, err
		}
		salt, hash, err := sealCode(code)
		if err != nil {
			return nil, err
		}
		id, err := s.newID()
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO identity_recovery_codes (code_id, principal_id, batch_id, salt, hash, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, id, principal, batch, salt, hash, now); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

func (s *Service) invalidateRecoveryCodes(ctx context.Context, tx *sql.Tx, principal ids.ID) error {
	_, err := tx.ExecContext(ctx, `UPDATE identity_recovery_codes SET invalidated_at = ?
		WHERE principal_id = ? AND invalidated_at IS NULL AND consumed_at IS NULL`, clock.Millis(s.now()), principal)
	return err
}

func (s *Service) invalidateSetupCodes(ctx context.Context, tx *sql.Tx, principal ids.ID) error {
	_, err := tx.ExecContext(ctx, `UPDATE identity_setup_codes SET invalidated_at = ?
		WHERE principal_id = ? AND invalidated_at IS NULL AND consumed_at IS NULL`, clock.Millis(s.now()), principal)
	return err
}

// issueSetupCode 为新成员或被重置的人生成一次性设置码，绑定设置操作。
func (s *Service) issueSetupCode(ctx context.Context, tx *sql.Tx, principal, op, createdBy ids.ID) (string, error) {
	if err := s.invalidateSetupCodes(ctx, tx, principal); err != nil {
		return "", err
	}
	code, err := newCode()
	if err != nil {
		return "", err
	}
	salt, hash, err := sealCode(code)
	if err != nil {
		return "", err
	}
	id, err := s.newID()
	if err != nil {
		return "", err
	}
	now := s.now()
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_setup_codes (code_id, principal_id, operation_id, salt, hash,
		created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, principal, op, salt, hash, createdBy,
		clock.Millis(now), clock.Millis(now.Add(s.cfg.SetupCodeTTL)))
	return code, err
}

// revokeGrants 吊销某人全部仍绑定的人类授权，并让其待兑换的挑战过期。
func (s *Service) revokeGrants(ctx context.Context, tx *sql.Tx, human ids.ID, reason string) (int64, error) {
	now := clock.Millis(s.now())
	res, err := tx.ExecContext(ctx, `UPDATE identity_human_grants SET state = 'revoked', revoked_at = ?, revoke_reason = ?
		WHERE human_id = ? AND state = 'bound'`, now, reason, human)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `UPDATE identity_challenges SET state = 'expired' WHERE human_id = ? AND state = 'pending'`, human); err != nil {
		return 0, err
	}
	return n, nil
}

// bumpAuthEpoch 推进主体的 auth_epoch：旧会话、旧人类授权与基于旧代次的
// 委托全部失效。返回新的修订与代次。
func bumpAuthEpoch(ctx context.Context, tx *sql.Tx, principal ids.ID, now int64) (rev, epoch int64, err error) {
	err = tx.QueryRowContext(ctx, `UPDATE identity_principals SET auth_epoch = auth_epoch + 1, revision = revision + 1, updated_at = ?
		WHERE principal_id = ? RETURNING revision, auth_epoch`, now, principal).Scan(&rev, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		err = errNotFound
	}
	return rev, epoch, err
}

func setAccount(ctx context.Context, tx *sql.Tx, principal ids.ID, state string, pending ids.ID, now int64) error {
	var p any
	if pending != "" {
		p = pending
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO identity_human_accounts (principal_id, factor_state, pending_operation_id, revision, updated_at)
		VALUES (?, ?, ?, 1, ?) ON CONFLICT (principal_id) DO UPDATE SET factor_state = excluded.factor_state,
		pending_operation_id = excluded.pending_operation_id, revision = identity_human_accounts.revision + 1,
		updated_at = excluded.updated_at`, principal, state, p, now)
	return err
}

func setPassword(ctx context.Context, tx *sql.Tx, principal ids.ID, r password.Record, now int64) error {
	params, err := r.ParamsJSON()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_passwords (principal_id, params, salt, hash, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (principal_id) DO UPDATE SET params = excluded.params, salt = excluded.salt,
		hash = excluded.hash, updated_at = excluded.updated_at`, principal, params, r.Salt, r.Hash, now)
	return err
}

func loadPassword(ctx context.Context, q commands.DBTX, principal ids.ID) (password.Record, error) {
	var params string
	var r password.Record
	err := q.QueryRowContext(ctx, `SELECT params, salt, hash FROM identity_passwords WHERE principal_id = ?`, principal).
		Scan(&params, &r.Salt, &r.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return r, errNotFound
	}
	if err != nil {
		return r, err
	}
	r.Params, err = password.ParseParams(params)
	return r, err
}

// verifyPassword 校验人的口令；主体不存在、不是人、已停用或没有口令时仍做
// 一次同等成本的计算，不能凭响应时间枚举主体名。返回校验通过的主体与
// 当时的口令记录（事务内用它确认口令未被并发修改）。
func (s *Service) verifyPassword(ctx context.Context, name, pw string) (Principal, password.Record, bool, error) {
	p, err := loadPrincipalByName(ctx, s.main, name)
	if err != nil && !errors.Is(err, errNotFound) {
		return p, password.Record{}, false, err
	}
	var rec password.Record
	if err == nil && p.Kind == "human" && p.State == StateActive {
		rec, err = loadPassword(ctx, s.main, p.ID)
		if err != nil && !errors.Is(err, errNotFound) {
			return p, rec, false, err
		}
		if err == nil {
			ok, err := s.hasher.Verify(ctx, pw, rec)
			return p, rec, ok, err
		}
	}
	return p, rec, false, s.hasher.Burn(ctx)
}
