package managementrecord

import (
	"encoding/json"
	"errors"
	"unicode/utf16"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

const BatchContract = "lantai.migration-batch/v1"

type Ref struct {
	AssetID   ids.ID `json:"asset_id"`
	VersionID ids.ID `json:"version_id"`
}
type BatchAsset struct {
	Key            string `json:"key"`
	AssetID        ids.ID `json:"asset_id"`
	VersionID      ids.ID `json:"version_id"`
	ManifestDigest string `json:"manifest_digest"`
	Status         string `json:"status"`
}
type BatchSummary struct {
	Planned            int `json:"planned"`
	Committed          int `json:"committed"`
	DescriptionPending int `json:"description_pending"`
	Failed             int `json:"failed"`
	Unknown            int `json:"unknown"`
	NotAttempted       int `json:"not_attempted"`
}
type BatchAction struct {
	ActorID ids.ID `json:"actor_id"`
	At      string `json:"at"`
	Action  string `json:"action"`
	Note    string `json:"note"`
}
type BatchReferenceJob struct {
	JobID  ids.ID `json:"job_id"`
	Action string `json:"action"`
}
type BatchEvidence struct {
	Assets              []BatchAsset        `json:"assets"`
	Worksets            []Ref               `json:"worksets"`
	Verifications       []Ref               `json:"verifications"`
	Recovery            []Ref               `json:"recovery"`
	NativeContinuations []Ref               `json:"native_continuations,omitempty"`
	ReferenceJobs       []BatchReferenceJob `json:"reference_jobs"`
}
type MigrationBatch struct {
	Schema          string          `json:"schema"`
	BatchID         string          `json:"batch_id"`
	Revision        int64           `json:"revision"`
	PreviousRef     *Ref            `json:"previous_ref,omitempty"`
	ProjectID       ids.ID          `json:"project_id"`
	InstanceID      ids.ID          `json:"instance_id"`
	Title           string          `json:"title"`
	OwnerID         ids.ID          `json:"owner_id"`
	LastAction      BatchAction     `json:"last_action"`
	Stage           string          `json:"stage"`
	Acceptance      string          `json:"acceptance"`
	SourceRetained  bool            `json:"source_retained"`
	IntendedPurpose string          `json:"intended_purpose"`
	ReferenceScope  string          `json:"reference_scope"`
	ImportSummary   BatchSummary    `json:"import_summary"`
	Evidence        BatchEvidence   `json:"evidence"`
	Checklist       map[string]bool `json:"checklist"`
}

// DecodeBatch validates only the portable inventory contract. Stored evidence,
// owner kind, current grants/rights/epoch and acceptance are checked by core.
func DecodeBatch(raw []byte) (MigrationBatch, error) {
	var out MigrationBatch
	if err := ValidatePayload(raw); err != nil {
		return out, err
	}
	tree, err := canonjson.Decode(raw)
	if err != nil {
		return out, err
	}
	r, err := schema.Default()
	if err != nil {
		return out, err
	}
	if err = r.Validate(BatchContract, tree); err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	s := out.ImportSummary
	if s.Planned != s.Committed+s.DescriptionPending+s.Failed+s.Unknown+s.NotAttempted {
		return out, errors.New("batch summary does not account for every planned row")
	}
	for _, v := range []struct {
		text string
		max  int
	}{{out.Title, 200}, {out.LastAction.Action, 100}, {out.LastAction.Note, 2000}} {
		if len(utf16.Encode([]rune(v.text))) > v.max {
			return out, errors.New("batch text exceeds shared UTF-16 budget")
		}
	}
	seen := map[string]bool{}
	for _, a := range out.Evidence.Assets {
		if seen[a.Key] || len(utf16.Encode([]rune(a.Key))) > 100 {
			return out, errors.New("duplicate or oversized batch asset key")
		}
		seen[a.Key] = true
	}
	seen = map[string]bool{}
	for _, j := range out.Evidence.ReferenceJobs {
		if seen[string(j.JobID)] {
			return out, errors.New("duplicate reference job")
		}
		seen[string(j.JobID)] = true
	}
	return out, nil
}
