package integration

import (
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"slices"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

func inboxIDs(t *testing.T, c *query.Collaboration, f *flowEnv, who identity.IssuedSession) []ids.ID {
	t.Helper()
	page, err := c.Inbox(t.Context(), who.Context, f.project.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	out := []ids.ID{}
	for _, it := range page.Items {
		if it.Object.Ref.Kind == "task" {
			out = append(out, it.Object.Ref.ID)
		}
	}
	return out
}

// 真实 T04 收件箱、T03 讨论与 T01 里程碑通过 T05 当前任务端口读取任务事实；
// 答复只解除阻塞，不复活旧租约。
func TestM2TaskInboxDiscussionAndMilestones(t *testing.T) {
	f := newFlowEnv(t)
	ctx := t.Context()
	access, err := f.ledger.NewDiscussionObjects(f.catalog, f.rights, f.id, f.tasks)
	if err != nil {
		t.Fatal(err)
	}
	// T04 收件箱经真实台账讨论访问与 T05 任务端口展开任务讨论和任务事件。
	source, err := f.ledger.NewCollaborationSource(f.reviews, nil, access)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := query.NewCoreCollaborationObjects(source, f.id, f.tasks)
	if err != nil {
		t.Fatal(err)
	}
	log, q := f.eventQuery()
	collab, err := q.NewCollaboration(f.inst.DB(ownership.Runtime), objects)
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.tasks.Create(ctx, f.owner, f.key(), tasks.CreateRequest{ProjectID: f.project.ProjectID, Type: "question", Title: "Which palette applies?", AcceptanceCriteria: []string{"answer cites decision"}, Role: identity.RoleContributor})
	if err != nil {
		t.Fatal(err)
	}
	task := created.TaskID
	f.relay(log)
	if _, err = collab.CatchUpInbox(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	makerSession := identity.IssuedSession{Context: f.maker}
	checkerSession := identity.IssuedSession{Context: f.checker}
	if got := inboxIDs(t, collab, f, makerSession); !slices.Contains(got, task) {
		t.Fatal("role pool task missing from contributor inbox", got)
	}
	if got := inboxIDs(t, collab, f, checkerSession); slices.Contains(got, task) {
		t.Fatal("checker is not in the contributor pool", got)
	}
	// 讨论：任务目标由 T05 核对可见范围；锚点必须属于任务资源。
	question, err := f.ledger.PostMessage(ctx, f.maker, f.key(), ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: f.project.ProjectID, Kind: "task", ID: task}, Kind: "question", Text: "Is the warm palette final?"}, access)
	if err != nil {
		t.Fatal(err)
	}
	foreign := f.ingest(f.owner, "unrelated", []byte("unrelated resource"), *rightsOwned())
	if _, err = f.ledger.PostMessage(ctx, f.maker, f.key(), ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: f.project.ProjectID, Kind: "task", ID: task}, Kind: "note", Text: "see file", Anchors: []ledger.Anchor{{Ref: foreign.Ref, FilePath: "content.txt", Kind: "line", Values: []float64{1, 1}}}}, access); errcode.CodeOf(err) != errcode.RefMismatch {
		t.Fatal("anchor outside task resources accepted", err)
	}
	a := f.claim(f.maker, task)
	if _, err = f.tasks.Block(ctx, f.maker, f.key(), tasks.BlockRequest{AttemptRef: attemptRef(f, task, a), QuestionMessageID: question.ID}); err != nil {
		t.Fatal(err)
	}
	f.relay(log)
	if _, err = collab.CatchUpInbox(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if got := inboxIDs(t, collab, f, identity.IssuedSession{Context: f.owner}); !slices.Contains(got, task) {
		t.Fatal("blocked task waits in the creator's inbox", got)
	}
	answer, err := f.ledger.PostMessage(ctx, f.owner, f.key(), ledger.MessageInput{Target: ledger.DiscussionTarget{ProjectID: f.project.ProjectID, Kind: "task", ID: task}, Kind: "answer", Text: "Yes, warm palette is decided.", ReplyTo: question.ID}, access)
	if err != nil {
		t.Fatal(err)
	}
	v := f.task(task)
	if _, err = f.tasks.Answer(ctx, f.owner, f.key(), tasks.AnswerRequest{TaskRef: tasks.TaskRef{ProjectID: f.project.ProjectID, TaskID: task, ExpectedRevision: v.Task.Revision}, BlockID: v.Blocks[0].ID, AnswerMessageID: answer.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Renew(ctx, f.maker, f.key(), attemptRef(f, task, *f.task(task).Attempt)); errcode.CodeOf(err) != errcode.LeaseStale {
		t.Fatal("answer revived the old lease", err)
	}
	v = f.task(task)
	if _, err = f.tasks.Reconcile(ctx, f.owner, f.key(), tasks.ReconcileRequest{ProjectID: f.project.ProjectID, TaskID: task, AttemptID: a.ID, ExpectedRevision: v.Attempt.Revision, TerminationConfirmed: true, Reason: "agent session ended"}); err != nil {
		t.Fatal(err)
	}
	// 里程碑进度来自 T05 当前任务事实，跨项目或缺失任务报错。
	res, err := f.id.PutMilestone(ctx, f.owner, f.key(), 0, identity.Milestone{ProjectID: f.project.ProjectID, Name: "chapter 3", OwnerID: f.owner.PrincipalID, TaskIDs: []ids.ID{task}}, f.tasks)
	if err != nil {
		t.Fatal(err)
	}
	var m identity.Milestone
	if err = json.Unmarshal(res.Summary, &m); err != nil {
		t.Fatal(err)
	}
	if _, err = f.id.PutMilestone(ctx, f.owner, f.key(), 0, identity.Milestone{ProjectID: f.project.ProjectID, Name: "bad", OwnerID: f.owner.PrincipalID, TaskIDs: []ids.ID{ids.New()}}, f.tasks); err == nil {
		t.Fatal("unknown task accepted in milestone")
	}
	b := f.claim(f.maker, task)
	if b.Fence.LeaseFence != 2 {
		t.Fatal("reclaim after answer uses a new attempt", b)
	}
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, task, b)}); err != nil {
		t.Fatal(err)
	}
	v = f.task(task)
	if _, err = f.tasks.Complete(ctx, f.owner, f.key(), tasks.CompleteRequest{TaskRef: tasks.TaskRef{ProjectID: f.project.ProjectID, TaskID: task, ExpectedRevision: v.Task.Revision}, AuthorityOperationID: v.Meta.SubmitOperationID}); err != nil {
		t.Fatal(err)
	}
	p, err := f.id.MilestoneProgress(ctx, f.owner, f.project.ProjectID, m.ID, f.tasks)
	if err != nil || p.Completed != 1 || p.Total != 1 {
		t.Fatal(p, err)
	}
	f.relay(log)
	if _, err = collab.CatchUpInbox(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if got := inboxIDs(t, collab, f, makerSession); slices.Contains(got, task) {
		t.Fatal("finished task still pending", got)
	}
}

func (f *flowEnv) start(title string) ids.ID {
	f.t.Helper()
	r, err := f.flows.Start(f.t.Context(), f.owner, f.key(), workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: f.definition.Ref, ProfileRef: f.profile.Ref, Title: title,
		AcceptanceCriteria: []string{"synthetic acceptance"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "out", AssetType: "doc", CandidateCount: 1}}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.dispatch()
	return r.FlowID
}

// deliver 让制作者领取当前制作任务、提交一个新版本并交付。
func (f *flowEnv) deliver(flow ids.ID, slug string, asset, base ids.ID) (tasks.Attempt, ids.PermanentRef) {
	f.t.Helper()
	v := f.flow(flow)
	task := f.latest(v, "produce").StepRun.TaskIDs[0]
	a := f.claim(f.maker, task)
	out, err := f.commitDoc(f.maker, slug, asset, base, []byte("delivery "+slug+string(a.ID)), bindTo(task, a))
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.tasks.Submit(f.t.Context(), f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, task, a), OutputRefs: []ids.PermanentRef{out.Ref}}); err != nil {
		f.t.Fatal(err)
	}
	f.sync()
	return a, out.Ref
}

// 检查失败与质检失败开启返工；审定拒绝使流程失败并取消制作任务；暂停阻止
// 派发与审定，取消传播到仍在执行的任务并等待对账。
func TestM2FlowFailuresPauseCancelAndRestart(t *testing.T) {
	f := newFlowEnv(t)
	ctx := t.Context()

	// 1) 检查失败 → 返工；质检失败 → 返工；超过定义上限前按轮次保留历史。
	flow := f.start("check and qa failures")
	_, out := f.deliver(flow, "fail-check", "", "")
	f.jobs.fail = true
	f.dispatch()
	if _, err := f.flows.SyncJobs(ctx, f.owner, 10); err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	v := f.flow(flow)
	if f.latest(v, "check").Meta.Outcome != "check_fail" || v.Meta.ProductionRound != 2 {
		t.Fatalf("legitimate check failure is evidence for rework: %+v", v.Meta)
	}
	f.jobs.fail = false
	_, out2 := f.deliver(flow, "", out.AssetID, out.VersionID)
	f.dispatch()
	if _, err := f.flows.SyncJobs(ctx, f.owner, 10); err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	f.qa(flow, "fail")
	f.sync()
	f.dispatch()
	v = f.flow(flow)
	if f.latest(v, "qa").Meta.Outcome != "qa_fail" || v.Meta.ProductionRound != 3 || len(v.Meta.Rounds) != 2 {
		t.Fatalf("%+v", v.Meta)
	}

	// 2) 审定拒绝：流程失败，已交付的制作任务取消；重试不能推翻业务结论。
	_, _ = f.deliver(flow, "", out.AssetID, out2.VersionID)
	f.dispatch()
	if _, err := f.flows.SyncJobs(ctx, f.owner, 10); err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	f.qa(flow, "pass")
	f.sync()
	f.dispatch()
	v = f.flow(flow)
	f.decide(v.Meta.ReviewTargetID, "reject")
	f.sync()
	f.dispatch()
	v = f.flow(flow)
	if v.Flow.State != "failed" || v.Meta.Failure != "review_rejected" || f.task(v.Meta.ProduceTaskID).Task.State != "cancelled" {
		t.Fatalf("rejection ends the flow: %+v", v.Meta)
	}
	if _, err := f.flows.Retry(ctx, f.owner, f.key(), workflow.FlowRef{ProjectID: f.project.ProjectID, FlowID: flow, ExpectedRevision: v.Flow.Revision, Reason: "try again"}); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("business rejection is not retryable", err)
	}

	// 3) 暂停：不派发、拒绝审定；恢复后继续。取消：执行中的任务先对账再取消。
	flow2 := f.start("pause and cancel")
	v = f.flow(flow2)
	task := f.latest(v, "produce").StepRun.TaskIDs[0]
	a := f.claim(f.maker, task)
	out3, err := f.commitDoc(f.maker, "paused", "", "", []byte("paused delivery"), bindTo(task, a))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.flows.Pause(ctx, f.owner, f.key(), workflow.FlowRef{ProjectID: f.project.ProjectID, FlowID: flow2, ExpectedRevision: v.Flow.Revision, Reason: "art direction review"}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, task, a), OutputRefs: []ids.PermanentRef{out3.Ref}}); err != nil {
		t.Fatal("agents may still deliver while a flow is paused", err)
	}
	f.sync()
	if n, err := f.flows.Dispatch(ctx, f.owner, 10); err != nil || n != 0 {
		t.Fatal("paused flow commands must not dispatch", n, err)
	}
	if err = f.flows.ReviewExecution().Task(ctx, f.owner, f.versionOf(out3.Ref), ledger.ReviewFlow{TaskID: task, AttemptID: a.ID, Round: 1, Fence: a.Fence.LeaseFence}, "review"); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("paused flow accepted a review", err)
	}
	v = f.flow(flow2)
	if _, err = f.flows.Resume(ctx, f.owner, f.key(), workflow.FlowRef{ProjectID: f.project.ProjectID, FlowID: flow2, ExpectedRevision: v.Flow.Revision, Reason: "approved direction"}); err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	if len(f.latest(f.flow(flow2), "check").StepRun.JobIDs) != 1 {
		t.Fatal("resumed flow dispatches its persisted command")
	}

	flow3 := f.start("cancel while working")
	v = f.flow(flow3)
	task3 := f.latest(v, "produce").StepRun.TaskIDs[0]
	a3 := f.claim(f.maker, task3)
	if err = f.flows.RequireProjectIdle(ctx, f.project.ProjectID); errcode.CodeOf(err) != errcode.AssetInUse {
		t.Fatal("project with open flows is not idle", err)
	}
	if _, err = f.flows.Cancel(ctx, f.owner, f.key(), workflow.FlowRef{ProjectID: f.project.ProjectID, FlowID: flow3, ExpectedRevision: v.Flow.Revision, Reason: "scope dropped"}); err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	tv := f.task(task3)
	if tv.Task.State != "reconciling" || !tv.Meta.CancelRequested || tv.Attempt.State != "reconciling" {
		t.Fatalf("cancel propagates but waits for the running attempt: %+v", tv.Task)
	}
	if _, err = f.commitDoc(f.maker, "late", "", "", []byte("late result"), bindTo(task3, a3)); errcode.CodeOf(err) != errcode.LeaseStale {
		t.Fatal("revoked attempt created an asset", err)
	}
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, task3, a3)}); errcode.CodeOf(err) != errcode.LeaseStale {
		t.Fatal("cancelled attempt delivered", err)
	}
	if _, err = f.tasks.Reconcile(ctx, f.owner, f.key(), tasks.ReconcileRequest{ProjectID: f.project.ProjectID, TaskID: task3, AttemptID: a3.ID, ExpectedRevision: tv.Attempt.Revision, TerminationConfirmed: true, Reason: "session stopped"}); err != nil {
		t.Fatal(err)
	}
	if f.task(task3).Task.State != "cancelled" || f.flow(flow3).Flow.State != "cancelled" {
		t.Fatal("cancellation completes after reconciliation")
	}

	// 4) 重启：以新服务实例接续同一运行库，重复投递与重复派发不产生新任务。
	restarted, err := workflow.New(workflow.Deps{DB: f.inst.DB("runtime"), Gate: f.inst.Gate(), Authority: authority{Service: f.id, epochs: f.inst}, Tasks: f.tasks, Ledger: f.ledger, Reviews: f.reviews, Manifests: f.storage, Events: f.log, Jobs: f.jobs, Checks: f.jobs, Clock: f.clk, IDs: &ids.Generator{Clock: f.clk}, InstanceID: f.inst.InstanceID()})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := f.tasks.List(ctx, f.owner, f.project.ProjectID, "", 500, false)
	if _, err = f.inst.DB("runtime").ExecContext(ctx, `UPDATE workflow_commands SET status='pending',actor_id='',retry_at=0 WHERE command_type='tasks.create'`); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(time.Minute)
	if _, err = restarted.Dispatch(ctx, f.owner, 100); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.CatchUp(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	after, _ := f.tasks.List(ctx, f.owner, f.project.ProjectID, "", 500, false)
	if len(after) != len(before) {
		t.Fatal("redelivered create commands duplicated tasks", len(before), len(after))
	}
}

func bindTo(task ids.ID, a tasks.Attempt) *catalog.TaskBinding {
	return &catalog.TaskBinding{TaskID: task, AttemptID: a.ID, LeaseFence: a.Fence.LeaseFence}
}

// 修改流程在开始时把目标资产的当前最新版本固定为输入，制作任务领取即独占
// 签出；流程未结束时资产归档所需的 T05 空闲判断拒绝。
func TestM2ModifyFlowChecksOutTargetAndBlocksArchival(t *testing.T) {
	f := newFlowEnv(t)
	ctx := t.Context()
	def, err := workflow.BuiltinDefinition("modify")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(def)
	var doc any
	_ = json.Unmarshal(raw, &doc)
	modify := f.config(f.owner, "flows/modify", map[string]any{"flow_definition": doc})
	target, err := f.commitDoc(f.owner, "published-hero", "", "", []byte("published hero v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.flows.Start(ctx, f.owner, f.key(), workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: modify.Ref, ProfileRef: f.profile.Ref, Title: "retouch", AcceptanceCriteria: []string{"keep silhouette"}}); errcode.CodeOf(err) != errcode.SchemaInvalid {
		t.Fatal("modify flow without target accepted", err)
	}
	started, err := f.flows.Start(ctx, f.owner, f.key(), workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: modify.Ref, ProfileRef: f.profile.Ref, TargetAssetID: target.AssetID, Title: "retouch", AcceptanceCriteria: []string{"keep silhouette"}})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	v := f.flow(started.FlowID)
	if len(v.Flow.Input.Refs) != 1 || v.Flow.Input.Refs[0] != target.Ref {
		t.Fatal("modify flow pins the target's current version", v.Flow.Input)
	}
	task := f.latest(v, "produce").StepRun.TaskIDs[0]
	if tv := f.task(task); len(tv.Meta.Checkouts) != 1 || tv.Meta.Checkouts[0].AssetID != target.AssetID || tv.Meta.Checkouts[0].Mode != "exclusive" {
		t.Fatal(tv.Meta.Checkouts)
	}
	if err = f.flows.RequireIdle(ctx, f.project.ProjectID, []ids.ID{target.AssetID}); errcode.CodeOf(err) != errcode.AssetInUse {
		t.Fatal("asset under an open flow reported idle", err)
	}
	a := f.claim(f.maker, task)
	if _, err = f.commitDoc(f.owner, "", target.AssetID, target.VersionID, []byte("owner hotfix"), nil); errcode.CodeOf(err) != errcode.AssetCheckedOut {
		t.Fatal("checked-out asset accepted an unbound write", err)
	}
	if _, err = f.commitDoc(f.maker, "", target.AssetID, target.VersionID, []byte("retouched"), bindTo(task, a)); err != nil {
		t.Fatal(err)
	}
	// 同一定义摘要固定在流程与步骤上；修改配置资产不影响已运行的实例。
	for _, s := range v.Steps {
		if s.StepRun.Definition != v.Flow.Definition {
			t.Fatal("step changed definition binding")
		}
	}
}
