package agentexec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/tasks"
)

func fixture[T any](t *testing.T, name, file string) T {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "examples", "agent-execution", "v1", name, file+".json"))
	if e != nil {
		t.Fatal(e)
	}
	var w struct {
		Document T `json:"document"`
	}
	if e = json.Unmarshal(b, &w); e != nil {
		t.Fatal(e)
	}
	return w.Document
}
func code(t *testing.T, e error, want errcode.Code) {
	t.Helper()
	if want == "" {
		if e != nil {
			t.Fatal(e)
		}
		return
	}
	if e == nil || errcode.CodeOf(e) != want {
		t.Fatalf("want %s, got %v", want, e)
	}
}
func rehash(t *testing.T, r *StartRequest) {
	t.Helper()
	h, e := RequestHash(*r)
	if e != nil {
		t.Fatal(e)
	}
	r.RequestHash = h
}
func TestStartKeyAndFixedRunSnapshots(t *testing.T) {
	req := fixture[StartRequest](t, "agent-request", "start")
	cap := fixture[Capabilities](t, "execution-capabilities", "valid")
	run := fixture[TaskRun](t, "task-run", "valid")
	ad := fixture[Admission](t, "agent-response", "start")
	code(t, req.Validate(cap), "")
	code(t, CheckStartForRun(req, run, cap, nil), "")
	replay, e := ReplayStart(ad, req)
	code(t, e, "")
	if replay.ExecutionID != ad.ExecutionID {
		t.Fatal("response loss changed stable ID")
	}
	req.OperationID = ids.New()
	h, e := RequestHash(req)
	code(t, e, "")
	if h != ad.AcceptedRequestHash {
		t.Fatal("transport operation changed start identity")
	}
	req.BudgetID = ids.New()
	rehash(t, &req)
	_, e = ReplayStart(ad, req)
	code(t, e, errcode.StartKeyConflict)
	code(t, CheckStartForRun(req, run, cap, nil), errcode.SchemaInvalid)
	req = fixture[StartRequest](t, "agent-request", "start")
	req.Profile.BudgetLimits.ToolCalls++
	code(t, req.Validate(cap), errcode.SchemaInvalid) // supplied hash cannot hide payload changes
	key, e := ExecutionKey(run.ID, ids.New())
	code(t, e, "")
	if key == req.ExecutionKey {
		t.Fatal("new Attempt reused key")
	}
	req = fixture[StartRequest](t, "agent-request", "start")
	req.Activation = nil
	rehash(t, &req)
	code(t, req.Validate(cap), errcode.ExtensionActivationStale)
}
func TestManualCapabilitiesCannotPromiseMeterOrTermination(t *testing.T) {
	cap := fixture[Capabilities](t, "execution-capabilities", "valid")
	req := fixture[StartRequest](t, "agent-request", "start")
	limit := int64(100)
	req.Profile.BudgetLimits.Tokens = &limit
	rehash(t, &req)
	code(t, req.Validate(cap), errcode.UnsupportedCapability)
	cancel := fixture[CancelRequest](t, "agent-request", "cancel")
	code(t, CheckCancel(cancel, cap), "")
	cancel.RequestedCancelMode = execution.CancelEnforced
	cancel.RequestHash, _ = RequestHash(cancel)
	code(t, CheckCancel(cancel, cap), errcode.UnsupportedCapability)
	cap.CancelMode = execution.CancelEnforced
	code(t, tasks.ValidateShape("lantai.execution-capabilities/v1", cap), errcode.SchemaInvalid)
	if execution.ResolveCancel(false, true, 0) != execution.RunNeedsReconciliation {
		t.Fatal("revoke-only cancellation claimed termination")
	}
}
func TestResultSealingAndFinalAcceptance(t *testing.T) {
	r := fixture[Result](t, "agent-result", "valid")
	ad := fixture[Admission](t, "agent-response", "start")
	req := fixture[StartRequest](t, "agent-request", "start")
	now, _ := clock.Parse("2026-09-27T08:01:00.000Z")
	code(t, r.Validate(), "")
	lease := execution.LeaseState{AttemptID: req.Fence.AttemptID, LeaseFence: 1, RecoveryEpoch: 1, ExpiresAt: now.Add(time.Minute)}
	act := execution.ActivationState{Current: *req.Activation, Enabled: true}
	accept := execution.Acceptance{Now: now, Fence: &req.Fence, Lease: &lease, Activation: req.Activation, ActivationState: &act}
	code(t, AcceptResult(r, req, ad, accept), "")
	r.CandidateIDs = append(r.CandidateIDs, ids.New())
	code(t, AcceptResult(r, req, ad, accept), errcode.SchemaInvalid)
	r = fixture[Result](t, "agent-result", "valid")
	r.ExecutionID = ids.New()
	r.ResultDigest, _ = ResultHash(r)
	code(t, AcceptResult(r, req, ad, accept), errcode.SchemaInvalid)
	r = fixture[Result](t, "agent-result", "valid")
	lease.RecoveryEpoch = 2
	code(t, AcceptResult(r, req, ad, accept), errcode.LeaseStale)
	lease.RecoveryEpoch = 1
	act.Revoked = true
	code(t, AcceptResult(r, req, ad, accept), errcode.ExtensionActivationStale)
	act.Revoked = false
	accept.Activation = nil
	accept.ActivationState = nil
	code(t, AcceptResult(r, req, ad, accept), errcode.ExtensionActivationStale)
	r.Outcome = "approved"
	r.ResultDigest, _ = ResultHash(r)
	code(t, r.Validate(), errcode.SchemaInvalid)
}
func TestResumeReconcilesCompleteLogAndDoesNotRestoreAuthority(t *testing.T) {
	req := fixture[StartRequest](t, "agent-request", "resume")
	cap := fixture[Capabilities](t, "execution-capabilities", "valid")
	op := fixture[ToolOperation](t, "tool-operation", "valid")
	code(t, CheckResume(req, cap, []ToolOperation{op}), "")
	code(t, CheckResume(req, cap, nil), errcode.SchemaInvalid)
	op.State = execution.EffectUnknown
	code(t, CheckResume(req, cap, []ToolOperation{op}), errcode.OperationNeedsReconciliation)
	op.EffectClass = execution.EffectExternalNonIdempotent
	code(t, CheckResume(req, cap, []ToolOperation{op}), errcode.OperationNeedsReconciliation)
	op = fixture[ToolOperation](t, "tool-operation", "valid")
	op.Sequence = 2
	code(t, CheckResume(req, cap, []ToolOperation{op}), errcode.SchemaInvalid)
	req.ResumeFrom.ProfileDigest = digest.Of([]byte("other profile"))
	rehash(t, &req)
	code(t, CheckResume(req, cap, nil), errcode.SchemaInvalid)
	req = fixture[StartRequest](t, "agent-request", "resume")
	req.ResumeFrom.ResumeClass = execution.ResumeManualOnly
	rehash(t, &req)
	code(t, CheckResume(req, cap, nil), errcode.ResumeUnsupported)
	req = fixture[StartRequest](t, "agent-request", "resume")
	req.ResumeFrom.AttemptID = req.Fence.AttemptID
	rehash(t, &req)
	code(t, CheckResume(req, cap, nil), errcode.SchemaInvalid)
}
func TestPackageEntryAndJobAttemptRemainIndependentFromTaskFence(t *testing.T) {
	p := fixture[ExecutionProfile](t, "execution-profile", "valid")
	code(t, p.Validate(), "")
	p.ActivationSnapshot.Entry.PackageDigest = digest.Of([]byte("replaced"))
	code(t, p.Validate(), errcode.SchemaInvalid)
	job := fixture[JobAttempt](t, "job-attempt", "valid")
	code(t, job.Validate(), "") // completed validator fail is not runtime fault
	job.Fence.JobAttemptID = ids.New()
	code(t, job.Validate(), errcode.SchemaInvalid)
	job = fixture[JobAttempt](t, "job-attempt", "valid")
	job.Outcome = execution.InvocationRuntimeFault
	job.Verdict = execution.VerdictPass
	code(t, job.Validate(), errcode.SchemaInvalid)
	a := fixture[ArtifactCandidate](t, "artifact-candidate", "valid")
	code(t, a.Validate(), "")
	a.Files = append(a.Files, a.Files[0])
	code(t, a.Validate(), errcode.SchemaInvalid)
}
func TestToolScopeOnlyNarrowsAndExcludesHumanAuthority(t *testing.T) {
	all := []string{"catalog.create_version", "ledger.publish", "identity.set_project_policy", "storage.read_content"}
	got := EffectiveTools(all, all, all, []string{"catalog.create_version", "ledger.publish"}, []string{"ledger.publish", "identity.set_project_policy"})
	if len(got) != 1 || got[0] != "catalog.create_version" {
		t.Fatal(got)
	}
	child := EffectiveTools(all, all, got, []string{"ledger.publish"}, nil)
	if len(child) != 0 {
		t.Fatal("subagent expanded scope")
	}
}

func TestStartAndResumeStatesCannotBypassReconciliation(t *testing.T) {
	req := fixture[StartRequest](t, "agent-request", "start")
	run := fixture[TaskRun](t, "task-run", "valid")
	cap := fixture[Capabilities](t, "execution-capabilities", "valid")
	for _, state := range []execution.TaskRunState{execution.RunPending, execution.RunStarting} {
		run.State = state
		code(t, CheckStartForRun(req, run, cap, nil), "")
	}
	run.State = execution.RunPaused
	code(t, CheckStartForRun(req, run, cap, nil), errcode.InvalidStateTransition)
	req = fixture[StartRequest](t, "agent-request", "resume")
	run.CurrentAttemptID = req.Fence.AttemptID
	op := fixture[ToolOperation](t, "tool-operation", "valid")
	code(t, CheckStartForRun(req, run, cap, []ToolOperation{op}), "")
	code(t, CheckStartForRun(req, run, cap, nil), errcode.SchemaInvalid)
	for _, state := range []execution.TaskRunState{execution.RunPending, execution.RunStarting, execution.RunRunning} {
		run.State = state
		code(t, CheckStartForRun(req, run, cap, []ToolOperation{op}), errcode.InvalidStateTransition)
	}
}
func TestBackendResumeRequiresDeclaredFormatAndVersion(t *testing.T) {
	req := fixture[StartRequest](t, "agent-request", "resume")
	cap := fixture[Capabilities](t, "execution-capabilities", "valid")
	op := fixture[ToolOperation](t, "tool-operation", "valid")
	cap.ResumeClasses = append(cap.ResumeClasses, execution.ResumeBackendCheckpoint)
	cap.CheckpointFormatVersions = []string{"progress-v1"}
	req.ResumeFrom.ResumeClass = execution.ResumeBackendCheckpoint
	req.ResumeFrom.BackendCheckpointRef = "opaque-checkpoint-1"
	rehash(t, &req)
	code(t, CheckResume(req, cap, []ToolOperation{op}), "")
	req.ResumeFrom.FormatVersion = "unsupported-v2"
	rehash(t, &req)
	code(t, CheckResume(req, cap, []ToolOperation{op}), errcode.ResumeUnsupported)
	req.ResumeFrom.FormatVersion = "progress-v1"
	req.ResumeFrom.BackendVersion = "2.0.0"
	rehash(t, &req)
	code(t, CheckResume(req, cap, []ToolOperation{op}), errcode.ResumeUnsupported)
}

func TestAuthoritySnapshotsAreSemanticallyValidated(t *testing.T) {
	req := fixture[StartRequest](t, "agent-request", "start")
	run := fixture[TaskRun](t, "task-run", "valid")
	cap := fixture[Capabilities](t, "execution-capabilities", "valid")
	run.Input.Refs[0].VersionID = ids.New()
	code(t, CheckStartForRun(req, run, cap, nil), errcode.SchemaInvalid)
	r := fixture[Result](t, "agent-result", "valid")
	ad := fixture[Admission](t, "agent-response", "start")
	ad.Revision = 0
	code(t, AcceptResult(r, req, ad, execution.Acceptance{}), errcode.SchemaInvalid)
}
