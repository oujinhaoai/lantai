package jobs

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

func (s *Service) finish(ctx context.Context, w authz.Context, dispatched Job, out extensions.InvocationResult, hostErr error) (Job, error) {
	acceptCtx := ctx
	ctx, release, e := s.lock(ctx, dispatched)
	if e != nil {
		return Job{}, e
	}
	j, e := s.load(ctx, dispatched.ID)
	if e != nil {
		release()
		return Job{}, e
	}
	if j.Attempt == nil || j.Attempt.ID != dispatched.Attempt.ID {
		release()
		return Job{}, errcode.New(errcode.LeaseStale, "")
	}
	c, e := s.command(ctx, w, "finish-"+string(j.Attempt.ID), "jobs.finish", j, out)
	if e != nil {
		release()
		return Job{}, e
	}
	outcome := execution.ClassifyInvocation(out.Observation)
	unsupported := outcome == execution.InvocationCompleted && out.Result != nil && out.Result.Status == "unsupported"
	if outcome == execution.InvocationCompleted && !unsupported {
		if out.Result == nil || len(out.Result.Checks) != 1 {
			outcome = execution.InvocationRuntimeFault
		} else {
			b, e := canonjson.CanonicalizeValue(out.Result)
			if e != nil {
				release()
				return Job{}, e
			}
			j.Attempt.ResultDigest = digest.Of(b)
		}
	}
	j.Attempt.Outcome = outcome
	j.Attempt.Verdict = execution.VerdictUnknown
	j.Attempt.Revision++
	j.Attempt.Termination.Confirmed = out.Observation.StopConfirmed || !out.Observation.Dispatched
	j.State = "failed"
	j.Attempt.State = "completed"
	if outcome == execution.InvocationUnresolved {
		j.State = "needs_reconciliation"
		j.Attempt.State = "needs_reconciliation"
	}
	switch {
	case hostErr != nil:
		// Admission refusals are recorded by reason; none of them dispatched.
		j.Failure = map[errcode.Code]string{errcode.ExtensionBreakerOpen: "breaker_open", errcode.ExtensionActivationStale: "activation_stale", errcode.Forbidden: "processor_not_allowed", errcode.UnsupportedCapability: "processor_not_enabled"}[errcode.CodeOf(hostErr)]
		if j.Failure == "" {
			j.Failure = "host_start_failed"
		}
	case unsupported:
		// A legal "unsupported input" answer is a business result, not a fault.
		j.Failure = "unsupported_input"
	case outcome != execution.InvocationCompleted:
		j.Failure = string(outcome)
	}
	if j.CancelRequested && out.Observation.StopConfirmed {
		j.State = "cancelled"
		j.Attempt.Outcome = execution.InvocationCancelled
	}
	if outcome == execution.InvocationCompleted && !j.CancelRequested && !unsupported {
		expires, parseErr := clock.Parse(j.ExpiresAt)
		if parseErr != nil || !s.d.Clock.Now().Before(expires) || j.Epoch != c.RecoveryEpoch {
			j.Failure = "stale_job_lease"
		} else if e = s.auth(ctx, w, "ledger.append_check", j.Request.ProjectID); e != nil {
			j.Failure = "worker_authorization_revoked"
		} else if e = s.d.Host.CheckSnapshot(ctx, j.Request.ProjectID, j.Attempt.ActivationSnapshot, j.Attempt.ID, c.RecoveryEpoch); e != nil {
			j.Failure = "activation_stale"
		} else {
			v, err := s.target(ctx, w, j)
			if err != nil {
				j.Failure = "target_changed"
			} else if out.Result == nil || len(out.Result.Checks) != 1 {
				j.Failure = "invalid_result"
			} else {
				base := out.Result.Checks[0]
				// File integrity and current rights are core-owned facts, independently
				// checked in addition to the one-shot plugin's structural result.
				integrity := s.d.Files.VerifyDeep(ctx, v.OperationID)
				license, le := s.d.Rights.EvaluateUse(ctx, w, j.Request.Target, authz.PurposeArchiveReview)
				purpose, pe := s.d.Rights.EvaluateUse(ctx, w, j.Request.Target, authz.PurposeProduction)
				checks := map[string]error{"schema": nil, "integrity": integrity, "license_evidence": le, "purpose": pe}
				if le == nil {
					checks["license_evidence"] = license.Err()
				}
				if pe == nil {
					checks["purpose"] = purpose.Err()
				}
				j.Checks = []ledger.ReviewEvidenceInput{}
				for _, key := range []string{"integrity", "schema", "license_evidence", "purpose"} {
					check := base
					if key != "schema" {
						check.Verdict = execution.VerdictPass
						check.Findings = []string{}
						if checks[key] != nil {
							check.Verdict = execution.VerdictFail
							check.Findings = []string{string(errcode.CodeOf(checks[key]))}
							if check.Findings[0] == "" {
								check.Findings = []string{"verification_failed"}
							}
						}
					}
					j.Checks = append(j.Checks, ledger.ReviewEvidenceInput{ProjectID: j.Request.ProjectID, Ref: j.Request.Target, ManifestDigest: v.ManifestDigest, Flow: j.Request.Flow, Kind: "check_result", CheckKey: key, SchemaVersion: 1, ConfigDigest: ConfigDigest(), Check: &check})
				}
				b, err := canonjson.CanonicalizeValue(j.Checks)
				if err != nil {
					release()
					return Job{}, err
				}
				j.Attempt.ResultDigest = digest.Of(b)
				j.Attempt.Verdict = base.Verdict
				for _, x := range j.Checks {
					if x.Check.Verdict != execution.VerdictPass {
						j.Attempt.Verdict = x.Check.Verdict
						break
					}
				}
				j.State = "accepting"
			}
		}
	}
	j, e = s.save(ctx, c, j)
	release()
	if e != nil {
		return Job{}, e
	}
	if j.State == "accepting" {
		return s.acceptEvidence(acceptCtx, w, j)
	}
	return j, nil
}
func (s *Service) acceptEvidence(ctx context.Context, w authz.Context, j Job) (Job, error) {
	s.mu.RLock()
	source := s.evidence
	s.mu.RUnlock()
	if source == nil {
		return j, errcode.New(errcode.UnsupportedCapability, "ledger evidence authority unavailable")
	}
	if j.WorkerID != w.PrincipalID || j.SessionID != w.SessionID {
		return Job{}, errcode.New(errcode.Forbidden, "")
	}
	evidence := []workflow.JobEvidence{}
	for _, in := range j.Checks {
		accepted, e := source.AppendEvidence(ctx, w, "job-"+string(j.Attempt.ID)+"-"+in.CheckKey, in)
		if e != nil {
			return j, e
		}
		// The ledger owner resolves operation identity; jobs never reads ledger SQL.
		operation, e := s.d.Ledger.EvidenceOperation(ctx, accepted.ID)
		if e != nil {
			return j, e
		}
		evidence = append(evidence, workflow.JobEvidence{EvidenceID: accepted.ID, OperationID: operation, Verdict: string(in.Check.Verdict)})
	}
	ctx, release, e := s.lock(ctx, j)
	if e != nil {
		return Job{}, e
	}
	defer release()
	current, e := s.load(ctx, j.ID)
	if e != nil {
		return Job{}, e
	}
	if current.State == "succeeded" {
		return current, nil
	}
	if current.State != "accepting" || current.CancelRequested || current.Attempt.ID != j.Attempt.ID {
		return current, errcode.New(errcode.LeaseStale, "")
	}
	if e = s.auth(ctx, w, "ledger.append_check", j.Request.ProjectID); e != nil {
		return Job{}, e
	}
	if _, e = s.target(ctx, w, j); e != nil {
		return Job{}, e
	}
	c, e := s.command(ctx, w, "accept-"+string(j.Attempt.ID), "jobs.accept", j, evidence)
	if e != nil {
		return Job{}, e
	}
	if c.RecoveryEpoch != current.Epoch {
		return Job{}, errcode.New(errcode.LeaseStale, "")
	}
	if e = s.d.Host.CheckSnapshot(ctx, current.Request.ProjectID, current.Attempt.ActivationSnapshot, current.Attempt.ID, c.RecoveryEpoch); e != nil {
		return Job{}, e
	}
	current.Evidence = evidence
	current.State = "succeeded"
	return s.save(ctx, c, current)
}
func (s *Service) JobResult(ctx context.Context, id ids.ID) (workflow.JobResult, error) {
	j, e := s.load(ctx, id)
	if e != nil {
		return workflow.JobResult{}, e
	}
	state := "running"
	if j.State == "succeeded" {
		state = "succeeded"
	}
	if slices.Contains([]string{"failed", "cancelled", "needs_reconciliation"}, j.State) {
		state = "failed"
	}
	return workflow.JobResult{State: state, Evidence: j.Evidence, Failure: j.Failure}, nil
}
func (s *Service) VerifyCheck(ctx context.Context, w authz.Context, v commit.Committed, actor ids.ID, in ledger.ReviewEvidenceInput, historical bool) error {
	rows, e := s.d.DB.QueryContext(ctx, `SELECT record FROM jobs_jobs WHERE project_id=? AND state IN ('accepting','succeeded')`, v.ProjectID)
	if e != nil {
		return e
	}
	var found *Job
	for rows.Next() {
		var b string
		var j Job
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal([]byte(b), &j); e != nil {
			rows.Close()
			return e
		}
		if j.WorkerID == actor && j.Request.Target.VersionID == v.VersionID && slices.ContainsFunc(j.Checks, func(x ledger.ReviewEvidenceInput) bool { return raw(x) == raw(in) }) {
			found = &j
			break
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if found == nil {
		return errcode.New(errcode.ChecksNotSatisfied, "no completed job matches this evidence")
	}
	if !historical {
		j := *found
		if j.CancelRequested || j.WorkerID != w.PrincipalID || j.SessionID != w.SessionID {
			return errcode.New(errcode.LeaseStale, "")
		}
		epoch, e := s.d.Authority.RecoveryEpoch(ctx)
		if e != nil {
			return e
		}
		if epoch != j.Epoch {
			return errcode.New(errcode.LeaseStale, "")
		}
		if e = s.d.Host.CheckSnapshot(ctx, j.Request.ProjectID, j.Attempt.ActivationSnapshot, j.Attempt.ID, epoch); e != nil {
			return e
		}
		if _, e = s.target(ctx, w, j); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) Cancel(ctx context.Context, w authz.Context, key string, in Control) (Job, error) {
	return s.control(ctx, w, key, "cancel", in, in, func(j *Job) error {
		if j.State == "succeeded" || j.State == "cancelled" {
			return errcode.New(errcode.InvalidStateTransition, "")
		}
		j.CancelRequested = true
		if j.State == "queued" || j.State == "failed" || j.Attempt != nil && j.Attempt.Termination.Confirmed {
			j.State = "cancelled"
		} else {
			j.State = "cancelling"
			if j.Attempt != nil && j.Attempt.Outcome == "" {
				j.Attempt.State = "cancelling"
			}
		}
		return nil
	})
}
func (s *Service) Retry(ctx context.Context, w authz.Context, key string, in Control) (Job, error) {
	return s.control(ctx, w, key, "retry", in, in, func(j *Job) error {
		retryable := []string{"runtime_fault", "host_start_failed", "breaker_open", "activation_stale", "processor_not_allowed", "processor_not_enabled"}
		if j.CancelRequested || j.State != "failed" || j.Attempt == nil || !j.Attempt.Termination.Confirmed || j.Attempts >= 3 || !slices.Contains(retryable, j.Failure) {
			return errcode.New(errcode.OperationNeedsReconciliation, "only a confirmed runtime fault or undispatched refusal can retry; at most three attempts")
		}
		j.State = "queued"
		j.Failure = ""
		return nil
	})
}
func (s *Service) control(ctx context.Context, w authz.Context, key, action string, in Control, body any, fn func(*Job) error) (Job, error) {
	j, e := s.Read(ctx, w, in.JobID)
	if e != nil {
		return Job{}, e
	}
	ctx, release, e := s.lock(ctx, j)
	if e != nil {
		return Job{}, e
	}
	defer release()
	j, e = s.load(ctx, j.ID)
	if e != nil {
		return Job{}, e
	}
	if e = s.auth(ctx, w, identity.ActTasksCancel, j.Request.ProjectID); e != nil {
		return Job{}, e
	}
	c, e := s.command(ctx, w, key, "jobs."+action, j, body)
	if e != nil {
		return Job{}, e
	}
	if old, e := s.replay(ctx, c); old != nil || e != nil {
		if old != nil {
			return *old, e
		}
		return Job{}, e
	}
	if in.ExpectedRevision != j.Revision {
		return Job{}, errcode.New(errcode.PreconditionFailed, "")
	}
	if e = fn(&j); e != nil {
		return Job{}, e
	}
	if action == "retry" {
		j.Epoch = c.RecoveryEpoch
	}
	j, e = s.save(ctx, c, j)
	if e == nil && action == "cancel" {
		s.mu.RLock()
		cancel := s.running[j.ID]
		s.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
	}
	return j, e
}

type Reconciliation struct {
	Request    ReconcileRequest `json:"request"`
	ActorID    ids.ID           `json:"actor_id"`
	RecordedAt string           `json:"recorded_at"`
}

// Reconcile records operator-confirmed stopping after a core crash. Unknown
// effects never requeue; deterministic checks may retry only after this proof.
type ReconcileRequest struct {
	Control
	Stopped      bool               `json:"stopped"`
	Reason       string             `json:"reason"`
	EvidenceRefs []ids.PermanentRef `json:"evidence_refs"`
}

func (s *Service) Reconcile(ctx context.Context, w authz.Context, key string, in ReconcileRequest) (Job, error) {
	// The complete stopping assertion participates in the idempotency digest.
	j, e := s.control(ctx, w, key, "reconcile", in.Control, in, func(j *Job) error {
		s.mu.RLock()
		active := s.running[j.ID] != nil
		s.mu.RUnlock()
		if active {
			return errcode.New(errcode.ResourceBusy, "live host must report stopping before manual reconciliation")
		}
		if in.Reason == "" || len(in.Reason) > 4096 || len(in.EvidenceRefs) < 1 || len(in.EvidenceRefs) > 16 {
			return errcode.New(errcode.SchemaInvalid, "")
		}
		for _, ref := range in.EvidenceRefs {
			if ref.Validate(true) != nil || ref.InstanceID != s.d.InstanceID {
				return errcode.New(errcode.RefMismatch, "")
			}
			if _, e := s.d.Catalog.ExecutionVersion(ctx, w, ref.AssetID, ref.VersionID); e != nil {
				return e
			}
		}
		if j.State != "running" && j.State != "cancelling" && j.State != "needs_reconciliation" {
			return errcode.New(errcode.InvalidStateTransition, "")
		}
		if len(j.Reconciliations) >= 32 {
			return errcode.New(errcode.QuotaExceeded, "reconciliation history limit reached")
		}
		j.Reconciliations = append(j.Reconciliations, Reconciliation{Request: in, ActorID: w.PrincipalID, RecordedAt: clock.Format(s.d.Clock.Now())})
		j.State = "needs_reconciliation"
		if in.Stopped {
			j.State = "failed"
			j.Failure = "runtime_fault"
			j.Attempt.State = "completed"
			j.Attempt.Outcome = execution.InvocationRuntimeFault
			j.Attempt.Verdict = execution.VerdictUnknown
			j.Attempt.Termination.Confirmed = true
			if j.CancelRequested {
				j.State = "cancelled"
				j.Attempt.Outcome = execution.InvocationCancelled
			}
		}
		return nil
	})
	if e == nil && in.Stopped && j.Attempt != nil {
		// A confirmed stop settles the host invocation once: cancelled or fault.
		e = s.d.Host.Settle(ctx, j.Attempt.ID, j.Attempt.Outcome)
	}
	return j, e
}
