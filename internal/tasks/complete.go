package tasks

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

type CompleteRequest struct {
	TaskRef
	AuthorityOperationID ids.ID `json:"authority_operation_id"`
}

type ReworkRequest struct {
	TaskRef
	AuthorityOperationID ids.ID `json:"authority_operation_id"`
	Reason               string `json:"reason"`
	// Checkouts 为下一轮追加签出，资产必须来自本轮已交付的输出。
	Checkouts []CheckoutSpec `json:"checkouts"`
}

// fact 是事务外读取、事务内复核的完成/返工依据。
type fact struct {
	kind    string
	verdict string
	flow    ledger.ReviewFlow
	version ids.ID
	actor   ids.ID
}

// Complete 按任务类型引用权威事实：制作类需要本轮交付被批准的审定，review
// 需要针对其对象的审定结论，qa 需要已接受的质检报告。执行成功、模型答复或
// 人工回复不满足这些条件。
func (s *Service) Complete(ctx context.Context, who authz.Context, key string, in CompleteRequest) (Result, error) {
	if !in.AuthorityOperationID.Valid() {
		return Result{}, invalid("authority operation required")
	}
	var f fact
	return s.manage(ctx, who, key, "tasks.complete", "tasks.complete", in.TaskRef, in, func(ctx context.Context, r row) error {
		var err error
		f, err = s.completionFact(ctx, who, r, in.AuthorityOperationID)
		return err
	}, func(ctx context.Context, tx *sql.Tx, r *row) ([]string, error) {
		if err := f.matches(*r); err != nil {
			return nil, err
		}
		e := tc.CompletionEvidence{AuthorityOperationID: in.AuthorityOperationID, ReviewDecided: f.kind == "review", QAReported: f.kind == "qa", OutputsAccepted: f.kind == "accepted"}
		if err := tc.CheckTaskCompletion(r.task, e); err != nil {
			return nil, err
		}
		if r.seat.State == "submitted" {
			r.seat.State = "done"
			if err := s.saveSeat(ctx, tx, &r.seat); err != nil {
				return nil, err
			}
		}
		now := clock.Format(s.now())
		r.meta.Completion = &Completion{Kind: f.kind, AuthorityOperationID: in.AuthorityOperationID, Verdict: f.verdict, At: now}
		r.meta.History = append(r.meta.History, RoundOutcome{Round: r.meta.Round, AttemptID: r.seat.CurrentAttemptID, OutputRefs: r.task.OutputRefs, Outcome: "done", AuthorityOperationID: in.AuthorityOperationID, At: now})
		r.task.State = "done"
		if err := s.releaseCheckouts(ctx, tx, r.task.ID); err != nil {
			return nil, err
		}
		return []string{"task.completed"}, nil
	})
}

func (s *Service) completionFact(ctx context.Context, who authz.Context, r row, op ids.ID) (fact, error) {
	_, authorities := s.wired()
	if authorities == nil {
		return fact{}, errcode.New(errcode.InvalidStateTransition, "review/evidence authority is not configured")
	}
	switch r.task.Type {
	case "review":
		rf, err := authorities.ReviewByOperation(ctx, op)
		if err != nil {
			return fact{}, err
		}
		return fact{kind: "review", verdict: rf.Review.Verdict, flow: rf.Target.Flow, version: rf.Target.VersionID, actor: rf.Review.ActorID}, nil
	case "qa":
		e, err := authorities.EvidenceByOperation(ctx, who, op)
		if err != nil {
			return fact{}, err
		}
		if e.Kind != "qa_report" {
			return fact{}, errcode.New(errcode.ChecksNotSatisfied, "").WithDetails(errcode.Detail{Reason: "qa_report_required"})
		}
		if e.ActorID != who.PrincipalID {
			// 质检任务由报告作者本人，或项目协调者代为登记完成。
			if err := s.authorize(ctx, who, "tasks.create", r.task.ProjectID, "task", r.task.ID); err != nil {
				return fact{}, err
			}
		}
		return fact{kind: "qa", verdict: e.QAVerdict, flow: e.Flow, version: e.Ref.VersionID, actor: e.ActorID}, nil
	}
	rf, err := authorities.ReviewByOperation(ctx, op)
	if err == nil {
		if rf.Review.Verdict != "approve" {
			return fact{}, errcode.New(errcode.ChecksNotSatisfied, "").WithDetails(errcode.Detail{Reason: "review_not_approved"})
		}
		return fact{kind: "accepted", verdict: "approve", flow: rf.Target.Flow, version: rf.Target.VersionID, actor: rf.Review.ActorID}, nil
	}
	if errcode.CodeOf(err) != errcode.NotFound {
		return fact{}, err
	}
	// 没有输出的提问/整理/维护任务由管理者按原交付操作验收。
	if len(r.task.OutputRefs) == 0 && op == r.meta.SubmitOperationID && slices.Contains([]string{"question", "curate", "maintain"}, r.task.Type) {
		if err := s.authorize(ctx, who, "tasks.create", r.task.ProjectID, "task", r.task.ID); err != nil {
			return fact{}, err
		}
		return fact{kind: "accepted", verdict: "accepted", actor: who.PrincipalID}, nil
	}
	return fact{}, errcode.New(errcode.ChecksNotSatisfied, "").WithDetails(errcode.Detail{Reason: "task_completion_authority_required"})
}

// matches 在事务内复核事实仍指向当前任务状态与轮次，防止旧轮次结论推进新轮次。
func (f fact) matches(r row) error {
	switch f.kind {
	case "review", "qa":
		if r.meta.Subject == nil || f.flow != r.meta.Subject.Flow || f.version != r.meta.Subject.Ref.VersionID {
			return errcode.New(errcode.ReviewTargetStale, "").WithDetails(errcode.Detail{Reason: "subject_mismatch"})
		}
		if f.kind == "qa" {
			if slices.Contains(r.meta.DistinctFrom, f.actor) {
				return errcode.New(errcode.SelfReviewForbidden, "")
			}
			if r.task.State == "submitted" && (r.attempt == nil || r.attempt.PrincipalID != f.actor) {
				return errcode.New(errcode.ChecksNotSatisfied, "").WithDetails(errcode.Detail{Reason: "qa_actor_mismatch"})
			}
		}
		return nil
	case "accepted":
		if f.version == "" {
			return nil
		}
		if err := currentFlow(r, f.flow); err != nil {
			return err
		}
		if !slices.ContainsFunc(r.task.OutputRefs, func(p ids.PermanentRef) bool { return p.VersionID == f.version }) {
			return errcode.New(errcode.ReviewTargetStale, "").WithDetails(errcode.Detail{Reason: "approved_version_not_delivered"})
		}
		return nil
	}
	return errcode.New(errcode.ChecksNotSatisfied, "")
}

// currentFlow 核对审定/证据绑定的是本任务当前轮次的交付 Attempt。
func currentFlow(r row, f ledger.ReviewFlow) error {
	if f.TaskID != r.task.ID || f.Round != int64(r.meta.Round) || r.attempt == nil || f.AttemptID != r.attempt.ID || f.Fence != r.attempt.Fence.LeaseFence {
		return errcode.New(errcode.ReviewTargetStale, "").WithDetails(errcode.Detail{Reason: "round_or_attempt_superseded"})
	}
	if f.CandidateGroup != "" && (f.CandidateGroup != r.task.ID || !multiCandidate(r.task)) {
		return errcode.New(errcode.ReviewTargetStale, "").WithDetails(errcode.Detail{Reason: "candidate_group_not_authorized"})
	}
	return nil
}

func multiCandidate(t Task) bool {
	return slices.ContainsFunc(t.ExpectedOutputs, func(o tc.OutputRequirement) bool { return o.CandidateCount > 1 })
}

// Rework 以审定退回或未通过的质检报告开启新轮次；旧轮次、输出与依据保留在
// 历史中，旧审定目标因轮次变化不能再被批准。
func (s *Service) Rework(ctx context.Context, who authz.Context, key string, in ReworkRequest) (Result, error) {
	if !in.AuthorityOperationID.Valid() || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 || len(in.Checkouts) > 100 {
		return Result{}, invalid("authority operation and reason required")
	}
	if in.Checkouts == nil {
		in.Checkouts = []CheckoutSpec{}
	}
	var f fact
	return s.manage(ctx, who, key, "tasks.rework", "tasks.complete", in.TaskRef, in, func(ctx context.Context, r row) error {
		_, authorities := s.wired()
		if authorities == nil {
			return errcode.New(errcode.InvalidStateTransition, "review/evidence authority is not configured")
		}
		rf, err := authorities.ReviewByOperation(ctx, in.AuthorityOperationID)
		switch {
		case err == nil:
			if rf.Review.Verdict != "return" {
				return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "review_did_not_return"})
			}
			f = fact{kind: "review_return", verdict: rf.Review.Verdict, flow: rf.Target.Flow, version: rf.Target.VersionID, actor: rf.Review.ActorID}
		case errcode.CodeOf(err) == errcode.NotFound:
			e, err := authorities.EvidenceByOperation(ctx, who, in.AuthorityOperationID)
			if err != nil {
				return err
			}
			switch {
			case e.Kind == "qa_report" && e.QAVerdict != "pass":
				f = fact{kind: "qa_return", verdict: e.QAVerdict, flow: e.Flow, version: e.Ref.VersionID, actor: e.ActorID}
			case e.Kind == "check_result" && e.Check != nil && e.Check.Verdict != execution.VerdictPass:
				// 合法的检查失败是证据，不是宿主故障；它只能开启返工，不能冒充通过。
				f = fact{kind: "check_return", verdict: string(e.Check.Verdict), flow: e.Flow, version: e.Ref.VersionID, actor: e.ActorID}
			default:
				return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "evidence_did_not_fail"})
			}
		default:
			return err
		}
		return nil
	}, func(ctx context.Context, tx *sql.Tx, r *row) ([]string, error) {
		if r.task.State != "submitted" || !tc.TaskTransition(r.task.State, "rework") {
			return nil, stateErr("task_not_submitted")
		}
		if err := currentFlow(*r, f.flow); err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(r.task.OutputRefs, func(p ids.PermanentRef) bool { return p.VersionID == f.version }) {
			return nil, errcode.New(errcode.ReviewTargetStale, "").WithDetails(errcode.Detail{Reason: "returned_version_not_delivered"})
		}
		if f.kind == "qa_return" && f.actor == r.attempt.PrincipalID {
			return nil, errcode.New(errcode.SelfReviewForbidden, "")
		}
		for _, c := range in.Checkouts {
			if c.Mode != "exclusive" && c.Mode != "advisory" || !slices.ContainsFunc(r.task.OutputRefs, func(p ids.PermanentRef) bool { return p.AssetID == c.AssetID }) {
				return nil, invalid("rework checkouts must name delivered assets")
			}
			if !slices.ContainsFunc(r.meta.Checkouts, func(x CheckoutSpec) bool { return x.AssetID == c.AssetID }) {
				r.meta.Checkouts = append(r.meta.Checkouts, c)
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tasks_refs(task_id,asset_id,version_id,role) VALUES(?,?,'','checkout')`, r.task.ID, c.AssetID); err != nil {
					return nil, err
				}
			}
		}
		r.seat.State = "open"
		if err := s.saveSeat(ctx, tx, &r.seat); err != nil {
			return nil, err
		}
		r.meta.History = append(r.meta.History, RoundOutcome{Round: r.meta.Round, AttemptID: r.attempt.ID, OutputRefs: r.task.OutputRefs, Outcome: f.kind, AuthorityOperationID: in.AuthorityOperationID, Reason: in.Reason, At: clock.Format(s.now())})
		r.meta.Round++
		r.meta.SubmitOperationID = ""
		r.task.OutputRefs = []ids.PermanentRef{}
		r.task.State = "rework"
		return []string{"task.rework"}, nil
	})
}
