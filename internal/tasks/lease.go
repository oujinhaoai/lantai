package tasks

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
)

// AttemptRef 指向执行者持有的当前轮次；fence 与恢复代次由领取回执给出。
type AttemptRef struct {
	ProjectID        ids.ID          `json:"project_id"`
	TaskID           ids.ID          `json:"task_id"`
	AttemptID        ids.ID          `json:"attempt_id"`
	ExpectedRevision int64           `json:"expected_revision"`
	Fence            execution.Fence `json:"fence"`
}

func (r AttemptRef) valid() error {
	if !r.ProjectID.Valid() || !r.TaskID.Valid() || !r.AttemptID.Valid() || r.ExpectedRevision < 1 || r.Fence.AttemptID != r.AttemptID || r.Fence.TaskID != "" && r.Fence.TaskID != r.TaskID || r.Fence.LeaseFence < 1 || r.Fence.RecoveryEpoch < 1 {
		return invalid("current attempt, revision and lease fence required")
	}
	return nil
}

type ClaimRequest struct {
	ProjectID        ids.ID `json:"project_id"`
	TaskID           ids.ID `json:"task_id"`
	SeatID           ids.ID `json:"seat_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type ReleaseRequest struct {
	AttemptRef
	Reason string `json:"reason"`
	// Stopped 是执行会话本身确认已停止；UnresolvedEffects>0 时仍进入待对账。
	Stopped           bool `json:"stopped"`
	UnresolvedEffects int  `json:"unresolved_effects"`
}

type SubmitRequest struct {
	AttemptRef
	OutputRefs []ids.PermanentRef `json:"output_refs"`
}

type BlockRequest struct {
	AttemptRef
	QuestionMessageID ids.ID `json:"question_message_id"`
	Stopped           bool   `json:"stopped"`
}

type HandoffRequest struct {
	AttemptRef
	MessageID ids.ID             `json:"message_id"`
	DraftRefs []ids.PermanentRef `json:"draft_refs"`
}

// Claim 原子领取席位：新 Attempt、严格递增的 fence 与签出在同一事务提交。
func (s *Service) Claim(ctx context.Context, who authz.Context, key string, in ClaimRequest) (Result, error) {
	if !in.ProjectID.Valid() || !in.TaskID.Valid() || !in.SeatID.Valid() || in.ExpectedRevision < 1 {
		return Result{}, invalid("task, seat and expected seat revision required")
	}
	ctx, release, err := s.lock(ctx, in.ProjectID, in.TaskID)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err = s.authorize(ctx, who, "tasks.read", in.ProjectID, "task", in.TaskID); err != nil {
		return Result{}, err
	}
	cmd, err := s.command(ctx, who, key, "tasks.claim", in.ProjectID, in)
	if err != nil {
		return Result{}, err
	}
	if r, err := s.replay(ctx, cmd); err != nil || r != nil {
		return deref(r), err
	}
	if err = s.authorize(ctx, who, "tasks.claim", in.ProjectID, "task", in.TaskID); err != nil {
		return Result{}, err
	}
	r, err := loadRow(ctx, s.d.DB, in.TaskID)
	if err != nil {
		return Result{}, err
	}
	if r.task.ProjectID != in.ProjectID || r.seat.ID != in.SeatID {
		return Result{}, errcode.New(errcode.NotFound, "")
	}
	if err = s.eligible(ctx, who, r); err != nil {
		return Result{}, err
	}
	lease, _, err := s.policies(ctx, in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	epoch, err := s.epoch(ctx)
	if err != nil {
		return Result{}, err
	}
	specs, err := s.claimCheckouts(ctx, r)
	if err != nil {
		return Result{}, err
	}
	attemptID, err := s.d.IDs.New()
	if err != nil {
		return Result{}, err
	}
	return s.execute(ctx, cmd, func(ctx context.Context, tx *sql.Tx) (Result, []event.Envelope, error) {
		r, err := loadRow(ctx, tx, in.TaskID)
		if err != nil {
			return Result{}, nil, err
		}
		// 没有执行轮次需要对账的空闲席位可以采用当前恢复代次。
		if r.seat.State == "open" && r.seat.RecoveryEpoch != epoch && (r.attempt == nil || r.attempt.State != "active" && r.attempt.Reconciliation.TerminationConfirmed && r.attempt.Reconciliation.UnresolvedEffects == 0) {
			r.seat.RecoveryEpoch = epoch
		}
		// 竞争领取的后到者看到的是稳定冲突码，而不是修订号变化。
		if r.seat.State == "claimed" || r.attempt != nil && r.attempt.State == "active" {
			return Result{}, nil, errcode.New(errcode.TaskAlreadyClaimed, "")
		}
		if err := tc.CheckClaim(r.task, r.seat, r.attempt, in.ExpectedRevision, epoch); err != nil {
			return Result{}, nil, err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks_attempts WHERE principal_id=? AND state='active'`, who.PrincipalID).Scan(&active); err != nil {
			return Result{}, nil, err
		}
		if active >= s.d.MaxActive {
			return Result{}, nil, errcode.New(errcode.QuotaExceeded, "").WithDetails(errcode.Detail{Reason: "active_task_limit"})
		}
		now := s.now()
		a := Attempt{Contract: "lantai.attempt/v1", ID: attemptID, TaskID: r.task.ID, SeatID: r.seat.ID, Revision: 1, OperationID: cmd.OperationID, PrincipalID: who.PrincipalID, SessionID: who.SessionID, State: "active",
			Fence: execution.Fence{TaskID: r.task.ID, AttemptID: attemptID, LeaseFence: r.seat.LastLeaseFence + 1, RecoveryEpoch: epoch}, IssuedAt: clock.Format(now), ExpiresAt: clock.Format(now.Add(lease)),
			Reconciliation: tc.Reconciliation{EvidenceRefs: []ids.ID{}}}
		if err := tc.CheckNewAttempt(r.seat, a); err != nil {
			return Result{}, nil, err
		}
		if err := s.acquire(ctx, tx, r.task, a, specs); err != nil {
			return Result{}, nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks_attempts(attempt_id,task_id,seat_id,round,revision,state,principal_id,session_id,lease_fence,expires_at,record) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			a.ID, a.TaskID, a.SeatID, r.meta.Round, a.Revision, a.State, a.PrincipalID, a.SessionID, a.Fence.LeaseFence, clock.Millis(now.Add(lease)), encode(a)); err != nil {
			return Result{}, nil, err
		}
		if !tc.SeatTransition(r.seat.State, "claimed") {
			return Result{}, nil, stateErr("seat_not_open")
		}
		r.seat.State, r.seat.CurrentAttemptID, r.seat.LastLeaseFence = "claimed", a.ID, a.Fence.LeaseFence
		if err := s.saveSeat(ctx, tx, &r.seat); err != nil {
			return Result{}, nil, err
		}
		changed := r.task.State != "claimed"
		r.task.State = "claimed"
		if err := s.saveTask(ctx, tx, &r.task, &r.meta, changed); err != nil {
			return Result{}, nil, err
		}
		e, err := s.event(cmd, "task.claimed", r.task, map[string]any{"attempt_id": a.ID, "principal_id": a.PrincipalID, "round": r.meta.Round, "lease_fence": a.Fence.LeaseFence})
		if err != nil {
			return Result{}, nil, err
		}
		return resultOf(r.task, r.meta, &a), []event.Envelope{e}, nil
	})
}

// eligible 核对指派或角色池、类型角色与独立性；授权动作另行检查。
func (s *Service) eligible(ctx context.Context, who authz.Context, r row) error {
	roles, ok := claimRoles[r.task.Type]
	if !ok {
		return stateErr("task_type_not_claimable")
	}
	have, err := s.memberRoles(ctx, r.task.ProjectID, who.PrincipalID)
	if err != nil {
		return err
	}
	if !hasAnyRole(have, roles...) {
		return errcode.New(errcode.Forbidden, "").WithDetails(errcode.Detail{Reason: "role_not_eligible"})
	}
	if r.meta.AssigneeID != "" && r.meta.AssigneeID != who.PrincipalID {
		return errcode.New(errcode.Forbidden, "").WithDetails(errcode.Detail{Reason: "assigned_to_other"})
	}
	if r.meta.Role != "" && !hasAnyRole(have, r.meta.Role) {
		return errcode.New(errcode.Forbidden, "").WithDetails(errcode.Detail{Reason: "role_pool_mismatch"})
	}
	if slices.Contains(r.meta.DistinctFrom, who.PrincipalID) {
		return errcode.New(errcode.SelfReviewForbidden, "").WithDetails(errcode.Detail{Reason: "distinct_principal_required"})
	}
	return nil
}

type checkoutPlan struct {
	CheckoutSpec
	base ids.ID
}

// claimCheckouts 在锁内、事务外读取 T03 的当前锁定与最新版本作为签出基线。
func (s *Service) claimCheckouts(ctx context.Context, r row) ([]checkoutPlan, error) {
	out := make([]checkoutPlan, 0, len(r.meta.Checkouts))
	for _, c := range r.meta.Checkouts {
		if err := s.d.Resources.CheckAssetWrite(ctx, c.AssetID); err != nil {
			return nil, err
		}
		v, err := s.d.Resources.LatestVersion(ctx, c.AssetID)
		if err != nil && errcode.CodeOf(err) != errcode.NotFound {
			return nil, err
		}
		out = append(out, checkoutPlan{CheckoutSpec: c, base: v.VersionID})
	}
	return out, nil
}

func (s *Service) acquire(ctx context.Context, tx *sql.Tx, t Task, a Attempt, plans []checkoutPlan) error {
	for _, p := range plans {
		var holder ids.ID
		err := tx.QueryRowContext(ctx, `SELECT task_id FROM tasks_checkouts WHERE asset_id=? AND state='active' AND mode='exclusive' AND task_id<>?`, p.AssetID, t.ID).Scan(&holder)
		if err == nil {
			return errcode.New(errcode.AssetCheckedOut, "").WithDetails(errcode.Detail{Reason: "exclusive_checkout_held"})
		}
		if err != sql.ErrNoRows {
			return err
		}
		if p.Mode == "exclusive" {
			var n int
			// 独占签出不能越过其他任务已持有的提示签出基线；它们各自在提交时复验。
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks_checkouts WHERE asset_id=? AND state='active' AND task_id<>?`, p.AssetID, t.ID).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return errcode.New(errcode.AssetCheckedOut, "").WithDetails(errcode.Detail{Reason: "checkout_held"})
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks_checkouts(task_id,asset_id,project_id,mode,attempt_id,base_version_id,state,acquired_at) VALUES(?,?,?,?,?,?,'active',?)
			ON CONFLICT(task_id,asset_id) DO UPDATE SET mode=excluded.mode,attempt_id=excluded.attempt_id,base_version_id=excluded.base_version_id,state='active',acquired_at=excluded.acquired_at,released_at=NULL`,
			t.ID, p.AssetID, t.ProjectID, p.Mode, a.ID, p.base, clock.Millis(s.now())); err != nil {
			return err
		}
	}
	return nil
}

// accept 在锁内、事务中按契约守卫核对当前 Session、Attempt、fence 与恢复代次。
func (s *Service) accept(who authz.Context, op ids.ID, typ string, in AttemptRef, r row, outputs []ids.PermanentRef, epoch int64) error {
	if r.task.ProjectID != in.ProjectID || r.attempt == nil || r.attempt.ID != in.AttemptID {
		return stale(execution.ReasonAttemptReplaced)
	}
	if r.attempt.PrincipalID != who.PrincipalID {
		return stale("principal_mismatch")
	}
	// 已失效的轮次（到期回收、答复、取消或已交付）一律按旧租约拒绝。
	if r.attempt.State != "active" || r.seat.State != "claimed" {
		return stale(execution.ReasonAttemptEnded)
	}
	fence := in.Fence
	c := tc.Command{Contract: "lantai.task-command/v1", OperationID: op, Type: typ, ProjectID: in.ProjectID, TargetID: in.AttemptID, ExpectedRevision: in.ExpectedRevision, RecoveryEpoch: fence.RecoveryEpoch, SessionID: who.SessionID, Fence: &fence}
	if typ == "tasks.submit" {
		c.OutputRefs = outputs
		if c.OutputRefs == nil {
			c.OutputRefs = []ids.PermanentRef{}
		}
	}
	h, err := tc.HashCommand(c)
	if err != nil {
		return err
	}
	c.RequestHash = h
	return tc.AcceptCommand(c, r.task, r.seat, *r.attempt, s.now(), epoch)
}

func (s *Service) Renew(ctx context.Context, who authz.Context, key string, in AttemptRef) (Result, error) {
	return s.attemptCommand(ctx, who, key, "tasks.renew", "tasks.renew", in, in, nil, func(ctx context.Context, tx *sql.Tx, r *row, op ids.ID, lease time.Duration) ([]string, error) {
		a := r.attempt
		a.ExpiresAt = clock.Format(s.now().Add(lease))
		return nil, s.saveAttempt(ctx, tx, a)
	})
}

// Release 放弃当前轮次：先失效写入权；只有执行会话确认停止且无未决副作用
// 时才结束 Attempt 并重新开放，否则保留待对账，不立即派出第二份执行。
func (s *Service) Release(ctx context.Context, who authz.Context, key string, in ReleaseRequest) (Result, error) {
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 || in.UnresolvedEffects < 0 {
		return Result{}, invalid("release reason and non-negative unresolved effects required")
	}
	return s.attemptCommand(ctx, who, key, "tasks.release", "tasks.release", in.AttemptRef, in, nil, func(ctx context.Context, tx *sql.Tx, r *row, op ids.ID, _ time.Duration) ([]string, error) {
		// 阻塞中的任务保留阻塞，等待答复后再重新开放。
		events, err := s.stop(ctx, tx, r, in.Stopped && in.UnresolvedEffects == 0, in.UnresolvedEffects, "released", r.task.State != "blocked")
		return append([]string{"task.released"}, events...), err
	})
}

// stop 把当前 Attempt 转入对账；confirmed 时同一事务结束轮次并重新开放席位。
// reopen 为 false 时任务状态保持不变（阻塞中等待答复）。
func (s *Service) stop(ctx context.Context, tx *sql.Tx, r *row, confirmed bool, effects int, terminal tc.AttemptState, reopen bool) ([]string, error) {
	a := r.attempt
	if a.State == "active" {
		a.State = "reconciling"
		a.Reconciliation.UnresolvedEffects = effects
		if err := s.saveAttempt(ctx, tx, a); err != nil {
			return nil, err
		}
	}
	if r.seat.State == "claimed" {
		r.seat.State = "reconciling"
		if err := s.saveSeat(ctx, tx, &r.seat); err != nil {
			return nil, err
		}
	}
	events := []string{}
	if reopen && r.task.State != "reconciling" {
		if !tc.TaskTransition(r.task.State, "reconciling") {
			return nil, stateErr("task_not_active")
		}
		r.task.State = "reconciling"
		events = append(events, "task.reconciling")
	}
	if confirmed {
		a.Reconciliation.TerminationConfirmed = true
		a.Reconciliation.UnresolvedEffects = 0
		if err := tc.SafeToReclaim(a.Reconciliation); err != nil {
			return nil, err
		}
		if !tc.AttemptTransition(a.State, terminal) {
			return nil, stateErr("attempt_not_reconciling")
		}
		a.State = terminal
		if err := s.saveAttempt(ctx, tx, a); err != nil {
			return nil, err
		}
		next := "open"
		if r.meta.CancelRequested {
			next = "cancelled"
		}
		r.seat.State = tc.SeatState(next)
		if err := s.saveSeat(ctx, tx, &r.seat); err != nil {
			return nil, err
		}
		if r.task.State == "reconciling" {
			if r.meta.CancelRequested {
				r.task.State = "cancelled"
				events = append(events, "task.cancelled")
			} else {
				r.task.State = "todo"
				events = append(events, "task.reopened")
			}
			if err := s.releaseCheckouts(ctx, tx, r.task.ID); err != nil {
				return nil, err
			}
		}
	}
	return events, nil
}

// Submit 固定当前轮次的输出版本；提交不等于验收或审定。
func (s *Service) Submit(ctx context.Context, who authz.Context, key string, in SubmitRequest) (Result, error) {
	if in.OutputRefs == nil {
		in.OutputRefs = []ids.PermanentRef{}
	}
	if len(in.OutputRefs) > 1000 {
		return Result{}, invalid("too many outputs")
	}
	return s.attemptCommand(ctx, who, key, "tasks.submit", "tasks.submit", in.AttemptRef, in, in.OutputRefs, func(ctx context.Context, tx *sql.Tx, r *row, op ids.ID, _ time.Duration) ([]string, error) {
		if len(r.task.ExpectedOutputs) > 0 && len(in.OutputRefs) == 0 {
			return nil, errcode.New(errcode.ChecksNotSatisfied, "").WithDetails(errcode.Detail{Reason: "expected_output_missing"})
		}
		for _, ref := range in.OutputRefs {
			var holder ids.ID
			err := tx.QueryRowContext(ctx, `SELECT task_id FROM tasks_checkouts WHERE asset_id=? AND state='active' AND mode='exclusive' AND task_id<>?`, ref.AssetID, r.task.ID).Scan(&holder)
			if err == nil {
				return nil, errcode.New(errcode.AssetCheckedOut, "")
			}
			if err != sql.ErrNoRows {
				return nil, err
			}
		}
		a := r.attempt
		a.State = "submitted"
		a.Reconciliation = tc.Reconciliation{TerminationConfirmed: true, UnresolvedEffects: 0, EvidenceRefs: a.Reconciliation.EvidenceRefs}
		if err := s.saveAttempt(ctx, tx, a); err != nil {
			return nil, err
		}
		r.seat.State = "submitted"
		if err := s.saveSeat(ctx, tx, &r.seat); err != nil {
			return nil, err
		}
		r.task.State = "submitted"
		r.task.OutputRefs = in.OutputRefs
		r.meta.SubmitOperationID = op
		if err := putRefs(ctx, tx, r.task.ID, "output", in.OutputRefs); err != nil {
			return nil, err
		}
		return []string{"task.submitted"}, nil
	})
}

// Block 报告阻塞并引用 T03 中已写入的提问；Stopped 时同时结束本轮执行。
func (s *Service) Block(ctx context.Context, who authz.Context, key string, in BlockRequest) (Result, error) {
	if !in.QuestionMessageID.Valid() {
		return Result{}, invalid("question message required")
	}
	return s.attemptCommand(ctx, who, key, "tasks.block", "tasks.release", in.AttemptRef, in, nil, func(ctx context.Context, tx *sql.Tx, r *row, op ids.ID, _ time.Duration) ([]string, error) {
		if r.task.State != "claimed" {
			return nil, stateErr("task_not_claimed")
		}
		if err := s.checkMessage(ctx, r.task, in.QuestionMessageID, "question", who.PrincipalID, ""); err != nil {
			return nil, err
		}
		id, err := s.d.IDs.New()
		if err != nil {
			return nil, err
		}
		b := Block{ID: id, TaskID: r.task.ID, AttemptID: r.attempt.ID, Round: r.meta.Round, State: "open", QuestionMessageID: in.QuestionMessageID, AskedBy: who.PrincipalID, CreatedAt: clock.Format(s.now())}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks_blocks(block_id,task_id,attempt_id,round,state,record) VALUES(?,?,?,?,?,?)`, b.ID, b.TaskID, b.AttemptID, b.Round, b.State, encode(b)); err != nil {
			return nil, err
		}
		r.task.State = "blocked"
		if in.Stopped {
			if _, err := s.stop(ctx, tx, r, true, 0, "released", false); err != nil {
				return nil, err
			}
		}
		return []string{"task.blocked"}, nil
	})
}

// Handoff 以已提交的草稿版本和 T03 交接消息为界结束本轮，任务回到待领。
func (s *Service) Handoff(ctx context.Context, who authz.Context, key string, in HandoffRequest) (Result, error) {
	if !in.MessageID.Valid() || len(in.DraftRefs) > 100 {
		return Result{}, invalid("handoff message and at most 100 drafts required")
	}
	if in.DraftRefs == nil {
		in.DraftRefs = []ids.PermanentRef{}
	}
	return s.attemptCommand(ctx, who, key, "tasks.handoff", "tasks.release", in.AttemptRef, in, in.DraftRefs, func(ctx context.Context, tx *sql.Tx, r *row, op ids.ID, _ time.Duration) ([]string, error) {
		if err := s.checkMessage(ctx, r.task, in.MessageID, "handoff", who.PrincipalID, ""); err != nil {
			return nil, err
		}
		id, err := s.d.IDs.New()
		if err != nil {
			return nil, err
		}
		h := Handoff{ID: id, TaskID: r.task.ID, AttemptID: r.attempt.ID, Round: r.meta.Round, MessageID: in.MessageID, DraftRefs: in.DraftRefs, AuthorID: who.PrincipalID, CreatedAt: clock.Format(s.now())}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks_handoffs(handoff_id,task_id,attempt_id,record) VALUES(?,?,?,?)`, h.ID, h.TaskID, h.AttemptID, encode(h)); err != nil {
			return nil, err
		}
		if err := putRefs(ctx, tx, r.task.ID, "draft", in.DraftRefs); err != nil {
			return nil, err
		}
		events, err := s.stop(ctx, tx, r, true, 0, "released", r.task.State != "blocked")
		return append([]string{"task.handoff"}, events...), err
	})
}

type attemptMutation func(context.Context, *sql.Tx, *row, ids.ID, time.Duration) ([]string, error)

// attemptCommand 是执行者命令的公共路径：锁 → 回执重放 → 授权 → 事务外读取
// 权威 → 事务内契约守卫与状态更新。refs 为本命令需要核对归属的版本。
func (s *Service) attemptCommand(ctx context.Context, who authz.Context, key, typ, contractType string, in AttemptRef, body any, refs []ids.PermanentRef, mutate attemptMutation) (Result, error) {
	if err := in.valid(); err != nil {
		return Result{}, err
	}
	ctx, release, err := s.lock(ctx, in.ProjectID, in.TaskID)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err = s.authorize(ctx, who, "tasks.read", in.ProjectID, "task", in.TaskID); err != nil {
		return Result{}, err
	}
	cmd, err := s.command(ctx, who, key, typ, in.ProjectID, body)
	if err != nil {
		return Result{}, err
	}
	cmd.TaskID, cmd.AttemptID, cmd.LeaseFence = in.TaskID, in.AttemptID, in.Fence.LeaseFence
	if r, err := s.replay(ctx, cmd); err != nil || r != nil {
		return deref(r), err
	}
	if err = s.authorize(ctx, who, "tasks.work", in.ProjectID, "task", in.TaskID); err != nil {
		return Result{}, err
	}
	if typ == "tasks.submit" || typ == "tasks.handoff" {
		if err = s.checkExecution(ctx, in.TaskID, in.AttemptID); err != nil {
			return Result{}, err
		}
	}
	for _, ref := range refs {
		if err = s.readable(ctx, who, ref, in.ProjectID); err != nil {
			return Result{}, err
		}
		v, err := s.d.Resources.VersionByID(ctx, ref.VersionID)
		if err != nil {
			return Result{}, err
		}
		if v.CommittedBy != who.PrincipalID {
			return Result{}, errcode.New(errcode.Forbidden, "").WithDetails(errcode.Detail{Reason: "output_not_committed_by_attempt"})
		}
	}
	lease, _, err := s.policies(ctx, in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	epoch, err := s.epoch(ctx)
	if err != nil {
		return Result{}, err
	}
	return s.execute(ctx, cmd, func(ctx context.Context, tx *sql.Tx) (Result, []event.Envelope, error) {
		r, err := loadRow(ctx, tx, in.TaskID)
		if err != nil {
			return Result{}, nil, err
		}
		if err := s.accept(who, cmd.OperationID, contractType, in, r, refs, epoch); err != nil {
			return Result{}, nil, err
		}
		before := r.task.State
		names, err := mutate(ctx, tx, &r, cmd.OperationID, lease)
		if err != nil {
			return Result{}, nil, err
		}
		if len(names) > 0 || r.task.State != before {
			if err := s.saveTask(ctx, tx, &r.task, &r.meta, r.task.State != before); err != nil {
				return Result{}, nil, err
			}
		}
		var events []event.Envelope
		for _, name := range names {
			e, err := s.event(cmd, name, r.task, map[string]any{"attempt_id": in.AttemptID, "round": r.meta.Round, "state": r.task.State, "output_refs": r.task.OutputRefs})
			if err != nil {
				return Result{}, nil, err
			}
			events = append(events, e)
		}
		return resultOf(r.task, r.meta, r.attempt), events, nil
	})
}

// checkMessage 核对阻塞/答复/交接引用的 T03 讨论消息：同一任务、指定类型与作者。
func (s *Service) checkMessage(ctx context.Context, t Task, id ids.ID, kind string, author, replyTo ids.ID) error {
	if s.d.Discussions == nil {
		return errcode.New(errcode.InvalidStateTransition, "discussion authority is not configured")
	}
	m, err := s.d.Discussions.DiscussionMessage(ctx, id)
	if err != nil {
		return err
	}
	if m.Target.ProjectID != t.ProjectID || m.Target.Kind != "task" || m.Target.ID != t.ID || m.Kind != kind || m.AuthorID != author || m.ReplyTo != replyTo {
		return errcode.New(errcode.RefMismatch, "").WithDetails(errcode.Detail{Reason: "discussion_message_mismatch"})
	}
	return nil
}
