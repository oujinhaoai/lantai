package client

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/managementrecord"
)

// MigrationBatchDraft is local, explicit input. Supplying an import plan derives
// every row's historical status; it never manufactures current acceptance.
type MigrationBatchDraft struct {
	Batch  managementrecord.MigrationBatch `json:"batch"`
	Plan   *ImportPlan                     `json:"import_plan,omitempty"`
	Report *ImportReport                   `json:"import_report,omitempty"`
}

func (d *MigrationBatchDraft) UnmarshalJSON(raw []byte) error {
	type plain MigrationBatchDraft
	var out plain
	if canonjson.Validate(raw) != nil {
		return managementInvalid("batch draft must be unambiguous JSON")
	}
	if err := importStrictJSON(raw, &out); err != nil {
		return err
	}
	*d = MigrationBatchDraft(out)
	return nil
}

// PrepareMigrationBatch prepares the original immutable input. Time, batch ID,
// human owner and statements are explicit caller input, never generated anew
// on a retry. Previous, when present, comes from a verified exact record read.
func (c *Client) PrepareMigrationBatch(ctx context.Context, draft MigrationBatchDraft, previous *ManagementRecordRead) (PreparedManagementRecord, error) {
	var empty PreparedManagementRecord
	instance, err := c.worksetInstance(ctx)
	if err != nil {
		return empty, err
	}
	who, err := c.Do(ctx, http.MethodGet, "/api/v1/whoami", nil, Options{})
	if err != nil {
		return empty, err
	}
	var identity struct {
		Principal struct {
			ID ids.ID `json:"principal_id"`
		} `json:"principal"`
	}
	if err = who.Decode(&identity); err != nil {
		return empty, err
	}
	b := draft.Batch
	if b.InstanceID != ids.ID(instance) || !identity.Principal.ID.Valid() || b.LastAction.ActorID != identity.Principal.ID {
		return empty, managementInvalid("batch instance and last actor must match the current session")
	}
	spec := ManagementRecordSpec{ProjectID: string(b.ProjectID), Kind: "migration", Title: b.Title}
	if previous != nil {
		if previous.Envelope.Kind != "migration" {
			return empty, managementInvalid("previous record is not a migration batch")
		}
		old, err := managementrecord.DecodeBatch(previous.Envelope.Payload)
		if err != nil {
			return empty, err
		}
		if old.BatchID != b.BatchID || old.ProjectID != b.ProjectID || old.InstanceID != b.InstanceID || previous.Envelope.ProjectID != b.ProjectID || previous.Envelope.InstanceID != b.InstanceID {
			return empty, managementInvalid("revision changed original batch identity or scope")
		}
		b.Revision = old.Revision + 1
		b.PreviousRef = &managementrecord.Ref{AssetID: ids.ID(previous.AssetID), VersionID: ids.ID(previous.VersionID)}
		b.Acceptance = "not_requested"
		spec.AssetID, spec.BaseVersionID = previous.AssetID, previous.VersionID
	} else if b.Revision != 1 || b.PreviousRef != nil {
		return empty, managementInvalid("new batch requires first revision without a predecessor")
	}
	if draft.Report != nil && draft.Plan == nil {
		return empty, managementInvalid("import report requires its original plan")
	}
	if draft.Plan != nil {
		plan := *draft.Plan
		digest, err := importPlanDigest(plan)
		if err != nil || plan.Schema != ImportPlanSchema || plan.Input.Schema != ImportInputSchema || digest != plan.Digest || len(plan.Input.Assets) == 0 || len(plan.Input.Assets) > MaxImportAssets {
			return empty, managementInvalid("invalid original import plan digest")
		}
		if draft.Report != nil {
			if err := c.validateImportReport(plan, *draft.Report); err != nil {
				return empty, err
			}
		}
		b.ImportSummary = managementrecord.BatchSummary{Planned: len(plan.Input.Assets)}
		b.Evidence.Assets = []managementrecord.BatchAsset{}
		for i, a := range plan.Input.Assets {
			if a.Push.ProjectID != string(b.ProjectID) {
				return empty, managementInvalid("import plan contains a different project")
			}
			row := ImportResult{Key: a.Key, Status: "not_attempted"}
			if draft.Report != nil {
				row = draft.Report.Results[i]
			}
			switch row.Status {
			case "committed":
				b.ImportSummary.Committed++
			case "description_pending":
				b.ImportSummary.DescriptionPending++
			case "failed":
				b.ImportSummary.Failed++
			case "unknown":
				b.ImportSummary.Unknown++
			case "not_attempted":
				b.ImportSummary.NotAttempted++
			default:
				return empty, managementInvalid("invalid original import row state")
			}
			if row.AssetID == "" && row.VersionID == "" {
				continue
			}
			if !ids.ID(row.AssetID).Valid() || !ids.ID(row.VersionID).Valid() {
				return empty, managementInvalid("partial import receipt identity")
			}
			response, err := c.ReadResourceJSON(ctx, "/api/v1/assets/"+row.AssetID+"/versions/"+row.VersionID+"?view=full")
			if err != nil {
				return empty, err
			}
			var exact struct {
				Manifest struct {
					InstanceID ids.ID `json:"instance_id"`
					ProjectID  ids.ID `json:"project_id"`
					AssetID    string `json:"asset_id"`
					VersionID  string `json:"version_id"`
					Content    struct {
						AssetType string      `json:"asset_type"`
						Files     []InputFile `json:"files"`
					} `json:"content"`
				} `json:"manifest"`
				Version struct {
					ManifestDigest string `json:"manifest_digest"`
				} `json:"version"`
			}
			if err = response.Decode(&exact); err != nil {
				return empty, err
			}
			m := exact.Manifest
			if m.InstanceID != b.InstanceID || m.ProjectID != b.ProjectID || m.AssetID != row.AssetID || m.VersionID != row.VersionID {
				return empty, managementInvalid("import receipt exact manifest scope mismatch")
			}
			if m.Content.AssetType != a.Push.Content.AssetType || !slices.Equal(m.Content.Files, a.Push.Content.Files) {
				return empty, managementInvalid("import receipt files differ from the original frozen plan")
			}
			b.Evidence.Assets = append(b.Evidence.Assets, managementrecord.BatchAsset{Key: row.Key, AssetID: ids.ID(row.AssetID), VersionID: ids.ID(row.VersionID), ManifestDigest: exact.Version.ManifestDigest, Status: row.Status})
		}
	}
	raw, err := canonjson.CanonicalizeValue(b)
	if err != nil {
		return empty, err
	}
	if _, err = managementrecord.DecodeBatch(raw); err != nil {
		return empty, managementInvalid(err.Error())
	}
	spec.Payload = json.RawMessage(raw)
	return c.PrepareManagementRecord(ctx, spec)
}
