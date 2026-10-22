package ledger

import (
	"context"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// The existing T03 policy owns whether an otherwise qualified human may review
// their own work. New decisions and current applicability use the same rule.
func validateHumanSelfReviewPolicy(self bool, policies map[string]identity.PolicyValue) error {
	if self && !policyBool(policies, "review.allow_self_human") {
		return errcode.New(errcode.SelfReviewForbidden, "")
	}
	return nil
}

// ValidateEffectiveApproval revalidates an existing exact T03 decision under
// current permissions/profile/check provenance. It neither approves nor publishes.
func (r *Reviews) ValidateEffectiveApproval(ctx context.Context, who authz.Context, fact ReviewFact) error {
	if !r.ledger.gate.HoldsSecurity(ctx) {
		guarded, held, e := r.ledger.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
		if e != nil {
			return e
		}
		defer held.Release()
		ctx = guarded
	}
	current, e := r.ReviewByID(ctx, fact.Review.ID)
	if e != nil {
		return e
	}
	if encoded(current) != encoded(fact) {
		return errcode.New(errcode.ReviewTargetStale, "")
	}
	t := current.Target
	v, e := r.ledger.Version(ctx, t.AssetID, t.VersionID)
	if e != nil {
		return e
	}
	if e = r.ledger.authorize(ctx, who, "catalog.read", v.ProjectID, "version", v.VersionID); e != nil {
		return e
	}
	if e = r.ledger.CheckVersionRead(ctx, v.AssetID, v.VersionID); e != nil {
		return e
	}
	state, e := r.ledger.VersionControl(ctx, v.VersionID)
	if e != nil {
		return e
	}
	if state.ReviewState != "approved" || state.EffectiveReviewID != fact.Review.ID || fact.Review.Verdict != "approve" || fact.Review.ManifestDigest != v.ManifestDigest || fact.Review.TargetID != t.ID {
		return errcode.New(errcode.ReviewTargetStale, "")
	}
	if e = r.sources.Task(ctx, who, v, t.Flow, "effective_approval"); e != nil {
		return e
	}
	policies, e := r.policies.ResolvePolicies(ctx, t.ProjectID)
	if e != nil {
		return e
	}
	self := t.MakerID == current.Review.ActorID
	if current.Review.SelfReview != self {
		return errcode.New(errcode.ReviewTargetStale, "self-review audit does not match the fixed target")
	}
	if e = validateHumanSelfReviewPolicy(self, policies); e != nil {
		return e
	}
	return r.satisfied(ctx, who, v, t, current.Review.Waivers, policyBool(policies, "review.qa_required"), current.Review.ActorID)
}
