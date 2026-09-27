package identity

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/password"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

// RecoveryRequest 用口令加一个恢复码开始因子恢复（验证器丢失）。
type RecoveryRequest struct {
	Name     string
	Password string
	Code     string
	Source   string
	// Channel 为 browser 或 cli。
	Channel string
}

// StartRecovery 用口令加未使用的恢复码换取绑定恢复操作的受限会话。主库在
// 一个事务中消费恢复码、推进 auth_epoch（旧会话全部失效）、停用旧因子、
// 吊销旧人类授权并进入 reset_pending；之后只能登记并确认新因子，登记完成前
// 不能正常登录。受限会话不能读取资源、提权或执行任何敏感操作。同一恢复
// 操作可以用同一个恢复码（或本批其他未用码）继续；已消费的码不能开启另一个操作。
func (s *Service) StartRecovery(ctx context.Context, req RecoveryRequest) (IssuedSession, error) {
	if req.Channel != ChannelBrowser && req.Channel != ChannelCLI {
		return IssuedSession{}, fixRequest("channel must be browser or cli")
	}
	source := normalizeSource(req.Source)
	p, err := loadPrincipalByName(ctx, s.main, req.Name)
	if err != nil && !errors.Is(err, errNotFound) {
		return IssuedSession{}, err
	}
	// 预记尝试并在任何锁之外校验口令：匿名请求不能借慢哈希长时间占住
	// security_guard 写锁。只有推进代次的短事务持写锁，并在事务内复核。
	seq, err := s.reserveAttempt(ctx, p.ID, source, failRecovery)
	if err != nil {
		return IssuedSession{}, err
	}
	p, rec, ok, err := s.verifyPassword(ctx, req.Name, req.Password)
	if err != nil {
		return IssuedSession{}, err
	}
	if !ok {
		return IssuedSession{}, invalidCredentials()
	}
	ctx, release, err := s.write(ctx, true)
	if err != nil {
		return IssuedSession{}, err
	}
	defer release()
	var op ids.ID
	var epoch int64
	err = inTx(ctx, s.main, func(tx *sql.Tx) error {
		if cur, err := loadPassword(ctx, tx, p.ID); err != nil || string(cur.Hash) != string(rec.Hash) {
			return invalidCredentials()
		}
		cur, err := loadPrincipal(ctx, tx, p.ID)
		if err != nil || cur.State != StateActive {
			return invalidCredentials()
		}
		acct, err := loadAccount(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		if acct.FactorState == factorSetupPending {
			// 管理员重置后旧恢复码已作废，只能用设置码。
			return invalidCredentials()
		}
		codeID, consumedOp, err := s.matchRecoveryCode(ctx, tx, p.ID, req.Code)
		if err != nil {
			return err
		}
		now := clock.Millis(s.now())
		switch {
		case codeID == "":
			return invalidCredentials()
		case acct.FactorState == factorResetPending:
			// 继续进行中的恢复：只接受属于该操作的码或本批未用码。
			if consumedOp != "" && consumedOp != acct.PendingOperation {
				return invalidCredentials()
			}
			op = acct.PendingOperation
			if consumedOp == "" {
				if _, err := tx.ExecContext(ctx, `UPDATE identity_recovery_codes SET consumed_at = ?, consumed_operation_id = ? WHERE code_id = ?`,
					now, op, codeID); err != nil {
					return err
				}
			}
			return dropAttempt(ctx, tx, seq)
		case consumedOp != "":
			// 已消费的码不能开启另一次恢复。
			return invalidCredentials()
		}
		if op, err = s.newID(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_recovery_codes SET consumed_at = ?, consumed_operation_id = ? WHERE code_id = ?`,
			now, op, codeID); err != nil {
			return err
		}
		rev, _, err := bumpAuthEpoch(ctx, tx, p.ID, now)
		if err != nil {
			return err
		}
		if err := s.disableFactors(ctx, tx, p.ID); err != nil {
			return err
		}
		revoked, err := s.revokeGrants(ctx, tx, p.ID, "factor_recovery")
		if err != nil {
			return err
		}
		if err := setAccount(ctx, tx, p.ID, factorResetPending, op, now); err != nil {
			return err
		}
		if _, err := bumpPolicyRevision(ctx, tx); err != nil {
			return err
		}
		ev, err := s.event(evParams{typ: EvRecoveryStarted, aggType: "principal", aggID: p.ID, revision: rev, actor: p.ID,
			operation: op, payload: map[string]any{"principal_id": p.ID, "method": "recovery_code", "revoked_grants": revoked}})
		if err != nil {
			return err
		}
		if err := s.mainStore.AppendEvents(ctx, tx, op, []event.Envelope{ev}); err != nil {
			return err
		}
		return dropAttempt(ctx, tx, seq)
	})
	if err != nil {
		return IssuedSession{}, err
	}
	p, err = loadPrincipal(ctx, s.main, p.ID)
	if err != nil {
		return IssuedSession{}, err
	}
	if epoch, err = s.recoveryEpoch(ctx); err != nil {
		return IssuedSession{}, err
	}
	return s.issueRecovery(ctx, p, op, req.Channel, epoch)
}

func (s *Service) issueRecovery(ctx context.Context, p Principal, op ids.ID, channel string, epoch int64) (IssuedSession, error) {
	exp := clock.Truncate(s.now().Add(s.cfg.RecoverySessionTTL))
	return s.issue(ctx, sessionRow{PrincipalID: p.ID, PrincipalKind: p.Kind, Kind: sessionRecovery, Channel: channel,
		Scopes: []Scope{ScopeRecovery, ScopeSelf}, OperationID: op, AuthEpoch: p.AuthEpoch, RecoveryEpoch: epoch, ExpiresAt: exp}, nil)
}

// matchRecoveryCode 以常数时间逐个比较未作废的恢复码；返回命中的码与其已
// 绑定的恢复操作（未消费为空）。
func (s *Service) matchRecoveryCode(ctx context.Context, tx *sql.Tx, principal ids.ID, input string) (codeID, consumedOp ids.ID, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT code_id, salt, hash, consumed_operation_id FROM identity_recovery_codes
		WHERE principal_id = ? AND invalidated_at IS NULL`, principal)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var id ids.ID
		var salt, hash []byte
		var op sql.NullString
		if err := rows.Scan(&id, &salt, &hash, &op); err != nil {
			return "", "", err
		}
		if codeMatches(input, salt, hash) && codeID == "" {
			codeID, consumedOp = id, ids.ID(op.String)
		}
	}
	return codeID, consumedOp, rows.Err()
}

// SetupRequest 用管理员发放的一次性设置码开始设置（新成员，或被管理员重置）。
type SetupRequest struct {
	Name    string
	Code    string
	Source  string
	Channel string
}

// StartSetup 用设置码换取受限设置会话；设置完成前可在有效期内重复使用同一
// 设置码继续，完成后作废。
func (s *Service) StartSetup(ctx context.Context, req SetupRequest) (IssuedSession, error) {
	if req.Channel != ChannelBrowser && req.Channel != ChannelCLI {
		return IssuedSession{}, fixRequest("channel must be browser or cli")
	}
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return IssuedSession{}, err
	}
	defer release()
	source := normalizeSource(req.Source)
	p, err := loadPrincipalByName(ctx, s.main, req.Name)
	if err != nil && !errors.Is(err, errNotFound) {
		return IssuedSession{}, err
	}
	var op ids.ID
	err = inTxKeep(ctx, s.main, func(tx *sql.Tx) error {
		// 限速检查与失败记录在同一个串行化事务中，并发尝试也逐次计数。
		if err := s.checkRate(ctx, tx, p.ID, source, bucketSignIn); err != nil {
			return err
		}
		fail := func() error {
			if err := s.recordFailure(ctx, tx, p.ID, source, failSetup); err != nil {
				return err
			}
			return errKeep{invalidCredentials()}
		}
		if p.ID == "" || p.Kind != authz.Human || p.State != StateActive {
			return fail()
		}
		acct, err := loadAccount(ctx, tx, p.ID)
		if err != nil || acct.FactorState != factorSetupPending {
			return fail()
		}
		rows, err := tx.QueryContext(ctx, `SELECT salt, hash, expires_at FROM identity_setup_codes WHERE principal_id = ?
			AND operation_id = ? AND invalidated_at IS NULL AND consumed_at IS NULL`, p.ID, acct.PendingOperation)
		if err != nil {
			return err
		}
		matched := false
		now := clock.Millis(s.now())
		for rows.Next() {
			var salt, hash []byte
			var exp int64
			if err := rows.Scan(&salt, &hash, &exp); err != nil {
				rows.Close()
				return err
			}
			if codeMatches(req.Code, salt, hash) && now < exp {
				matched = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !matched {
			return fail()
		}
		op = acct.PendingOperation
		return nil
	})
	if err != nil {
		return IssuedSession{}, err
	}
	epoch, err := s.recoveryEpoch(ctx)
	if err != nil {
		return IssuedSession{}, err
	}
	return s.issueRecovery(ctx, p, op, req.Channel, epoch)
}

// EnrollFactor 在受限会话中生成新的待确认 TOTP 因子；再次调用替换未确认的因子。
func (s *Service) EnrollFactor(ctx context.Context, who authz.Context) (Enrollment, error) {
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return Enrollment{}, err
	}
	defer release()
	v, err := s.current(ctx, who)
	if err != nil {
		return Enrollment{}, err
	}
	if err := s.require(ctx, s.main, v, ActRecoveryEnroll, authz.Resource{}); err != nil {
		return Enrollment{}, err
	}
	secret, err := totp.NewSecret(nil)
	if err != nil {
		return Enrollment{}, err
	}
	err = inTx(ctx, s.main, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE identity_totp_factors SET state = 'disabled', disabled_at = ?
			WHERE principal_id = ? AND state = 'pending'`, clock.Millis(s.now()), v.principal.ID); err != nil {
			return err
		}
		_, err := s.insertFactor(ctx, tx, v.principal.ID, secret, factorPending, -1)
		return err
	})
	if err != nil {
		return Enrollment{}, err
	}
	return s.enrollment(v.principal.Name, secret), nil
}

// SetPassword 在受限会话中设置新口令。管理员发放的设置流程必须先设置口令。
func (s *Service) SetPassword(ctx context.Context, who authz.Context, pw string) error {
	if _, err := s.current(ctx, who); err != nil {
		return err
	}
	// 慢哈希在写入口之外计算；写入前在锁内重新核对会话与权限。
	rec, err := s.hasher.Hash(ctx, pw)
	if err != nil {
		if errors.Is(err, password.ErrPolicy) {
			return fixRequest("%v", err)
		}
		return err
	}
	ctx, release, err := s.write(ctx, false)
	if err != nil {
		return err
	}
	defer release()
	v, err := s.current(ctx, who)
	if err != nil {
		return err
	}
	if err := s.require(ctx, s.main, v, ActRecoverySetPassword, authz.Resource{}); err != nil {
		return err
	}
	return inTx(ctx, s.main, func(tx *sql.Tx) error {
		return setPassword(ctx, tx, v.principal.ID, rec, clock.Millis(s.now()))
	})
}

// ConfirmFactor 用新验证器的当前动态码确认登记，完成恢复或设置：启用新因子、
// 作废旧恢复码并生成新的一批（只展示一次）、作废设置码、回到 enrolled，并
// 结束受限会话。之后需要用口令与新动态码重新正常登录。
func (s *Service) ConfirmFactor(ctx context.Context, who authz.Context, code string) ([]string, error) {
	ctx, release, err := s.write(ctx, true)
	if err != nil {
		return nil, err
	}
	defer release()
	v, err := s.current(ctx, who)
	if err != nil {
		return nil, err
	}
	if err := s.require(ctx, s.main, v, ActRecoveryConfirm, authz.Resource{}); err != nil {
		return nil, err
	}
	var codes []string
	err = inTxKeep(ctx, s.main, func(tx *sql.Tx) error {
		acct, err := loadAccount(ctx, tx, v.principal.ID)
		if err != nil {
			return err
		}
		if acct.FactorState == factorEnrolled || acct.PendingOperation != v.sess.OperationID {
			return sessionErr(errcode.TokenRevoked)
		}
		if _, err := loadPassword(ctx, tx, v.principal.ID); errors.Is(err, errNotFound) {
			return errcode.New(errcode.InvalidStateTransition, "set a password before confirming the authenticator")
		} else if err != nil {
			return err
		}
		f, err := loadFactor(ctx, tx, v.principal.ID, factorPending)
		if errors.Is(err, errNotFound) {
			return errcode.New(errcode.InvalidStateTransition, "enroll an authenticator first")
		}
		if err != nil {
			return err
		}
		if err := s.checkRate(ctx, tx, v.principal.ID, "", bucketProof); err != nil {
			return err
		}
		secret, err := s.openFactor(f)
		if err != nil {
			return err
		}
		res, counter := totp.Verify(secret, code, s.now(), -1)
		if res != totp.Accepted {
			if err := s.recordFailure(ctx, tx, v.principal.ID, "", failTOTP); err != nil {
				return err
			}
			return errKeep{errcode.New(errcode.HumanProofRequired, "the code does not match the new authenticator")}
		}
		now := clock.Millis(s.now())
		if err := s.disableFactors(ctx, tx, v.principal.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_totp_factors SET state = 'enabled', enabled_at = ?, disabled_at = NULL,
			last_accepted_counter = ? WHERE factor_id = ?`, now, counter, f.ID); err != nil {
			return err
		}
		if codes, err = s.issueRecoveryCodes(ctx, tx, v.principal.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_setup_codes SET consumed_at = ? WHERE principal_id = ? AND operation_id = ?
			AND consumed_at IS NULL AND invalidated_at IS NULL`, now, v.principal.ID, acct.PendingOperation); err != nil {
			return err
		}
		if err := setAccount(ctx, tx, v.principal.ID, factorEnrolled, "", now); err != nil {
			return err
		}
		var rev int64
		if err := tx.QueryRowContext(ctx, `UPDATE identity_principals SET revision = revision + 1, updated_at = ? WHERE principal_id = ?
			RETURNING revision`, now, v.principal.ID).Scan(&rev); err != nil {
			return err
		}
		ev, err := s.event(evParams{typ: EvFactorEnrolled, aggType: "principal", aggID: v.principal.ID, revision: rev,
			actor: v.principal.ID, session: v.sess.ID, operation: acct.PendingOperation,
			payload: map[string]any{"principal_id": v.principal.ID, "operation_id": acct.PendingOperation}})
		if err != nil {
			return err
		}
		return s.mainStore.AppendEvents(ctx, tx, acct.PendingOperation, []event.Envelope{ev})
	})
	if err != nil {
		return nil, err
	}
	// 主库已经完成恢复，enrolled 状态使该操作的全部受限会话立即无效。
	// runtime 的结束标记失败不能丢弃只在此响应交付的新恢复码；遗留会话
	// 由 ReconcileRecoverySessions 依据持久的账户状态安全重试清理。
	_ = s.endSession(ctx, v.sess, "factor_enrolled", v.principal.ID, v.sess.ID)
	return codes, nil
}

// ReconcileRecoverySessions 补记已完成或被替换恢复操作的会话结束状态。
// 它是可重复调用的内部维护入口：主库账户状态已经使这些会话失效，runtime
// 清理失败不影响该安全事实；下次调用从两库各自的持久记录继续，不跨库联表。
// 本方法只关闭不再对应当前待完成恢复操作的会话，保留新恢复操作的会话。
func (s *Service) ReconcileRecoverySessions(ctx context.Context) (int, error) {
	ctx, release, err := s.write(ctx, true)
	if err != nil {
		return 0, err
	}
	defer release()
	rows, err := s.runtime.QueryContext(ctx, `SELECT session_id FROM identity_sessions
		WHERE session_kind = 'recovery' AND ended_at IS NULL ORDER BY session_id`)
	if err != nil {
		return 0, err
	}
	var sessions []ids.ID
	for rows.Next() {
		var id ids.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		sessions = append(sessions, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	closed := 0
	for _, id := range sessions {
		sess, err := loadSession(ctx, s.runtime, id)
		if err != nil {
			return closed, err
		}
		acct, err := loadAccount(ctx, s.main, sess.PrincipalID)
		if err != nil {
			return closed, err
		}
		if acct.FactorState != factorEnrolled && acct.PendingOperation == sess.OperationID {
			continue
		}
		if err := s.endSession(ctx, sess, "recovery_operation_closed", sess.PrincipalID, sess.ID); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// BeginOfflineReset 为指定的人（必须是管理员）准备离线重置：新口令与新 TOTP
// 种子只在内存中，确认前不写任何数据。
func (s *Service) BeginOfflineReset(ctx context.Context, name, newPassword string) (*OfflineResetSession, error) {
	if err := s.localOnly(); err != nil {
		return nil, err
	}
	p, err := loadPrincipalByName(ctx, s.main, name)
	if errors.Is(err, errNotFound) || (err == nil && p.Kind != authz.Human) {
		return nil, errcode.Newf(errcode.NotFound, "no human principal named %q", name)
	}
	if err != nil {
		return nil, err
	}
	admin, err := hasSystemRole(ctx, s.main, p.ID, RoleAdmin)
	if err != nil {
		return nil, err
	}
	if !admin {
		return nil, errcode.New(errcode.Forbidden, "offline reset is only for administrators; other people are reset by an administrator")
	}
	rec, err := s.hasher.Hash(ctx, newPassword)
	if err != nil {
		return nil, fixRequest("%v", err)
	}
	secret, err := totp.NewSecret(nil)
	if err != nil {
		return nil, err
	}
	return &OfflineResetSession{s: s, principal: p, rec: rec, Enrollment: s.enrollment(p.Name, secret)}, nil
}

// OfflineResetSession 是单管理员场景的本机灾难恢复（恢复码也丢失时）：只能
// 在持有数据根锁、未对外开放写入的本机进程中进行，由已控制数据根与主密钥
// 的主机管理员明确确认后执行。它不是远程自助提权接口。
type OfflineResetSession struct {
	s          *Service
	principal  Principal
	rec        password.Record
	Enrollment Enrollment
}

// Principal 返回被重置的主体。
func (o *OfflineResetSession) Principal() Principal { return o.principal }

// Confirm 用新验证器的当前动态码确认，然后在主库一个事务中：推进 auth_epoch
// （旧会话与委托全部失效）、吊销人类授权、停用旧因子、作废旧恢复码与设置码、
// 写入新口令与新因子、生成新恢复码，并写审计事件。
func (o *OfflineResetSession) Confirm(ctx context.Context, code, note string) ([]string, error) {
	s := o.s
	if err := s.localOnly(); err != nil {
		return nil, err
	}
	res, counter := totp.Verify(o.Enrollment.raw, code, s.now(), -1)
	if res != totp.Accepted {
		return nil, errcode.New(errcode.HumanProofRequired, "the code does not match the new authenticator")
	}
	if !shortTextRE.MatchString(note) {
		return nil, fixRequest("note must be short plain text")
	}
	var codes []string
	err := inTx(ctx, s.main, func(tx *sql.Tx) error {
		p, err := loadPrincipal(ctx, tx, o.principal.ID)
		if err != nil {
			return err
		}
		if p.AuthEpoch != o.principal.AuthEpoch {
			return errcode.New(errcode.PreconditionFailed, "the principal changed while the reset was being prepared; start again")
		}
		now := clock.Millis(s.now())
		rev, _, err := bumpAuthEpoch(ctx, tx, p.ID, now)
		if err != nil {
			return err
		}
		revoked, err := s.revokeGrants(ctx, tx, p.ID, "offline_recovery")
		if err != nil {
			return err
		}
		if err := s.disableFactors(ctx, tx, p.ID); err != nil {
			return err
		}
		if err := s.invalidateSetupCodes(ctx, tx, p.ID); err != nil {
			return err
		}
		if err := setPassword(ctx, tx, p.ID, o.rec, now); err != nil {
			return err
		}
		if _, err := s.insertFactor(ctx, tx, p.ID, o.Enrollment.raw, factorEnabled, counter); err != nil {
			return err
		}
		if codes, err = s.issueRecoveryCodes(ctx, tx, p.ID); err != nil {
			return err
		}
		if err := setAccount(ctx, tx, p.ID, factorEnrolled, "", now); err != nil {
			return err
		}
		if _, err := bumpPolicyRevision(ctx, tx); err != nil {
			return err
		}
		op, err := s.newID()
		if err != nil {
			return err
		}
		ev, err := s.event(evParams{typ: EvOfflineRecovery, aggType: "principal", aggID: p.ID, revision: rev, actor: p.ID,
			operation: op, payload: map[string]any{"principal_id": p.ID, "revoked_grants": revoked, "note": note}})
		if err != nil {
			return err
		}
		return s.mainStore.AppendEvents(ctx, tx, op, []event.Envelope{ev})
	})
	return codes, err
}
