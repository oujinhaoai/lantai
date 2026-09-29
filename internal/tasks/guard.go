package tasks

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

var _ ledger.CheckoutGuard = (*Service)(nil)

// CheckVersionWrite 是台账写版本时的签出与租约守卫（Prepare 与最终 Commit
// 各调用一次；asset 为空表示绑定任务的新建资产）。调用者持 security_guard
// 与项目锁；本方法只读 runtime。未绑定任务的提交遇到独占签出即拒绝；绑定
// 任务的提交还须是当前 Session 的有效 Attempt，旧 fence、到期、换会话或
// 恢复代次变化都返回 LEASE_STALE。
func (s *Service) CheckVersionWrite(ctx context.Context, who authz.Context, cmd commands.Context, project, asset ids.ID) error {
	var holder ids.ID
	exclusive := false
	if asset != "" {
		err := s.d.DB.QueryRowContext(ctx, `SELECT task_id FROM tasks_checkouts WHERE asset_id=? AND state='active' AND mode='exclusive'`, asset).Scan(&holder)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		exclusive = err == nil
	}
	if cmd.TaskID == "" {
		if cmd.AttemptID != "" {
			return stale("task_binding_incomplete")
		}
		if exclusive {
			return errcode.New(errcode.AssetCheckedOut, "")
		}
		return nil
	}
	r, err := loadRow(ctx, s.d.DB, cmd.TaskID)
	if err != nil {
		return err
	}
	if r.task.ProjectID != project {
		return errcode.New(errcode.RefMismatch, "")
	}
	if exclusive && holder != r.task.ID {
		// 候选组例外只属于同一任务；流程身份不能越过其他任务的独占签出。
		return errcode.New(errcode.AssetCheckedOut, "")
	}
	if r.attempt == nil || r.attempt.ID != cmd.AttemptID {
		return stale(execution.ReasonAttemptReplaced)
	}
	if r.attempt.PrincipalID != who.PrincipalID || r.attempt.SessionID != who.SessionID || cmd.SessionID != r.attempt.SessionID {
		return stale("session_mismatch")
	}
	if r.task.State != "claimed" && r.task.State != "blocked" {
		return stale(execution.ReasonAttemptEnded)
	}
	epoch, err := s.epoch(ctx)
	if err != nil {
		return err
	}
	expires, err := clock.Parse(r.attempt.ExpiresAt)
	if err != nil {
		return err
	}
	return execution.CheckFence(execution.Fence{TaskID: cmd.TaskID, AttemptID: cmd.AttemptID, LeaseFence: cmd.LeaseFence, RecoveryEpoch: cmd.RecoveryEpoch},
		execution.LeaseState{AttemptID: r.attempt.ID, LeaseFence: r.seat.LastLeaseFence, RecoveryEpoch: epoch, ExpiresAt: expires, Terminated: r.attempt.State != "active" || r.seat.State != "claimed"}, s.now())
}
