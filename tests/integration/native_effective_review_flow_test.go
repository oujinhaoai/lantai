package integration

import (
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

type nativeEffectiveFlowFixture struct {
	f      *appFlow
	flowID ids.ID
	report catalog.VersionResult
	fact   ledger.ReviewFact
	jobID  ids.ID
}

// This fixture exercises real T05/Corecheck/T03 owners, with a real separately
// enrolled human maker and an independent human Challenge/TOTP/HumanGrant.
// Its synthetic JSON is a lifecycle protocol fixture, not a DCC execution claim.
// No approval, flow state, task completion or check evidence is written via SQL.
func newNativeEffectiveFlow(t *testing.T, approveBeforeQA bool) nativeEffectiveFlowFixture {
	t.Helper()
	ctx := t.Context()
	f := newAppFlow(t)
	producer, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	profile := f.approvedProfile("profiles/native-effective", profileDocument("native-continuation", producer), ids.PermanentRef{})
	f.maker = nativeOperator(t, f)
	definition := f.definition("flows/native-effective", "org.lantai.corecheck.manifest")
	started, err := f.flows.Start(ctx, f.owner, f.key(), workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: definition.Ref, ProfileRef: profile.Ref, Title: "Synthetic native-report approval lifecycle", AcceptanceCriteria: []string{"Protocol binding only; no DCC process was executed"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "report", AssetType: "config", CandidateCount: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	produce := f.latest(f.flow(started.FlowID), "produce").StepRun.TaskIDs[0]
	attempt := f.claim(f.maker, produce)
	raw, _ := json.Marshal(map[string]any{"schema": "lantai.synthetic-native-report-lifecycle/v1", "evidence_basis": "human_attested", "machine_execution_claimed": false, "observation": "Synthetic fixed configuration report; tests only review applicability after real workflow transitions."})
	upload, files := f.uploadFiles(f.maker, map[string][]byte{"native-continuation.json": raw})
	files[0].Role = "record"
	rights := *rightsOwned()
	rights.NoAI = true
	report, err := f.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: f.maker, IdempotencyKey: f.key(), UploadID: upload, Slug: "native/effective-report", Task: bindTo(produce, attempt), Content: catalog.ContentInput{AssetType: manifest.TypeConfig, Rights: &rights, Files: files}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f.flowEnv, produce, attempt), OutputRefs: []ids.PermanentRef{report.Ref}}); err != nil {
		t.Fatal(err)
	}
	f.sync()
	f.dispatch()
	check := f.latest(f.flow(started.FlowID), "check").StepRun
	if len(check.JobIDs) != 1 {
		t.Fatalf("expected one real corecheck job, got %v", check.JobIDs)
	}
	job, err := f.app.Jobs.Read(ctx, f.owner, check.JobIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.app.Nodes.Observe(ctx, f.worker, f.key(), node.Observation{ProjectID: f.project.ProjectID, Capabilities: []string{"org.lantai.corecheck.manifest"}, Slots: 1}); err != nil {
		t.Fatal(err)
	}
	job, err = f.app.Jobs.Run(ctx, f.worker, f.key(), jobs.Control{JobID: job.ID, ExpectedRevision: job.Revision})
	if err != nil || job.State != "succeeded" || len(job.Checks) != 4 || len(job.Evidence) != 4 {
		t.Fatalf("actual corecheck did not complete: state=%s failure=%s checks=%d evidence=%d err=%v", job.State, job.Failure, len(job.Checks), len(job.Evidence), err)
	}
	for _, c := range job.Checks {
		if c.Check == nil || c.Check.Verdict != "pass" {
			t.Fatalf("corecheck %s did not pass", c.CheckKey)
		}
	}
	if _, err = f.flows.SyncJobs(ctx, f.owner, 10); err != nil {
		t.Fatal(err)
	}
	f.dispatch()
	view := f.flow(started.FlowID)
	if len(f.latest(view, "qa").StepRun.TaskIDs) != 1 {
		t.Fatal("real independent QA task was not dispatched")
	}
	var target ledger.ReviewTarget
	if approveBeforeQA {
		// Public T03 accepts this configured profile (QARequired=false) while the
		// enclosing workflow still has a QA task. Canceling only that QA task later
		// fails the Flow without invalidating the production Task or ledger review.
		flow := view.Meta.Subject.Flow
		target = f.submitChecked(report, flow, profile.Ref, job, false)
	} else {
		f.qa(started.FlowID, "pass")
		f.sync()
		f.dispatch()
		view = f.flow(started.FlowID)
		target, err = f.reviews.Target(ctx, f.owner, view.Meta.ReviewTargetID)
		if err != nil {
			t.Fatal(err)
		}
	}
	decision := f.reviewDecision(target, nil)
	decision.Reason = "Independent synthetic report lifecycle review; no native software execution claimed"
	review, err := f.recordDecision(decision)
	if err != nil {
		t.Fatal(err)
	}
	fact, err := f.reviews.ReviewByID(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review.SelfReview || review.ActorID == f.maker.PrincipalID || !review.HumanGrantID.Valid() || fact.Target.Profile.Profile.ID != "native-continuation" || fact.Target.Profile.Profile.Defaults.Publication != "manual" {
		t.Fatal("fixture must use an independent human grant and exact manual native profile")
	}
	out := nativeEffectiveFlowFixture{f: f, flowID: started.FlowID, report: report, fact: fact, jobID: job.ID}
	out.assertRetainedReview(t, "submitted")
	if err = f.reviews.ValidateEffectiveApproval(ctx, f.login().Context, fact); err != nil {
		t.Fatalf("current active approval should be applicable before the transition: %v", err)
	}
	if !approveBeforeQA {
		f.sync()
		f.dispatch()
	}
	return out
}

func (x nativeEffectiveFlowFixture) assertRetainedReview(t *testing.T, taskState tc.TaskState) {
	t.Helper()
	state, err := x.f.ledger.VersionControl(t.Context(), x.report.VersionID)
	if err != nil || state.ReviewState != "approved" || state.EffectiveReviewID != x.fact.Review.ID {
		t.Fatalf("the original exact ledger approval must remain: %+v %v", state, err)
	}
	task := x.f.task(x.fact.Target.Flow.TaskID)
	if task.Task.State != taskState || task.Attempt == nil || task.Attempt.ID != x.fact.Target.Flow.AttemptID {
		t.Fatalf("original production task/attempt changed: %s %+v", task.Task.State, task.Attempt)
	}
	job, err := x.f.app.Jobs.Read(t.Context(), x.f.owner, x.jobID)
	if err != nil || job.State != "succeeded" {
		t.Fatalf("original completed check must remain: %s %v", job.State, err)
	}
}

func TestNativeEffectiveReviewFlowLifecycle(t *testing.T) {
	t.Run("cancelled_flow_invalidates_retained_approval", func(t *testing.T) {
		x := newNativeEffectiveFlow(t, false)
		view := x.f.flow(x.flowID)
		if _, err := x.f.flows.Cancel(t.Context(), x.f.owner, x.f.key(), workflow.FlowRef{ProjectID: x.f.project.ProjectID, FlowID: x.flowID, ExpectedRevision: view.Flow.Revision, Reason: "Synthetic cancellation after actual human approval"}); err != nil {
			t.Fatal(err)
		}
		if state := x.f.flow(x.flowID).Flow.State; state != "cancelled" {
			t.Fatalf("expected cancelled Flow, got %s", state)
		}
		// Do not dispatch cancellation to the production task: the new flow guard,
		// rather than an already-cancelled Task, must reject this approval now.
		x.assertRetainedReview(t, "submitted")
		if err := x.f.reviews.ValidateEffectiveApproval(t.Context(), x.f.login().Context, x.fact); errcode.CodeOf(err) != errcode.InvalidStateTransition {
			t.Fatalf("cancelled Flow retained applicability: %v", err)
		}
	})
	t.Run("failed_flow_invalidates_retained_approval", func(t *testing.T) {
		x := newNativeEffectiveFlow(t, true)
		qa := x.f.latest(x.f.flow(x.flowID), "qa").StepRun.TaskIDs[0]
		task := x.f.task(qa)
		if _, err := x.f.tasks.Cancel(t.Context(), x.f.owner, x.f.key(), tasks.CancelRequest{TaskRef: tasks.TaskRef{ProjectID: x.f.project.ProjectID, TaskID: qa, ExpectedRevision: task.Task.Revision}, Reason: "Synthetic QA cancellation causes real enclosing Flow failure"}); err != nil {
			t.Fatal(err)
		}
		x.f.sync()
		if view := x.f.flow(x.flowID); view.Flow.State != "failed" || view.Meta.Failure != "qa_task_cancelled" {
			t.Fatalf("expected real qa_task_cancelled Flow failure: state=%s reason=%s", view.Flow.State, view.Meta.Failure)
		}
		x.assertRetainedReview(t, "submitted")
		if err := x.f.reviews.ValidateEffectiveApproval(t.Context(), x.f.login().Context, x.fact); errcode.CodeOf(err) != errcode.InvalidStateTransition {
			t.Fatalf("failed Flow retained applicability: %v", err)
		}
	})
	t.Run("normally_completed_flow_retains_exact_approval", func(t *testing.T) {
		x := newNativeEffectiveFlow(t, false)
		pointer, err := x.f.ledger.AssetControl(t.Context(), x.report.AssetID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = x.f.reviews.Publish(t.Context(), x.f.login().Context, x.f.key(), ledger.PublishRequest{ProjectID: x.f.project.ProjectID, VersionID: x.report.VersionID, ReviewID: x.fact.Review.ID, ExpectedRevision: pointer.PublicationRevision, Action: "publish", Reason: "Synthetic explicit publication of reviewed lifecycle fixture"}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 6 && x.f.flow(x.flowID).Flow.State != "completed"; i++ {
			x.f.sync()
			x.f.dispatch()
		}
		if state := x.f.flow(x.flowID).Flow.State; state != "completed" {
			t.Fatalf("normal protocol did not finish Flow: %s", state)
		}
		x.assertRetainedReview(t, "done")
		if err = x.f.reviews.ValidateEffectiveApproval(t.Context(), x.f.login().Context, x.fact); err != nil {
			t.Fatalf("normal completed Flow incorrectly invalidated its exact approval: %v", err)
		}
	})
}
