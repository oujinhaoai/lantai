package agent_execution

import (
	"context"
	"slices"
	"strings"

	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
)

type ProgressRequest struct {
	Mutation
	Usage ae.BudgetUsage   `json:"usage"`
	Step  *ae.AgentStepRun `json:"step,omitempty"`
}

// Progress records cumulative reported usage; it cannot reset a retry's budget.
// manual-cli observations are not a claim of OS enforced time or model metering.
func (s *Service) Progress(ctx context.Context, who authz.Context, key string, in ProgressRequest) (Record, error) {
	return s.mutate(ctx, who, key, "agent_execution.progress", in.Mutation, in, "work", func(ctx context.Context, r *Record) error {
		u := in.Usage
		if u.WallTimeMS < r.Usage.WallTimeMS || u.ModelCalls < r.Usage.ModelCalls || u.ToolCalls < r.Usage.ToolCalls || u.ArtifactBytes < r.Usage.ArtifactBytes {
			return bad("usage cannot decrease")
		}
		if e := checkBudget(r.Profile.BudgetLimits, u); e != nil {
			return e
		}
		if in.Step != nil {
			p := *in.Step
			if e := tc.ValidateShape("lantai.agent-step-run/v1", p); e != nil {
				return e
			}
			if p.TaskRunID != r.Run.ID || p.AttemptID != r.Run.CurrentAttemptID || p.ParentID != "" {
				return bad("manual steps require current run and attempt; subagent hosting unsupported")
			}
			if p.Kind == "delegate" {
				return errcode.New(errcode.UnsupportedCapability, "manual subagent hosting is unavailable")
			}
			links := []ids.ID{}
			active := 0
			for _, tool := range r.Tools {
				if tool.AgentStepRunID == p.ID {
					links = append(links, tool.ID)
					if p.State == "completed" && (tool.State == execution.EffectDispatched || tool.State == execution.EffectUnknown) {
						return errcode.New(errcode.OperationNeedsReconciliation, "")
					}
				}
			}
			if !slices.Equal(p.ToolOperationIDs, links) {
				return bad("tools are linked by the intent log")
			}
			if p.State == "running" {
				for _, old := range r.Steps {
					if old.ID != p.ID && old.AttemptID == p.AttemptID && old.State == "running" {
						active++
					}
				}
				if int64(active) >= r.Profile.BudgetLimits.Concurrency {
					return errcode.New(errcode.BudgetExhausted, "step concurrency limit reached")
				}
			}
			for _, ref := range append(slices.Clone(p.InputRefs), p.OutputRefs...) {
				if e := s.d.Assets.CheckExecutionRef(ctx, who, ref); e != nil {
					return e
				}
			}
			i := slices.IndexFunc(r.Steps, func(x ae.AgentStepRun) bool { return x.ID == p.ID })
			if i < 0 {
				if p.Revision != 1 {
					return bad("new step revision must be one")
				}
				r.Steps = append(r.Steps, p)
			} else {
				old := r.Steps[i]
				if p.Revision != old.Revision+1 || old.AttemptID != p.AttemptID || p.PlanRevision < old.PlanRevision || old.State == "completed" || old.State == "cancelled" || old.State == "failed" {
					return state("stale or terminal step")
				}
				r.Steps[i] = p
			}
		}
		r.Usage = u
		return nil
	})
}

type ToolRequest struct {
	Mutation
	Tool ae.ToolOperation `json:"tool"`
}

func unresolved(r Record) int {
	n := 0
	for _, t := range r.Tools {
		if t.State == execution.EffectDispatched || t.State == execution.EffectUnknown {
			n++
		}
	}
	return n
}
func (s *Service) Tool(ctx context.Context, who authz.Context, key string, in ToolRequest) (Record, error) {
	return s.mutate(ctx, who, key, "agent_execution.tool", in.Mutation, in, "work", func(ctx context.Context, r *Record) error {
		t := in.Tool
		if e := tc.ValidateShape("lantai.tool-operation/v1", t); e != nil {
			return e
		}
		if t.TaskRunID != r.Run.ID || t.AttemptID != r.Run.CurrentAttemptID {
			return bad("tool run or attempt mismatch")
		}
		if !slices.ContainsFunc(r.Steps, func(x ae.AgentStepRun) bool {
			return x.ID == t.AgentStepRunID && x.AttemptID == t.AttemptID && x.State == "running"
		}) {
			return bad("running step required")
		}
		if !slices.Contains(r.Profile.AllowedTools, t.Tool) {
			return errcode.New(errcode.Forbidden, "tool not in fixed profile")
		}
		spec, ok := identity.Spec(authz.Action(t.Tool))
		if !ok || spec.HumanOnly || spec.HumanGrant {
			return errcode.New(errcode.Forbidden, "tool is not an agent action")
		}
		if e := s.authorize(ctx, who, spec.Action, *r); e != nil {
			return e
		}
		i := slices.IndexFunc(r.Tools, func(x ae.ToolOperation) bool { return x.ID == t.ID })
		if i < 0 {
			if t.Sequence != int64(len(r.Tools)+1) || t.State != execution.EffectIntended || t.ReceiptDigest != "" || len(t.ReconciliationEvidenceRefs) > 0 {
				return bad("persist a sequential intent before dispatch")
			}
			if t.IdempotencyKey != "" && slices.ContainsFunc(r.Tools, func(old ae.ToolOperation) bool { return old.Tool == t.Tool && old.IdempotencyKey == t.IdempotencyKey }) {
				return errcode.New(errcode.IdempotencyConflict, "tool key already belongs to a persisted intent")
			}
			// Only the original operation may use a known request/key; unknown effects
			// cannot be bypassed by appending a new intent with a different ID.
			if unresolved(*r) > 0 {
				return errcode.New(errcode.OperationNeedsReconciliation, "")
			}
			u := r.Usage
			u.ToolCalls++
			if e := checkBudget(r.Profile.BudgetLimits, u); e != nil {
				return e
			}
			r.Usage = u
			r.Tools = append(r.Tools, t)
			step := slices.IndexFunc(r.Steps, func(x ae.AgentStepRun) bool { return x.ID == t.AgentStepRunID })
			r.Steps[step].ToolOperationIDs = append(r.Steps[step].ToolOperationIDs, t.ID)
			r.Steps[step].Revision++
		} else {
			old := r.Tools[i]
			base := t
			base.State = old.State
			base.ReceiptDigest = old.ReceiptDigest
			base.ReconciliationEvidenceRefs = old.ReconciliationEvidenceRefs
			if encoded(base) != encoded(old) {
				return bad("tool intent is immutable")
			}
			allowed := old.State == execution.EffectIntended && t.State == execution.EffectDispatched || old.State == execution.EffectDispatched && (t.State == execution.EffectCompleted || t.State == execution.EffectFailed || t.State == execution.EffectUnknown)
			if !allowed {
				return state("tool state requires explicit reconciliation")
			}
			if old.State == execution.EffectIntended && t.State == execution.EffectDispatched && unresolved(*r) > 0 {
				return errcode.New(errcode.OperationNeedsReconciliation, "another dispatched tool must be reconciled first")
			}
			if t.State == execution.EffectCompleted && !t.ReceiptDigest.Valid() {
				return bad("completed tool requires receipt digest")
			}
			r.Tools[i] = t
		}
		r.Run.Termination.UnresolvedEffects = unresolved(*r)
		return nil
	})
}

type CheckpointRequest struct {
	Mutation
	Checkpoint ae.Checkpoint `json:"checkpoint"`
	Stopped    bool          `json:"stopped"`
}

func (s *Service) checkpoint(ctx context.Context, who authz.Context, r *Record, p ae.Checkpoint) error {
	if e := tc.ValidateShape("lantai.checkpoint/v1", p); e != nil {
		return e
	}
	if p.TaskRunID != r.Run.ID || p.AttemptID != r.Run.CurrentAttemptID || p.Sequence != int64(len(r.Checkpoints)+1) || p.InputSnapshotDigest != r.Run.Input.Digest || p.ProfileDigest != r.Run.Profile.Digest || p.SideEffectWatermark != int64(len(r.Tools)) {
		return bad("checkpoint binding or watermark mismatch")
	}
	if p.BackendVersion != Capabilities().BackendVersion || !slices.Contains(Capabilities().CheckpointFormatVersions, p.FormatVersion) || !slices.Contains(Capabilities().ResumeClasses, p.ResumeClass) {
		return errcode.New(errcode.ResumeUnsupported, "")
	}
	if unresolved(*r) > 0 {
		return errcode.New(errcode.OperationNeedsReconciliation, "")
	}
	for _, ref := range p.ArtifactRefs {
		if e := s.d.Assets.CheckExecutionRef(ctx, who, ref); e != nil {
			return e
		}
	}
	for _, id := range p.CompletedStepIDs {
		if !slices.ContainsFunc(r.Steps, func(x ae.AgentStepRun) bool { return x.ID == id && x.State == "completed" }) {
			return bad("checkpoint cites an incomplete step")
		}
	}
	p.CreatedAt = clock.Format(s.d.Clock.Now())
	r.Checkpoints = append(r.Checkpoints, p)
	return nil
}
func (s *Service) Checkpoint(ctx context.Context, who authz.Context, key string, in CheckpointRequest) (Record, error) {
	return s.mutate(ctx, who, key, "agent_execution.checkpoint", in.Mutation, in, "work", func(ctx context.Context, r *Record) error {
		if !in.Stopped {
			return state("manual checkpoint handoff requires session stop confirmation")
		}
		if e := s.checkpoint(ctx, who, r, in.Checkpoint); e != nil {
			return e
		}
		r.Run.State = execution.RunPaused
		r.Run.Termination.Confirmed = true
		return nil
	})
}

type InputRequest struct {
	CheckpointRequest
	Input HumanInput `json:"input"`
}

func (s *Service) Ask(ctx context.Context, who authz.Context, key string, in InputRequest) (Record, error) {
	return s.mutate(ctx, who, key, "agent_execution.ask", in.Mutation, in, "work", func(ctx context.Context, r *Record) error {
		q := in.Input
		expires, e := clock.Parse(q.ExpiresAt)
		if e != nil || !expires.After(s.d.Clock.Now()) || !q.ID.Valid() || q.Answer != "" || q.AnsweredBy != "" || strings.TrimSpace(q.Question) == "" || len(q.Question) > 8192 || !slices.Contains([]string{"clarification", "scope_change", "external_action_approval", "budget_change"}, q.Kind) {
			return bad("invalid input request")
		}
		target, e := InputTargetDigest(*r, q)
		if e != nil {
			return e
		}
		if q.TargetDigest != "" && q.TargetDigest != target {
			return errcode.New(errcode.RefMismatch, "input target digest differs")
		}
		q.TargetDigest = target
		if !in.Stopped {
			return state("waiting requires confirmed stopped session")
		}
		if slices.ContainsFunc(r.Inputs, func(x HumanInput) bool { return x.ID == q.ID }) {
			return bad("duplicate input request")
		}
		if e = s.checkpoint(ctx, who, r, in.Checkpoint); e != nil {
			return e
		}
		r.Inputs = append(r.Inputs, q)
		r.Run.State = execution.RunWaitingInput
		r.Run.Termination.Confirmed = true
		return nil
	})
}

// InputTargetDigest binds the question to immutable run inputs and budget.
func InputTargetDigest(r Record, q HumanInput) (digest.Digest, error) {
	raw, e := canonjson.CanonicalizeValue(struct {
		RunID    ids.ID        `json:"run_id"`
		Input    digest.Digest `json:"input"`
		Profile  digest.Digest `json:"profile"`
		Budget   ids.ID        `json:"budget_id"`
		ID       ids.ID        `json:"input_id"`
		Kind     string        `json:"kind"`
		Question string        `json:"question"`
		Expires  string        `json:"expires_at"`
	}{r.Run.ID, r.Run.Input.Digest, r.Run.Profile.Digest, r.Run.BudgetID, q.ID, q.Kind, q.Question, q.ExpiresAt})
	if e != nil {
		return "", e
	}
	return digest.Of(raw), nil
}

type AnswerRequest struct {
	Mutation
	InputID      ids.ID        `json:"input_id"`
	TargetDigest digest.Digest `json:"target_digest"`
	Answer       string        `json:"answer"`
}

func (s *Service) Answer(ctx context.Context, who authz.Context, key string, in AnswerRequest) (Record, error) {
	return s.mutate(ctx, who, key, "agent_execution.answer", in.Mutation, in, "answer", func(ctx context.Context, r *Record) error {
		current, e := s.d.Tasks.Task(ctx, who, r.Run.TaskID)
		if e != nil {
			return e
		}
		epoch, e := s.d.Authority.RecoveryEpoch(ctx)
		if e != nil {
			return e
		}
		if current.Task.State == "cancelled" || current.Task.State == "done" || current.Attempt == nil || current.Attempt.ID != r.Run.CurrentAttemptID || current.Attempt.Fence.RecoveryEpoch != epoch || current.Task.Input.Digest != r.Run.Input.Digest {
			return errcode.New(errcode.LeaseStale, "input target was superseded")
		}
		if r.Run.State != execution.RunWaitingInput {
			return state("not waiting for input")
		}
		if who.PrincipalKind != authz.Human || who.DelegatedBy != "" {
			return errcode.New(errcode.Forbidden, "human input requires a human session")
		}
		i := slices.IndexFunc(r.Inputs, func(x HumanInput) bool { return x.ID == in.InputID })
		if i < 0 {
			return errcode.New(errcode.NotFound, "")
		}
		q := &r.Inputs[i]
		expires, e := clock.Parse(q.ExpiresAt)
		if e != nil || !s.d.Clock.Now().Before(expires) || q.Answer != "" || q.TargetDigest != in.TargetDigest || strings.TrimSpace(in.Answer) == "" || len(in.Answer) > 8192 {
			return bad("stale input or invalid answer")
		}
		q.Answer = in.Answer
		q.AnsweredBy = who.PrincipalID
		r.Run.State = execution.RunPaused
		return nil // Never restores a lease, raises a budget or approves an asset.
	})
}

type CancelRequest struct {
	Mutation
	Reason string               `json:"reason"`
	Mode   execution.CancelMode `json:"cancel_mode"`
}

func (s *Service) Cancel(ctx context.Context, who authz.Context, key string, in CancelRequest) (Record, error) {
	return s.cancel(ctx, who, key, in, in)
}
func (s *Service) cancel(ctx context.Context, who authz.Context, key string, in CancelRequest, body any) (Record, error) {
	return s.mutate(ctx, who, key, "agent_execution.cancel", in.Mutation, body, "cancel", func(_ context.Context, r *Record) error {
		if in.Mode != execution.CancelRevokeOnly {
			return errcode.New(errcode.UnsupportedCapability, "")
		}
		if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 {
			return bad("bounded cancel reason required")
		}
		if r.Run.State.Terminal() {
			return state("sealed run")
		}
		r.CancelRequested = true
		r.Run.State = execution.ResolveCancel(r.Run.Termination.Confirmed, false, unresolved(*r))
		return nil
	})
}

type Reconciliation struct {
	Request    ReconcileRequest `json:"request"`
	ActorID    ids.ID           `json:"actor_id"`
	RecordedAt string           `json:"recorded_at"`
}

type ReconcileRequest struct {
	Checkpoint *ae.Checkpoint `json:"checkpoint,omitempty"`
	Mutation
	Stopped       bool                  `json:"stopped"`
	ToolID        ids.ID                `json:"tool_id,omitempty"`
	ToolState     execution.EffectState `json:"tool_state,omitempty"`
	ReceiptDigest digest.Digest         `json:"receipt_digest,omitempty"`
	EvidenceRefs  []ids.PermanentRef    `json:"evidence_refs"`
	Reason        string                `json:"reason"`
}

func (s *Service) Reconcile(ctx context.Context, who authz.Context, key string, in ReconcileRequest) (Record, error) {
	return s.mutate(ctx, who, key, "agent_execution.reconcile", in.Mutation, in, "reconcile", func(ctx context.Context, r *Record) error {
		if r.Run.State.Terminal() {
			return state("sealed run")
		}
		if strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 || len(in.EvidenceRefs) == 0 || len(in.EvidenceRefs) > 32 {
			return bad("bounded reconciliation evidence and reason required")
		}
		for _, ref := range in.EvidenceRefs {
			if e := s.d.Assets.CheckExecutionRef(ctx, who, ref); e != nil {
				return e
			}
		}
		if in.ToolID != "" {
			i := slices.IndexFunc(r.Tools, func(x ae.ToolOperation) bool { return x.ID == in.ToolID })
			if i < 0 {
				return errcode.New(errcode.NotFound, "")
			}
			t := &r.Tools[i]
			if t.State != execution.EffectUnknown && t.State != execution.EffectDispatched {
				return state("tool has no unresolved effect")
			}
			if in.ToolState != execution.EffectCompleted && in.ToolState != execution.EffectNotExecuted && in.ToolState != execution.EffectFailed {
				return bad("reconciliation requires a definite outcome")
			}
			if in.ToolState == execution.EffectCompleted && !in.ReceiptDigest.Valid() {
				return bad("receipt required")
			}
			t.State = in.ToolState
			t.ReceiptDigest = in.ReceiptDigest
			for _, ref := range in.EvidenceRefs {
				t.ReconciliationEvidenceRefs = append(t.ReconciliationEvidenceRefs, ref.VersionID)
			}
		}
		if in.Checkpoint != nil {
			if !in.Stopped || r.CancelRequested {
				return state("recovery checkpoint requires stopped noncancelled session")
			}
			if e := s.checkpoint(ctx, who, r, *in.Checkpoint); e != nil {
				return e
			}
		}
		if len(r.Reconciliations) >= 32 {
			return errcode.New(errcode.QuotaExceeded, "reconciliation history limit reached")
		}
		r.Reconciliations = append(r.Reconciliations, Reconciliation{Request: in, ActorID: who.PrincipalID, RecordedAt: clock.Format(s.d.Clock.Now())})
		r.Run.Termination = ae.Termination{Confirmed: in.Stopped, UnresolvedEffects: unresolved(*r)}
		if r.CancelRequested {
			r.Run.State = execution.ResolveCancel(in.Stopped, true, unresolved(*r))
		} else if !in.Stopped || unresolved(*r) > 0 {
			r.Run.State = execution.RunNeedsReconciliation
		} else {
			r.Run.State = execution.RunPaused
		}
		return nil
	})
}

type CandidateRequest struct {
	Mutation
	Candidate ae.ArtifactCandidate `json:"candidate"`
	Ref       ids.PermanentRef     `json:"ref"`
}
type CandidateVerifier interface {
	CheckCandidate(context.Context, authz.Context, ids.PermanentRef, ae.ArtifactCandidate) error
}

func (s *Service) Candidate(ctx context.Context, who authz.Context, key string, in CandidateRequest) (Record, error) {
	return s.mutate(ctx, who, key, "agent_execution.candidate", in.Mutation, in, "work", func(ctx context.Context, r *Record) error {
		a := in.Candidate
		if e := a.Validate(); e != nil {
			return e
		}
		if a.TaskRunID != r.Run.ID || a.AttemptID != r.Run.CurrentAttemptID || slices.ContainsFunc(r.Candidates, func(x ae.ArtifactCandidate) bool { return x.ID == a.ID }) {
			return bad("candidate binding or duplicate ID")
		}
		verifier, ok := s.d.Assets.(CandidateVerifier)
		if !ok {
			return errcode.New(errcode.UnsupportedCapability, "candidate byte verification unavailable")
		}
		if e := verifier.CheckCandidate(ctx, who, in.Ref, a); e != nil {
			return e
		}
		u := r.Usage
		for _, f := range a.Files {
			if f.Size > r.Profile.BudgetLimits.ArtifactBytes-u.ArtifactBytes {
				return errcode.New(errcode.BudgetExhausted, "")
			}
			u.ArtifactBytes += f.Size
		}
		if e := checkBudget(r.Profile.BudgetLimits, u); e != nil {
			return e
		}
		r.Usage = u
		r.Candidates = append(r.Candidates, a)
		if r.CandidateRefs == nil {
			r.CandidateRefs = map[ids.ID]ids.PermanentRef{}
		}
		r.CandidateRefs[a.ID] = in.Ref
		return nil
	})
}

type SealRequest struct {
	Mutation
	Outcome      execution.TaskRunState `json:"outcome"`
	Stopped      bool                   `json:"stopped"`
	CandidateIDs []ids.ID               `json:"candidate_ids"`
	Limitations  []string               `json:"limitations"`
}

func (s *Service) Seal(ctx context.Context, who authz.Context, key string, in SealRequest) (Record, error) {
	return s.mutate(ctx, who, key, "agent_execution.seal", in.Mutation, in, "work", func(ctx context.Context, r *Record) error {
		if !in.Stopped || unresolved(*r) > 0 {
			return errcode.New(errcode.OperationNeedsReconciliation, "")
		}
		if in.Outcome != execution.RunExecutionSucceeded && in.Outcome != execution.RunFailed {
			return bad("execution result is not an asset approval")
		}
		for _, id := range in.CandidateIDs {
			if !slices.ContainsFunc(r.Candidates, func(x ae.ArtifactCandidate) bool { return x.ID == id && x.AttemptID == r.Run.CurrentAttemptID }) {
				return bad("result refers to an unaccepted candidate")
			}
			verifier, ok := s.d.Assets.(CandidateVerifier)
			if !ok {
				return errcode.New(errcode.UnsupportedCapability, "candidate verifier unavailable")
			}
			candidate := r.Candidates[slices.IndexFunc(r.Candidates, func(x ae.ArtifactCandidate) bool { return x.ID == id })]
			if e := verifier.CheckCandidate(ctx, who, r.CandidateRefs[id], candidate); e != nil {
				return e
			}
		}
		out := ae.Result{Contract: "lantai.agent-result/v1", ExecutionID: r.Current.ExecutionID, TaskRunID: r.Run.ID, AttemptID: r.Run.CurrentAttemptID, AcceptedRequestHash: r.Current.AcceptedRequestHash, Outcome: in.Outcome, CandidateIDs: in.CandidateIDs, EvidenceRefs: []ids.ID{}, BudgetUsage: r.Usage, Limitations: in.Limitations, SealedAt: clock.Format(s.d.Clock.Now()), Termination: ae.Termination{Confirmed: true}}
		h, e := ae.ResultHash(out)
		if e != nil {
			return e
		}
		out.ResultDigest = h
		if e = out.Validate(); e != nil {
			return e
		}
		r.Result = &out
		r.Run.State = out.Outcome
		r.Run.Termination = out.Termination
		return nil
	})
}
