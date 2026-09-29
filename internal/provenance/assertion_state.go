package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/storage"
)

const AssertionContract = "lantai.rights-assertion/v1"

// Pointer fields distinguish an explicit replacement from an omitted patch.
// Uses is the entire effective declaration; the frozen manifest remains intact.
type AssertionFields struct {
	Usage                *string          `json:"usage,omitempty"`
	LicenseExpression    *string          `json:"license_expression,omitempty"`
	Sensitivity          *string          `json:"sensitivity,omitempty"`
	RedistributeRaw      *bool            `json:"redistribute_raw,omitempty"`
	NoAI                 *bool            `json:"noai,omitempty"`
	RestrictedSource     *bool            `json:"restricted_source,omitempty"`
	DenyPurposes         *[]authz.Purpose `json:"deny_purposes,omitempty"`
	ConfirmedEvidenceIDs *[]ids.ID        `json:"confirmed_evidence_ids,omitempty"`
	Uses                 *[]manifest.Use  `json:"uses,omitempty"`
}
type AssertionRequest struct {
	Subject             ids.PermanentRef `json:"subject"`
	ManifestDigest      digest.Digest    `json:"manifest_digest"`
	Kind                string           `json:"kind"`
	ReplacesAssertionID ids.ID           `json:"replaces_assertion_id,omitempty"`
	ExpectedRevision    int64            `json:"expected_revision"`
	Fields              AssertionFields  `json:"fields"`
	EvidenceIDs         []ids.ID         `json:"evidence_ids"`
	Reason              string           `json:"reason"`
}
type RightsAssertion struct {
	AssertionRequest
	ID           ids.ID `json:"assertion_id"`
	ActorID      ids.ID `json:"actor_id"`
	HumanGrantID ids.ID `json:"human_grant_id,omitempty"`
	OperationID  ids.ID `json:"operation_id"`
	CreatedAt    string `json:"created_at"`
}
type AssertionReceipt struct {
	storage.RecordRef
	Revision    int64  `json:"revision"`
	RightsEpoch int64  `json:"rights_epoch"`
	OperationID ids.ID `json:"operation_id"`
}
type effectiveAssertions struct {
	Rights           manifest.Rights
	Uses             []manifest.Use
	RestrictedSource bool
	DenyPurposes     []authz.Purpose
	Confirmed        map[ids.ID]bool
	Revision         int64
	LastID           ids.ID
	EvidenceRevision int64
}

func (s *Service) assertionRevision(ctx context.Context, q commands.DBTX, version ids.ID) (int64, ids.ID, error) {
	var revision int64
	var last ids.ID
	err := q.QueryRowContext(ctx, `SELECT revision,assertion_id FROM provenance_assertion_targets WHERE version_id=?`, version).Scan(&revision, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, "", nil
	}
	return revision, last, err
}
func (s *Service) RightsEpoch(ctx context.Context) (int64, error) {
	var epoch int64
	err := s.DB.QueryRowContext(ctx, `SELECT epoch FROM provenance_rights_epoch WHERE singleton=1`).Scan(&epoch)
	if err == nil && epoch < 1 {
		err = errcode.New(errcode.OperationNeedsReconciliation, "invalid rights epoch")
	}
	return epoch, err
}
func bumpRightsEpoch(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE provenance_rights_epoch SET epoch=epoch+1 WHERE singleton=1`); err != nil {
		return 0, err
	}
	var epoch int64
	err := tx.QueryRowContext(ctx, `SELECT epoch FROM provenance_rights_epoch WHERE singleton=1`).Scan(&epoch)
	return epoch, err
}
func applyAssertionFields(state *effectiveAssertions, f AssertionFields) {
	if f.Usage != nil {
		state.Rights.Usage = *f.Usage
	}
	if f.LicenseExpression != nil {
		state.Rights.License = *f.LicenseExpression
	}
	if f.Sensitivity != nil {
		state.Rights.Sensitivity = *f.Sensitivity
	}
	if f.RedistributeRaw != nil {
		state.Rights.RedistributeRaw = *f.RedistributeRaw
	}
	if f.NoAI != nil {
		state.Rights.NoAI = *f.NoAI
	}
	if f.RestrictedSource != nil {
		state.RestrictedSource = *f.RestrictedSource
	}
	if f.DenyPurposes != nil {
		state.DenyPurposes = slices.Clone(*f.DenyPurposes)
	}
	if f.Uses != nil {
		state.Uses = slices.Clone(*f.Uses)
	}
	if f.ConfirmedEvidenceIDs != nil {
		for _, id := range *f.ConfirmedEvidenceIDs {
			state.Confirmed[id] = true
		}
	}
}
func (s *Service) assertionState(ctx context.Context, v commit.Committed, doc manifest.Document) (effectiveAssertions, error) {
	out := effectiveAssertions{Rights: doc.Content.Rights, Uses: slices.Clone(doc.Content.Uses), Confirmed: map[ids.ID]bool{}, Revision: 1}
	type row struct {
		id, op             ids.ID
		revision, evidence int64
		hash               digest.Digest
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT assertion_id,operation_id,revision,evidence_revision,digest FROM provenance_assertions WHERE version_id=? ORDER BY revision`, v.VersionID)
	if err != nil {
		return out, err
	}
	var all []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.id, &r.op, &r.revision, &r.evidence, &r.hash); err != nil {
			rows.Close()
			return out, err
		}
		all = append(all, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	reg, err := schema.Default()
	if err != nil {
		return out, err
	}
	for _, r := range all {
		if r.revision != out.Revision+1 {
			return out, errcode.New(errcode.OperationNeedsReconciliation, "assertion revision gap")
		}
		raw, err := s.Files.ReadRecord(ctx, v.ProjectID, v.AssetID, v.VersionID, r.id)
		if err != nil {
			return out, err
		}
		if digest.Of(raw) != r.hash {
			return out, errcode.New(errcode.HashMismatch, "assertion evidence changed")
		}
		var record storage.Record
		if err = json.Unmarshal(raw, &record); err != nil {
			return out, err
		}
		if record.RecordID != r.id || record.OperationID != r.op || record.VersionID != v.VersionID || record.AssetID != v.AssetID || record.ProjectID != v.ProjectID || record.ManifestDigest != v.ManifestDigest || record.Kind != "rights_assertion" || record.PayloadSchema != AssertionContract {
			return out, errcode.New(errcode.RefMismatch, "assertion record target differs")
		}
		if err = reg.ValidateJSON(AssertionContract, record.Payload); err != nil {
			return out, err
		}
		var a RightsAssertion
		if err = json.Unmarshal(record.Payload, &a); err != nil {
			return out, err
		}
		if a.ID != r.id || a.OperationID != r.op || a.Subject != v.Ref(s.InstanceID) || a.ManifestDigest != v.ManifestDigest || a.ActorID != record.AuthorID || a.CreatedAt != record.CreatedAt || a.ExpectedRevision != out.Revision {
			return out, errcode.New(errcode.RefMismatch, "assertion identity differs")
		}
		if a.ReplacesAssertionID != "" && a.ReplacesAssertionID != out.LastID {
			return out, errcode.New(errcode.OperationNeedsReconciliation, "assertion replacement chain differs")
		}
		applyAssertionFields(&out, a.Fields)
		out.Revision = r.revision
		out.LastID = r.id
		out.EvidenceRevision = r.evidence
	}
	revision, last, err := s.assertionRevision(ctx, s.DB, v.VersionID)
	if err != nil {
		return out, err
	}
	if out.Revision != revision || out.LastID != last {
		return out, errcode.New(errcode.OperationNeedsReconciliation, "assertion current revision differs")
	}
	return out, nil
}

// Conservative restriction commands cannot remove an edge, confirm unknown
// evidence or loosen any existing field. Such changes require human correction.
func monotoneAssertion(before effectiveAssertions, f AssertionFields) bool {
	rank := map[string]int{"production": 0, "reference": 1, "restricted": 2}
	if f.Usage != nil && rank[*f.Usage] < rank[before.Rights.Usage] {
		return false
	}
	if f.Sensitivity != nil && before.Rights.Sensitivity == "personal" && *f.Sensitivity != "personal" {
		return false
	}
	if f.RedistributeRaw != nil && !before.Rights.RedistributeRaw && *f.RedistributeRaw {
		return false
	}
	if f.NoAI != nil && before.Rights.NoAI && !*f.NoAI {
		return false
	}
	if f.RestrictedSource != nil && before.RestrictedSource && !*f.RestrictedSource {
		return false
	}
	if f.LicenseExpression != nil && *f.LicenseExpression != before.Rights.License && !unknownLicense(*f.LicenseExpression) {
		return false
	}
	if f.Uses != nil || f.ConfirmedEvidenceIDs != nil {
		return false
	}
	if f.DenyPurposes != nil {
		for _, p := range before.DenyPurposes {
			if !slices.Contains(*f.DenyPurposes, p) {
				return false
			}
		}
	}
	return true
}

// EffectiveUses is a trusted core projection port. Callers must authorize both
// endpoints before disclosure; this is not an end-user listing API.
func (s *Service) EffectiveUses(ctx context.Context, asset, version ids.ID) ([]manifest.Use, error) {
	v, doc, err := s.read(ctx, ids.PermanentRef{InstanceID: s.InstanceID, AssetID: asset, VersionID: version})
	if err != nil {
		return nil, err
	}
	state, err := s.assertionState(ctx, v, doc)
	if err != nil {
		return nil, err
	}
	return state.Uses, nil
}
