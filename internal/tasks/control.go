package tasks

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// TaskRef 是按任务修订条件更新的管理命令目标。
type TaskRef struct {
	ProjectID        ids.ID `json:"project_id"`
	TaskID           ids.ID `json:"task_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func (r TaskRef) valid() error {
	if !r.ProjectID.Valid() || !r.TaskID.Valid() || r.ExpectedRevision < 1 {
		return invalid("task and expected revision required")
	}
	return nil
}

type AssignRequest struct {
	TaskRef
	AssigneeID ids.ID        `json:"assignee_id,omitempty"`
	Role       identity.Role `json:"role,omitempty"`
}

type AnswerRequest struct {
	TaskRef
	BlockID         ids.ID `json:"block_id"`
	AnswerMessageID ids.ID `json:"answer_message_id"`
}

type CancelRequest struct {
	TaskRef
	Reason string `json:"reason"`
}

// ReconcileRequest 记录操作者核实的停止与副作用事实；只有确认停止且无未决
// 副作用时旧轮次才结束，席位才重新开放（契约中的 requeue）。
type ReconcileRequest struct {
	ProjectID            ids.ID   `json:"project_id"`
	TaskID               ids.ID   `json:"task_id"`
	AttemptID            ids.ID   `json:"attempt_id"`
	ExpectedRevision     int64    `json:"expected_revision"`
	TerminationConfirmed bool     `json:"termination_confirmed"`
	UnresolvedEffects    int      `json:"unresolved_effects"`
	EvidenceRefs         []ids.ID `json:"evidence_refs"`
	Reason               string   `json:"reason"`
}

type taskMutation func(context.Context, *sql.Tx, *row) ([]string, error)

// manage 是管理命令的公共路径：锁 → 回执重放 → 授权 → 事务内修订条件更新。
func (s *Service) manage(ctx context.Context, who authz.Context, key, typ string, action authz.Action, in TaskRef, body any, prepare func(context.Context, row) error, mutate taskMutation) (Result, error) {
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
	if r, err := s.replay(ctx, cmd); err != nil || r != nil {
		return deref(r), err
	}
	if err = s.authorize(ctx, who, action, in.ProjectID, "task", in.TaskID); err != nil {
		return Result{}, err
	}
	current, err := loadRow(ctx, s.d.DB, in.TaskID)
	if err != nil {
		return Result{}, err
	}
	if current.task.ProjectID != in.ProjectID {
		return Result{}, errcode.New(errcode.NotFound, "")
	}
	if prepare != nil {
		if err = prepare(ctx, current); err != nil {
			return Result{}, err
		}
	}
	return s.execute(ctx, cmd, func(ctx context.Context, tx *sql.Tx) (Result, []event.Envelope, error) {
		r, err := loadRow(ctx, tx, in.TaskID)
		if err != nil {
			return Result{}, nil, err
		}
		if err := tc.CheckRevision(in.ExpectedRevision, r.task.Revision); err != nil {
			return Result{}, nil, err
		}
		before := r.task.State
		names, err := mutate(ctx, tx, &r)
		if err != nil {
			return Result{}, nil, err
		}
		if err := s.saveTask(ctx, tx, &r.task, &r.meta, r.task.State != before); err != nil {
			return Result{}, nil, err
		}
		var events []event.Envelope
		for _, name := range names {
			e, err := s.event(cmd, name, r.task, map[string]any{"state": r.task.State, "round": r.meta.Round})
			if err != nil {
				return Result{}, nil, err
			}
			events = append(events, e)
		}
		if r.task.State == "done" {
			more, err := s.unblockDependents(ctx, tx, cmd, r.task.ID)
			if err != nil {
				return Result{}, nil, err
			}
			events = append(events, more...)
		}
		return resultOf(r.task, r.meta, r.attempt), events, nil
	})
}

// Assign 只在无人执行时改派；已领取的轮次须先释放或对账。
func (s *Service) Assign(ctx context.Context, who authz.Context, key string, in AssignRequest) (Result, error) {
	if in.AssigneeID != "" && (!in.AssigneeID.Valid() || in.Role != "") || in.Role != "" && !in.Role.Valid() {
		return Result{}, invalid("assign to one principal or one role pool")
	}
	return s.manage(ctx, who, key, "tasks.assign", "tasks.assign", in.TaskRef, in, nil, func(ctx context.Context, tx *sql.Tx, r *row) ([]string, error) {
		if !slices.Contains([]tc.TaskState{"waiting", "todo", "rework"}, r.task.State) || r.seat.State != "open" {
			return nil, stateErr("task_in_progress")
		}
		r.meta.AssigneeID, r.meta.Role = in.AssigneeID, in.Role
		return []string{"task.assigned"}, nil
	})
}

// Answer 只解除阻塞：仍在执行的旧轮次被失效并进入对账，旧租约不会复活；
// 旧执行已确认停止时任务回到待领，需要重新领取取得新 Attempt 与 fence。
func (s *Service) Answer(ctx context.Context, who authz.Context, key string, in AnswerRequest) (Result, error) {
	if !in.BlockID.Valid() || !in.AnswerMessageID.Valid() {
		return Result{}, invalid("block and answer message required")
	}
	var question ids.ID
	return s.manage(ctx, who, key, "tasks.answer", "tasks.answer", in.TaskRef, in, func(ctx context.Context, r row) error {
		b, err := openBlock(ctx, s.d.DB, r.task.ID)
		if err != nil {
			return err
		}
		if b == nil || b.ID != in.BlockID {
			return stateErr("block_not_open")
		}
		question = b.QuestionMessageID
		return s.checkMessage(ctx, r.task, in.AnswerMessageID, "answer", who.PrincipalID, question)
	}, func(ctx context.Context, tx *sql.Tx, r *row) ([]string, error) {
		b, err := openBlock(ctx, tx, r.task.ID)
		if err != nil {
			return nil, err
		}
		if b == nil || b.ID != in.BlockID || b.QuestionMessageID != question || r.task.State != "blocked" {
			return nil, stateErr("block_not_open")
		}
		b.State, b.AnswerMessageID, b.AnsweredBy, b.AnsweredAt = "answered", in.AnswerMessageID, who.PrincipalID, clock.Format(s.now())
		res, err := tx.ExecContext(ctx, `UPDATE tasks_blocks SET state='answered',record=? WHERE block_id=? AND state='open'`, encode(b), b.ID)
		if err != nil {
			return nil, err
		}
		if err := oneRow(res); err != nil {
			return nil, err
		}
		r.task.State = "reconciling"
		names := []string{"task.answered"}
		if r.attempt != nil && (r.attempt.State == "active" || r.attempt.State == "reconciling") {
			more, err := s.stop(ctx, tx, r, false, r.attempt.Reconciliation.UnresolvedEffects, "released", false)
			return append(names, more...), err
		}
		// 旧轮次已结束（或从未开始）：安全回到待领。
		if r.seat.State == "reconciling" {
			return nil, stateErr("seat_needs_reconciliation")
		}
		r.task.State = "todo"
		if err := s.releaseCheckouts(ctx, tx, r.task.ID); err != nil {
			return nil, err
		}
		return append(names, "task.reopened"), nil
	})
}

// Cancel 由管理者取消任务。仍有执行的轮次先失效写入权并等待对账，之后才记
// cancelled；取消意图不等于取消已完成。
func (s *Service) Cancel(ctx context.Context, who authz.Context, key string, in CancelRequest) (Result, error) {
	if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 {
		return Result{}, invalid("cancel reason required")
	}
	return s.manage(ctx, who, key, "tasks.cancel", "tasks.cancel", in.TaskRef, in, nil, func(ctx context.Context, tx *sql.Tx, r *row) ([]string, error) {
		return s.cancel(ctx, tx, r, in.Reason)
	})
}

func (s *Service) cancel(ctx context.Context, tx *sql.Tx, r *row, reason string) ([]string, error) {
	switch r.task.State {
	case "done", "cancelled":
		return nil, stateErr("task_finished")
	case "reconciling":
		r.meta.CancelRequested, r.meta.CancelReason = true, reason
		return []string{"task.cancelling"}, nil
	case "claimed", "blocked":
		r.meta.CancelRequested, r.meta.CancelReason = true, reason
		if r.task.State == "blocked" {
			if err := s.withdrawBlock(ctx, tx, r.task.ID); err != nil {
				return nil, err
			}
		}
		if r.attempt != nil && r.attempt.State == "active" {
			if _, err := s.stop(ctx, tx, r, false, r.attempt.Reconciliation.UnresolvedEffects, "cancelled", true); err != nil {
				return nil, err
			}
			return []string{"task.cancelling"}, nil
		}
		// 旧执行已停止：阻塞任务经 reconciling 结束，不越过契约状态边。
		r.task.State = "reconciling"
		if r.seat.State == "reconciling" {
			return []string{"task.cancelling"}, nil
		}
	}
	if !tc.TaskTransition(r.task.State, "cancelled") {
		return nil, stateErr("task_not_cancellable")
	}
	r.meta.CancelRequested, r.meta.CancelReason = true, reason
	if r.seat.State != "cancelled" {
		if !tc.SeatTransition(r.seat.State, "cancelled") {
			return nil, stateErr("seat_not_cancellable")
		}
		r.seat.State = "cancelled"
		if err := s.saveSeat(ctx, tx, &r.seat); err != nil {
			return nil, err
		}
	}
	r.task.State = "cancelled"
	if err := s.releaseCheckouts(ctx, tx, r.task.ID); err != nil {
		return nil, err
	}
	return []string{"task.cancelled"}, nil
}

func (s *Service) withdrawBlock(ctx context.Context, tx *sql.Tx, task ids.ID) error {
	b, err := openBlock(ctx, tx, task)
	if err != nil || b == nil {
		return err
	}
	b.State = "withdrawn"
	_, err = tx.ExecContext(ctx, `UPDATE tasks_blocks SET state='withdrawn',record=? WHERE block_id=?`, encode(b), b.ID)
	return err
}

// Reconcile 由管理者记录旧轮次的停止与副作用对账结果。确认后结束 Attempt，
// 刷新席位恢复代次，任务回到待领（或完成已请求的取消）。
func (s *Service) Reconcile(ctx context.Context, who authz.Context, key string, in ReconcileRequest) (Result, error) {
	if !in.AttemptID.Valid() || in.UnresolvedEffects < 0 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 || len(in.EvidenceRefs) > 100 {
		return Result{}, invalid("attempt, reason and non-negative unresolved effects required")
	}
	if in.EvidenceRefs == nil {
		in.EvidenceRefs = []ids.ID{}
	}
	ref := TaskRef{ProjectID: in.ProjectID, TaskID: in.TaskID, ExpectedRevision: 1}
	if err := ref.valid(); err != nil {
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
	cmd, err := s.command(ctx, who, key, "tasks.reconcile", in.ProjectID, in)
	if err != nil {
		return Result{}, err
	}
	if r, err := s.replay(ctx, cmd); err != nil || r != nil {
		return deref(r), err
	}
	if err = s.authorize(ctx, who, "tasks.reconcile", in.ProjectID, "task", in.TaskID); err != nil {
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
		if r.task.ProjectID != in.ProjectID || r.attempt == nil || r.attempt.ID != in.AttemptID {
			return Result{}, nil, errcode.New(errcode.NotFound, "")
		}
		if err := tc.CheckRevision(in.ExpectedRevision, r.attempt.Revision); err != nil {
			return Result{}, nil, err
		}
		a := r.attempt
		if a.State != "reconciling" {
			return Result{}, nil, stateErr("attempt_not_reconciling")
		}
		for _, id := range in.EvidenceRefs {
			if !id.Valid() {
				return Result{}, nil, invalid("invalid evidence reference")
			}
			if !slices.Contains(a.Reconciliation.EvidenceRefs, id) {
				a.Reconciliation.EvidenceRefs = append(a.Reconciliation.EvidenceRefs, id)
			}
		}
		before := r.task.State
		names := []string{"task.reconciliation_recorded"}
		if !in.TerminationConfirmed || in.UnresolvedEffects > 0 {
			a.Reconciliation.TerminationConfirmed = false
			a.Reconciliation.UnresolvedEffects = in.UnresolvedEffects
			if err := s.saveAttempt(ctx, tx, a); err != nil {
				return Result{}, nil, err
			}
		} else {
			terminal := tc.AttemptState("released")
			if r.meta.CancelRequested {
				terminal = "cancelled"
			} else if r.meta.ExpiryCount > 0 && expired(a, s.now()) {
				terminal = "expired"
			}
			r.seat.RecoveryEpoch = epoch
			more, err := s.stop(ctx, tx, &r, true, 0, terminal, false)
			if err != nil {
				return Result{}, nil, err
			}
			names = append(names, more...)
		}
		if err := s.saveTask(ctx, tx, &r.task, &r.meta, r.task.State != before); err != nil {
			return Result{}, nil, err
		}
		var events []event.Envelope
		for _, name := range names {
			e, err := s.event(cmd, name, r.task, map[string]any{"attempt_id": a.ID, "state": r.task.State, "termination_confirmed": a.Reconciliation.TerminationConfirmed, "unresolved_effects": a.Reconciliation.UnresolvedEffects})
			if err != nil {
				return Result{}, nil, err
			}
			events = append(events, e)
		}
		return resultOf(r.task, r.meta, a), events, nil
	})
}

func expired(a *Attempt, now time.Time) bool {
	t, err := clock.Parse(a.ExpiresAt)
	return err == nil && !t.After(now)
}

// SweepExpired 失效已到期或属于旧恢复代次的租约：Attempt 进入待对账；到期
// 另使超时次数加一，达到 task.max_attempts 标记卡住。它不推断外部执行已停止，
// 也不重新派发。调用者须在各项目有 tasks.reconcile 权限；无权项目跳过。
// 只由显式调用触发（恢复完成后的对账、运维调度由 T08 接入）。
func (s *Service) SweepExpired(ctx context.Context, who authz.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, invalid("sweep limit must be 1-1000")
	}
	epoch, err := s.epoch(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := s.d.DB.QueryContext(ctx, `SELECT a.attempt_id,a.task_id,t.project_id FROM tasks_attempts a JOIN tasks_tasks t ON t.task_id=a.task_id
		WHERE a.state='active' AND (a.expires_at<=? OR json_extract(a.record,'$.fence.recovery_epoch')<>?) ORDER BY a.expires_at LIMIT ?`, clock.Millis(s.now()), epoch, limit)
	if err != nil {
		return 0, err
	}
	type due struct{ attempt, task, project ids.ID }
	var list []due
	for rows.Next() {
		var d due
		if err = rows.Scan(&d.attempt, &d.task, &d.project); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range list {
		if err := s.authorize(ctx, who, "tasks.reconcile", d.project, "task", d.task); err != nil {
			if errcode.CodeOf(err) == errcode.Forbidden || errcode.CodeOf(err) == errcode.NotFound {
				continue
			}
			return n, err
		}
		if err := s.expire(ctx, who, d.project, d.task, d.attempt); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Service) expire(ctx context.Context, who authz.Context, project, task, attempt ids.ID) error {
	ctx, release, err := s.lock(ctx, project, task)
	if err != nil {
		return err
	}
	defer release()
	cmd, err := s.command(ctx, who, "expire-"+string(attempt), "tasks.expire", project, map[string]any{"attempt_id": attempt})
	if err != nil {
		return err
	}
	if r, err := s.replay(ctx, cmd); err != nil || r != nil {
		return err
	}
	_, maxAttempts, err := s.policies(ctx, project)
	if err != nil {
		return err
	}
	epoch, err := s.epoch(ctx)
	if err != nil {
		return err
	}
	_, err = s.execute(ctx, cmd, func(ctx context.Context, tx *sql.Tx) (Result, []event.Envelope, error) {
		r, err := loadRow(ctx, tx, task)
		if err != nil {
			return Result{}, nil, err
		}
		timedOut := r.attempt != nil && expired(r.attempt, s.now())
		if r.attempt == nil || r.attempt.ID != attempt || r.attempt.State != "active" || !timedOut && r.attempt.Fence.RecoveryEpoch == epoch {
			return resultOf(r.task, r.meta, r.attempt), nil, nil
		}
		before := r.task.State
		if timedOut {
			r.meta.ExpiryCount++
			r.meta.Stuck = r.meta.ExpiryCount >= maxAttempts
		}
		// 阻塞中的任务仍等待答复；其他状态进入待对账。
		if _, err := s.stop(ctx, tx, &r, false, r.attempt.Reconciliation.UnresolvedEffects, "expired", r.task.State != "blocked"); err != nil {
			return Result{}, nil, err
		}
		if err := s.saveTask(ctx, tx, &r.task, &r.meta, r.task.State != before); err != nil {
			return Result{}, nil, err
		}
		reason := "lease_expired"
		if !timedOut {
			reason = "recovery_epoch_changed"
		}
		e, err := s.event(cmd, "task.expired", r.task, map[string]any{"attempt_id": attempt, "reason": reason, "expiry_count": r.meta.ExpiryCount, "stuck": r.meta.Stuck, "state": r.task.State})
		if err != nil {
			return Result{}, nil, err
		}
		return resultOf(r.task, r.meta, r.attempt), []event.Envelope{e}, nil
	})
	return err
}

// unblockDependents 把前置全部完成的等待任务转为待领；同一 tasks 事务提交。
func (s *Service) unblockDependents(ctx context.Context, tx *sql.Tx, cmd commands.Context, done ids.ID) ([]event.Envelope, error) {
	rows, err := tx.QueryContext(ctx, `SELECT d.task_id FROM tasks_dependencies d JOIN tasks_tasks t ON t.task_id=d.task_id WHERE d.depends_on=? AND t.state='waiting' ORDER BY d.task_id`, done)
	if err != nil {
		return nil, err
	}
	var waiting []ids.ID
	for rows.Next() {
		var id ids.ID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		waiting = append(waiting, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var events []event.Envelope
	for _, id := range waiting {
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks_dependencies d JOIN tasks_tasks t ON t.task_id=d.depends_on WHERE d.task_id=? AND t.state<>'done'`, id).Scan(&open); err != nil {
			return nil, err
		}
		if open > 0 {
			continue
		}
		t, m, err := loadTask(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		t.State = "todo"
		if err := s.saveTask(ctx, tx, &t, &m, true); err != nil {
			return nil, err
		}
		e, err := s.event(cmd, "task.ready", t, map[string]any{"state": t.State})
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}
