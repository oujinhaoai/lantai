// Package localassistant executes one explicitly approved, file-based job using
// the existing public client. It is not a daemon, device credential issuer, or
// operating-system sandbox.
package localassistant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const JobSchema = "lantai.local-assistant-job/v1"
const InspectionSchema = "lantai.local-assistant-inspection/v1"
const ResultSchema = "lantai.local-assistant-result/v1"

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Job contains declarative inputs, never an executable, arbitrary argv, local
// credentials, or a server-issued transfer URL. All paths are workspace-relative.
type Job struct {
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

type Inspection struct {
	Schema           string            `json:"schema"`
	JobID            string            `json:"job_id"`
	Action           string            `json:"action"`
	JobDigest        string            `json:"job_digest"`
	ApprovalDigest   string            `json:"approval_digest"`
	WorkspaceBinding string            `json:"workspace_binding"`
	InputSHA256      string            `json:"input_sha256"`
	Origin           string            `json:"origin,omitempty"`
	Paths            map[string]string `json:"paths"`
	NetworkRequired  bool              `json:"network_required"`
	RemoteWrites     bool              `json:"remote_writes"`
	SourceDeleted    bool              `json:"source_deleted"`
	TimeoutSeconds   int               `json:"timeout_seconds"`
	Warnings         []string          `json:"warnings"`
}

type Options struct {
	Workspace      string
	ApprovalDigest string
	SessionFile    string
	CAFile         string
}

type Result struct {
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

type prepared struct {
	job        Job
	input      json.RawMessage
	report     json.RawMessage
	workspace  string
	root       *os.Root
	rootInfo   os.FileInfo
	jobPath    string
	inputPath  string
	reportPath string
	directory  string
	state      string
	target     string
	output     string
	inspection Inspection
}

// InspectFile is read-only: it validates syntax, input binding and paths without
// creating directories, requesting a session, or making any network request.
func InspectFile(jobPath, workspace string) (Inspection, error) {
	p, err := prepare(jobPath, workspace)
	if err != nil {
		return Inspection{}, err
	}
	defer p.root.Close()
	return p.inspection, nil
}

func prepare(jobPath, workspace string) (_ *prepared, err error) {
	abs, err := filepath.Abs(workspace)
	if err != nil || workspace == "" {
		return nil, invalid("an explicit existing workspace is required")
	}
	if err = noLinks(abs, false); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, invalid("workspace cannot be opened")
	}
	p := &prepared{workspace: abs, root: root}
	defer func() {
		if err != nil {
			root.Close()
		}
	}()
	p.rootInfo, err = root.Stat(".")
	if err != nil || !p.rootInfo.IsDir() {
		return nil, invalid("workspace must be a real directory")
	}
	p.jobPath, err = p.operatorPath(jobPath)
	if err != nil {
		return nil, err
	}
	raw, err := client.ReadPrivate(p.jobPath)
	if err != nil {
		return nil, invalid("job must be a private regular JSON file in the workspace")
	}
	if err = strict(raw, &p.job); err != nil {
		return nil, invalid("job JSON is invalid, ambiguous, or contains unknown fields")
	}
	j := &p.job
	if j.Schema != JobSchema || !safeID.MatchString(j.JobID) || !allowed(j.Action) {
		return nil, invalid("unsupported schema, job ID, or action")
	}
	if j.TimeoutSeconds == 0 {
		j.TimeoutSeconds = 3600
	}
	if j.TimeoutSeconds < 1 || j.TimeoutSeconds > 86400 {
		return nil, invalid("timeout_seconds must be 1..86400")
	}
	if (len(j.Input) == 0) == (j.InputFile == "") {
		return nil, invalid("exactly one input or input_file is required")
	}
	p.input = j.Input
	if j.InputFile != "" {
		if p.inputPath, err = p.relativePath(j.InputFile, false); err != nil {
			return nil, err
		}
		if p.input, err = client.ReadPrivate(p.inputPath); err != nil {
			return nil, invalid("input_file must be a private regular JSON file")
		}
	}
	if err = validateDocument(p.input); err != nil {
		return nil, invalid("input is not a bounded, unambiguous JSON document")
	}
	if p.input, err = resolveLocalResult(abs, p.input, j.Action, false); err != nil {
		return nil, err
	}
	if sensitiveJSON(p.input) {
		return nil, invalid("input contains credential or short-lived transfer fields; remove them before creating a job")
	}
	if len(j.Report) > 0 && j.ReportFile != "" {
		return nil, invalid("report and report_file are mutually exclusive")
	}
	p.report = j.Report
	if j.ReportFile != "" {
		if p.reportPath, err = p.relativePath(j.ReportFile, false); err != nil {
			return nil, err
		}
		if p.report, err = client.ReadPrivate(p.reportPath); err != nil {
			return nil, invalid("report_file must be a private regular JSON file")
		}
	}
	if len(p.report) > 0 {
		if err = validateDocument(p.report); err != nil {
			return nil, invalid("report is not a bounded, unambiguous JSON document")
		}
		if p.report, err = resolveLocalResult(abs, p.report, j.Action, true); err != nil {
			return nil, err
		}
		if sensitiveJSON(p.report) {
			return nil, invalid("report contains credentials or short-lived transfer fields")
		}
	}
	if p.directory, err = p.relativePath(j.Directory, !sourceRequired(j.Action)); err != nil {
		return nil, err
	}
	if j.State != "" {
		if p.state, err = p.relativePath(j.State, true); err != nil {
			return nil, err
		}
	}
	if j.Destination != "" {
		if p.target, err = p.relativePath(j.Destination, true); err != nil {
			return nil, err
		}
	}
	if p.output, err = p.relativePath(j.Output, true); err != nil {
		return nil, err
	}
	if err = p.validateShape(); err != nil {
		return nil, err
	}
	if err = p.checkOutput(); err != nil {
		return nil, err
	}
	if network(j.Action) {
		c, e := client.New(client.Config{BaseURL: j.Origin})
		if e != nil {
			return nil, invalid("remote jobs require a strict HTTPS origin")
		}
		origin := c.Origin()
		c.Close()
		if origin != j.Origin {
			return nil, invalid("origin must be exact, without a trailing path")
		}
	} else if j.Origin != "" {
		return nil, invalid("offline actions must not contain an origin")
	}
	if err = p.validateInput(); err != nil {
		return nil, err
	}
	binding := struct {
		Schema       string `json:"schema"`
		Workspace    string `json:"workspace"`
		JobSHA256    string `json:"job_sha256"`
		InputSHA256  string `json:"input_sha256"`
		ReportSHA256 string `json:"report_sha256"`
	}{JobSchema, abs, hash(raw), hash(p.input), hash(p.report)}
	bound, _ := json.Marshal(binding)
	canonicalJob, err := canonjson.Canonicalize(raw)
	if err != nil {
		return nil, invalid("job cannot be canonicalized")
	}
	paths := map[string]string{"directory": j.Directory, "output": j.Output}
	for key, value := range map[string]string{"state": j.State, "destination": j.Destination, "input_file": j.InputFile, "report_file": j.ReportFile} {
		if value != "" {
			paths[key] = value
		}
	}
	p.inspection = Inspection{Schema: InspectionSchema, JobID: j.JobID, Action: j.Action, JobDigest: hash(canonicalJob), ApprovalDigest: hash(bound), WorkspaceBinding: hash([]byte(abs)), InputSHA256: hash(p.input), Origin: j.Origin, Paths: paths, NetworkRequired: network(j.Action), RemoteWrites: j.Action == "import.apply", TimeoutSeconds: j.TimeoutSeconds, Warnings: []string{"Explicit one-shot local approval only; no device credential or persistent access is issued.", "Cancellation preserves original operation state and is not a rollback.", "This program is not a security sandbox against other local processes or privileged mount changes."}}
	if _, e := os.Lstat(p.output); e == nil {
		prior, e := client.ReadPrivate(p.output)
		var result Result
		if e != nil || strict(prior, &result) != nil || result.Schema != ResultSchema || result.JobID != j.JobID || result.Action != j.Action || result.JobDigest != p.inspection.JobDigest || result.ApprovalDigest != p.inspection.ApprovalDigest {
			return nil, invalid("existing output belongs to another job; choose a new result path")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, invalid("existing result output cannot be checked")
	}
	return p, p.checkWorkspace()
}

func allowed(action string) bool {
	switch action {
	case "import.plan", "import.apply", "import.verify", "workset.plan", "workset.pull", "references.plan", "references.copy", "references.rollback":
		return true
	}
	return false
}

func network(action string) bool {
	return action == "import.apply" || action == "import.verify" || strings.HasPrefix(action, "workset.")
}

func sourceRequired(action string) bool {
	return strings.HasPrefix(action, "import.") || strings.HasPrefix(action, "references.")
}

func (p *prepared) validateShape() error {
	j := p.job
	needState := j.Action == "import.apply" || j.Action == "import.verify"
	needDestination := j.Action == "import.verify" || j.Action == "references.copy" || j.Action == "references.rollback"
	if needState != (j.State != "") || needDestination != (j.Destination != "") || (j.Action == "import.verify") != (len(p.report) > 0) {
		return invalid("state, destination, or report does not match the chosen action")
	}
	if (j.Action == "references.rollback") != (j.PostCutoverRefs != nil) {
		return invalid("rollback requires an explicit post_cutover_refs array; other actions reject it")
	}
	read := j.Action == "workset.pull" || j.Action == "import.verify"
	if read {
		switch j.Purpose {
		case "archive_review", "reference", "production", "generative_input", "raw_export":
		default:
			return invalid("an explicit supported read purpose is required")
		}
	} else if j.Purpose != "" && !(j.Action == "workset.plan" && j.Purpose == "archive_review") {
		return invalid("purpose is not used by the selected action")
	}
	var paths []string
	for _, path := range []string{p.directory, p.state, p.target} {
		if path != "" {
			paths = append(paths, path)
		}
	}
	for i, path := range paths {
		for _, other := range paths[i+1:] {
			if overlap(path, other) {
				return invalid("source, state, and destination must be independent directories")
			}
		}
	}
	return nil
}

func (p *prepared) validateInput() error {
	switch p.job.Action {
	case "workset.plan", "workset.pull":
		in, err := client.DecodeWorkset(bytes.NewReader(p.input))
		if err != nil || client.ValidateWorkset(in, p.directory) != nil {
			return invalid("workset input is invalid")
		}
	case "import.plan":
		var in client.ImportInput
		if strict(p.input, &in) != nil || in.Schema != client.ImportInputSchema {
			return invalid("import input is invalid")
		}
	case "import.apply", "import.verify":
		var in client.ImportPlan
		if strict(p.input, &in) != nil || in.Schema != client.ImportPlanSchema || in.SourceRoot != p.directory {
			return invalid("import plan must bind the selected source directory")
		}
		if p.job.Action == "import.verify" {
			var report client.ImportReport
			if strict(p.report, &report) != nil {
				return invalid("import report is invalid")
			}
		}
	case "references.plan":
		var in client.ReferenceInput
		if strict(p.input, &in) != nil {
			return invalid("reference input is invalid")
		}
	case "references.copy", "references.rollback":
		var in client.ReferencePlan
		if strict(p.input, &in) != nil || in.SourceRoot == "" || !inside(p.workspace, in.SourceRoot) {
			return invalid("reference plan must bind a source inside the selected workspace")
		}
		if err := noLinks(in.SourceRoot, false); err != nil {
			return err
		}
		if overlap(p.output, in.SourceRoot) {
			return invalid("output must be outside the original note source")
		}
	}
	return nil
}

func (p *prepared) relativePath(path string, allowMissing bool) (string, error) {
	if path == "" || client.SafeRelative(path) != nil || path == "." {
		return "", invalid("all job paths must be safe workspace-relative paths below the root")
	}
	full := filepath.Join(p.workspace, filepath.FromSlash(path))
	if !inside(p.workspace, full) {
		return "", invalid("path escapes the selected workspace")
	}
	if err := noLinks(full, allowMissing); err != nil {
		return "", err
	}
	return full, nil
}

func (p *prepared) operatorPath(path string) (string, error) {
	if path == "" {
		return "", invalid("an explicit local file is required")
	}
	if !filepath.IsAbs(path) {
		return p.relativePath(filepath.ToSlash(path), false)
	}
	clean := filepath.Clean(path)
	if !inside(p.workspace, clean) {
		return "", invalid("operator files must stay inside the selected workspace")
	}
	if err := noLinks(clean, false); err != nil {
		return "", err
	}
	return clean, nil
}

func (p *prepared) checkOutput() error {
	for _, path := range []string{p.directory, p.state, p.target} {
		if path != "" && overlap(path, p.output) {
			return invalid("output must stay outside source, state, and working-copy directories")
		}
	}
	for _, path := range []string{p.jobPath, p.inputPath, p.reportPath} {
		if path != "" && sameFile(path, p.output) {
			return invalid("output must not overwrite a job, input, or prior report")
		}
	}
	return nil
}

func (p *prepared) checkWorkspace() error {
	current, err := os.Lstat(p.workspace)
	if err != nil || !current.IsDir() || !os.SameFile(p.rootInfo, current) {
		return invalid("selected workspace changed; preserve state and stop")
	}
	return noLinks(p.workspace, false)
}

func noLinks(path string, allowMissing bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return invalid("local path cannot be resolved")
	}
	volume := filepath.VolumeName(abs)
	current := volume + string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(abs, current), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		st, e := os.Lstat(current)
		if errors.Is(e, os.ErrNotExist) && allowMissing {
			return nil
		}
		if e != nil || st.Mode()&os.ModeSymlink != 0 {
			return invalid("local path is missing, inaccessible, or linked")
		}
	}
	return nil
}

func fold(path string) string { return cases.Fold().String(norm.NFC.String(filepath.Clean(path))) }
func inside(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func overlap(a, b string) bool {
	r, p := fold(a), fold(b)
	return r == p || strings.HasPrefix(p, r+string(filepath.Separator)) || strings.HasPrefix(r, p+string(filepath.Separator))
}
func sameFile(a, b string) bool {
	if fold(a) == fold(b) {
		return true
	}
	x, e1 := os.Stat(a)
	y, e2 := os.Stat(b)
	return e1 == nil && e2 == nil && os.SameFile(x, y)
}
func validateDocument(raw []byte) error {
	if len(raw) == 0 || int64(len(raw)) > client.MaxJSONBytes {
		return errors.New("invalid document size")
	}
	return canonjson.Validate(raw)
}
func strict(raw []byte, value any) error {
	if err := validateDocument(raw); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("more than one document")
	}
	return nil
}
func hash(b []byte) string        { value := sha256.Sum256(b); return hex.EncodeToString(value[:]) }
func invalid(reason string) error { return fmt.Errorf("assistant: %s", reason) }

// RunFile requires a digest obtained from a separate read-only inspection. The
// same approved job may be explicitly rerun to resume its ORIGINAL client state;
// the assistant never retries automatically or creates replacement operation keys.
func RunFile(ctx context.Context, jobPath string, opts Options) (out Result, err error) {
	ctx, release, err := lockWorkspace(ctx, opts.Workspace)
	if err != nil {
		return out, err
	}
	defer release()
	p, err := prepare(jobPath, opts.Workspace)
	if err != nil {
		return out, err
	}
	defer p.root.Close()
	if !digestPattern.MatchString(opts.ApprovalDigest) || opts.ApprovalDigest != p.inspection.ApprovalDigest {
		return out, invalid("approval digest is absent or no longer matches this job/input/workspace")
	}
	var c *client.Client
	if network(p.job.Action) {
		if opts.SessionFile == "" {
			return out, invalid("remote execution requires an explicitly selected private session file")
		}
		sessionPath, e := p.operatorPath(opts.SessionFile)
		if e != nil {
			return out, e
		}
		if overlap(sessionPath, p.directory) || (p.state != "" && overlap(sessionPath, p.state)) || (p.target != "" && overlap(sessionPath, p.target)) || sameFile(sessionPath, p.output) {
			return out, invalid("credentials must be separate from source, state, destination, and output")
		}
		var saved struct {
			Schema string `json:"schema"`
			Origin string `json:"origin"`
			Token  string `json:"token"`
		}
		secret, e := client.ReadPrivate(sessionPath)
		if e != nil || strict(secret, &saved) != nil || saved.Schema != "lantai.client-session/v1" || saved.Origin != p.job.Origin || saved.Token == "" {
			return out, invalid("private session is missing, invalid, or bound to another origin")
		}
		caPath := ""
		if opts.CAFile != "" {
			if caPath, e = p.operatorPath(opts.CAFile); e != nil {
				return out, e
			}
			if sameFile(caPath, p.output) || overlap(caPath, p.directory) || (p.state != "" && overlap(caPath, p.state)) || (p.target != "" && overlap(caPath, p.target)) {
				return out, invalid("CA file must be separate from business input and output directories")
			}
		}
		c, err = client.New(client.Config{BaseURL: p.job.Origin, SessionToken: saved.Token, CAFile: caPath})
		if err != nil {
			return out, invalid("strict-TLS client configuration failed")
		}
		defer c.Close()
	} else if opts.SessionFile != "" || opts.CAFile != "" {
		return out, invalid("offline actions reject credential and CA arguments")
	}
	// Pin the report parent before potentially long I/O. This uses the existing
	// private output primitive and never resolves a replacement parent to write.
	output, err := client.OpenPinnedPrivateOutput(p.output)
	if err != nil {
		return out, invalid("private result output cannot be pinned")
	}
	defer output.Close()
	if err = p.checkWorkspace(); err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.job.TimeoutSeconds)*time.Second)
	defer cancel()
	out = Result{Schema: ResultSchema, JobID: p.job.JobID, Action: p.job.Action, JobDigest: p.inspection.JobDigest, ApprovalDigest: p.inspection.ApprovalDigest, StartedAt: time.Now().UTC()}
	value, actionErr := p.execute(ctx, c)
	out.FinishedAt = time.Now().UTC()
	if value != nil {
		out.Result, err = json.Marshal(value)
		if err != nil {
			out.Status, out.ExitCode, out.ErrorCode = "unknown", 3, "ASSISTANT_RESULT_ENCODING_FAILED"
		}
		if err == nil && sensitiveJSON(out.Result) {
			out.Result = nil
			out.Status, out.ExitCode, out.ErrorCode = "unknown", 3, "ASSISTANT_SENSITIVE_RESULT_REJECTED"
			err = invalid("result contains credential-like data; retain the original private state without exporting it")
		}
	}
	if err == nil {
		out.Status, out.ExitCode, out.ErrorCode = resultState(p.job.Action, value, actionErr)
	}
	if workspaceErr := p.checkWorkspace(); workspaceErr != nil {
		out.Status, out.ExitCode, out.ErrorCode = "unknown", 3, "ASSISTANT_WORKSPACE_CHANGED"
		actionErr = workspaceErr
	}
	encoded, e := json.Marshal(out)
	if e != nil || output.Write(append(encoded, '\n')) != nil {
		out.Status, out.ExitCode, out.ErrorCode = "unknown", 3, "ASSISTANT_RESULT_NOT_SAVED"
		return out, invalid("result could not be saved; preserve all original operation state before retrying")
	}
	if err != nil {
		return out, err
	}
	return out, actionErr
}

// This conservatively rejects credential fields and recognizable signed/transfer
// URLs. It is not a general secret/content classifier; local text remains private.
func sensitiveJSON(raw []byte) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	var visit func(any) bool
	visit = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			for key, item := range x {
				switch strings.ToLower(key) {
				case "token", "session_token", "csrf_token", "password", "totp_secret", "master_key", "private_key", "authorization", "recovery_codes", "part_url_template", "upload_url", "download_url", "read_grant_url":
					return true
				}
				if visit(item) {
					return true
				}
			}
		case []any:
			for _, item := range x {
				if visit(item) {
					return true
				}
			}
		case string:
			lower := strings.ToLower(strings.TrimSpace(x))
			if strings.HasPrefix(lower, "bearer ") {
				return true
			}
			if u, err := url.Parse(x); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
				if strings.HasPrefix(u.Path, "/xfer/") || u.User != nil {
					return true
				}
				for key := range u.Query() {
					switch strings.ToLower(key) {
					case "token", "access_token", "sig", "signature", "credential", "csrf_token":
						return true
					}
				}
			}
		}
		return false
	}
	return visit(value)
}

func (p *prepared) execute(ctx context.Context, c *client.Client) (any, error) {
	switch p.job.Action {
	case "workset.plan", "workset.pull":
		in, err := client.DecodeWorkset(bytes.NewReader(p.input))
		if err != nil {
			return nil, err
		}
		if p.job.Action == "workset.plan" {
			return c.PlanWorkset(ctx, in, p.directory)
		}
		return c.PullWorkset(ctx, in, p.directory, p.job.Purpose)
	case "import.plan":
		var in client.ImportInput
		if err := strict(p.input, &in); err != nil {
			return nil, err
		}
		return client.PrepareImport(ctx, in, p.directory)
	case "import.apply", "import.verify":
		var in client.ImportPlan
		if err := strict(p.input, &in); err != nil {
			return nil, err
		}
		if p.job.Action == "import.apply" {
			return c.ApplyImport(ctx, in, p.directory, p.state)
		}
		var report client.ImportReport
		if err := strict(p.report, &report); err != nil {
			return nil, err
		}
		return c.VerifyImport(ctx, in, report, p.directory, p.state, p.target, p.job.Purpose)
	case "references.plan":
		var in client.ReferenceInput
		if err := strict(p.input, &in); err != nil {
			return nil, err
		}
		plan, err := client.PrepareReferencePlan(ctx, in, p.directory)
		if err == nil && !plan.Complete {
			err = errors.New("reference plan has unresolved mappings")
		}
		return plan, err
	case "references.copy", "references.rollback":
		var in client.ReferencePlan
		if err := strict(p.input, &in); err != nil {
			return nil, err
		}
		if p.job.Action == "references.copy" {
			return client.CopyReferenceNotes(ctx, in, p.directory, p.target)
		}
		return client.RollbackReferenceNotes(ctx, in, p.directory, p.target, p.job.PostCutoverRefs)
	}
	return nil, invalid("unsupported action")
}

func resultState(action string, value any, err error) (string, int, string) {
	// Unknown external effects outrank the observer's cancellation. A lost
	// commit response must remain unresolved even when SIGINT/deadline caused it.
	if r, ok := value.(client.ImportReport); ok {
		for _, item := range r.Results {
			if item.Status == "unknown" {
				return "unknown", 3, actionErrorCode(err, "IMPORT_INCOMPLETE")
			}
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled", 130, "ASSISTANT_CANCELLED_PRESERVE_STATE"
	}
	if err == nil {
		return "completed", 0, ""
	}
	switch r := value.(type) {
	case client.ImportReport:
		for _, item := range r.Results {
			if item.Status == "unknown" {
				return "unknown", 3, actionErrorCode(err, "IMPORT_INCOMPLETE")
			}
		}
		return "partial", 1, actionErrorCode(err, "IMPORT_INCOMPLETE")
	case client.ImportVerifyReport:
		return "partial", 1, actionErrorCode(err, "IMPORT_VERIFICATION_INCOMPLETE")
	case client.WorksetReceipt:
		if r.State == "partial" {
			return "partial", 1, actionErrorCode(err, "WORKSET_INCOMPLETE")
		}
	case client.ReferenceReceipt, client.ReferencePlan:
		return "partial", 1, "REFERENCE_INCOMPLETE"
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return "failed", 1, actionErrorCode(err, "ASSISTANT_REMOTE_REJECTED")
	}
	if action == "import.apply" {
		return "unknown", 3, "ASSISTANT_OPERATION_UNKNOWN"
	}
	return "failed", 3, "ASSISTANT_OPERATION_FAILED"
}

func actionErrorCode(err error, fallback string) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		code := string(apiErr.Body.Code)
		if regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`).MatchString(code) {
			return code
		}
	}
	return fallback
}
