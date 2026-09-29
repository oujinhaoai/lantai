package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/plugins/corecheck"
)

// ProcessorInput uses the existing file protocol. The host supplies fixed
// manifest bytes at in/manifest.json; the child never receives session secrets.
type ProcessorInput struct {
	Contract    string                `json:"contract"`
	OperationID ids.ID                `json:"operation_id"`
	Fence       ae.JobFence           `json:"job_fence"`
	InputRefs   []ids.PermanentRef    `json:"input_refs"`
	InputDigest digest.Digest         `json:"input_digest"`
	Deadline    string                `json:"deadline"`
	Activation  ae.ActivationSnapshot `json:"activation_snapshot"`
	Producer    storage.Producer      `json:"producer"`
}
type ProcessorResult struct {
	Contract    string            `json:"contract"`
	OperationID ids.ID            `json:"operation_id"`
	Status      string            `json:"status"`
	Files       []ae.ArtifactFile `json:"files"`
	Records     []json.RawMessage `json:"records"`
	Checks      []CheckResult     `json:"checks"`
	Producer    storage.Producer  `json:"producer"`
}
type InvocationResult struct {
	Observation execution.Invocation
	Result      *ProcessorResult
}

// BuiltinSnapshot authorizes only the compiled official validator, for the
// current recovery epoch. It does not activate externally supplied packages.
func (r *Registry) BuiltinSnapshot(ctx context.Context, epoch int64) (ae.ActivationSnapshot, error) {
	p, e := r.Producer(ctx, "org.lantai.corecheck.manifest")
	if e != nil {
		return ae.ActivationSnapshot{}, e
	}
	reg, e := r.Lookup(ctx, p.ExtensionID, p.ExtensionVersion)
	if e != nil {
		return ae.ActivationSnapshot{}, e
	}
	target := reg.Manifest.Targets["server"]
	var entry digest.Digest
	for _, f := range reg.Manifest.Files {
		if f.Path == target.Entry {
			entry = digest.Digest("sha256:" + f.SHA256)
		}
	}
	s := ae.ActivationSnapshot{Contract: "lantai.activation-snapshot/v1", Activation: execution.Activation{ExtensionID: p.ExtensionID, ExtensionVersion: p.ExtensionVersion, PackageDigest: p.PackageDigest, Generation: epoch}, Entry: ae.PackageEntry{ExtensionID: p.ExtensionID, ExtensionVersion: p.ExtensionVersion, PackageDigest: p.PackageDigest, Target: "server", Entry: target.Entry, EntryDigest: entry}, EffectiveConfigRevision: 1, EnablementPolicyRevision: 1}
	return s, s.Validate()
}
func (r *Registry) CheckBuiltinSnapshot(ctx context.Context, s ae.ActivationSnapshot, epoch int64) error {
	current, e := r.BuiltinSnapshot(ctx, epoch)
	if e != nil {
		return e
	}
	if s != current {
		return errcode.New(errcode.ExtensionActivationStale, "")
	}
	return nil
}

// RunBuiltin executes the same immutable core binary's private processor entry.
// This is a bounded trusted process, not a sandbox for third-party code. The
// official checker cannot spawn children or perform network/storage operations.
func (r *Registry) RunBuiltin(ctx context.Context, in ProcessorInput, raw []byte) (InvocationResult, error) {
	var out InvocationResult
	if e := validate("lantai.processor-input/v1", in); e != nil {
		return out, e
	}
	if len(raw) > 8<<20 || len(in.InputRefs) != 1 {
		return out, errcode.New(errcode.QuotaExceeded, "")
	}
	if e := r.VerifyProducer(ctx, in.Producer); e != nil {
		return out, e
	}
	if e := r.CheckBuiltinSnapshot(ctx, in.Activation, in.Fence.RecoveryEpoch); e != nil {
		return out, e
	}
	deadline, e := clock.Parse(in.Deadline)
	if e != nil {
		return out, e
	}
	if !deadline.After(time.Now()) {
		return out, errcode.New(errcode.LeaseStale, "job deadline expired")
	}
	exe, e := os.Executable()
	if e != nil {
		return out, e
	}
	actual, e := CurrentReleaseDigest()
	if e != nil {
		return out, e
	}
	if actual != r.release {
		return out, errcode.New(errcode.HashMismatch, "release changed before dispatch")
	}
	dir, e := os.MkdirTemp("", "lantai-check-")
	if e != nil {
		return out, e
	}
	defer os.RemoveAll(dir)
	if e = os.Mkdir(filepath.Join(dir, "in"), 0700); e != nil {
		return out, e
	}
	if e = os.Mkdir(filepath.Join(dir, "out"), 0700); e != nil {
		return out, e
	}
	if e = os.WriteFile(filepath.Join(dir, "in", "manifest.json"), raw, 0600); e != nil {
		return out, e
	}
	job, e := json.Marshal(in)
	if e != nil {
		return out, e
	}
	if e = os.WriteFile(filepath.Join(dir, "job.json"), job, 0600); e != nil {
		return out, e
	}
	runCtx, cancel := context.WithDeadline(ctx, minTime(deadline, time.Now().Add(10*time.Second)))
	defer cancel()
	cmd := exec.CommandContext(runCtx, exe, "_processor-check")
	cmd.Dir = dir
	cmd.Env = []string{}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		return out, e
	}
	out.Observation.Dispatched = true
	e = cmd.Wait()
	out.Observation.StopConfirmed = true
	out.Observation.Exited = true
	out.Observation.ExitCode = cmd.ProcessState.ExitCode()
	out.Observation.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
	out.Observation.Cancelled = errors.Is(runCtx.Err(), context.Canceled)
	if e != nil {
		return out, nil
	}
	resultPath := filepath.Join(dir, "out", "result.json")
	info, e := os.Lstat(resultPath)
	if e != nil || !info.Mode().IsRegular() {
		return out, nil
	}
	f, e := os.Open(resultPath)
	if e != nil {
		return out, nil
	}
	defer f.Close()
	stat, e := f.Stat()
	if e != nil || !stat.Mode().IsRegular() || stat.Size() > 1<<20 {
		return out, nil
	}
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return out, nil
	}
	var res ProcessorResult
	if e = json.Unmarshal(b, &res); e != nil {
		return out, nil
	}
	if e = validate("lantai.processor-result/v1", res); e != nil {
		return out, nil
	}
	if res.OperationID != in.OperationID || res.Producer != in.Producer || len(res.Files) != 0 || len(res.Records) != 0 || len(res.Checks) != 1 {
		return out, nil
	}
	check := res.Checks[0]
	if check.Ref != in.InputRefs[0] || check.ManifestDigest != in.InputDigest || check.Producer != in.Producer {
		return out, nil
	}
	out.Observation.ResultValid = true
	out.Result = &res
	return out, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// ProcessorMain is reachable only by an explicit private executable argument.
// Its input is a schema-checked file document, never a shell command.
func ProcessorMain() error {
	jobFile, e := os.Open("job.json")
	if e != nil {
		return e
	}
	defer jobFile.Close()
	b, e := io.ReadAll(io.LimitReader(jobFile, (64<<10)+1))
	if e != nil {
		return e
	}
	if len(b) > 64<<10 {
		return errors.New("job too large")
	}
	var in ProcessorInput
	if e = json.Unmarshal(b, &in); e != nil {
		return e
	}
	if e = validate("lantai.processor-input/v1", in); e != nil {
		return e
	}
	if len(in.InputRefs) != 1 {
		return errors.New("one fixed input required")
	}
	f, e := os.Open(filepath.Join("in", "manifest.json"))
	if e != nil {
		return e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if e != nil {
		return e
	}
	if len(raw) > 8<<20 {
		return errors.New("input too large")
	}
	check := CheckResult{Contract: "lantai.check-result/v1", Ref: in.InputRefs[0], ManifestDigest: in.InputDigest, Verdict: execution.VerdictFail, Findings: []string{"manifest_identity_or_content_invalid"}, Producer: in.Producer}
	if corecheck.Check(raw, check.Ref, check.ManifestDigest) {
		check.Verdict = execution.VerdictPass
		check.Findings = []string{}
	}
	out := ProcessorResult{Contract: "lantai.processor-result/v1", OperationID: in.OperationID, Status: "completed", Files: []ae.ArtifactFile{}, Records: []json.RawMessage{}, Checks: []CheckResult{check}, Producer: in.Producer}
	if e = validate("lantai.processor-result/v1", out); e != nil {
		return e
	}
	b, e = json.Marshal(out)
	if e != nil {
		return e
	}
	return os.WriteFile(filepath.Join("out", "result.json"), b, 0600)
}
