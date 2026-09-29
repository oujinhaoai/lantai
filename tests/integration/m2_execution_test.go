package integration

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"testing"
	"time"

	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

func executionEnv(t *testing.T) (*flowEnv, *ax.Service, ae.StartRequest, tasks.Attempt) {
	t.Helper()
	f := newFlowEnv(t)
	ctx := t.Context()
	s, e := ax.New(ax.Deps{DB: f.inst.DB(ownership.Runtime), Gate: f.inst.Gate(), Authority: authority{Service: f.id, epochs: f.inst}, Tasks: f.tasks, Assets: ax.CatalogAssets{Files: f.storage, Evidence: f.rights, Catalog: f.catalog, InstanceID: f.inst.InstanceID()}, IDs: &ids.Generator{Clock: f.clk, Rand: rand.Reader}, Clock: f.clk})
	if e != nil {
		t.Fatal(e)
	}
	f.tasks.SetExecutions(s)
	created, e := f.tasks.Create(ctx, f.owner, f.key(), tasks.CreateRequest{ProjectID: f.project.ProjectID, Type: "question", Title: "manual session", AcceptanceCriteria: []string{"documented answer"}, Role: identity.RoleContributor})
	if e != nil {
		t.Fatal(e)
	}
	a := f.claim(f.maker, created.TaskID)
	raw, e := os.ReadFile("../../schemas/examples/agent-execution/v1/agent-request/start.json")
	if e != nil {
		t.Fatal(e)
	}
	var wrap struct {
		Document ae.StartRequest `json:"document"`
	}
	if e = json.Unmarshal(raw, &wrap); e != nil {
		t.Fatal(e)
	}
	in := wrap.Document
	in.Fence = a.Fence
	in.Fence.TaskID = created.TaskID
	in.OperationID = ids.New()
	in.TaskRunID = ids.New()
	in.BudgetID = ids.New()
	in.Input = f.task(created.TaskID).Task.Input
	in.ExecutionKey, _ = ae.ExecutionKey(in.TaskRunID, a.ID)
	in.Activation = nil
	in.Profile.ActivationSnapshot = ae.ActivationSnapshot{}
	in.Profile.PlaybookRefs = []ids.PermanentRef{}
	in.Profile.RequiredCapabilities = []string{"manual_session"}
	in.Profile.AllowedTools = []string{"catalog.read"}
	in.RequestHash, e = ae.RequestHash(in)
	if e != nil {
		t.Fatal(e)
	}
	return f, s, in, a
}
func mut(r ax.Record, f execution.Fence) ax.Mutation {
	return ax.Mutation{RunID: r.Run.ID, ExpectedRevision: r.Run.Revision, Fence: f}
}
func TestM2ManualRegistrationReplayAndCancellation(t *testing.T) {
	f, s, in, a := executionEnv(t)
	ctx := t.Context()
	r, e := s.Start(ctx, f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Start(ctx, f.maker, f.key(), in)
	if e != nil || again.Current.ExecutionID != r.Current.ExecutionID {
		t.Fatal("lost response replay", again, e)
	}
	changed := in
	changed.BudgetID = ids.New()
	changed.RequestHash, _ = ae.RequestHash(changed)
	if _, e = s.Start(ctx, f.maker, f.key(), changed); errcode.CodeOf(e) != errcode.StartKeyConflict {
		t.Fatal("changed hash", e)
	}
	key := f.key()
	p := ax.ProgressRequest{Mutation: mut(r, in.Fence), Usage: ae.BudgetUsage{ModelCalls: 2}}
	r, e = s.Progress(ctx, f.maker, key, p)
	if e != nil {
		t.Fatal(e)
	}
	dup, e := s.Progress(ctx, f.maker, key, p)
	if e != nil || dup.Run.Revision != r.Run.Revision || dup.Usage.ModelCalls != 2 {
		t.Fatal("duplicate observation", e)
	}
	p.Mutation = mut(r, in.Fence)
	p.Usage.ModelCalls = 1
	if _, e = s.Progress(ctx, f.maker, f.key(), p); errcode.CodeOf(e) != errcode.SchemaInvalid {
		t.Fatal("budget reset", e)
	}
	p.Usage.ModelCalls = 2
	p.Fence.LeaseFence++
	if _, e = s.Progress(ctx, f.maker, f.key(), p); errcode.CodeOf(e) != errcode.LeaseStale {
		t.Fatal("stale fence", e)
	}
	r, e = s.Cancel(ctx, f.maker, f.key(), ax.CancelRequest{Mutation: mut(r, in.Fence), Mode: execution.CancelRevokeOnly, Reason: "stop requested"})
	if e != nil || r.Run.State != execution.RunCancelling || r.Run.Termination.Confirmed {
		t.Fatal("false stop confirmation", r, e)
	}
	if _, e = s.Progress(ctx, f.maker, f.key(), ax.ProgressRequest{Mutation: mut(r, in.Fence), Usage: r.Usage}); errcode.CodeOf(e) != errcode.InvalidStateTransition {
		t.Fatal("write after cancel", e)
	}
	if _, e = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, in.Fence.TaskID, a)}); errcode.CodeOf(e) != errcode.LeaseStale {
		t.Fatal("task submit bypassed run cancellation", e)
	}
	if _, e = f.commitDoc(f.maker, "cancelled-output", "", "", []byte("late result"), bindTo(in.Fence.TaskID, a)); errcode.CodeOf(e) != errcode.LeaseStale {
		t.Fatal("catalog commit bypassed run cancellation", e)
	}
	proof := f.ingest(f.owner, "stop-proof", []byte("session has stopped"), *rightsOwned())
	r, e = s.Reconcile(ctx, f.owner, f.key(), ax.ReconcileRequest{Mutation: mut(r, in.Fence), Stopped: true, EvidenceRefs: []ids.PermanentRef{proof.Ref}, Reason: "operator confirmed"})
	if e != nil || r.Run.State != execution.RunCancelled {
		t.Fatal(r, e)
	}
	if f.task(in.Fence.TaskID).Task.State == "done" {
		t.Fatal("execution changed task approval")
	}
}
func TestM2ManualCheckpointResumeCumulativeBudget(t *testing.T) {
	f, s, in, a := executionEnv(t)
	ctx := t.Context()
	r, e := s.Start(ctx, f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	r, e = s.Progress(ctx, f.maker, f.key(), ax.ProgressRequest{Mutation: mut(r, in.Fence), Usage: ae.BudgetUsage{ModelCalls: 4}})
	if e != nil {
		t.Fatal(e)
	}
	cp := ae.Checkpoint{Contract: "lantai.checkpoint/v1", ID: ids.New(), TaskRunID: r.Run.ID, AttemptID: a.ID, Sequence: 1, InputSnapshotDigest: r.Run.Input.Digest, ProfileDigest: r.Run.Profile.Digest, BackendVersion: "1.0.0", FormatVersion: "1", ResumeClass: execution.ResumePortableArtifacts, ArtifactRefs: []ids.PermanentRef{}, CompletedStepIDs: []ids.ID{}, CreatedAt: clock.Format(f.clk.Now())}
	r, e = s.Checkpoint(ctx, f.maker, f.key(), ax.CheckpointRequest{Mutation: mut(r, in.Fence), Checkpoint: cp, Stopped: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.tasks.Release(ctx, f.maker, f.key(), tasks.ReleaseRequest{AttemptRef: attemptRef(f, in.Fence.TaskID, a), Reason: "handoff", Stopped: true}); e != nil {
		t.Fatal(e)
	}
	b := f.claim(f.maker, in.Fence.TaskID)
	originalStart := in
	originalAdmission := r.Current
	in.Action = execution.ActResume
	in.Fence = b.Fence
	in.Fence.TaskID = b.TaskID
	in.ExecutionKey, _ = ae.ExecutionKey(r.Run.ID, b.ID)
	in.ResumeFrom = &r.Checkpoints[0]
	in.RequestHash, _ = ae.RequestHash(in)
	r, e = s.Start(ctx, f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	oldReplay, replayErr := s.Start(ctx, f.maker, f.key(), originalStart)
	if replayErr != nil || oldReplay.Current.ExecutionID != originalAdmission.ExecutionID {
		t.Fatal("old start changed admission", oldReplay, replayErr)
	}
	if r.Usage.ModelCalls != 4 {
		t.Fatal("retry reset usage")
	}
	r, e = s.Seal(ctx, f.maker, f.key(), ax.SealRequest{Mutation: mut(r, in.Fence), Outcome: execution.RunExecutionSucceeded, Stopped: true, CandidateIDs: []ids.ID{}, Limitations: []string{"manual observations"}})
	if e != nil || r.Result == nil {
		t.Fatal(r, e)
	}
	if f.task(in.Fence.TaskID).Task.State != "claimed" {
		t.Fatal("execution success completed task")
	}
}
func TestM2ManualExpiredLeaseRejectsProgress(t *testing.T) {
	f, s, in, _ := executionEnv(t)
	r, e := s.Start(t.Context(), f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	f.clk.Advance(31 * time.Minute)
	if _, e = s.Progress(t.Context(), f.maker, f.key(), ax.ProgressRequest{Mutation: mut(r, in.Fence), Usage: r.Usage}); errcode.CodeOf(e) != errcode.LeaseStale {
		t.Fatal(e)
	}
}

func manualCheckpoint(f *flowEnv, r ax.Record) ae.Checkpoint {
	return ae.Checkpoint{Contract: "lantai.checkpoint/v1", ID: ids.New(), TaskRunID: r.Run.ID, AttemptID: r.Run.CurrentAttemptID, Sequence: int64(len(r.Checkpoints) + 1), InputSnapshotDigest: r.Run.Input.Digest, ProfileDigest: r.Run.Profile.Digest, BackendVersion: "1.0.0", FormatVersion: "1", ResumeClass: execution.ResumePortableArtifacts, ArtifactRefs: []ids.PermanentRef{}, CompletedStepIDs: []ids.ID{}, SideEffectWatermark: int64(len(r.Tools)), CreatedAt: clock.Format(f.clk.Now())}
}
func TestM2ManualInputAndToolReconciliation(t *testing.T) {
	f, s, in, _ := executionEnv(t)
	ctx := t.Context()
	r, e := s.Start(ctx, f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	step := ae.AgentStepRun{Contract: "lantai.agent-step-run/v1", ID: ids.New(), TaskRunID: r.Run.ID, AttemptID: r.Run.CurrentAttemptID, Revision: 1, PlanRevision: 1, Kind: "tool", State: "running", InputRefs: []ids.PermanentRef{}, ToolOperationIDs: []ids.ID{}, OutputRefs: []ids.PermanentRef{}}
	r, e = s.Progress(ctx, f.maker, f.key(), ax.ProgressRequest{Mutation: mut(r, in.Fence), Usage: r.Usage, Step: &step})
	if e != nil {
		t.Fatal(e)
	}
	tool := ae.ToolOperation{Contract: "lantai.tool-operation/v1", ID: ids.New(), TaskRunID: r.Run.ID, AttemptID: r.Run.CurrentAttemptID, AgentStepRunID: step.ID, Sequence: 1, Tool: "catalog.read", ToolVersion: "1.0.0", RequestHash: digest.Of([]byte("request")), EffectClass: execution.EffectExternalNonIdempotent, State: execution.EffectIntended, ReconciliationEvidenceRefs: []ids.ID{}}
	for _, state := range []execution.EffectState{execution.EffectIntended, execution.EffectDispatched, execution.EffectUnknown} {
		tool.State = state
		r, e = s.Tool(ctx, f.maker, f.key(), ax.ToolRequest{Mutation: mut(r, in.Fence), Tool: tool})
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.Seal(ctx, f.maker, f.key(), ax.SealRequest{Mutation: mut(r, in.Fence), Outcome: execution.RunExecutionSucceeded, Stopped: true, CandidateIDs: []ids.ID{}, Limitations: []string{}}); errcode.CodeOf(e) != errcode.OperationNeedsReconciliation {
		t.Fatal("unknown effect sealed", e)
	}
	next := tool
	next.ID = ids.New()
	next.Sequence = 2
	next.State = execution.EffectIntended
	if _, e = s.Tool(ctx, f.maker, f.key(), ax.ToolRequest{Mutation: mut(r, in.Fence), Tool: next}); errcode.CodeOf(e) != errcode.OperationNeedsReconciliation {
		t.Fatal("new key bypassed unknown effect", e)
	}
	proof := f.ingest(f.owner, "tool-proof", []byte("operator verified no external write"), *rightsOwned())
	r, e = s.Reconcile(ctx, f.owner, f.key(), ax.ReconcileRequest{Mutation: mut(r, in.Fence), Stopped: true, ToolID: tool.ID, ToolState: execution.EffectNotExecuted, EvidenceRefs: []ids.PermanentRef{proof.Ref}, Reason: "checked original operation", Checkpoint: ptrCheckpoint(manualCheckpoint(f, r))})
	if e != nil || r.Run.State != execution.RunPaused || r.CancelRequested || len(r.Reconciliations) != 1 || r.Reconciliations[0].Request.EvidenceRefs[0] != proof.Ref {
		t.Fatal("recovery became cancellation or lost evidence", r, e)
	}
}
func ptrCheckpoint(cp ae.Checkpoint) *ae.Checkpoint { return &cp }
func TestM2ManualHumanInputDigestAndNoApproval(t *testing.T) {
	f, s, in, _ := executionEnv(t)
	ctx := t.Context()
	r, e := s.Start(ctx, f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	q := ax.HumanInput{ID: ids.New(), Kind: "budget_change", Question: "Need another attempt?", ExpiresAt: clock.Format(f.clk.Now().Add(time.Hour))}
	r, e = s.Ask(ctx, f.maker, f.key(), ax.InputRequest{CheckpointRequest: ax.CheckpointRequest{Mutation: mut(r, in.Fence), Stopped: true, Checkpoint: manualCheckpoint(f, r)}, Input: q})
	if e != nil {
		t.Fatal(e)
	}
	q = r.Inputs[0]
	if !q.TargetDigest.Valid() {
		t.Fatal("server omitted fixed input digest")
	}
	answer := ax.AnswerRequest{Mutation: mut(r, in.Fence), InputID: q.ID, TargetDigest: q.TargetDigest, Answer: "prepare a new bounded plan"}
	if _, e = s.Answer(ctx, f.maker, f.key(), answer); errcode.CodeOf(e) != errcode.Forbidden {
		t.Fatal("agent answered human input", e)
	}
	changed := answer
	changed.TargetDigest = digest.Of([]byte("another target"))
	if _, e = s.Answer(ctx, f.owner, f.key(), changed); errcode.CodeOf(e) != errcode.SchemaInvalid {
		t.Fatal(e)
	}
	r, e = s.Answer(ctx, f.owner, f.key(), answer)
	if e != nil || r.Run.State != execution.RunPaused || r.Profile.BudgetLimits != in.Profile.BudgetLimits {
		t.Fatal("answer raised budget or resumed", r, e)
	}
	if f.task(in.Fence.TaskID).Task.State != "claimed" {
		t.Fatal("human clarification accepted task")
	}
}
func TestM2ManualCandidateMatchesCommittedManifest(t *testing.T) {
	f, s, in, a := executionEnv(t)
	ctx := t.Context()
	r, e := s.Start(ctx, f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	data := []byte("candidate bytes")
	v, e := f.commitDoc(f.maker, "candidate", "", "", data, bindTo(in.Fence.TaskID, a))
	if e != nil {
		t.Fatal(e)
	}
	candidate := ae.ArtifactCandidate{Contract: "lantai.artifact-candidate/v1", ID: ids.New(), TaskRunID: r.Run.ID, AttemptID: a.ID, ManifestDigest: v.ManifestDigest, Files: []ae.ArtifactFile{{Path: "content.txt", SHA256: shaOf(data), Size: int64(len(data))}}, Purpose: "synthetic answer", ProvenanceRefs: []ids.ID{}, LicenseEvidenceRefs: []ids.ID{}, ValidationState: "pending"}
	forged := candidate
	forged.ManifestDigest = digest.Of([]byte("other content"))
	if _, e = s.Candidate(ctx, f.maker, f.key(), ax.CandidateRequest{Mutation: mut(r, in.Fence), Candidate: forged, Ref: v.Ref}); errcode.CodeOf(e) != errcode.RefMismatch {
		t.Fatal(e)
	}
	forged = candidate
	forged.ProvenanceRefs = []ids.ID{ids.New()}
	if _, e = s.Candidate(ctx, f.maker, f.key(), ax.CandidateRequest{Mutation: mut(r, in.Fence), Candidate: forged, Ref: v.Ref}); errcode.CodeOf(e) != errcode.RefMismatch {
		t.Fatal(e)
	}
	r, e = s.Candidate(ctx, f.maker, f.key(), ax.CandidateRequest{Mutation: mut(r, in.Fence), Candidate: candidate, Ref: v.Ref})
	if e != nil {
		t.Fatal(e)
	}
	if r.Usage.ArtifactBytes != int64(len(data)) {
		t.Fatal("candidate usage not counted")
	}
	r, e = s.Seal(ctx, f.maker, f.key(), ax.SealRequest{Mutation: mut(r, in.Fence), Outcome: execution.RunExecutionSucceeded, Stopped: true, CandidateIDs: []ids.ID{candidate.ID}, Limitations: []string{}})
	if e != nil {
		t.Fatal(e)
	}
	if r.Result == nil || len(r.Result.CandidateIDs) != 1 {
		t.Fatal(r)
	}
}

func TestM2ManualRestartLookupAndSupersede(t *testing.T) {
	f, s, in, a := executionEnv(t)
	ctx := t.Context()
	r, e := s.Start(ctx, f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	// A new owner service has no in-memory map but recovers the same admission.
	restarted, e := ax.New(ax.Deps{DB: f.inst.DB(ownership.Runtime), Gate: f.inst.Gate(), Authority: authority{Service: f.id, epochs: f.inst}, Tasks: f.tasks, Assets: ax.CatalogAssets{Files: f.storage, Evidence: f.rights, Catalog: f.catalog, InstanceID: f.inst.InstanceID()}, IDs: &ids.Generator{Clock: f.clk, Rand: rand.Reader}, Clock: f.clk})
	if e != nil {
		t.Fatal(e)
	}
	replay, e := restarted.Start(ctx, f.maker, f.key(), in)
	if e != nil || replay.Current.ExecutionID != r.Current.ExecutionID {
		t.Fatal(replay, e)
	}
	r, e = s.Checkpoint(ctx, f.maker, f.key(), ax.CheckpointRequest{Mutation: mut(r, in.Fence), Checkpoint: manualCheckpoint(f, r), Stopped: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.tasks.Release(ctx, f.maker, f.key(), tasks.ReleaseRequest{AttemptRef: attemptRef(f, in.Fence.TaskID, a), Reason: "new profile", Stopped: true}); e != nil {
		t.Fatal(e)
	}
	b := f.claim(f.maker, in.Fence.TaskID)
	next := in
	next.TaskRunID = ids.New()
	next.OperationID = ids.New()
	next.BudgetID = ids.New()
	next.Fence = b.Fence
	next.Fence.TaskID = b.TaskID
	next.ExecutionKey, _ = ae.ExecutionKey(next.TaskRunID, b.ID)
	next.RequestHash, _ = ae.RequestHash(next)
	if _, e = s.Start(ctx, f.maker, f.key(), next); errcode.CodeOf(e) != errcode.InvalidStateTransition {
		t.Fatal("reset budget without changed input/profile", e)
	}
	next.Profile.Revision++
	next.RequestHash, _ = ae.RequestHash(next)
	newer, e := s.Start(ctx, f.maker, f.key(), next)
	if e != nil || newer.Run.SupersedesRunID != r.Run.ID {
		t.Fatal(newer, e)
	}
	if _, e = s.Progress(ctx, f.maker, f.key(), ax.ProgressRequest{Mutation: mut(r, in.Fence), Usage: r.Usage}); e == nil {
		t.Fatal("superseded attempt accepted progress")
	}
}

func TestM2ManualStepConcurrencyAndIntentLinks(t *testing.T) {
	f, s, in, _ := executionEnv(t)
	in.Profile.BudgetLimits.Concurrency = 1
	in.RequestHash, _ = ae.RequestHash(in)
	r, e := s.Start(t.Context(), f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	step := ae.AgentStepRun{Contract: "lantai.agent-step-run/v1", ID: ids.New(), TaskRunID: r.Run.ID, AttemptID: r.Run.CurrentAttemptID, Revision: 1, PlanRevision: 1, Kind: "tool", State: "running", InputRefs: []ids.PermanentRef{}, ToolOperationIDs: []ids.ID{}, OutputRefs: []ids.PermanentRef{}}
	r, e = s.Progress(t.Context(), f.maker, f.key(), ax.ProgressRequest{Mutation: mut(r, in.Fence), Usage: r.Usage, Step: &step})
	if e != nil {
		t.Fatal(e)
	}
	second := step
	second.ID = ids.New()
	if _, e = s.Progress(t.Context(), f.maker, f.key(), ax.ProgressRequest{Mutation: mut(r, in.Fence), Usage: r.Usage, Step: &second}); errcode.CodeOf(e) != errcode.BudgetExhausted {
		t.Fatal(e)
	}
	tool := ae.ToolOperation{Contract: "lantai.tool-operation/v1", ID: ids.New(), TaskRunID: r.Run.ID, AttemptID: r.Run.CurrentAttemptID, AgentStepRunID: step.ID, Sequence: 1, Tool: "catalog.read", ToolVersion: "1.0.0", RequestHash: digest.Of([]byte("read")), EffectClass: execution.EffectExternalNonIdempotent, State: execution.EffectIntended, ReconciliationEvidenceRefs: []ids.ID{}}
	r, e = s.Tool(t.Context(), f.maker, f.key(), ax.ToolRequest{Mutation: mut(r, in.Fence), Tool: tool})
	if e != nil || len(r.Steps[0].ToolOperationIDs) != 1 || r.Steps[0].ToolOperationIDs[0] != tool.ID {
		t.Fatal(r, e)
	}
}

func TestM2ManualOfflineRecoveryRetainsExecutionGuard(t *testing.T) {
	f, s, in, a := executionEnv(t)
	ctx := t.Context()
	r, e := s.Start(ctx, f.maker, f.key(), in)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Cancel(ctx, f.maker, f.key(), ax.CancelRequest{Mutation: mut(r, in.Fence), Mode: execution.CancelRevokeOnly, Reason: "stop"}); e != nil {
		t.Fatal(e)
	}
	data := []byte("late write through offline recovery")
	upload := f.upload(f.maker, data)
	home := f.inst.Layout().Home
	f.xfer.Close()
	if e = f.inst.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	app, e := application.OpenOffline(ctx, application.Options{Instance: operations.Options{Home: home, Clock: f.clk}, Identity: identity.Config{Password: fastPassword}})
	if e != nil {
		t.Fatal(e)
	}
	defer app.Close(context.Background())
	mctx, held, e := app.Instance.Gate().Maintain(ctx, commands.ReasonRecovering)
	if e != nil {
		t.Fatal(e)
	}
	defer held.Release()
	_, e = app.Catalog.CommitVersion(mctx, catalog.VersionRequest{Who: f.maker, IdempotencyKey: f.key(), UploadID: upload, Slug: "offline-late", Task: bindTo(in.Fence.TaskID, a), Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", Size: int64(len(data)), SHA256: shaOf(data)}}}})
	if errcode.CodeOf(e) != errcode.LeaseStale {
		t.Fatal("offline assembly bypassed cancelled execution", e)
	}
}
