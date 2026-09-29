package ledger

import (
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

func validateReviewContract(contract string, value any) error {
	raw, err := canonjson.CanonicalizeValue(value)
	if err != nil {
		return err
	}
	registry, err := schema.Default()
	if err != nil {
		return err
	}
	if err = registry.ValidateJSON(contract, raw); err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "invalid review record", err)
	}
	return nil
}

// Waiver fields are immutable within the accepted Review, and must agree with
// both the exact human decision and its fixed target. No separate waiver table
// or mutable waiver authority is introduced.
func validateReviewWaivers(review Review, target ReviewTarget) error {
	if err := validateReviewContract("lantai.review/v1", review); err != nil {
		return err
	}
	if review.TargetID != target.ID || review.VersionID != target.VersionID || review.ManifestDigest != target.ManifestDigest || review.ProfileDigest != target.Profile.ManifestDigest || len(review.WaiverRecords) != len(review.Waivers) {
		return errcode.New(errcode.RefMismatch, "review binding differs")
	}
	for i, w := range review.WaiverRecords {
		expected, err := ids.DeriveChild(review.OperationID, "waiver:"+digest.Of([]byte(review.Waivers[i].CheckKey)).Hex())
		if err != nil {
			return err
		}
		a, err := canonjson.CanonicalizeValue(w.ReviewWaiver)
		if err != nil {
			return err
		}
		b, err := canonjson.CanonicalizeValue(review.Waivers[i])
		if err != nil {
			return err
		}
		if w.ID != expected || string(a) != string(b) || w.VersionID != review.VersionID || w.ManifestDigest != review.ManifestDigest || w.TargetID != review.TargetID || w.ReviewID != review.ID || w.ApproverID != review.ActorID || w.HumanGrantID != review.HumanGrantID || w.OperationID != review.OperationID || w.CreatedAt != review.CreatedAt || w.Scope != "review_target" {
			return errcode.New(errcode.RefMismatch, "waiver binding differs")
		}
	}
	return nil
}
