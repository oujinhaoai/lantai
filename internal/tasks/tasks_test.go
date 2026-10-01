package tasks

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

func TestConcurrentClaimYieldsOneAttempt(t *testing.T) {
	f := newFixture(t)
	task := f.create(CreateRequest{}).TaskID
	v := f.view(task)
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, who := range []authz.Context{f.makerA, f.makerB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = f.s.Claim(t.Context(), who, "claim", ClaimRequest{ProjectID: f.project, TaskID: task, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
			continue
		}
		expectCode(t, err, errcode.TaskAlreadyClaimed)
	}
	if ok != 1 {
		t.Fatalf("exactly one claim must win: %v", results)
	}
	attempts, err := f.s.Attempts(t.Context(), f.owner, task)
	if err != nil || len(attempts) != 1 || attempts[0].Fence.LeaseFence != 1 {
		t.Fatal(attempts, err)
	}
	// 同一主体的另一个会话既领不到，也不能用有效 fence 续期。
	winner := f.makerA
	if attempts[0].PrincipalID == f.makerB.PrincipalID {
		winner = f.makerB
	}
	_, err = f.claim(f.session(winner), task)
	expectCode(t, err, errcode.TaskAlreadyClaimed)
	_, err = f.s.Renew(t.Context(), f.session(winner), f.key(), f.current(task))
	expectCode(t, err, errcode.LeaseStale)
}

func TestExpiredAttemptIsFencedAndNeedsReconciliation(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task := f.create(CreateRequest{}).TaskID
	a := f.mustClaim(f.makerA, task)
	if _, err := f.s.Renew(ctx, f.makerA, f.key(), ref(f, task, a)); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(31 * time.Minute)
	_, err := f.s.Renew(ctx, f.makerA, f.key(), f.current(task))
	expectCode(t, err, errcode.LeaseStale)
	if n, err := f.s.SweepExpired(ctx, f.owner, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	v := f.view(task)
	if v.Task.State != "reconciling" || v.Attempt.State != "reconciling" || v.Meta.ExpiryCount != 1 || v.Attempt.Reconciliation.TerminationConfirmed {
		t.Fatalf("expiry must revoke without inferring stop: %+v %+v", v.Task, v.Attempt)
	}
	// 到期不是停止证据：未对账前不能派出第二份执行。
	_, err = f.claim(f.makerB, task)
	expectCode(t, err, errcode.OperationNeedsReconciliation)
	if n, err := f.s.SweepExpired(ctx, f.owner, 10); err != nil || n != 0 {
		t.Fatal("sweep must not repeat", n, err)
	}
	_, err = f.s.Reconcile(ctx, f.owner, f.key(), ReconcileRequest{ProjectID: f.project, TaskID: task, AttemptID: a.ID, ExpectedRevision: v.Attempt.Revision, UnresolvedEffects: 1, Reason: "upload may have been sent"})
	if err != nil {
		t.Fatal(err)
	}
	if v = f.view(task); v.Task.State != "reconciling" || v.Attempt.Reconciliation.UnresolvedEffects != 1 {
		t.Fatal("unresolved effects keep reconciliation open", v.Task.State)
	}
	if _, err = f.s.Reconcile(ctx, f.owner, f.key(), ReconcileRequest{ProjectID: f.project, TaskID: task, AttemptID: a.ID, ExpectedRevision: v.Attempt.Revision, TerminationConfirmed: true, Reason: "session confirmed stopped", EvidenceRefs: []ids.ID{ids.New()}}); err != nil {
		t.Fatal(err)
	}
	if v = f.view(task); v.Task.State != "todo" || v.Seat.State != "open" || v.Attempt.State != "expired" {
		t.Fatalf("confirmed expiry reopens: %+v", v)
	}
	b := f.mustClaim(f.makerB, task)
	if b.ID == a.ID || b.Fence.LeaseFence != 2 {
		t.Fatal("new round needs a new attempt and larger fence", b)
	}
	// 旧 Attempt 的续期、交付与版本写入在所有接受入口被拒绝。
	_, err = f.s.Submit(ctx, f.makerA, f.key(), SubmitRequest{AttemptRef: ref(f, task, a)})
	expectCode(t, err, errcode.LeaseStale)
	expectCode(t, f.guard(f.makerA, ids.New(), &a), errcode.LeaseStale)
	if err = f.guard(f.makerB, ids.New(), &b); err != nil {
		t.Fatal(err)
	}
	attempts, _ := f.s.Attempts(ctx, f.owner, task)
	if len(attempts) != 2 || attempts[0].ID != a.ID {
		t.Fatal("history must keep old attempts", attempts)
	}
}

func TestReleaseWithUnknownEffectsDoesNotReopen(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task := f.create(CreateRequest{}).TaskID
	a := f.mustClaim(f.makerA, task)
	if _, err := f.s.Release(ctx, f.makerA, f.key(), ReleaseRequest{AttemptRef: ref(f, task, a), Reason: "switching tools", Stopped: true, UnresolvedEffects: 1}); err != nil {
		t.Fatal(err)
	}
	v := f.view(task)
	if v.Task.State != "reconciling" || v.Seat.State != "reconciling" {
		t.Fatal("unknown side effects need reconciliation", v.Task.State)
	}
	_, err := f.claim(f.makerB, task)
	expectCode(t, err, errcode.OperationNeedsReconciliation)
	task2 := f.create(CreateRequest{}).TaskID
	a2 := f.mustClaim(f.makerA, task2)
	if _, err = f.s.Release(ctx, f.makerA, f.key(), ReleaseRequest{AttemptRef: ref(f, task2, a2), Reason: "done for today", Stopped: true}); err != nil {
		t.Fatal(err)
	}
	if v = f.view(task2); v.Task.State != "todo" || v.Attempt.State != "released" {
		t.Fatal("confirmed stop reopens immediately", v.Task.State)
	}
}

func TestExclusiveCheckoutGuardsCommitsAndOtherTasks(t *testing.T) {
	f := newFixture(t)
	x := f.version("", f.makerA.PrincipalID)
	y := f.version("", f.makerA.PrincipalID)
	taskA := f.create(CreateRequest{Checkouts: []CheckoutSpec{{AssetID: x.AssetID, Mode: "exclusive"}}}).TaskID
	taskB := f.create(CreateRequest{Checkouts: []CheckoutSpec{{AssetID: x.AssetID, Mode: "advisory"}}}).TaskID
	taskC := f.create(CreateRequest{Checkouts: []CheckoutSpec{{AssetID: y.AssetID, Mode: "advisory"}}}).TaskID
	if err := f.guard(f.makerB, x.AssetID, nil); err != nil {
		t.Fatal("no checkout yet", err)
	}
	a := f.mustClaim(f.makerA, taskA)
	v := f.view(taskA)
	if len(v.Checkouts) != 1 || v.Checkouts[0].State != "active" || v.Checkouts[0].BaseVersionID != x.VersionID || v.Checkouts[0].AttemptID != a.ID {
		t.Fatal(v.Checkouts)
	}
	expectCode(t, f.guard(f.makerB, x.AssetID, nil), errcode.AssetCheckedOut)
	_, err := f.claim(f.makerB, taskB)
	expectCode(t, err, errcode.AssetCheckedOut)
	if err = f.guard(f.makerA, x.AssetID, &a); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.guard(f.session(f.makerA), x.AssetID, &a), errcode.LeaseStale)
	c := f.mustClaim(f.makerB, taskC)
	if err = f.guard(f.makerA, y.AssetID, nil); err != nil {
		t.Fatal("advisory checkout only marks, base checks stay with the ledger", err)
	}
	if err = f.guard(f.makerA, y.AssetID, &a); err != nil {
		t.Fatal("another task's advisory checkout does not block", err)
	}
	if err = f.guard(f.makerB, y.AssetID, &c); err != nil {
		t.Fatal(err)
	}
	f.res.locked[x.AssetID] = true
	if _, err = f.s.Release(t.Context(), f.makerA, f.key(), ReleaseRequest{AttemptRef: ref(f, taskA, a), Reason: "handover", Stopped: true}); err != nil {
		t.Fatal(err)
	}
	if v = f.view(taskA); v.Checkouts[0].State != "released" {
		t.Fatal("reopened task releases checkout")
	}
	_, err = f.claim(f.makerA, taskA)
	expectCode(t, err, errcode.AssetLocked)
}

func TestAnswerUnblocksWithoutRevivingLease(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task := f.create(CreateRequest{}).TaskID
	a := f.mustClaim(f.makerA, task)
	wrong := f.message(task, "question", f.makerB.PrincipalID, "")
	_, err := f.s.Block(ctx, f.makerA, f.key(), BlockRequest{AttemptRef: ref(f, task, a), QuestionMessageID: wrong})
	expectCode(t, err, errcode.RefMismatch)
	q := f.message(task, "question", f.makerA.PrincipalID, "")
	res, err := f.s.Block(ctx, f.makerA, f.key(), BlockRequest{AttemptRef: ref(f, task, a), QuestionMessageID: q})
	if err != nil || res.State != "blocked" {
		t.Fatal(res, err)
	}
	v := f.view(task)
	ans := f.message(task, "answer", f.owner.PrincipalID, q)
	if _, err = f.s.Answer(ctx, f.owner, f.key(), AnswerRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task, ExpectedRevision: v.Task.Revision}, BlockID: v.Blocks[0].ID, AnswerMessageID: ans}); err != nil {
		t.Fatal(err)
	}
	v = f.view(task)
	if v.Task.State != "reconciling" || v.Attempt.State != "reconciling" || v.Blocks[0].State != "answered" {
		t.Fatalf("answer revokes the old attempt: %+v %+v", v.Task, v.Attempt)
	}
	_, err = f.s.Renew(ctx, f.makerA, f.key(), f.current(task))
	expectCode(t, err, errcode.LeaseStale)
	if _, err = f.s.Reconcile(ctx, f.owner, f.key(), ReconcileRequest{ProjectID: f.project, TaskID: task, AttemptID: a.ID, ExpectedRevision: v.Attempt.Revision, TerminationConfirmed: true, Reason: "agent confirmed"}); err != nil {
		t.Fatal(err)
	}
	b := f.mustClaim(f.makerA, task)
	if b.ID == a.ID || b.Fence.LeaseFence != 2 {
		t.Fatal("reclaim after answer needs a new attempt", b)
	}
	// 已停止的阻塞：答复后直接回到待领。
	task2 := f.create(CreateRequest{}).TaskID
	a2 := f.mustClaim(f.makerA, task2)
	q2 := f.message(task2, "question", f.makerA.PrincipalID, "")
	if _, err = f.s.Block(ctx, f.makerA, f.key(), BlockRequest{AttemptRef: ref(f, task2, a2), QuestionMessageID: q2, Stopped: true}); err != nil {
		t.Fatal(err)
	}
	v = f.view(task2)
	if v.Task.State != "blocked" || v.Seat.State != "open" || v.Attempt.State != "released" {
		t.Fatal(v.Task.State, v.Seat.State, v.Attempt.State)
	}
	_, err = f.claim(f.makerB, task2)
	expectCode(t, err, errcode.InvalidStateTransition)
	ans2 := f.message(task2, "answer", f.owner.PrincipalID, q2)
	if _, err = f.s.Answer(ctx, f.owner, f.key(), AnswerRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task2, ExpectedRevision: v.Task.Revision}, BlockID: v.Blocks[0].ID, AnswerMessageID: ans2}); err != nil {
		t.Fatal(err)
	}
	if v = f.view(task2); v.Task.State != "todo" {
		t.Fatal(v.Task.State)
	}
}

func TestHandoffRecordsDraftsAndReopens(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task := f.create(CreateRequest{}).TaskID
	a := f.mustClaim(f.makerA, task)
	draft := f.version("", f.makerA.PrincipalID)
	other := f.version("", f.makerB.PrincipalID)
	msg := f.message(task, "handoff", f.makerA.PrincipalID, "")
	_, err := f.s.Handoff(ctx, f.makerA, f.key(), HandoffRequest{AttemptRef: ref(f, task, a), MessageID: msg, DraftRefs: []ids.PermanentRef{other}})
	expectCode(t, err, errcode.Forbidden)
	if _, err = f.s.Handoff(ctx, f.makerA, f.key(), HandoffRequest{AttemptRef: ref(f, task, a), MessageID: msg, DraftRefs: []ids.PermanentRef{draft}}); err != nil {
		t.Fatal(err)
	}
	v := f.view(task)
	if v.Task.State != "todo" || len(v.Handoffs) != 1 || v.Handoffs[0].DraftRefs[0] != draft || v.Attempt.State != "released" {
		t.Fatalf("%+v", v)
	}
	b := f.mustClaim(f.makerB, task)
	if b.Fence.LeaseFence != 2 {
		t.Fatal(b)
	}
}

func TestCancelWaitsForStopAndEndsDeliveredTasks(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task := f.create(CreateRequest{}).TaskID
	a := f.mustClaim(f.makerA, task)
	v := f.view(task)
	if _, err := f.s.Cancel(ctx, f.owner, f.key(), CancelRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task, ExpectedRevision: v.Task.Revision}, Reason: "scope changed"}); err != nil {
		t.Fatal(err)
	}
	if v = f.view(task); v.Task.State != "reconciling" || !v.Meta.CancelRequested {
		t.Fatal("cancel intent is not a completed cancellation", v.Task.State)
	}
	_, err := f.s.Submit(ctx, f.makerA, f.key(), SubmitRequest{AttemptRef: ref(f, task, a)})
	expectCode(t, err, errcode.LeaseStale)
	if _, err = f.s.Reconcile(ctx, f.owner, f.key(), ReconcileRequest{ProjectID: f.project, TaskID: task, AttemptID: a.ID, ExpectedRevision: v.Attempt.Revision, TerminationConfirmed: true, Reason: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if v = f.view(task); v.Task.State != "cancelled" || v.Attempt.State != "cancelled" || v.Seat.State != "cancelled" {
		t.Fatal(v.Task.State, v.Attempt.State, v.Seat.State)
	}
	// 已交付待验收的任务可以直接取消（流程取消传播）。
	task2 := f.create(CreateRequest{}).TaskID
	a2 := f.mustClaim(f.makerA, task2)
	out := f.version("", f.makerA.PrincipalID)
	if _, err = f.s.Submit(ctx, f.makerA, f.key(), SubmitRequest{AttemptRef: ref(f, task2, a2), OutputRefs: []ids.PermanentRef{out}}); err != nil {
		t.Fatal(err)
	}
	v = f.view(task2)
	if _, err = f.s.Cancel(ctx, f.owner, f.key(), CancelRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task2, ExpectedRevision: v.Task.Revision}, Reason: "flow cancelled"}); err != nil {
		t.Fatal(err)
	}
	if v = f.view(task2); v.Task.State != "cancelled" || v.Seat.State != "cancelled" {
		t.Fatal(v.Task.State)
	}
}

func TestReworkOpensRoundAndRejectsStaleAuthority(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task := f.create(CreateRequest{}).TaskID
	downstream := f.create(CreateRequest{DependencyIDs: []ids.ID{task}}).TaskID
	if f.view(downstream).Task.State != "waiting" {
		t.Fatal("dependency keeps downstream waiting")
	}
	a := f.mustClaim(f.makerA, task)
	v1 := f.version("", f.makerA.PrincipalID)
	if _, err := f.s.Submit(ctx, f.makerA, f.key(), SubmitRequest{AttemptRef: ref(f, task, a), OutputRefs: []ids.PermanentRef{v1}}); err != nil {
		t.Fatal(err)
	}
	approveOld := f.reviewFact(task, a, 1, v1.VersionID, "approve")
	returned := f.reviewFact(task, a, 1, v1.VersionID, "return")
	v := f.view(task)
	_, err := f.s.Rework(ctx, f.owner, f.key(), ReworkRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task, ExpectedRevision: v.Task.Revision}, AuthorityOperationID: approveOld, Reason: "not a return"})
	expectCode(t, err, errcode.InvalidStateTransition)
	if _, err = f.s.Rework(ctx, f.owner, f.key(), ReworkRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task, ExpectedRevision: v.Task.Revision}, AuthorityOperationID: returned, Reason: "fix topology",
		Checkouts: []CheckoutSpec{{AssetID: v1.AssetID, Mode: "exclusive"}}}); err != nil {
		t.Fatal(err)
	}
	v = f.view(task)
	if v.Task.State != "rework" || v.Meta.Round != 2 || len(v.Meta.History) != 1 || len(v.Task.OutputRefs) != 0 || len(v.Meta.Checkouts) != 1 {
		t.Fatalf("%+v %+v", v.Task, v.Meta)
	}
	// 旧轮次批准不能完成新轮次。
	_, err = f.s.Complete(ctx, f.owner, f.key(), CompleteRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task, ExpectedRevision: v.Task.Revision}, AuthorityOperationID: approveOld})
	expectCode(t, err, errcode.ReviewTargetStale)
	b := f.mustClaim(f.makerA, task)
	if v = f.view(task); v.Checkouts[0].AttemptID != b.ID {
		t.Fatal("rework round checks out the delivered asset")
	}
	v2 := f.version(v1.AssetID, f.makerA.PrincipalID)
	if _, err = f.s.Submit(ctx, f.makerA, f.key(), SubmitRequest{AttemptRef: ref(f, task, b), OutputRefs: []ids.PermanentRef{v2}}); err != nil {
		t.Fatal(err)
	}
	v = f.view(task)
	_, err = f.s.Complete(ctx, f.owner, f.key(), CompleteRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task, ExpectedRevision: v.Task.Revision}, AuthorityOperationID: approveOld})
	expectCode(t, err, errcode.ReviewTargetStale)
	approve := f.reviewFact(task, b, 2, v2.VersionID, "approve")
	if _, err = f.s.Complete(ctx, f.owner, f.key(), CompleteRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task, ExpectedRevision: v.Task.Revision}, AuthorityOperationID: approve}); err != nil {
		t.Fatal(err)
	}
	if v = f.view(task); v.Task.State != "done" || v.Seat.State != "done" || v.Checkouts[0].State != "released" || v.Meta.Completion.AuthorityOperationID != approve {
		t.Fatalf("%+v", v)
	}
	if f.view(downstream).Task.State != "todo" {
		t.Fatal("dependents unlock only after done")
	}
}

func TestQATaskIsIndependentAndCompletesFromReport(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	task := f.create(CreateRequest{}).TaskID
	a := f.mustClaim(f.makerA, task)
	out := f.version("", f.makerA.PrincipalID)
	if _, err := f.s.Submit(ctx, f.makerA, f.key(), SubmitRequest{AttemptRef: ref(f, task, a), OutputRefs: []ids.PermanentRef{out}}); err != nil {
		t.Fatal(err)
	}
	subject := &Subject{Flow: ledger.ReviewFlow{TaskID: task, AttemptID: a.ID, Round: 1, Fence: a.Fence.LeaseFence}, Ref: out}
	qa := f.create(CreateRequest{Type: "qa", Subject: subject}).TaskID
	// 制作者即使兼有质检角色也不能质检自己的交付。
	f.members.members[1].Roles = append(f.members.members[1].Roles, "checker")
	_, err := f.claim(f.makerA, qa)
	expectCode(t, err, errcode.SelfReviewForbidden)
	q := f.mustClaim(f.checker, qa)
	op := ids.New()
	f.facts.evidence[op] = ledger.AcceptedEvidence{ID: ids.New(), Kind: "qa_report", QAVerdict: "pass", Ref: out, Flow: subject.Flow, ActorID: f.checker.PrincipalID}
	if _, err = f.s.Submit(ctx, f.checker, f.key(), SubmitRequest{AttemptRef: ref(f, qa, q)}); err != nil {
		t.Fatal(err)
	}
	v := f.view(qa)
	if _, err = f.s.Complete(ctx, f.checker, f.key(), CompleteRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: qa, ExpectedRevision: v.Task.Revision}, AuthorityOperationID: op}); err != nil {
		t.Fatal(err)
	}
	if v = f.view(qa); v.Task.State != "done" || v.Meta.Completion.Verdict != "pass" {
		t.Fatal(v.Task.State)
	}
	// 未通过的质检报告只能开启制作任务返工。
	fail := ids.New()
	f.facts.evidence[fail] = ledger.AcceptedEvidence{ID: ids.New(), Kind: "qa_report", QAVerdict: "fail", Ref: out, Flow: subject.Flow, ActorID: f.checker.PrincipalID}
	pv := f.view(task)
	if _, err = f.s.Rework(ctx, f.owner, f.key(), ReworkRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task, ExpectedRevision: pv.Task.Revision}, AuthorityOperationID: fail, Reason: "qa failed"}); err != nil {
		t.Fatal(err)
	}
}

func TestReplayAndIdempotencyConflict(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	in := CreateRequest{ProjectID: f.project, Type: "produce", Title: "once", AcceptanceCriteria: []string{"one"}}
	r1, err := f.s.Create(ctx, f.owner, "same-key", in)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := f.s.Create(ctx, f.owner, "same-key", in)
	if err != nil || r2.TaskID != r1.TaskID || r2.OperationID != r1.OperationID {
		t.Fatal(r1, r2, err)
	}
	in.Title = "different"
	_, err = f.s.Create(ctx, f.owner, "same-key", in)
	expectCode(t, err, errcode.IdempotencyConflict)
	list, err := f.s.List(ctx, f.owner, f.project, "", 10, true)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	// 领取回执重放在租约到期后仍返回原结果，不重新要求写租约有效。
	v := f.view(r1.TaskID)
	claim := ClaimRequest{ProjectID: f.project, TaskID: r1.TaskID, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision}
	c1, err := f.s.Claim(ctx, f.makerA, "claim-key", claim)
	if err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(2 * time.Hour)
	c2, err := f.s.Claim(ctx, f.makerA, "claim-key", claim)
	if err != nil || c2.Attempt.ID != c1.Attempt.ID {
		t.Fatal(c2, err)
	}
}

func TestRecoveryEpochChangeNeedsReconciliationForActiveSeat(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	idle := f.create(CreateRequest{}).TaskID
	busy := f.create(CreateRequest{}).TaskID
	a := f.mustClaim(f.makerA, busy)
	f.auth.epoch = 2
	// 恢复代次变化后旧 fence 失效；空闲席位直接采用新代次。
	_, err := f.s.Renew(ctx, f.makerA, f.key(), ref(f, busy, a))
	if errcode.CodeOf(err) != errcode.LeaseStale && errcode.CodeOf(err) != errcode.OperationNeedsReconciliation {
		t.Fatal(err)
	}
	b := f.mustClaim(f.makerB, idle)
	if b.Fence.RecoveryEpoch != 2 {
		t.Fatal(b.Fence)
	}
	stale := b.Fence
	stale.RecoveryEpoch = 1
	_, err = f.s.Renew(ctx, f.makerB, f.key(), AttemptRef{ProjectID: f.project, TaskID: idle, AttemptID: b.ID, ExpectedRevision: b.Revision, Fence: stale})
	if err == nil {
		t.Fatal("old recovery epoch fence must be rejected")
	}
	// 恢复后旧代次的有效轮次经显式清扫进入对账，不计为超时。
	_, err = f.claim(f.makerB, busy)
	expectCode(t, err, errcode.TaskAlreadyClaimed)
	if n, err := f.s.SweepExpired(ctx, f.owner, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	v := f.view(busy)
	if v.Task.State != "reconciling" || v.Attempt.State != "reconciling" || v.Meta.ExpiryCount != 0 {
		t.Fatal(v.Task.State, v.Attempt.State, v.Meta.ExpiryCount)
	}
	if _, err = f.s.Reconcile(ctx, f.owner, f.key(), ReconcileRequest{ProjectID: f.project, TaskID: busy, AttemptID: a.ID, ExpectedRevision: v.Attempt.Revision, TerminationConfirmed: true, Reason: "restored instance, session gone"}); err != nil {
		t.Fatal(err)
	}
	c := f.mustClaim(f.makerA, busy)
	if c.Fence.RecoveryEpoch != 2 || c.Fence.LeaseFence != 2 {
		t.Fatal(c.Fence)
	}
}

type fakeContexts struct{ bundle catalog.ContextBundle }

func (c fakeContexts) EffectiveContext(context.Context, authz.Context, ids.ID, manifest.AssetType) (catalog.ContextBundle, error) {
	return c.bundle, nil
}

func TestContextSnapshotAndCandidateGroupAuthorization(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	doc := f.version("", f.owner.PrincipalID)
	f.s.d.Contexts = fakeContexts{catalog.ContextBundle{ProjectID: f.project, Documents: []catalog.ContextDocument{{Ref: doc}}, Digest: digest.Of([]byte("effective context"))}}
	task := f.create(CreateRequest{ContextAssetType: "model", ExpectedOutputs: []tc.OutputRequirement{{Slug: "pose", AssetType: "model", CandidateCount: 1}}}).TaskID
	v := f.view(task)
	if v.Meta.Context == nil || v.Meta.Context.Digest != digest.Of([]byte("effective context")) || len(v.Meta.Context.Refs) != 1 || v.Meta.Context.Refs[0] != doc {
		t.Fatal("task must pin the effective context version set", v.Meta.Context)
	}
	// 生效集合之后变化不改变已固定的任务输入。
	f.s.d.Contexts = fakeContexts{catalog.ContextBundle{ProjectID: f.project, Documents: []catalog.ContextDocument{}, Digest: digest.Of([]byte("newer"))}}
	if f.view(task).Meta.Context.Digest != digest.Of([]byte("effective context")) {
		t.Fatal("pinned context drifted")
	}
	a := f.mustClaim(f.makerA, task)
	out := f.version("", f.makerA.PrincipalID)
	if _, err := f.s.Submit(ctx, f.makerA, f.key(), SubmitRequest{AttemptRef: ref(f, task, a), OutputRefs: []ids.PermanentRef{out}}); err != nil {
		t.Fatal(err)
	}
	// 单候选任务不能借候选组身份绕过当前轮次绑定。
	op := f.reviewFact(task, a, 1, out.VersionID, "approve")
	fact := f.facts.reviews[op]
	fact.Target.Flow.CandidateGroup = task
	f.facts.reviews[op] = fact
	v = f.view(task)
	_, err := f.s.Complete(ctx, f.owner, f.key(), CompleteRequest{TaskRef: TaskRef{ProjectID: f.project, TaskID: task, ExpectedRevision: v.Task.Revision}, AuthorityOperationID: op})
	expectCode(t, err, errcode.ReviewTargetStale)
}

// 任务输入引用先按版本实际所属项目授权，再比较归属：可见资产配无权版本与
// 配不存在的版本得到相同错误；能读取该版本时才报告 REF_MISMATCH
// （BUG-20260930-04）。
func TestInputRefToUnreadableVersionIsIndistinguishableFromAbsent(t *testing.T) {
	f := newFixture(t)
	own := f.version("", f.owner.PrincipalID)
	foreignProject := ids.New()
	foreignAsset := ids.New()
	foreign := commit.Committed{OperationID: ids.New(), ProjectID: foreignProject, AssetID: foreignAsset, VersionID: ids.New(), VersionNumber: 1}
	f.res.mu.Lock()
	f.res.assets[foreignAsset] = commit.Asset{AssetID: foreignAsset, ProjectID: foreignProject}
	f.res.versions[foreign.VersionID] = foreign
	f.res.mu.Unlock()
	f.auth.mu.Lock()
	f.auth.hidden = map[ids.ID]bool{foreignProject: true}
	f.auth.mu.Unlock()
	create := func(ref ids.PermanentRef) error {
		_, err := f.s.Create(t.Context(), f.owner, f.key(), CreateRequest{ProjectID: f.project, Type: "produce", Title: "synthetic task",
			AcceptanceCriteria: []string{"matches the synthetic brief"}, InputRefs: []ids.PermanentRef{ref}})
		return err
	}
	unreadable := own
	unreadable.VersionID = foreign.VersionID
	absent := own
	absent.VersionID = ids.New()
	errUnreadable, errAbsent := create(unreadable), create(absent)
	expectCode(t, errUnreadable, errcode.NotFound)
	if errAbsent == nil || errUnreadable.Error() != errAbsent.Error() {
		t.Fatalf("unreadable %v differs from absent %v", errUnreadable, errAbsent)
	}
	f.auth.mu.Lock()
	f.auth.hidden = nil
	f.auth.mu.Unlock()
	expectCode(t, create(unreadable), errcode.RefMismatch)
}
