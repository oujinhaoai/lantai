package integration

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

// The official host launches the current immutable executable. Give the test
// executable the same private entry as cmd/lantai, without invoking a shell.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "_processor-check" {
		if extensions.ProcessorMain() != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func jobEnv(t *testing.T) (*flowEnv, *jobs.Service, *node.Service, authz.Context) {
	return jobEnvHost(t, nil)
}
func jobEnvHost(t *testing.T, wrap func(jobs.Host) jobs.Host) (*flowEnv, *jobs.Service, *node.Service, authz.Context) {
	t.Helper()
	release, e := extensions.CurrentReleaseDigest()
	if e != nil {
		t.Fatal(e)
	}
	f := newFlowEnvConfig(t, release, jobs.ConfigDigest())
	gen := &ids.Generator{Clock: f.clk, Rand: rand.Reader}
	auth := authority{Service: f.id, epochs: f.inst}
	nodes, e := node.New(node.Deps{DB: f.inst.DB(ownership.Runtime), Gate: f.inst.Gate(), Authority: auth, IDs: gen, Clock: f.clk})
	if e != nil {
		t.Fatal(e)
	}
	var host jobs.Host = f.jobs.registry
	if wrap != nil {
		host = wrap(host)
	}
	js, e := jobs.New(jobs.Deps{DB: f.inst.DB(ownership.Runtime), Gate: f.inst.Gate(), Authority: auth, Tasks: f.tasks, Catalog: f.catalog, Ledger: f.ledger, Files: f.storage, Rights: f.rights, Host: host, Nodes: nodes, IDs: gen, Clock: f.clk, InstanceID: f.inst.InstanceID()})
	if e != nil {
		t.Fatal(e)
	}
	f.flows, e = workflow.New(workflow.Deps{DB: f.inst.DB(ownership.Runtime), Gate: f.inst.Gate(), Authority: auth, Tasks: f.tasks, Ledger: f.ledger, Manifests: f.storage, Events: f.log, Jobs: js, Checks: js, Clock: f.clk, IDs: gen, InstanceID: f.inst.InstanceID()})
	if e != nil {
		t.Fatal(e)
	}
	f.source, e = f.ledger.NewFileReviewSources(f.storage, f.rights, f.flows.ReviewExecution(), f.jobs.registry, f.rights)
	if e != nil {
		t.Fatal(e)
	}
	f.reviews, e = f.ledger.NewReviews(f.source, f.id)
	if e != nil {
		t.Fatal(e)
	}
	f.flows.SetReviews(f.reviews)
	js.SetEvidence(f.source)
	js.SetExecution(f.flows.ReviewExecution())
	f.tasks.SetAuthorities(tasks.LedgerAuthorities{Reviews: f.reviews, Evidence: f.source})
	worker := f.taskPrincipal("worker:corecheck@local", identity.RoleChecker, authz.Worker)
	return f, js, nodes, worker
}

func queuedCheck(t *testing.T, f *flowEnv, js *jobs.Service) (ids.ID, jobs.Job) {
	t.Helper()
	ctx := t.Context()
	start, e := f.flows.Start(ctx, f.owner, f.key(), workflow.StartRequest{ProjectID: f.project.ProjectID, DefinitionRef: f.definition.Ref, ProfileRef: f.profile.Ref, Title: "real check", AcceptanceCriteria: []string{"valid document"}, ExpectedOutputs: []tc.OutputRequirement{{Slug: "checked", AssetType: "doc", CandidateCount: 1}}})
	if e != nil {
		t.Fatal(e)
	}
	f.dispatch()
	task := f.latest(f.flow(start.FlowID), "produce").StepRun.TaskIDs[0]
	a := f.claim(f.maker, task)
	v, e := f.commitDoc(f.maker, "checked", "", "", []byte("actual content"), bindTo(task, a))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f, task, a), OutputRefs: []ids.PermanentRef{v.Ref}}); e != nil {
		t.Fatal(e)
	}
	f.sync()
	f.dispatch()
	list, e := js.List(ctx, f.owner, f.project.ProjectID, "", 100)
	if e != nil || len(list) != 1 {
		t.Fatal(list, e)
	}
	return start.FlowID, list[0]
}
func observeWorker(t *testing.T, f *flowEnv, n *node.Service, w authz.Context) {
	t.Helper()
	_, e := n.Observe(t.Context(), w, f.key(), node.Observation{ProjectID: f.project.ProjectID, Capabilities: []string{"org.lantai.corecheck.manifest"}, Slots: 1})
	if e != nil {
		t.Fatal(e)
	}
}
func TestM2RealJobChecksAndEvidence(t *testing.T) {
	f, js, n, w := jobEnv(t)
	ctx := t.Context()
	flow, j := queuedCheck(t, f, js)
	control := jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision}
	if _, e := js.Run(ctx, w, f.key(), control); errcode.CodeOf(e) != errcode.UnsupportedCapability {
		t.Fatal("missing observation", e)
	}
	observeWorker(t, f, n, w)
	if _, e := js.Run(ctx, f.maker, f.key(), control); errcode.CodeOf(e) != errcode.Forbidden {
		t.Fatal("agent cannot pretend worker", e)
	}
	key := f.key()
	got, e := js.Run(ctx, w, key, control)
	if e != nil {
		t.Fatal(e)
	}
	if got.State != "succeeded" || got.Attempt.Outcome != execution.InvocationCompleted || got.Attempt.Verdict != execution.VerdictPass || len(got.Evidence) != 4 || !got.Attempt.Termination.Confirmed {
		t.Fatalf("%+v", got)
	}
	again, e := js.Run(ctx, w, key, control)
	if e != nil || again.Attempt.ID != got.Attempt.ID || again.Attempts != 1 {
		t.Fatal("duplicate process", again, e)
	}
	if _, e = js.Retry(ctx, f.owner, f.key(), jobs.Control{JobID: got.ID, ExpectedRevision: got.Revision}); errcode.CodeOf(e) != errcode.OperationNeedsReconciliation {
		t.Fatal("successful checks cannot retry", e)
	}
	if _, e = f.flows.SyncJobs(ctx, f.owner, 10); e != nil {
		t.Fatal(e)
	}
	if f.latest(f.flow(flow), "check").StepRun.State != "completed" {
		t.Fatal("flow did not consume actual checks")
	}
	// Check execution success cannot approve or publish the candidate.
	v := f.versionOf(got.Request.Target)
	state, e := f.ledger.VersionControl(ctx, v.VersionID)
	if e != nil || state.ReviewState == "approved" {
		t.Fatal("job approved candidate", state, e)
	}
}
func TestM2WorkerObservationExpires(t *testing.T) {
	f, js, n, w := jobEnv(t)
	_, j := queuedCheck(t, f, js)
	observeWorker(t, f, n, w)
	f.clk.Advance(6 * time.Minute)
	if _, e := js.Run(t.Context(), w, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision}); errcode.CodeOf(e) != errcode.ResourceBusy {
		t.Fatal(e)
	}
	current, e := js.Read(t.Context(), f.owner, j.ID)
	if e != nil || current.Attempt != nil {
		t.Fatal("expired observation dispatched", current, e)
	}
}

// Inject observations at the host boundary to exercise failure acceptance. The
// success test above separately launches the real compiled checker.
type observedHost struct {
	jobs.Host
	run func(context.Context, extensions.ProcessorInput, []byte) (extensions.InvocationResult, error)
}

func (h observedHost) Run(c context.Context, _ ids.ID, in extensions.ProcessorInput, b []byte) (extensions.InvocationResult, error) {
	return h.run(c, in, b)
}
func TestM2JobFaultRetryAndUnknownReconciliation(t *testing.T) {
	calls := 0
	f, js, n, w := jobEnvHost(t, func(h jobs.Host) jobs.Host {
		return observedHost{h, func(ctx context.Context, in extensions.ProcessorInput, b []byte) (extensions.InvocationResult, error) {
			calls++
			if calls == 1 {
				return extensions.InvocationResult{}, errors.New("synthetic start failure")
			}
			if calls == 2 {
				return extensions.InvocationResult{Observation: execution.Invocation{Dispatched: true}}, nil
			}
			return h.Run(ctx, "", in, b)
		}}
	})
	_, j := queuedCheck(t, f, js)
	observeWorker(t, f, n, w)
	control := func() jobs.Control { return jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision} }
	var err error
	j, err = js.Run(t.Context(), w, f.key(), control())
	if err != nil || j.Failure != "host_start_failed" || !j.Attempt.Termination.Confirmed {
		t.Fatal(j, err)
	}
	j, err = js.Retry(t.Context(), f.owner, f.key(), control())
	if err != nil {
		t.Fatal(err)
	}
	j, err = js.Run(t.Context(), w, f.key(), control())
	if err != nil || j.State != "needs_reconciliation" || j.Attempt.Termination.Confirmed {
		t.Fatal(j, err)
	}
	if _, err = js.Retry(t.Context(), f.owner, f.key(), control()); errcode.CodeOf(err) != errcode.OperationNeedsReconciliation {
		t.Fatal("blind retry", err)
	}
	if _, err = js.Reconcile(t.Context(), f.owner, f.key(), jobs.ReconcileRequest{Control: control(), Stopped: true, Reason: "unsubstantiated"}); errcode.CodeOf(err) != errcode.SchemaInvalid {
		t.Fatal("missing proof", err)
	}
	proof := f.ingest(f.owner, "job-proof", []byte("synthetic operator stopping evidence"), *rightsOwned())
	j, err = js.Reconcile(t.Context(), f.owner, f.key(), jobs.ReconcileRequest{Control: control(), Stopped: true, Reason: "checked local process", EvidenceRefs: []ids.PermanentRef{proof.Ref}})
	if err != nil {
		t.Fatal(err)
	}
	j, err = js.Retry(t.Context(), f.owner, f.key(), control())
	if err != nil {
		t.Fatal(err)
	}
	j, err = js.Run(t.Context(), w, f.key(), control())
	if err != nil || j.State != "succeeded" || j.Attempts != 3 {
		t.Fatal(j, err)
	}
}
func TestM2JobCancellationWaitsForHostAndWorkerScope(t *testing.T) {
	started := make(chan struct{})
	f, js, n, w := jobEnvHost(t, func(h jobs.Host) jobs.Host {
		return observedHost{h, func(ctx context.Context, in extensions.ProcessorInput, b []byte) (extensions.InvocationResult, error) {
			close(started)
			<-ctx.Done()
			return extensions.InvocationResult{Observation: execution.Invocation{Dispatched: true, StopConfirmed: true, Cancelled: true, Exited: true, ExitCode: -1}}, nil
		}}
	})
	// Worker scope grants only the explicitly registered checker surfaces.
	for _, action := range []string{"tasks.work", "catalog.commit", "identity.manage", "ledger.publish"} {
		d, e := f.id.Authorize(t.Context(), w, authz.Action(action), authz.Resource{ProjectID: f.project.ProjectID, Kind: "project", ID: f.project.ProjectID})
		if e == nil && d.Err() == nil {
			t.Fatal("worker escalated", action)
		}
	}
	_, j := queuedCheck(t, f, js)
	observeWorker(t, f, n, w)
	type result struct {
		j   jobs.Job
		err error
	}
	done := make(chan result, 1)
	key := f.key()
	go func() {
		out, e := js.Run(t.Context(), w, key, jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision})
		done <- result{out, e}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("host not dispatched")
	}
	current, e := js.Read(t.Context(), f.owner, j.ID)
	if e != nil {
		t.Fatal(e)
	}
	cancelled, e := js.Cancel(t.Context(), f.owner, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: current.Revision})
	if e != nil || cancelled.State != "cancelling" || cancelled.Attempt.Termination.Confirmed {
		t.Fatal(cancelled, e)
	}
	select {
	case out := <-done:
		if out.err != nil || out.j.State != "cancelled" || !out.j.Attempt.Termination.Confirmed || len(out.j.Evidence) != 0 {
			t.Fatal(out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not stop host")
	}
}
func TestM2JobLegalFailRemainsEvidence(t *testing.T) {
	f, js, n, w := jobEnvHost(t, func(h jobs.Host) jobs.Host {
		return observedHost{h, func(ctx context.Context, in extensions.ProcessorInput, b []byte) (extensions.InvocationResult, error) {
			out, e := h.Run(ctx, "", in, b)
			if e == nil && out.Result != nil {
				out.Result.Checks[0].Verdict = execution.VerdictFail
				out.Result.Checks[0].Findings = []string{"synthetic_validation_failure"}
			}
			return out, e
		}}
	})
	_, j := queuedCheck(t, f, js)
	observeWorker(t, f, n, w)
	out, e := js.Run(t.Context(), w, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision})
	if e != nil || out.State != "succeeded" || out.Attempt.Outcome != execution.InvocationCompleted || out.Attempt.Verdict != execution.VerdictFail || len(out.Evidence) != 4 {
		t.Fatal(out, e)
	}
	if _, e = js.Retry(t.Context(), f.owner, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: out.Revision}); errcode.CodeOf(e) != errcode.OperationNeedsReconciliation {
		t.Fatal("legal fail retried as host fault", e)
	}
}

func TestM2RealWorkerThroughHumanReviewAndPublication(t *testing.T) {
	f, js, n, w := jobEnv(t)
	ctx := t.Context()
	flow, j := queuedCheck(t, f, js)
	observeWorker(t, f, n, w)
	if _, e := js.Run(ctx, w, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.flows.SyncJobs(ctx, f.owner, 10); e != nil {
		t.Fatal(e)
	}
	f.dispatch()
	f.qa(flow, "pass")
	f.sync()
	f.dispatch()
	v := f.flow(flow)
	if v.Meta.ReviewTargetID == "" {
		t.Fatal("no review target", v)
	}
	f.decide(v.Meta.ReviewTargetID, "approve")
	f.sync()
	f.dispatch()
	f.sync()
	v = f.flow(flow)
	if v.Flow.State != "completed" {
		t.Fatal(v.Flow, v.Commands)
	}
	state, e := f.ledger.AssetControl(ctx, j.Request.Target.AssetID)
	if e != nil || state.PublishedVersionID != j.Request.Target.VersionID {
		t.Fatal(state, e)
	}
}

func TestM2JobFinalAcceptanceRejectsExpiredLeaseAndRevokedWorker(t *testing.T) {
	for _, mode := range []string{"lease", "worker"} {
		t.Run(mode, func(t *testing.T) {
			var f *flowEnv
			var w authz.Context
			var js *jobs.Service
			var n *node.Service
			f, js, n, w = jobEnvHost(t, func(h jobs.Host) jobs.Host {
				return observedHost{h, func(ctx context.Context, in extensions.ProcessorInput, b []byte) (extensions.InvocationResult, error) {
					out, e := h.Run(ctx, "", in, b)
					if e != nil {
						return out, e
					}
					if mode == "lease" {
						f.clk.Advance(31 * time.Second)
					} else {
						if e = f.id.EndSession(ctx, w, w.SessionID); e != nil {
							return out, e
						}
					}
					return out, nil
				}}
			})
			_, j := queuedCheck(t, f, js)
			observeWorker(t, f, n, w)
			out, e := js.Run(t.Context(), w, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision})
			want := "stale_job_lease"
			if mode == "worker" {
				want = "worker_authorization_revoked"
			}
			if e != nil || out.State != "failed" || out.Failure != want || len(out.Evidence) != 0 {
				t.Fatal(out, e)
			}
		})
	}
}

type unavailableEvidence struct{}

func (unavailableEvidence) AppendEvidence(context.Context, authz.Context, string, ledger.ReviewEvidenceInput) (ledger.AcceptedEvidence, error) {
	return ledger.AcceptedEvidence{}, errors.New("synthetic ledger outage")
}
func TestM2JobCancelAcceptingAndUnknown(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			f, js, n, w := jobEnvHost(t, func(h jobs.Host) jobs.Host {
				if !unknown {
					return h
				}
				return observedHost{h, func(context.Context, extensions.ProcessorInput, []byte) (extensions.InvocationResult, error) {
					return extensions.InvocationResult{Observation: execution.Invocation{Dispatched: true}}, nil
				}}
			})
			_, j := queuedCheck(t, f, js)
			observeWorker(t, f, n, w)
			if !unknown {
				js.SetEvidence(unavailableEvidence{})
			}
			out, e := js.Run(t.Context(), w, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision})
			if unknown && e != nil || !unknown && (e == nil || out.State != "accepting") {
				t.Fatal(out, e)
			}
			cancelled, e := js.Cancel(t.Context(), f.owner, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: out.Revision})
			if e != nil {
				t.Fatal(e)
			}
			if unknown && cancelled.State != "cancelling" || !unknown && cancelled.State != "cancelled" {
				t.Fatal(cancelled)
			}
			if len(cancelled.Evidence) != 0 {
				t.Fatal("cancel accepted evidence")
			}
		})
	}
}
