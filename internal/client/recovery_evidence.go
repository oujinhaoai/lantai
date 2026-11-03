package client

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type RecoveryEvidenceSpec struct {
	ProjectID     string       `json:"project_id"`
	Title         string       `json:"title"`
	RunID         string       `json:"run_id"`
	Workset       WorksetInput `json:"workset"`
	Purpose       string       `json:"purpose"`
	AssetID       string       `json:"asset_id,omitempty"`
	BaseVersionID string       `json:"base_version_id,omitempty"`
}
type FixedVersionVerification struct {
	// Existing cross-client schema name is retained for page interoperability.
	Schema                string      `json:"schema"`
	Origin                string      `json:"origin"`
	InstanceID            string      `json:"instance_id"`
	AssetID               string      `json:"asset_id"`
	VersionID             string      `json:"version_id"`
	ManifestDigest        string      `json:"manifest_digest"`
	VerifiedAt            string      `json:"verified_at"`
	CompletionBasis       string      `json:"completion_basis"`
	CurrentRemoteVerified bool        `json:"current_remote_verified"`
	LocalSourceVerified   bool        `json:"local_source_verified"`
	Files                 []InputFile `json:"files"`
}
type RecoveryEvidence struct {
	Schema                             string                        `json:"schema"`
	EvidenceBasis                      string                        `json:"evidence_basis"`
	InstanceID                         string                        `json:"instance_id"`
	CurrentRecoveryEpoch               int64                         `json:"current_recovery_epoch"`
	ObservedAt                         string                        `json:"observed_at"`
	Restore                            apiv1.CompletedRestoreReceipt `json:"restore"`
	Verifications                      []FixedVersionVerification    `json:"verifications"`
	Scope                              string                        `json:"scope"`
	NativeSoftwareContinuationVerified bool                          `json:"native_software_continuation_verified"`
	SourceRetirementAuthorized         bool                          `json:"source_retirement_authorized"`
	UnknownAfterCaptureRemainsUnknown  bool                          `json:"unknown_after_capture_remains_unknown"`
}

func (c *Client) currentRecoveryReceipt(ctx context.Context, run string) (apiv1.InstanceManagementInfo, apiv1.CompletedRestoreReceipt, error) {
	var info apiv1.InstanceManagementInfo
	response, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/instance", nil, Options{})
	if err != nil {
		return info, apiv1.CompletedRestoreReceipt{}, err
	}
	if err := response.Decode(&info); err != nil {
		return info, apiv1.CompletedRestoreReceipt{}, err
	}
	if !ids.ID(info.InstanceId).Valid() || info.State != "ready" || info.RecoveryEpoch < 1 || info.RecoveryError != nil || info.ObservedAt.IsZero() {
		return info, apiv1.CompletedRestoreReceipt{}, errcode.New(errcode.PreconditionFailed, "current core recovery receipts are unavailable, partial or not ready")
	}
	for _, receipt := range info.RecoveryReceipts {
		if receipt.RunId != run {
			continue
		}
		if !ids.ID(receipt.RunId).Valid() || !ids.ID(receipt.BackupId).Valid() || !digest.Digest(receipt.ManifestDigest).Valid() || !digest.Digest(receipt.EvidenceDigest).Valid() || receipt.RecoveryEpoch < 1 || receipt.RecoveryEpoch > info.RecoveryEpoch || receipt.AcceptanceBasis != "accepted_capture_point" && receipt.AcceptanceBasis != "legacy_reconciliation" || receipt.AcceptanceBasis == "accepted_capture_point" && !receipt.UnknownAfterCapture || receipt.AcceptanceBasis == "legacy_reconciliation" && receipt.UnknownAfterCapture {
			break
		}
		return info, receipt, nil
	}
	return info, apiv1.CompletedRestoreReceipt{}, errcode.New(errcode.PreconditionFailed, "current core has no matching accepted restore receipt")
}

// PrepareRecoveryEvidence accepts no caller-created proof booleans. It reads
// the unchanged HumanOnly instance projection, downloads every selected fixed
// version, then rechecks core identity/epoch/receipt and current authorization.
func (c *Client) PrepareRecoveryEvidence(ctx context.Context, spec RecoveryEvidenceSpec, directory string) (PreparedManagementRecord, error) {
	var empty PreparedManagementRecord
	if !ids.ID(spec.RunID).Valid() || spec.Purpose == "" {
		return empty, managementInvalid("exact restore run and explicit selected-version purpose required")
	}
	info, receipt, err := c.currentRecoveryReceipt(ctx, spec.RunID)
	if err != nil {
		return empty, err
	}
	input := spec.Workset
	if err := ValidateWorkset(input, directory); err != nil {
		return empty, err
	}
	seen := map[string]bool{}
	for _, entry := range input.Entries {
		key := entry.AssetID + "/" + entry.VersionID
		if seen[key] {
			return empty, managementInvalid("recovery verification requires unique explicitly selected versions")
		}
		seen[key] = true
	}
	pulled, err := c.PullWorkset(ctx, input, directory, spec.Purpose)
	if err != nil {
		return empty, err
	}
	if pulled.State != "completed" || len(pulled.Entries) != len(input.Entries) {
		return empty, errcode.New(errcode.PreconditionFailed, "selected files were not fully verified")
	}
	out := RecoveryEvidence{Schema: "lantai.recovery-evidence/v1", EvidenceBasis: "current_core_restore_receipt_and_selected_fixed_version_verification", InstanceID: info.InstanceId, CurrentRecoveryEpoch: info.RecoveryEpoch, ObservedAt: info.ObservedAt.UTC().Format(time.RFC3339Nano), Restore: receipt, Scope: "explicit_selected_versions_only", UnknownAfterCaptureRemainsUnknown: receipt.UnknownAfterCapture, Verifications: []FixedVersionVerification{}}
	for _, entry := range pulled.Entries {
		if entry.InstanceID != info.InstanceId || len(entry.Files) == 0 {
			return empty, errcode.New(errcode.RefMismatch, "selected verification differs from recovery instance")
		}
		response, err := c.Do(ctx, http.MethodGet, "/api/v1/assets/"+entry.AssetID+"/versions/"+entry.VersionID+"?view=full", nil, Options{})
		if err != nil {
			return empty, err
		}
		var exact struct {
			Manifest struct {
				InstanceID string          `json:"instance_id"`
				AssetID    string          `json:"asset_id"`
				VersionID  string          `json:"version_id"`
				Content    json.RawMessage `json:"content"`
			} `json:"manifest"`
		}
		if err := response.Decode(&exact); err != nil {
			return empty, err
		}
		canonical, err := canonjson.Canonicalize(exact.Manifest.Content)
		if err != nil {
			return empty, err
		}
		if exact.Manifest.InstanceID != info.InstanceId || exact.Manifest.AssetID != entry.AssetID || exact.Manifest.VersionID != entry.VersionID || digest.Of(canonical) != entry.ManifestDigest {
			return empty, errcode.New(errcode.RefMismatch, "selected manifest changed after actual download")
		}
		var content struct {
			Files []InputFile `json:"files"`
		}
		if err := json.Unmarshal(exact.Manifest.Content, &content); err != nil {
			return empty, err
		}
		if len(content.Files) != len(entry.Files) {
			return empty, errcode.New(errcode.RefMismatch, "selected file count changed")
		}
		for _, file := range content.Files {
			matched := false
			for _, actual := range entry.Files {
				if file.Path == actual.SourcePath && file.SHA256 == actual.SHA256 && file.Size == actual.Size {
					matched = true
					break
				}
			}
			if !matched {
				return empty, errcode.New(errcode.HashMismatch, "selected actual file differs from manifest")
			}
			// Metadata visibility is separate from the selected read purpose. Recheck
			// that purpose after the actual download, including cached-file resumes.
			grantResponse, err := c.FileDownloadLink(ctx, entry.AssetID, entry.VersionID, file.Path, spec.Purpose)
			if err != nil {
				return empty, err
			}
			var grant DownloadLink
			if err := grantResponse.Decode(&grant); err != nil {
				return empty, err
			}
			if grant.SHA256 != file.SHA256 || grant.Size != file.Size {
				return empty, errcode.New(errcode.RefMismatch, "selected purpose grant changed after actual verification")
			}
		}
		out.Verifications = append(out.Verifications, FixedVersionVerification{Schema: "lantai.browser-verification/v1", Origin: c.Origin(), InstanceID: entry.InstanceID, AssetID: entry.AssetID, VersionID: entry.VersionID, ManifestDigest: string(entry.ManifestDigest), VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano), CompletionBasis: "current_remote_and_downloaded_files", CurrentRemoteVerified: true, Files: content.Files})
	}
	after, afterReceipt, err := c.currentRecoveryReceipt(ctx, spec.RunID)
	if err != nil {
		return empty, err
	}
	if after.InstanceId != info.InstanceId || after.RecoveryEpoch != info.RecoveryEpoch || afterReceipt != receipt {
		return empty, errcode.New(errcode.PreconditionFailed, "recovery epoch or accepted receipt changed during verification")
	}
	out.ObservedAt = after.ObservedAt.UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(out)
	if err != nil {
		return empty, err
	}
	return c.PrepareManagementRecord(ctx, ManagementRecordSpec{ProjectID: spec.ProjectID, Kind: "recovery", Title: spec.Title, Payload: raw, AssetID: spec.AssetID, BaseVersionID: spec.BaseVersionID})
}
func (c *Client) ReadRecoveryEvidence(ctx context.Context, asset, version, directory string) (ManagementRecordRead, RecoveryEvidence, error) {
	record, err := c.ReadManagementRecord(ctx, asset, version, directory)
	if err != nil {
		return record, RecoveryEvidence{}, err
	}
	var out RecoveryEvidence
	if record.Envelope.Kind != "recovery" {
		return record, out, managementInvalid("selected config is not a recovery record")
	}
	if err := json.Unmarshal(record.Envelope.Payload, &out); err != nil {
		return record, out, err
	}
	if out.Schema != "lantai.recovery-evidence/v1" || out.Scope != "explicit_selected_versions_only" || out.NativeSoftwareContinuationVerified || out.SourceRetirementAuthorized || out.InstanceID != string(record.Envelope.InstanceID) {
		return record, out, managementInvalid("invalid captured recovery evidence")
	}
	// Reopen returns captured observations only. Call PrepareRecoveryEvidence to
	// produce current proof; never rewrite a historical record's inner values.
	return record, out, nil
}
