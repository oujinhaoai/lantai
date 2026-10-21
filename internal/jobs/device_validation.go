package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	dc "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/node"
)

type deviceNodes interface {
	OwnedDevice(context.Context, authz.Context, ids.ID, bool) (dc.Device, error)
	WorkerDevice(context.Context, authz.Context, ids.ID) (dc.Device, error)
}
type localJob struct {
	Schema          string          `json:"schema"`
	JobID           string          `json:"job_id"`
	Action          string          `json:"action"`
	Origin          string          `json:"origin,omitempty"`
	Input           json.RawMessage `json:"input,omitempty"`
	InputFile       string          `json:"input_file,omitempty"`
	Directory       string          `json:"directory"`
	State           string          `json:"state,omitempty"`
	Destination     string          `json:"destination,omitempty"`
	Report          json.RawMessage `json:"report,omitempty"`
	ReportFile      string          `json:"report_file,omitempty"`
	PostCutoverRefs []string        `json:"post_cutover_refs"`
	Output          string          `json:"output"`
	Purpose         string          `json:"purpose,omitempty"`
	TimeoutSeconds  int             `json:"timeout_seconds,omitempty"`
}

// localResult is the exact original local-assistant result envelope. Private
// action payloads stay on the device; the wire carries only a bound reference.
type localResult struct {
	Schema           string          `json:"schema"`
	JobID            string          `json:"job_id"`
	Action           string          `json:"action"`
	JobDigest        string          `json:"job_digest"`
	ApprovalDigest   string          `json:"approval_digest"`
	Status           string          `json:"status"`
	ExitCode         int             `json:"exit_code"`
	ErrorCode        string          `json:"error_code,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	StartedAt        time.Time       `json:"started_at"`
	FinishedAt       time.Time       `json:"finished_at"`
	SourceDeleted    bool            `json:"source_deleted"`
	AutomaticRetry   bool            `json:"automatic_retry"`
	CredentialIssued bool            `json:"credential_issued"`
}
type localResultRef struct {
	Schema       string `json:"schema"`
	JobID        string `json:"job_id"`
	ResultSHA256 string `json:"result_sha256"`
}

func decodeResultRef(raw json.RawMessage) (localResultRef, error) {
	var ref localResultRef
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&ref) != nil || ref.Schema != "lantai.local-assistant-result-ref/v1" || !ids.ID(ref.JobID).Valid() || !node.DeviceDigest.MatchString(ref.ResultSHA256) {
		return ref, deviceInvalid("exact private result reference required")
	}
	return ref, nil
}
func deviceNetwork(action string) bool {
	return action == "import.apply" || action == "import.verify" || strings.HasPrefix(action, "workset.")
}

var localJobName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)
var credentialKey = regexp.MustCompile(`(?i)^(password|password_hash|secret|secret_hash|totp_secret|totp_uri|recovery_codes|master_key|csrf|csrf_token|session_token|access_token|refresh_token|authorization|part_url_template|read_grant_url|download_token|grant_id|token|session_file|ca_file|source_root)$`)

func deviceInvalid(message string) error { return errcode.New(errcode.SchemaInvalid, message) }
func deviceHash(raw []byte) string       { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func safeDeviceJSON(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return deviceInvalid("bounded device JSON required")
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		return err
	}
	var walk func(any, int) error
	walk = func(v any, depth int) error {
		if depth > 64 {
			return deviceInvalid("device JSON nesting limit")
		}
		switch x := v.(type) {
		case map[string]any:
			for k, value := range x {
				if credentialKey.MatchString(k) {
					return deviceInvalid("device protocol cannot carry credentials, transfer grants or private source roots")
				}
				if err := walk(value, depth+1); err != nil {
					return err
				}
			}
		case []any:
			for _, value := range x {
				if err := walk(value, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(v, 0)
}
func validateLocalJob(raw json.RawMessage, want string) (localJob, json.RawMessage, error) {
	var j localJob
	if err := safeDeviceJSON(raw); err != nil {
		return j, nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&j); err != nil {
		return j, nil, deviceInvalid("unknown local job fields")
	}
	if j.Schema != "lantai.local-assistant-job/v1" || !localJobName.MatchString(j.JobID) || !containsDeviceAction(j.Action) || j.TimeoutSeconds < 0 || j.TimeoutSeconds > 86400 {
		return j, nil, deviceInvalid("unsupported local job identity or action")
	}
	if len(j.Input) == 0 || j.InputFile != "" || j.ReportFile != "" || j.Directory == "" || j.Output == "" {
		return j, nil, deviceInvalid("precise input and relative workspace paths required")
	}
	for _, p := range []string{j.Directory, j.Output, j.State, j.Destination, j.InputFile, j.ReportFile} {
		if p != "" && (pathrule.Check(p) != nil || path.IsAbs(p) || strings.Contains(p, "\\")) {
			return j, nil, deviceInvalid("local paths must remain inside the selected workspace alias")
		}
	}

	needState := j.Action == "import.apply" || j.Action == "import.verify"
	needDestination := j.Action == "import.verify" || j.Action == "references.copy" || j.Action == "references.rollback"
	if needState != (j.State != "") || needDestination != (j.Destination != "") || (j.Action == "import.verify") != (len(j.Report) > 0) || (j.Action == "references.rollback") != (j.PostCutoverRefs != nil) || deviceNetwork(j.Action) != (j.Origin != "") {
		return j, nil, deviceInvalid("action paths, report and exact network origin must match the declared action")
	}
	read := j.Action == "workset.pull" || j.Action == "import.verify"
	if read {
		switch j.Purpose {
		case "archive_review", "reference", "production", "generative_input", "raw_export":
		default:
			return j, nil, deviceInvalid("explicit supported read purpose required")
		}
	} else if j.Purpose != "" && !(j.Action == "workset.plan" && j.Purpose == "archive_review") {
		return j, nil, deviceInvalid("read purpose does not match action")
	}
	if j.Origin != "" {
		u, err := url.Parse(j.Origin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return j, nil, deviceInvalid("exact HTTPS origin required")
		}
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		return j, nil, err
	}
	if !node.DeviceDigest.MatchString(want) || deviceHash(canonical) != want {
		return j, nil, errcode.New(errcode.HashMismatch, "local job digest does not match its canonical bytes")
	}
	return j, canonical, nil
}
func containsDeviceAction(a string) bool {
	for _, v := range node.DeviceActions {
		if a == v {
			return true
		}
	}
	return false
}
func (s *Service) deviceActionAuth(ctx context.Context, w authz.Context, project ids.ID, action string) error {
	if err := s.auth(ctx, w, identity.ActDeviceWork, project); err != nil {
		return err
	}
	if action == "import.apply" {
		for _, a := range []authz.Action{identity.ActStorageUpload, identity.ActCatalogCreateAsset, identity.ActLedgerCommitVersion} {
			if err := s.auth(ctx, w, a, project); err != nil {
				return err
			}
		}
	}
	if action == "workset.pull" || action == "import.verify" {
		return s.auth(ctx, w, identity.ActStorageReadContent, project)
	}
	return nil
}
func (s *Service) deviceRefs(ctx context.Context, current dc.Device, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		return err
	}
	var walk func(any) error
	walk = func(v any) error {
		switch x := v.(type) {
		case map[string]any:
			if x["schema"] == "lantai.local-assistant-result-ref/v1" {
				id, _ := x["job_id"].(string)
				digest, _ := x["result_sha256"].(string)
				if len(x) != 3 || !ids.ID(id).Valid() || !node.DeviceDigest.MatchString(digest) {
					return deviceInvalid("precise private result reference required")
				}
				j, err := s.load(ctx, ids.ID(id))
				if err != nil {
					return err
				}
				if j.Device == nil || j.Device.DeviceID != current.ID || j.Device.OwnerID != current.OwnerID || j.Request.ProjectID != current.ProjectID || j.Device.WorkspaceAlias != current.WorkspaceAlias || j.Device.WorkspaceBinding != current.WorkspaceBinding || j.State != "succeeded" || !j.Device.Stopped {
					return errcode.New(errcode.RefMismatch, "private result belongs to another device, workspace or incomplete job")
				}
				var r struct {
					Result json.RawMessage `json:"result"`
				}
				if json.Unmarshal(j.Device.Result, &r) != nil {
					return deviceInvalid("invalid observed result envelope")
				}
				observed, refErr := decodeResultRef(r.Result)
				if refErr != nil || observed.JobID != id || observed.ResultSHA256 != digest {
					return errcode.New(errcode.RefMismatch, "private result digest is not an observed original result")
				}
				return nil
			}
			for _, value := range x {
				if err := walk(value); err != nil {
					return err
				}
			}
		case []any:
			for _, value := range x {
				if err := walk(value); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(v)
}
