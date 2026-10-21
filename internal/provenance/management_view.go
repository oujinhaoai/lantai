package provenance

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type RightsEvidenceView struct {
	RecordID ids.ID `json:"record_id"`
	Revision int64  `json:"revision"`
}

type PurposeView struct {
	Purpose     authz.Purpose `json:"purpose"`
	Allowed     bool          `json:"allowed"`
	Code        errcode.Code  `json:"code,omitempty"`
	RightsEpoch int64         `json:"rights_epoch"`
}

// VersionRightsView describes current authority, never a replacement manifest,
// download capability, approval or durable guarantee about future access.
type VersionRightsView struct {
	Subject          ids.PermanentRef     `json:"subject"`
	ManifestDigest   digest.Digest        `json:"manifest_digest"`
	Revision         int64                `json:"revision"`
	LastAssertionID  ids.ID               `json:"last_assertion_id,omitempty"`
	Rights           manifest.Rights      `json:"rights"`
	RestrictedSource bool                 `json:"restricted_source"`
	DenyPurposes     []authz.Purpose      `json:"deny_purposes"`
	EvidenceRevision int64                `json:"evidence_revision"`
	Evidence         []RightsEvidenceView `json:"evidence"`
	Purposes         []PurposeView        `json:"purposes"`
}

// VersionRights resolves an exact local version under the shared security guard.
// It checks current read/personal/risk controls and accepted immutable evidence
// before exposing any state. It never enumerates source URLs or hidden uses.
func (s *Service) VersionRights(ctx context.Context, who authz.Context, ref ids.PermanentRef) (VersionRightsView, error) {
	var out VersionRightsView
	if err := who.Validate(); err != nil {
		return out, err
	}
	if ref.InstanceID != s.InstanceID || !ref.AssetID.Valid() || !ref.VersionID.Valid() {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	v, err := s.Reader.Version(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return out, err
	}
	ctx, guard, err := s.Gate.Acquire(ctx, commands.Request{Security: commands.ModeShared, Projects: []string{string(v.ProjectID)}, Assets: []string{string(v.AssetID)}})
	if err != nil {
		return out, err
	}
	defer guard.Release()
	ok, err := s.allowed(ctx, who, "catalog.read", v)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, errcode.New(errcode.NotFound, "")
	}
	risk, err := s.EvaluateRiskAccess(ctx, who, ref)
	if err != nil {
		return out, err
	}
	if !risk.Allowed {
		if risk.Code == errcode.RightsPending {
			return out, risk.Err()
		}
		return out, errcode.New(errcode.NotFound, "")
	}
	v, doc, err := s.read(ctx, ref)
	if err != nil {
		return out, err
	}
	state, err := s.assertionState(ctx, v, doc)
	if err != nil {
		return out, err
	}
	if _, _, err = s.evidenceState(ctx, v, state.Confirmed); err != nil {
		return out, errcode.New(errcode.RightsPending, "accepted rights evidence cannot be verified")
	}
	evidenceRevision, err := s.CurrentRevision(ctx, v.VersionID)
	if err != nil {
		return out, err
	}
	if evidenceRevision > 1000 {
		return out, errcode.New(errcode.RightsPending, "rights evidence exceeds management view budget")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT record_id,revision FROM provenance_records WHERE version_id=? ORDER BY revision LIMIT 1001`, v.VersionID)
	if err != nil {
		return out, err
	}
	evidence := []RightsEvidenceView{}
	for rows.Next() {
		var item RightsEvidenceView
		if err = rows.Scan(&item.RecordID, &item.Revision); err != nil {
			rows.Close()
			return out, err
		}
		evidence = append(evidence, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if int64(len(evidence)) != evidenceRevision {
		return out, errcode.New(errcode.RightsPending, "rights evidence changed during observation")
	}
	purposes := []PurposeView{}
	for _, purpose := range []authz.Purpose{authz.PurposeArchiveReview, authz.PurposeReference, authz.PurposeProduction, authz.PurposeGenerativeInput, authz.PurposeRawExport} {
		decision, err := s.EvaluateUse(ctx, who, ref, purpose)
		if err != nil {
			return out, err
		}
		purposes = append(purposes, PurposeView{Purpose: purpose, Allowed: decision.Allowed, Code: decision.Code, RightsEpoch: decision.RightsEpoch})
	}
	deny := append([]authz.Purpose{}, state.DenyPurposes...)
	return VersionRightsView{Subject: ref, ManifestDigest: v.ManifestDigest, Revision: state.Revision, LastAssertionID: state.LastID, Rights: state.Rights, RestrictedSource: state.RestrictedSource, DenyPurposes: deny, EvidenceRevision: evidenceRevision, Evidence: evidence, Purposes: purposes}, nil
}
