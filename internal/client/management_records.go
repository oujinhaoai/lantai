package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/managementrecord"
)

const PreparedManagementRecordSchema = "lantai.management-record-prepared/v1"

type ManagementRecordSpec struct {
	ProjectID     string          `json:"project_id"`
	Kind          string          `json:"kind"`
	Title         string          `json:"title"`
	Payload       json.RawMessage `json:"payload"`
	AssetID       string          `json:"asset_id,omitempty"`
	BaseVersionID string          `json:"base_version_id,omitempty"`
}

type PreparedManagementRecord struct {
	Schema   string                    `json:"schema"`
	Origin   string                    `json:"origin"`
	Spec     ManagementRecordSpec      `json:"spec"`
	Envelope managementrecord.Envelope `json:"envelope"`
	Input    PushInput                 `json:"input"`
	SHA256   string                    `json:"sha256"`
	Size     int64                     `json:"size"`
}

type ManagementRecordRead struct {
	Envelope  managementrecord.Envelope `json:"record"`
	AssetID   string                    `json:"asset_id"`
	VersionID string                    `json:"version_id"`
	SHA256    string                    `json:"sha256"`
	Size      int64                     `json:"size"`
	Data      []byte                    `json:"-"`
}

func managementInvalid(message string) error { return errcode.New(errcode.SchemaInvalid, message) }

// PrepareManagementRecord performs only reads. No remote operation
// or random identity is introduced until the original prepared input is saved.
func (c *Client) PrepareManagementRecord(ctx context.Context, spec ManagementRecordSpec) (PreparedManagementRecord, error) {
	var out PreparedManagementRecord
	rules, kinds, err := managementrecord.Definition()
	if err != nil {
		return out, err
	}
	spec.Title = strings.TrimSpace(spec.Title)
	if !ids.ID(spec.ProjectID).Valid() || !slices.Contains(kinds, spec.Kind) || spec.Title == "" || len(utf16.Encode([]rune(spec.Title))) > rules.MaxTitleUnits {
		return out, managementInvalid("valid project, record kind and title required")
	}
	if (spec.AssetID == "") != (spec.BaseVersionID == "") || spec.AssetID != "" && (!ids.ID(spec.AssetID).Valid() || !ids.ID(spec.BaseVersionID).Valid()) {
		return out, managementInvalid("record revision requires both the original asset and exact base version")
	}
	if err = managementrecord.ValidatePayload(spec.Payload); err != nil {
		return out, managementInvalid(err.Error())
	}
	instance, err := c.worksetInstance(ctx)
	if err != nil {
		return out, err
	}
	if spec.AssetID != "" {
		base, err := c.Do(ctx, http.MethodGet, "/api/v1/assets/"+spec.AssetID+"/versions/"+spec.BaseVersionID+"?view=full", nil, Options{})
		if err != nil {
			return out, err
		}
		var exact struct {
			Manifest struct {
				InstanceID string `json:"instance_id"`
				ProjectID  string `json:"project_id"`
				AssetID    string `json:"asset_id"`
				VersionID  string `json:"version_id"`
				Content    struct {
					AssetType string `json:"asset_type"`
					Metadata  struct {
						Extra struct {
							Record struct {
								Schema string `json:"schema"`
								Kind   string `json:"kind"`
							} `json:"management_record"`
						} `json:"extra"`
					} `json:"metadata"`
				} `json:"content"`
			} `json:"manifest"`
		}
		if err := base.Decode(&exact); err != nil {
			return out, err
		}
		m := exact.Manifest
		if m.InstanceID != instance || m.ProjectID != spec.ProjectID || m.AssetID != spec.AssetID || m.VersionID != spec.BaseVersionID || m.Content.AssetType != "config" || m.Content.Metadata.Extra.Record.Schema != managementrecord.Contract || m.Content.Metadata.Extra.Record.Kind != spec.Kind {
			return out, managementInvalid("revision baseline is not the original management record project and kind")
		}
	}
	payload, err := canonjson.Canonicalize(spec.Payload)
	if err != nil {
		return out, err
	}
	spec.Payload = payload
	env := managementrecord.Envelope{Schema: managementrecord.Contract, Kind: spec.Kind, InstanceID: ids.ID(instance), ProjectID: ids.ID(spec.ProjectID), Payload: payload}
	data, err := managementrecord.Render(env)
	if err != nil {
		return out, managementInvalid(err.Error())
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	metadata, _ := json.Marshal(map[string]any{"tool": "lantai-management", "extra": map[string]any{"management_record": map[string]any{"schema": managementrecord.Contract, "kind": spec.Kind}}})
	in := PushInput{ProjectID: spec.ProjectID, AssetID: spec.AssetID, BaseVersionID: spec.BaseVersionID, Content: ContentInput{AssetType: "config", Files: []InputFile{{Path: "record.json", Role: "record", SHA256: hash, Size: int64(len(data))}}, Rights: json.RawMessage(`{"usage":"reference","license":"LicenseRef-Lantai-Private-Record","redistribute_raw":false,"noai":true,"sensitivity":"normal"}`), Metadata: metadata}}
	if spec.AssetID == "" {
		in.Slug = "management/" + spec.Kind + "-" + hash[:24]
		in.Describe, _ = json.Marshal(map[string]any{"title": spec.Title, "tags": []string{"lantai-management-record", "record-kind:" + spec.Kind}})
	}
	return PreparedManagementRecord{Schema: PreparedManagementRecordSchema, Origin: c.Origin(), Spec: spec, Envelope: env, Input: in, SHA256: hash, Size: int64(len(data))}, nil
}

// SaveManagementRecord reconstitutes and verifies the prepared input before any
// file publication or remote mutation. Push owns original keys/state/receipts.
func (c *Client) SaveManagementRecord(ctx context.Context, p PreparedManagementRecord, directory, statePath string) (Response, error) {
	if statePath == "" {
		return Response{}, managementInvalid("original private push state path is required")
	}
	if p.Schema != PreparedManagementRecordSchema || p.Origin != c.Origin() {
		return Response{}, managementInvalid("prepared record belongs to a different client contract or server")
	}
	physical, err := importAbsolute(directory)
	if err != nil {
		return Response{}, err
	}
	directory = physical
	root, err := openWorksetRoot(directory, false)
	if err != nil {
		return Response{}, err
	}
	if root == nil {
		return Response{}, managementInvalid("record directory must already exist")
	}
	defer root.Close()
	current, err := c.PrepareManagementRecord(ctx, p.Spec)
	if err != nil {
		return Response{}, err
	}
	old, err := canonjson.CanonicalizeValue(p)
	if err != nil {
		return Response{}, err
	}
	expected, err := canonjson.CanonicalizeValue(current)
	if err != nil {
		return Response{}, err
	}
	if !bytes.Equal(old, expected) {
		return Response{}, errcode.New(errcode.IdempotencyConflict, "prepared scope, baseline or record changed; preserve the original input and state")
	}
	data, err := managementrecord.Render(current.Envelope)
	if err != nil {
		return Response{}, err
	}
	if err = checkWorksetRoot(directory, root); err != nil {
		return Response{}, err
	}
	if err = writeManagementFile(ctx, directory, data); err != nil {
		return Response{}, err
	}
	if err = checkWorksetRoot(directory, root); err != nil {
		return Response{}, err
	}
	return c.Push(ctx, current.Input, directory, statePath, false)
}

// Publish through a pinned directory and an exclusive hard link. Existing
// record.json is accepted only if it is private, regular and byte-identical;
// neither a conflict nor a raced target can be overwritten by preparation.
func writeManagementFile(ctx context.Context, directory string, data []byte) error {
	return writeManagementPrivateDocument(ctx, filepath.Join(directory, "record.json"), data)
}

// WriteManagementPreparation saves immutable original input before execution.
// An existing different plan is a conflict, never an invitation to reset keys.
func WriteManagementPreparation(ctx context.Context, path string, p PreparedManagementRecord) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeManagementPrivateDocument(ctx, path, append(raw, '\n'))
}

func writeManagementPrivateDocument(ctx context.Context, path string, data []byte) error {
	if int64(len(data)) > MaxJSONBytes {
		return managementInvalid("private management document exceeds byte budget")
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	directory, name := filepath.Dir(target), filepath.Base(target)
	if name == "." || name == string(filepath.Separator) {
		return managementInvalid("private document filename required")
	}
	physical, err := importAbsolute(directory)
	if err != nil {
		return err
	}
	directory = physical
	root, err := openWorksetRoot(directory, false)
	if err != nil {
		return err
	}
	if root == nil {
		return managementInvalid("record directory must already exist")
	}
	defer root.Close()
	if err = ctx.Err(); err != nil {
		return err
	}
	tempName := ".lantai-record-" + rand.Text()
	temp, err := createPinnedOutputTemp(root, tempName)
	if err != nil {
		return err
	}
	defer func() { _ = temp.Close(); _ = root.Remove(tempName) }()
	if _, err = temp.Write(data); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	opened, err := temp.Stat()
	if err != nil {
		return err
	}
	current, err := root.Lstat(tempName)
	if err != nil || !os.SameFile(opened, current) || !current.Mode().IsRegular() {
		return errcode.New(errcode.PathConflict, "record preparation file changed")
	}
	if err = checkPinnedOutputPrivate(temp); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = checkWorksetRoot(directory, root); err != nil {
		return err
	}
	if err = root.Link(tempName, name); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	st, err := root.Lstat(name)
	if err != nil || !st.Mode().IsRegular() {
		return errcode.New(errcode.PathConflict, "record target is not a regular private file")
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(st, actual) {
		return errcode.New(errcode.PathConflict, "record target changed")
	}
	if err = checkPinnedOutputPrivate(f); err != nil {
		return err
	}
	got, err := io.ReadAll(io.LimitReader(f, int64(len(data))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, data) {
		return errcode.New(errcode.PathConflict, "record target differs; preserve original input/state and choose a new explicit preparation directory")
	}
	if err = syncPinnedOutputDirectory(root); err != nil {
		return err
	}
	return checkWorksetRoot(directory, root)
}

func (c *Client) ListManagementRecords(ctx context.Context, project, kind, cursor string, limit int) (Response, error) {
	_, kinds, err := managementrecord.Definition()
	if err != nil {
		return Response{}, err
	}
	if !ids.ID(project).Valid() || !slices.Contains(kinds, kind) || limit < 1 || limit > 100 {
		return Response{}, managementInvalid("valid project, record kind and page limit required")
	}
	q := url.Values{"project_id": {project}, "asset_type": {"config"}, "tag": {"lantai-management-record", "record-kind:" + kind}, "cursor": {cursor}, "limit": {strconv.Itoa(limit)}}
	return c.Do(ctx, http.MethodGet, "/api/v1/assets?"+q.Encode(), nil, Options{})
}

func (c *Client) ReadManagementRecord(ctx context.Context, asset, version, directory string) (ManagementRecordRead, error) {
	var out ManagementRecordRead
	if !ids.ID(asset).Valid() || !ids.ID(version).Valid() {
		return out, managementInvalid("fixed record asset/version required")
	}
	physical, err := importAbsolute(directory)
	if err != nil {
		return out, err
	}
	directory = physical
	root, err := openWorksetRoot(directory, false)
	if err != nil {
		return out, err
	}
	if root == nil {
		return out, managementInvalid("record read directory must already exist")
	}
	defer root.Close()
	instance, err := c.worksetInstance(ctx)
	if err != nil {
		return out, err
	}
	endpoint := "/api/v1/assets/" + asset + "/versions/" + version
	response, err := c.Do(ctx, http.MethodGet, endpoint+"?view=full", nil, Options{})
	if err != nil {
		return out, err
	}
	var exact struct {
		Manifest struct {
			InstanceID string `json:"instance_id"`
			ProjectID  string `json:"project_id"`
			AssetID    string `json:"asset_id"`
			VersionID  string `json:"version_id"`
			Content    struct {
				AssetType string      `json:"asset_type"`
				Files     []InputFile `json:"files"`
			} `json:"content"`
		} `json:"manifest"`
	}
	if err = response.Decode(&exact); err != nil {
		return out, err
	}
	rules, _, err := managementrecord.Definition()
	if err != nil {
		return out, err
	}
	m := exact.Manifest
	if m.InstanceID != instance || m.AssetID != asset || m.VersionID != version || m.Content.AssetType != "config" || len(m.Content.Files) != 1 || m.Content.Files[0].Path != "record.json" || m.Content.Files[0].Role != "record" || m.Content.Files[0].Size < 1 || m.Content.Files[0].Size > int64(rules.MaxRecordBytes) {
		return out, managementInvalid("exact version is not a bounded management record")
	}
	if _, err = c.Pull(ctx, asset, version, directory, "archive_review"); err != nil {
		return out, err
	}
	if err = checkWorksetRoot(directory, root); err != nil {
		return out, err
	}
	st, err := root.Lstat("record.json")
	if err != nil || !st.Mode().IsRegular() {
		return out, errcode.New(errcode.PathConflict, "record read target changed")
	}
	f, err := root.Open("record.json")
	if err != nil {
		return out, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return out, errcode.New(errcode.PathConflict, "record file changed while opening")
	}
	if err = checkPinnedOutputPrivate(f); err != nil {
		return out, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(rules.MaxRecordBytes)+1))
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if hash != m.Content.Files[0].SHA256 || int64(len(data)) != m.Content.Files[0].Size {
		return out, errcode.New(errcode.HashMismatch, "record bytes differ from fixed manifest")
	}
	env, err := managementrecord.Decode(data)
	if err != nil {
		return out, managementInvalid(err.Error())
	}
	if env.InstanceID != ids.ID(instance) || env.ProjectID != ids.ID(m.ProjectID) {
		return out, managementInvalid("record envelope scope differs from exact manifest")
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	return ManagementRecordRead{Envelope: env, AssetID: asset, VersionID: version, SHA256: hash, Size: int64(len(data)), Data: data}, nil
}

func (c *Client) CheckManagementBatch(ctx context.Context, asset, version string) (Response, error) {
	if !ids.ID(asset).Valid() || !ids.ID(version).Valid() {
		return Response{}, managementInvalid("fixed batch record version required")
	}
	return c.Do(ctx, http.MethodPost, "/api/v1/management/batches/check", map[string]string{"asset_id": asset, "version_id": version}, Options{})
}

func (s *ManagementRecordSpec) UnmarshalJSON(raw []byte) error {
	type plain ManagementRecordSpec
	var value plain
	if canonjson.Validate(raw) != nil {
		return managementInvalid("management spec must be unambiguous JSON")
	}
	if err := importStrictJSON(raw, &value); err != nil {
		return managementInvalid("management spec contains invalid or unknown fields")
	}
	*s = ManagementRecordSpec(value)
	return nil
}

func (p *PreparedManagementRecord) UnmarshalJSON(raw []byte) error {
	type plain PreparedManagementRecord
	var value plain
	if canonjson.Validate(raw) != nil {
		return managementInvalid("management preparation must be unambiguous JSON")
	}
	if err := importStrictJSON(raw, &value); err != nil {
		return managementInvalid("management preparation contains invalid or unknown fields")
	}
	*p = PreparedManagementRecord(value)
	return nil
}
