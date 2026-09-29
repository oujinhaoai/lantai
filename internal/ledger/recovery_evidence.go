package ledger

import (
	"context"
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/storage"
)

type EvidenceFiles interface {
	ReadRecord(context.Context, ids.ID, ids.ID, ids.ID, ids.ID) ([]byte, error)
}

// CheckEvidenceFiles verifies accepted review records without task execution or
// end-user authority. It is a trusted integrity port, not a delivery API.
func (s *Service) CheckEvidenceFiles(ctx context.Context, files EvidenceFiles, records []storage.RecordEntry) (findings, residuals []storage.Finding, err error) {
	reg, err := schema.Default()
	if err != nil {
		return nil, nil, err
	}
	for _, r := range records {
		v, e := s.Version(ctx, r.AssetID, r.VersionID)
		reason := ""
		if e != nil || v.ProjectID != r.ProjectID {
			reason = "check_target_missing"
		} else {
			location, e := s.VersionFileLocation(ctx, v.AssetID, v.VersionID)
			if e != nil {
				return findings, residuals, e
			}
			if location.Purged {
				continue
			}
			if location.PendingOperationID != "" {
				residuals = append(residuals, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: "check_lifecycle_pending", OperationID: r.OperationID})
				continue
			}
			raw, e := files.ReadRecord(ctx, r.ProjectID, r.AssetID, r.VersionID, r.RecordID)
			var record storage.Record
			var input ReviewEvidenceInput
			switch {
			case e != nil:
				reason = "check_file_missing"
			case digest.Of(raw) != r.Digest:
				reason = "check_file_digest"
			case json.Unmarshal(raw, &record) != nil:
				reason = "check_file_invalid"
			case record.RecordID != r.RecordID || record.OperationID != r.OperationID || record.ProjectID != r.ProjectID || record.AssetID != r.AssetID || record.VersionID != r.VersionID || record.ManifestDigest != v.ManifestDigest || record.PayloadSchema != "lantai.review-evidence/v1":
				reason = "check_file_identity"
			case reg.ValidateJSON("lantai.review-evidence/v1", record.Payload) != nil || json.Unmarshal(record.Payload, &input) != nil:
				reason = "check_payload_invalid"
			case input.Ref.AssetID != v.AssetID || input.Ref.VersionID != v.VersionID || input.ManifestDigest != v.ManifestDigest || input.Kind != record.Kind:
				reason = "check_payload_identity"
			}
		}
		if reason != "" {
			findings = append(findings, storage.Finding{Code: errcode.OperationNeedsReconciliation, Reason: reason, OperationID: r.OperationID})
		}
	}
	return findings, residuals, nil
}
