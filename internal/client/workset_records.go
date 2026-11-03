package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

const ManagedWorksetSchema = "lantai.managed-workset/v1"
const WorksetReturnSchema = "lantai.workset-return/v1"

// The payload matches web StoredWorkset. Optional observations remain historical
// and are deliberately omitted from a new revision when fixed input changes.
type ManagedWorkset struct {
	Schema               string          `json:"schema"`
	Title                string          `json:"title"`
	Input                WorksetInput    `json:"input"`
	RemoteCheck          json.RawMessage `json:"remote_check,omitempty"`
	AssistantObservation json.RawMessage `json:"assistant_observation,omitempty"`
}
type WorksetRecordSpec struct {
	ProjectID     string       `json:"project_id"`
	Title         string       `json:"title"`
	Input         WorksetInput `json:"input"`
	AssetID       string       `json:"asset_id,omitempty"`
	BaseVersionID string       `json:"base_version_id,omitempty"`
}
type WorksetReturnSpec struct {
	Entry   WorksetEntry `json:"entry"`
	Files   []InputFile  `json:"files"`
	Purpose string       `json:"purpose"`
	Usage   string       `json:"usage"`
	Note    string       `json:"note"`
}
type WorksetReturn struct {
	Schema     string       `json:"schema"`
	InstanceID string       `json:"instance_id"`
	Entry      WorksetEntry `json:"entry"`
	Purpose    string       `json:"purpose"`
	Input      PushInput    `json:"input"`
	State      PushState    `json:"state"`
}
type WorksetAdoption struct {
	RecordAssetID   string             `json:"record_asset_id"`
	RecordVersionID string             `json:"record_version_id"`
	Return          WorksetReturn      `json:"return"`
	Receipt         apiv1.CommitResult `json:"receipt"`
}

func (c *Client) PrepareWorksetRecord(ctx context.Context, spec WorksetRecordSpec) (PreparedManagementRecord, error) {
	if _, err := c.InspectWorkset(ctx, spec.Input); err != nil {
		return PreparedManagementRecord{}, err
	}
	payload, err := json.Marshal(ManagedWorkset{Schema: ManagedWorksetSchema, Title: strings.TrimSpace(spec.Title), Input: spec.Input})
	if err != nil {
		return PreparedManagementRecord{}, err
	}
	return c.PrepareManagementRecord(ctx, ManagementRecordSpec{ProjectID: spec.ProjectID, Kind: "workset", Title: spec.Title, Payload: payload, AssetID: spec.AssetID, BaseVersionID: spec.BaseVersionID})
}
func (c *Client) ReadWorksetRecord(ctx context.Context, asset, version, directory string) (ManagementRecordRead, ManagedWorkset, error) {
	record, err := c.ReadManagementRecord(ctx, asset, version, directory)
	if err != nil {
		return record, ManagedWorkset{}, err
	}
	var payload ManagedWorkset
	if record.Envelope.Kind != "workset" {
		return record, payload, worksetInvalid("selected record is not a workset")
	}
	if err := importStrictJSON(record.Envelope.Payload, &payload); err != nil {
		return record, payload, err
	}
	if payload.Schema != ManagedWorksetSchema || strings.TrimSpace(payload.Title) == "" {
		return record, payload, worksetInvalid("unsupported workset payload")
	}
	_, err = c.InspectWorkset(ctx, payload.Input)
	return record, payload, err
}

func validateReturnState(in PushInput, state PushState, origin string) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if state.Schema != "lantai.client-push/v1" || state.Origin != origin || state.RequestHash != importSHA(raw) || state.CreateKey == "" || state.CommitKey == "" || state.CreateKey == state.CommitKey || len(state.CreateKey) > 128 || len(state.CommitKey) > 128 {
		return errcode.New(errcode.IdempotencyConflict, "return input does not match original Push state/keys")
	}
	for _, key := range []string{state.CreateKey, state.CommitKey} {
		for _, char := range key {
			if char < 0x21 || char > 0x7e {
				return worksetInvalid("invalid original return key")
			}
		}
	}
	if state.UploadID != "" && !ids.ID(state.UploadID).Valid() || state.OperationID != "" && !ids.ID(state.OperationID).Valid() || state.Committing && state.UploadID == "" || (state.UploadID == "") != (state.OperationID == "") {
		return worksetInvalid("invalid original return upload/operation")
	}
	if len(state.Result) > 0 {
		var receipt apiv1.CommitResult
		if err := json.Unmarshal(state.Result, &receipt); err != nil {
			return err
		}
		if receipt.AssetId != in.AssetID || receipt.VersionId == in.BaseVersionID || !ids.ID(receipt.VersionId).Valid() || receipt.OperationId != state.OperationID {
			return worksetInvalid("return state receipt differs from original asset/operation")
		}
	}
	return nil
}

// Preparation hashes only explicitly selected local files and reads current
// source metadata/rights. It creates no remote upload. The embedded state uses
// the original cross-client Push format, including browser-compatible keys.
func (c *Client) PrepareWorksetReturn(ctx context.Context, spec WorksetReturnSpec, directory string, original *WorksetReturn) (WorksetReturn, error) {
	var out WorksetReturn
	if err := validateWorksetDefinition(WorksetInput{Schema: WorksetSchema, Entries: []WorksetEntry{spec.Entry}}); err != nil {
		return out, err
	}
	if spec.Purpose != "reference" && spec.Purpose != "production" {
		return out, worksetInvalid("explicit reference or production return purpose required")
	}
	directory, err := importAbsolute(directory)
	if err != nil {
		return out, err
	}
	root, err := openWorksetRoot(directory, false)
	if err != nil {
		return out, err
	}
	if root == nil {
		return out, os.ErrNotExist
	}
	defer root.Close()
	for _, file := range spec.Files {
		if err := worksetPath(file.Path); err != nil {
			return out, err
		}
		if err := checkWorksetTarget(root, file.Path, false); err != nil {
			return out, err
		}
	}
	instance, err := c.worksetInstance(ctx)
	if err != nil {
		return out, err
	}
	base, err := c.worksetManifest(ctx, spec.Entry, instance)
	if err != nil {
		return out, err
	}
	exact, err := c.ReadResourceJSON(ctx, "/api/v1/assets/"+spec.Entry.AssetID+"/versions/"+spec.Entry.VersionID+"?view=full")
	if err != nil {
		return out, err
	}
	var frozen struct {
		Manifest struct {
			InstanceID string          `json:"instance_id"`
			ProjectID  string          `json:"project_id"`
			Content    json.RawMessage `json:"content"`
		} `json:"manifest"`
	}
	if err := exact.Decode(&frozen); err != nil {
		return out, err
	}
	canonical, err := canonjson.Canonicalize(frozen.Manifest.Content)
	if err != nil || digest.Of(canonical) != base.ManifestDigest || frozen.Manifest.InstanceID != instance || !ids.ID(frozen.Manifest.ProjectID).Valid() {
		return out, errcode.New(errcode.RefMismatch, "return source manifest changed")
	}
	var content struct {
		AssetType string `json:"asset_type"`
	}
	if err := json.Unmarshal(frozen.Manifest.Content, &content); err != nil {
		return out, err
	}
	rightsResponse, err := c.ReadResourceJSON(ctx, "/api/v1/assets/"+spec.Entry.AssetID+"/versions/"+spec.Entry.VersionID+"/rights")
	if err != nil {
		return out, err
	}
	var view apiv1.VersionRightsView
	if err := rightsResponse.Decode(&view); err != nil {
		return out, err
	}
	if view.Subject.InstanceId != instance || view.Subject.AssetId != spec.Entry.AssetID || view.Subject.VersionId != spec.Entry.VersionID || view.ManifestDigest != string(base.ManifestDigest) {
		return out, errcode.New(errcode.RefMismatch, "return rights scope differs from exact source")
	}
	allowed := false
	for _, purpose := range view.Purposes {
		if purpose.Purpose == spec.Purpose {
			allowed = purpose.Allowed
		}
	}
	if !allowed {
		return out, errcode.New(errcode.UseRestricted, "current source purpose does not allow this return")
	}
	ranks := map[string]int{"production": 0, "reference": 1, "restricted": 2}
	sourceRank, known := ranks[string(view.Rights.Usage)]
	requestedRank, valid := ranks[spec.Usage]
	if !known || !valid || requestedRank < sourceRank || spec.Purpose == "production" && spec.Usage != "production" {
		return out, errcode.New(errcode.UseRestricted, "return usage cannot broaden current source rights")
	}
	rights := view.Rights
	rights.Usage = apiv1.VersionManifestRightsUsage(spec.Usage)
	rightsRaw, _ := json.Marshal(rights)
	relation := "reference"
	if spec.Purpose == "production" {
		relation = "derived_from"
	}
	uses, _ := json.Marshal([]map[string]string{{"asset_id": spec.Entry.AssetID, "version_id": spec.Entry.VersionID, "relation": relation}})
	input := PushInput{ProjectID: frozen.Manifest.ProjectID, AssetID: spec.Entry.AssetID, BaseVersionID: spec.Entry.VersionID, Content: ContentInput{AssetType: content.AssetType, VersionNote: spec.Note, Files: spec.Files, Rights: rightsRaw, Uses: uses}}
	input, err = PreparePush(ctx, input, directory)
	if err != nil {
		return out, err
	}
	if err := checkWorksetRoot(directory, root); err != nil {
		return out, err
	}
	current, err := c.ReadResourceJSON(ctx, "/api/v1/assets/"+spec.Entry.AssetID+"?view=full")
	if err != nil {
		return out, err
	}
	var latest struct {
		AssetID   string `json:"asset_id"`
		ProjectID string `json:"project_id"`
		Latest    struct {
			VersionID string `json:"version_id"`
		} `json:"latest"`
	}
	if err := current.Decode(&latest); err != nil {
		return out, err
	}
	if latest.AssetID != spec.Entry.AssetID || latest.ProjectID != input.ProjectID {
		return out, errcode.New(errcode.RefMismatch, "current asset differs from return source")
	}
	if original != nil {
		oldEntry, _ := canonjson.CanonicalizeValue(original.Entry)
		newEntry, _ := canonjson.CanonicalizeValue(spec.Entry)
		oldInput, e1 := canonjson.CanonicalizeValue(original.Input)
		newInput, e2 := canonjson.CanonicalizeValue(input)
		if original.Schema != WorksetReturnSchema || original.InstanceID != instance || original.Purpose != spec.Purpose || !bytes.Equal(oldEntry, newEntry) || e1 != nil || e2 != nil || !bytes.Equal(oldInput, newInput) {
			return out, errcode.New(errcode.IdempotencyConflict, "return scope/base/purpose/bytes changed; preserve original preparation and state")
		}
		if err := validateReturnState(original.Input, original.State, c.Origin()); err != nil {
			return out, err
		}
		if latest.Latest.VersionID != spec.Entry.VersionID && original.State.UploadID == "" && len(original.State.Result) == 0 {
			return out, errcode.New(errcode.BaseVersionConflict, "original return base is stale before upload")
		}
		return *original, nil
	}
	if latest.Latest.VersionID != spec.Entry.VersionID {
		return out, errcode.New(errcode.BaseVersionConflict, "fixed return base is no longer current; select a new explicit draft")
	}
	raw, _ := json.Marshal(input)
	return WorksetReturn{Schema: WorksetReturnSchema, InstanceID: instance, Entry: spec.Entry, Purpose: spec.Purpose, Input: input, State: PushState{Schema: "lantai.client-push/v1", Origin: c.Origin(), RequestHash: importSHA(raw), CreateKey: string(ids.New()), CommitKey: string(ids.New())}}, nil
}

func (c *Client) ApplyWorksetReturn(ctx context.Context, p WorksetReturn, directory, statePath string) (Response, error) {
	if p.Schema != WorksetReturnSchema || !ids.ID(p.InstanceID).Valid() || p.Input.AssetID != p.Entry.AssetID || p.Input.BaseVersionID != p.Entry.VersionID {
		return Response{}, worksetInvalid("invalid original return preparation")
	}
	if err := validateReturnState(p.Input, p.State, c.Origin()); err != nil {
		return Response{}, err
	}
	source, err := importAbsolute(directory)
	if err != nil {
		return Response{}, err
	}
	if statePath == "" {
		return Response{}, worksetInvalid("original return state path required")
	}
	stateDir, err := importAbsolute(filepath.Dir(statePath))
	if err != nil {
		return Response{}, err
	}
	statePath = filepath.Join(stateDir, filepath.Base(statePath))
	file, err := OpenBatchPrivateOutput(statePath, source)
	if err != nil {
		return Response{}, err
	}
	defer file.Close()
	unlock, err := file.lockState()
	if err != nil {
		return Response{}, err
	}
	var state PushState
	err = file.loadState(&state)
	if errors.Is(err, os.ErrNotExist) {
		state = p.State
		raw, marshalErr := json.Marshal(state)
		if marshalErr != nil {
			err = marshalErr
		} else {
			err = file.WriteOriginal(append(raw, '\n'))
		}
	}
	if err == nil {
		err = validateReturnState(p.Input, state, c.Origin())
	}
	if err == nil && (state.CreateKey != p.State.CreateKey || state.CommitKey != p.State.CommitKey) {
		err = errcode.New(errcode.IdempotencyConflict, "state file contains different original return keys")
	}
	unlock()
	if err != nil {
		return Response{}, err
	}
	var rights apiv1.VersionManifestRights
	if err := json.Unmarshal(p.Input.Content.Rights, &rights); err != nil {
		return Response{}, err
	}
	current := p
	current.State = state
	spec := WorksetReturnSpec{Entry: p.Entry, Files: p.Input.Content.Files, Purpose: p.Purpose, Usage: string(rights.Usage), Note: p.Input.Content.VersionNote}
	if _, err := c.PrepareWorksetReturn(ctx, spec, source, &current); err != nil {
		return Response{}, err
	}
	return c.pushWithStateFile(ctx, p.Input, source, file, false)
}

// Adoption prepares a new config-record version only. It verifies the actual
// returned manifest/files/operation, then rechecks the complete revised workset.
// Neither the original input nor its old stored record is changed by this call.
func (c *Client) PrepareWorksetAdoption(ctx context.Context, in WorksetAdoption, recordDirectory string) (PreparedManagementRecord, error) {
	receipt, p := in.Receipt, in.Return
	if p.Schema != WorksetReturnSchema || receipt.DescriptionPending != nil && *receipt.DescriptionPending || receipt.AssetId != p.Entry.AssetID || receipt.VersionId == p.Entry.VersionID || !ids.ID(receipt.VersionId).Valid() || !ids.ID(receipt.OperationId).Valid() {
		return PreparedManagementRecord{}, worksetInvalid("return receipt is incomplete or differs from original entry")
	}
	if p.Input.AssetID != p.Entry.AssetID || p.Input.BaseVersionID != p.Entry.VersionID {
		return PreparedManagementRecord{}, worksetInvalid("return input differs from original fixed entry")
	}
	if err := validateReturnState(p.Input, p.State, c.Origin()); err != nil {
		return PreparedManagementRecord{}, err
	}
	if p.State.OperationID != "" && p.State.OperationID != receipt.OperationId {
		return PreparedManagementRecord{}, worksetInvalid("return receipt differs from original operation")
	}
	record, stored, err := c.ReadWorksetRecord(ctx, in.RecordAssetID, in.RecordVersionID, recordDirectory)
	if err != nil {
		return PreparedManagementRecord{}, err
	}
	instance, err := c.worksetInstance(ctx)
	if err != nil {
		return PreparedManagementRecord{}, err
	}
	if p.InstanceID != instance {
		return PreparedManagementRecord{}, errcode.New(errcode.RefMismatch, "return belongs to another instance")
	}
	response, err := c.ReadResourceJSON(ctx, "/api/v1/assets/"+p.Entry.AssetID+"/versions/"+receipt.VersionId+"?view=full")
	if err != nil {
		return PreparedManagementRecord{}, err
	}
	var identity struct {
		Manifest struct {
			InstanceID  string          `json:"instance_id"`
			ProjectID   string          `json:"project_id"`
			AssetID     string          `json:"asset_id"`
			VersionID   string          `json:"version_id"`
			OperationID string          `json:"operation_id"`
			Content     json.RawMessage `json:"content"`
		} `json:"manifest"`
	}
	if err := response.Decode(&identity); err != nil {
		return PreparedManagementRecord{}, err
	}
	m := identity.Manifest
	if m.InstanceID != instance || m.ProjectID != p.Input.ProjectID || m.AssetID != p.Entry.AssetID || m.VersionID != receipt.VersionId || m.OperationID != receipt.OperationId {
		return PreparedManagementRecord{}, errcode.New(errcode.RefMismatch, "accepted return identity differs from core manifest")
	}
	newEntry := p.Entry
	newEntry.VersionID = receipt.VersionId
	planned, err := c.worksetManifest(ctx, newEntry, instance)
	if err != nil {
		return PreparedManagementRecord{}, err
	}
	if len(planned.Files) != len(p.Input.Content.Files) {
		return PreparedManagementRecord{}, worksetInvalid("accepted return file count differs from frozen input")
	}
	if receipt.ManifestDigest != string(planned.ManifestDigest) || receipt.Ref.InstanceId != instance || receipt.Ref.AssetId != p.Entry.AssetID || receipt.Ref.VersionId != receipt.VersionId {
		return PreparedManagementRecord{}, errcode.New(errcode.RefMismatch, "return receipt digest/ref differs from actual core version")
	}
	var content struct {
		AssetType string      `json:"asset_type"`
		Files     []InputFile `json:"files"`
		Uses      []struct {
			InstanceID string `json:"instance_id"`
			AssetID    string `json:"asset_id"`
			VersionID  string `json:"version_id"`
			Relation   string `json:"relation"`
		} `json:"uses"`
	}
	if err := json.Unmarshal(m.Content, &content); err != nil {
		return PreparedManagementRecord{}, err
	}
	relation := "reference"
	if p.Purpose == "production" {
		relation = "derived_from"
	}
	if content.AssetType != p.Input.Content.AssetType || len(content.Uses) != 1 || content.Uses[0].InstanceID != instance || content.Uses[0].AssetID != p.Entry.AssetID || content.Uses[0].VersionID != p.Entry.VersionID || content.Uses[0].Relation != relation {
		return PreparedManagementRecord{}, worksetInvalid("actual returned type/source use differs from original preparation")
	}
	expected := map[string]InputFile{}
	for _, f := range p.Input.Content.Files {
		expected[f.Path] = f
	}
	for _, f := range planned.Files {
		want, ok := expected[f.SourcePath]
		if !ok || want.SHA256 != f.SHA256 || want.Size != f.Size {
			return PreparedManagementRecord{}, errcode.New(errcode.HashMismatch, "accepted return bytes differ from frozen input")
		}
	}
	for _, f := range content.Files {
		if expected[f.Path] != f {
			return PreparedManagementRecord{}, worksetInvalid("actual returned file role/bytes differ from original input")
		}
	}
	for _, version := range []string{p.Entry.VersionID, receipt.VersionId} {
		response, err := c.ReadResourceJSON(ctx, "/api/v1/assets/"+p.Entry.AssetID+"/versions/"+version+"/rights")
		if err != nil {
			return PreparedManagementRecord{}, err
		}
		var view apiv1.VersionRightsView
		if err := response.Decode(&view); err != nil {
			return PreparedManagementRecord{}, err
		}
		allowed := false
		for _, purpose := range view.Purposes {
			if purpose.Purpose == p.Purpose {
				allowed = purpose.Allowed
			}
		}
		if view.Subject.InstanceId != instance || view.Subject.AssetId != p.Entry.AssetID || view.Subject.VersionId != version || !allowed {
			return PreparedManagementRecord{}, errcode.New(errcode.UseRestricted, "current purpose no longer allows adopting this return")
		}
	}
	updated := stored.Input
	updated.Entries = append([]WorksetEntry(nil), stored.Input.Entries...)
	matches := 0
	for i, entry := range updated.Entries {
		if entry == p.Entry {
			updated.Entries[i] = newEntry
			matches++
		}
	}
	if matches != 1 {
		return PreparedManagementRecord{}, worksetInvalid("original fixed workset entry changed or is not unique")
	}
	if _, err := c.InspectWorkset(ctx, updated); err != nil {
		return PreparedManagementRecord{}, err
	}
	return c.PrepareWorksetRecord(ctx, WorksetRecordSpec{ProjectID: string(record.Envelope.ProjectID), Title: stored.Title, Input: updated, AssetID: in.RecordAssetID, BaseVersionID: in.RecordVersionID})
}
