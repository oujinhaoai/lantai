package identity

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity/password"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

// ErrBootstrapClosed 表示实例已有管理员或主库非空，初始化永久关闭。
var ErrBootstrapClosed = errors.New("identity: bootstrap is closed: an administrator already exists or the main database is not empty")

// ErrNotLocal 表示本机专用流程（初始化、离线恢复）在实例对外开放写入时被调用。
var ErrNotLocal = errors.New("identity: this procedure is only available on a local instance that is not serving")

// BootstrapRequest 是首个管理员的登记信息。
type BootstrapRequest struct {
	Name        string
	DisplayName string
	Password    string
}

// Bootstrap 是一次进行中的本机初始化：种子只在内存里，确认前不写任何数据。
type Bootstrap struct {
	s          *Service
	req        BootstrapRequest
	record     password.Record
	Enrollment Enrollment
}

// BootstrapResult 是初始化结果；RecoveryCodes 只展示这一次，主库只存校验值。
type BootstrapResult struct {
	Principal     Principal
	RecoveryCodes []string
}

// localOnly 确认实例从未开放服务：初始化与离线恢复只在持有数据根锁、尚未
// 启动（或正在初始化）的本机进程中执行；运行中实例的维护窗口也不算。
func (s *Service) localOnly() error {
	open, reason := s.gate.State()
	if open || (reason != commands.ReasonInitializing && reason != commands.ReasonStarting) {
		return ErrNotLocal
	}
	return nil
}

func (s *Service) bootstrapOpen(ctx context.Context, q commands.DBTX) error {
	var state string
	if err := q.QueryRowContext(ctx, `SELECT bootstrap_state FROM identity_state WHERE id = 1`).Scan(&state); err != nil {
		return err
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM identity_principals`).Scan(&n); err != nil {
		return err
	}
	if state != "open" || n > 0 {
		return ErrBootstrapClosed
	}
	return nil
}

// BootstrapOpen 报告是否还能初始化首个管理员。
func (s *Service) BootstrapOpen(ctx context.Context) (bool, error) {
	err := s.bootstrapOpen(ctx, s.main)
	if errors.Is(err, ErrBootstrapClosed) {
		return false, nil
	}
	return err == nil, err
}

// BeginBootstrap 开始本机一次性初始化：校验名称与口令、计算口令校验值、生成
// TOTP 种子。只允许在未开放写入的本机实例上、主库没有任何主体时调用。
func (s *Service) BeginBootstrap(ctx context.Context, req BootstrapRequest) (*Bootstrap, error) {
	if err := s.localOnly(); err != nil {
		return nil, err
	}
	if !ValidName(authz.Human, req.Name) {
		return nil, fixRequest("name %q must be lowercase letters, digits, '-' or '_' (at most 32)", req.Name)
	}
	if !shortTextRE.MatchString(req.DisplayName) {
		return nil, fixRequest("display name must be short plain text")
	}
	if err := s.bootstrapOpen(ctx, s.main); err != nil {
		return nil, err
	}
	rec, err := s.hasher.Hash(ctx, req.Password)
	if err != nil {
		return nil, err
	}
	secret, err := totp.NewSecret(nil)
	if err != nil {
		return nil, err
	}
	return &Bootstrap{s: s, req: req, record: rec, Enrollment: s.enrollment(req.Name, secret)}, nil
}

// Confirm 用验证器上的当前动态码确认登记，然后在主库一个事务中建立首个
// 管理员、口令、已启用的因子与恢复码，并永久关闭初始化。动态码不符时
// 什么都不写，可以重试。
func (b *Bootstrap) Confirm(ctx context.Context, code string) (BootstrapResult, error) {
	s := b.s
	if err := s.localOnly(); err != nil {
		return BootstrapResult{}, err
	}
	res, counter := totp.Verify(b.Enrollment.raw, code, s.now(), -1)
	if res != totp.Accepted {
		return BootstrapResult{}, errcode.New(errcode.HumanProofRequired, "the code does not match the new authenticator; check the device clock and try the current code")
	}
	var out BootstrapResult
	err := inTx(ctx, s.main, func(tx *sql.Tx) error {
		if err := s.bootstrapOpen(ctx, tx); err != nil {
			return err
		}
		id, err := s.newID()
		if err != nil {
			return err
		}
		now := s.now()
		ms := clock.Millis(now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO identity_principals (principal_id, kind, name, display_name, state,
			auth_epoch, profile, revision, created_at, created_by, updated_at) VALUES (?, 'human', ?, ?, 'active', 1, '{}', 1, ?, NULL, ?)`,
			id, b.req.Name, b.req.DisplayName, ms, ms); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO identity_system_roles (principal_id, role, granted_at, granted_by) VALUES (?, 'admin', ?, NULL)`, id, ms); err != nil {
			return err
		}
		if err := setAccount(ctx, tx, id, factorEnrolled, "", ms); err != nil {
			return err
		}
		if err := setPassword(ctx, tx, id, b.record, ms); err != nil {
			return err
		}
		if _, err := s.insertFactor(ctx, tx, id, b.Enrollment.raw, factorEnabled, counter); err != nil {
			return err
		}
		codes, err := s.issueRecoveryCodes(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identity_state SET bootstrap_state = 'closed', bootstrap_admin_id = ?,
			bootstrap_closed_at = ?, policy_revision = policy_revision + 1 WHERE id = 1 AND bootstrap_state = 'open'`, id, ms); err != nil {
			return err
		}
		op, err := s.newID()
		if err != nil {
			return err
		}
		ev, err := s.event(evParams{typ: EvBootstrapCompleted, aggType: "principal", aggID: id, revision: 1, actor: id,
			operation: op, payload: map[string]any{"principal_id": id, "name": b.req.Name, "system_roles": []string{"admin"}}})
		if err != nil {
			return err
		}
		if err := s.mainStore.AppendEvents(ctx, tx, op, []event.Envelope{ev}); err != nil {
			return err
		}
		p, err := loadPrincipal(ctx, tx, id)
		if err != nil {
			return err
		}
		out = BootstrapResult{Principal: p, RecoveryCodes: codes}
		return nil
	})
	return out, err
}

// ClosedBy 返回完成初始化的管理员 ID；尚未初始化时为空。
func (s *Service) ClosedBy(ctx context.Context) (ids.ID, error) {
	var admin sql.NullString
	err := s.main.QueryRowContext(ctx, `SELECT bootstrap_admin_id FROM identity_state WHERE id = 1`).Scan(&admin)
	return ids.ID(admin.String), err
}
