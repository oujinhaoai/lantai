package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

const (
	MetadataBatchInputSchema  = "lantai.metadata-batch-input/v1"
	MetadataBatchPlanSchema   = "lantai.metadata-batch-plan/v1"
	MetadataBatchReportSchema = "lantai.metadata-batch-report/v1"
	MaxMetadataBatchItems     = 1000
)

type MetadataBatchItem struct {
	AssetID string          `json:"asset_id"`
	Patch   json.RawMessage `json:"patch"`
}
type MetadataBatchInput struct {
	Schema string              `json:"schema"`
	Items  []MetadataBatchItem `json:"items"`
}
type MetadataBatchPlannedItem struct {
	MetadataBatchItem
	ProjectID string `json:"project_id"`
	Revision  int64  `json:"revision"`
	IfMatch   string `json:"if_match"`
}
type MetadataBatchPlan struct {
	Schema      string                     `json:"schema"`
	Origin      string                     `json:"origin"`
	InstanceID  string                     `json:"instance_id"`
	PrincipalID string                     `json:"principal_id"`
	Digest      string                     `json:"digest"`
	Items       []MetadataBatchPlannedItem `json:"items"`
}
type MetadataBatchResult struct {
	AssetID          string `json:"asset_id"`
	Status           string `json:"status"`
	ErrorCode        string `json:"error_code,omitempty"`
	AcceptedRevision int64  `json:"accepted_revision,omitempty"`
	AcceptedSHA256   string `json:"accepted_sha256,omitempty"`
}
type MetadataBatchReport struct {
	Schema                string                `json:"schema"`
	Origin                string                `json:"origin"`
	PlanDigest            string                `json:"plan_digest"`
	Complete              bool                  `json:"complete"`
	CurrentRemoteVerified bool                  `json:"current_remote_verified"`
	Results               []MetadataBatchResult `json:"results"`
}
type metadataBatchStateRow struct {
	MetadataBatchResult
	Key string `json:"idempotency_key"`
}
type metadataBatchState struct {
	Schema      string                  `json:"schema"`
	Origin      string                  `json:"origin"`
	PlanDigest  string                  `json:"plan_digest"`
	PrincipalID string                  `json:"principal_id"`
	Rows        []metadataBatchStateRow `json:"rows"`
}

func metadataPatch(raw []byte) error {
	if err := canonjson.Validate(raw); err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return worksetInvalid("metadata patch exceeds1MiB")
	}
	var patch apiv1.AssetPatch
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return worksetInvalid("metadata patch must be an object")
	}
	if patch.Sensitivity != nil || patch.Defaults != nil {
		return errcode.New(errcode.FieldRequiresSpecialCommand, "sensitivity/default rights require the original dedicated command")
	}
	return nil
}
func (c *Client) metadataActor(ctx context.Context) (instance, principal string, err error) {
	meta, e := c.Do(ctx, http.MethodGet, "/api/v1/meta", nil, Options{})
	if e != nil {
		return "", "", e
	}
	var m struct {
		InstanceID string `json:"instance_id"`
	}
	if e := meta.Decode(&m); e != nil {
		return "", "", e
	}
	who, e := c.Do(ctx, http.MethodGet, "/api/v1/whoami", nil, Options{})
	if e != nil {
		return "", "", e
	}
	var w struct {
		Principal struct {
			ID string `json:"principal_id"`
		} `json:"principal"`
	}
	if e := who.Decode(&w); e != nil {
		return "", "", e
	}
	if !ids.ID(m.InstanceID).Valid() || !ids.ID(w.Principal.ID).Valid() {
		return "", "", worksetInvalid("server omitted exact instance/principal identity")
	}
	return m.InstanceID, w.Principal.ID, nil
}
func metadataPlanDigest(plan MetadataBatchPlan) (string, error) {
	plan.Digest = ""
	raw, err := canonjson.CanonicalizeValue(plan)
	if err != nil {
		return "", err
	}
	return importSHA(raw), nil
}

// PlanMetadataBatch reads an explicit ordered set and freezes each original
// revision and raw patch. It never sends an unchanged description/unknown extra
// back to the server and never performs a write or grants permissions.
func (c *Client) PlanMetadataBatch(ctx context.Context, input MetadataBatchInput) (MetadataBatchPlan, error) {
	plan := MetadataBatchPlan{Schema: MetadataBatchPlanSchema, Origin: c.Origin(), Items: []MetadataBatchPlannedItem{}}
	if input.Schema != MetadataBatchInputSchema || len(input.Items) < 1 || len(input.Items) > MaxMetadataBatchItems {
		return plan, worksetInvalid("metadata batch requires1..1000 explicit assets")
	}
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > 4<<20 {
		return plan, worksetInvalid("metadata batch selection exceeds4MiB")
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return plan, err
	}
	seen := map[string]bool{}
	for _, item := range input.Items {
		if !ids.ID(item.AssetID).Valid() || seen[item.AssetID] {
			return plan, worksetInvalid("metadata batch assets must be unique exact IDs")
		}
		seen[item.AssetID] = true
		if err := metadataPatch(item.Patch); err != nil {
			return plan, err
		}
	}
	plan.InstanceID, plan.PrincipalID, err = c.metadataActor(ctx)
	if err != nil {
		return plan, err
	}
	for _, item := range input.Items {
		response, err := c.ReadResourceJSON(ctx, "/api/v1/assets/"+item.AssetID+"?view=full")
		if err != nil {
			return plan, err
		}
		_, project, revision, err := metadataDescription(response.Body, item.AssetID, false)
		if err != nil {
			return plan, err
		}
		if response.ETag != "\""+strconv.FormatInt(revision, 10)+"\"" {
			return plan, worksetInvalid("metadata revision and ETag differ")
		}
		plan.Items = append(plan.Items, MetadataBatchPlannedItem{MetadataBatchItem: item, ProjectID: project, Revision: revision, IfMatch: response.ETag})
	}
	plan.Digest, err = metadataPlanDigest(plan)
	return plan, err
}
func (c *Client) validateMetadataPlan(plan MetadataBatchPlan) error {
	if plan.Schema != MetadataBatchPlanSchema || plan.Origin != c.Origin() || !ids.ID(plan.InstanceID).Valid() || !ids.ID(plan.PrincipalID).Valid() || len(plan.Items) < 1 || len(plan.Items) > MaxMetadataBatchItems {
		return worksetInvalid("metadata plan identity/scope mismatch")
	}
	want, err := metadataPlanDigest(plan)
	if err != nil || want != plan.Digest {
		return worksetInvalid("metadata plan digest mismatch")
	}
	raw, err := json.Marshal(plan)
	if err != nil || int64(len(raw)) > MaxJSONBytes {
		return worksetInvalid("metadata plan exceeds8MiB")
	}
	seen := map[string]bool{}
	for _, item := range plan.Items {
		if !ids.ID(item.AssetID).Valid() || !ids.ID(item.ProjectID).Valid() || seen[item.AssetID] || item.Revision < 0 || item.IfMatch != "\""+strconv.FormatInt(item.Revision, 10)+"\"" {
			return worksetInvalid("metadata plan row identity/revision mismatch")
		}
		seen[item.AssetID] = true
		if err := metadataPatch(item.Patch); err != nil {
			return err
		}
	}
	return nil
}
func metadataDescription(raw []byte, asset string, direct bool) (snapshot, project string, revision int64, err error) {
	if !direct {
		var view struct {
			AssetID     string          `json:"asset_id"`
			Description json.RawMessage `json:"description"`
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			return "", "", 0, err
		}
		if view.AssetID != asset {
			return "", "", 0, worksetInvalid("current metadata asset mismatch")
		}
		raw = view.Description
	}
	var description struct {
		AssetID   string `json:"asset_id"`
		ProjectID string `json:"project_id"`
		Revision  int64  `json:"revision"`
	}
	if err := json.Unmarshal(raw, &description); err != nil {
		return "", "", 0, err
	}
	if description.AssetID != asset || !ids.ID(description.ProjectID).Valid() || description.Revision < 0 {
		return "", "", 0, worksetInvalid("metadata receipt omitted exact identity/revision")
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		return "", "", 0, err
	}
	return importSHA(canonical), description.ProjectID, description.Revision, nil
}
func metadataBatchReport(state metadataBatchState) MetadataBatchReport {
	out := MetadataBatchReport{Schema: MetadataBatchReportSchema, Origin: state.Origin, PlanDigest: state.PlanDigest, Results: []MetadataBatchResult{}, Complete: true}
	for _, row := range state.Rows {
		out.Results = append(out.Results, row.MetadataBatchResult)
		out.Complete = out.Complete && row.Status == "verified"
	}
	// Callers set this only after every row was freshly re-read in this call.
	out.CurrentRemoteVerified = false
	return out
}

// ApplyMetadataBatch is not a multi-asset transaction. Stable keys for every row
// are persisted before the first PATCH. Retries reconcile accepted rows through
// their original key (no new mutation), then freshly read each current result.
// Original revision guards never refresh to overwrite another writer.
func (c *Client) ApplyMetadataBatch(ctx context.Context, plan MetadataBatchPlan, statePath string) (MetadataBatchReport, error) {
	partial := MetadataBatchReport{Schema: MetadataBatchReportSchema, Origin: c.Origin(), PlanDigest: plan.Digest, Results: []MetadataBatchResult{}}
	if err := c.validateMetadataPlan(plan); err != nil {
		return partial, err
	}
	for _, item := range plan.Items {
		partial.Results = append(partial.Results, MetadataBatchResult{AssetID: item.AssetID, Status: "not_attempted"})
	}
	instance, principal, err := c.metadataActor(ctx)
	if err != nil {
		return partial, err
	}
	if instance != plan.InstanceID || principal != plan.PrincipalID {
		return partial, errcode.New(errcode.Forbidden, "metadata batch belongs to a different instance/principal")
	}
	if statePath == "" {
		return partial, worksetInvalid("original metadata state file required")
	}
	stateDirectory, err := importAbsolute(filepath.Dir(statePath))
	if err != nil {
		return partial, err
	}
	statePath = filepath.Join(stateDirectory, filepath.Base(statePath))
	file, err := OpenPinnedPrivateOutput(statePath)
	if err != nil {
		return partial, err
	}
	defer file.Close()
	unlock, err := file.lockState()
	if err != nil {
		return partial, err
	}
	defer unlock()
	var state metadataBatchState
	err = file.loadState(&state)
	if errors.Is(err, os.ErrNotExist) {
		state = metadataBatchState{Schema: "lantai.metadata-batch-state/v1", Origin: c.Origin(), PlanDigest: plan.Digest, PrincipalID: principal, Rows: []metadataBatchStateRow{}}
		for _, item := range plan.Items {
			state.Rows = append(state.Rows, metadataBatchStateRow{MetadataBatchResult: MetadataBatchResult{AssetID: item.AssetID, Status: "not_attempted"}, Key: string(ids.New())})
		}
		if err := file.saveState(state); err != nil {
			return partial, err
		}
	} else if err != nil {
		return partial, err
	}
	if state.Schema != "lantai.metadata-batch-state/v1" || state.Origin != c.Origin() || state.PlanDigest != plan.Digest || state.PrincipalID != principal || len(state.Rows) != len(plan.Items) {
		return partial, worksetInvalid("original metadata state does not match the frozen plan")
	}
	keys := map[string]bool{}
	for i, row := range state.Rows {
		if row.AssetID != plan.Items[i].AssetID || !ids.ID(row.Key).Valid() || keys[row.Key] {
			return partial, worksetInvalid("original metadata row/key mismatch")
		}
		keys[row.Key] = true
		if row.AcceptedSHA256 != "" && !digest.ValidHex(row.AcceptedSHA256) {
			return partial, worksetInvalid("invalid accepted metadata digest")
		}
	}
	var failures []error
	verified := 0
	for i, item := range plan.Items {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		row := &state.Rows[i]
		row.ErrorCode = ""
		previousDigest, previousRevision := row.AcceptedSHA256, row.AcceptedRevision
		{
			if previousDigest == "" {
				row.Status = "unknown"
				if err := file.saveState(state); err != nil {
					return metadataBatchReport(state), err
				}
			}
			response, err := c.Do(ctx, http.MethodPatch, "/api/v1/assets/"+item.AssetID+"/metadata", item.Patch, Options{IfMatch: item.IfMatch, IdempotencyKey: row.Key})
			if err == nil {
				var project string
				row.AcceptedSHA256, project, row.AcceptedRevision, err = metadataDescription(response.Body, item.AssetID, true)
				if err == nil && (project != item.ProjectID || row.AcceptedRevision != item.Revision+1) {
					row.AcceptedSHA256 = ""
					err = worksetInvalid("metadata acceptance changed original project/revision")
				}
				if err == nil && previousDigest != "" && (row.AcceptedSHA256 != previousDigest || row.AcceptedRevision != previousRevision) {
					err = worksetInvalid("original metadata acceptance differs from saved receipt")
				}
			}
			if err != nil {
				row.AcceptedSHA256, row.AcceptedRevision = previousDigest, previousRevision
				if previousDigest != "" {
					row.Status = "verification_failed"
				}
				row.ErrorCode = importErrorCode(err)
				var api *APIError
				if previousDigest == "" && errors.As(err, &api) && !api.Protocol && api.Status >= 400 && api.Status < 500 {
					row.Status = "failed"
					if api.Status == 409 || api.Status == 412 {
						row.Status = "conflict"
					}
				}
				failures = append(failures, fmt.Errorf("metadata asset %s: %w", item.AssetID, err))
				if err := file.saveState(state); err != nil {
					return metadataBatchReport(state), err
				}
				continue
			}
			row.Status = "applied"
			if err := file.saveState(state); err != nil {
				return metadataBatchReport(state), err
			}
		}
		response, err := c.ReadResourceJSON(ctx, "/api/v1/assets/"+item.AssetID+"?view=full")
		if err == nil {
			current, project, revision, checkErr := metadataDescription(response.Body, item.AssetID, false)
			err = checkErr
			if err == nil && (current != row.AcceptedSHA256 || project != item.ProjectID || revision != row.AcceptedRevision) {
				err = errcode.New(errcode.PreconditionFailed, "accepted metadata changed; preserve original intent without another PATCH")
			}
		}
		if err != nil {
			row.Status = "verification_failed"
			row.ErrorCode = importErrorCode(err)
			failures = append(failures, fmt.Errorf("metadata asset %s verification: %w", item.AssetID, err))
		} else {
			row.Status = "verified"
			verified++
		}
		if err := file.saveState(state); err != nil {
			return metadataBatchReport(state), err
		}
	}
	out := metadataBatchReport(state)
	out.Complete = verified == len(plan.Items)
	out.CurrentRemoteVerified = out.Complete
	return out, errors.Join(failures...)
}
