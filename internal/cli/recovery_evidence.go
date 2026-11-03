package cli

import (
	"context"
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/client"
)

func executeRecoveryEvidence(ctx context.Context, action string, o assetOptions) (any, error) {
	if o.output == "" || o.directory == "" {
		return nil, usage("recovery evidence requires explicit --directory and private --output")
	}
	c, err := assetClient(o)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	switch action {
	case "recovery-evidence-prepare":
		var in client.RecoveryEvidenceSpec
		if err := decodeAssetInput(o.input, &in); err != nil {
			return nil, err
		}
		return c.PrepareRecoveryEvidence(ctx, in, o.directory)
	case "recovery-evidence-save":
		var in client.PreparedManagementRecord
		if err := decodeAssetInput(o.input, &in); err != nil {
			return nil, err
		}
		if in.Spec.Kind != "recovery" || o.state == "" {
			return nil, usage("original recovery preparation and --state required")
		}
		response, err := c.SaveManagementRecord(ctx, in, o.directory, o.state)
		if len(response.Body) == 0 {
			return nil, err
		}
		return json.RawMessage(response.Body), err
	case "recovery-evidence-open":
		var ref struct {
			AssetID   string `json:"asset_id"`
			VersionID string `json:"version_id"`
		}
		if err := decodeAssetInput(o.input, &ref); err != nil {
			return nil, err
		}
		record, evidence, err := c.ReadRecoveryEvidence(ctx, ref.AssetID, ref.VersionID, o.directory)
		if err != nil {
			return nil, err
		}
		return map[string]any{"asset_id": ref.AssetID, "version_id": ref.VersionID, "record_sha256": record.SHA256, "captured_evidence": evidence, "historical_receipt": true, "current_remote_verified": false, "verification_scope": "captured_record_bytes_only"}, nil
	}
	return nil, usage("unknown recovery evidence action")
}
