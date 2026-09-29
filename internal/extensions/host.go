package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
)

// IsolationCapabilities is what this host can actually enforce on the current
// platform. It is published to administrators and compared with each package's
// declared constraints; nothing here is a sandbox claim.
type IsolationCapabilities struct {
	Platform            string `json:"platform"`
	EnvAllowlist        bool   `json:"env_allowlist"`
	PrivateWorkdir      bool   `json:"private_workdir"`
	VerifiedEntryCopy   bool   `json:"verified_entry_copy"`
	Deadline            bool   `json:"deadline"`
	OutputLimits        bool   `json:"output_limits"`
	ProcessTree         string `json:"process_tree"`
	InputReadOnly       bool   `json:"input_read_only_enforced"`
	MemoryLimit         bool   `json:"memory_limit"`
	CPULimit            bool   `json:"cpu_limit"`
	NetworkIsolation    bool   `json:"network_isolation"`
	FilesystemIsolation bool   `json:"filesystem_isolation"`
	Sandbox             bool   `json:"sandbox"`
}

// HostCapabilities reports measured properties of the one-shot host. Process
// tree reclaim is a process group (Unix) or kill-on-close job object (Windows);
// a descendant that deliberately leaves its group can escape on Unix.
func HostCapabilities() IsolationCapabilities {
	return IsolationCapabilities{Platform: Platform(), EnvAllowlist: true, PrivateWorkdir: true, VerifiedEntryCopy: true, Deadline: true, OutputLimits: true, ProcessTree: processTreeMode}
}

// Platform is the artifact platform key used by platform_artifacts.
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Unenforced lists declared package constraints this host cannot enforce. A
// non-empty list requires an explicit trusted deployment decision at enablement.
func (c IsolationCapabilities) Unenforced(m Manifest) []string {
	out := []string{}
	if !c.MemoryLimit {
		out = append(out, "memory_limit")
	}
	if !c.CPULimit {
		out = append(out, "cpu_limit")
	}
	// An empty network list means "no network"; only isolation can enforce it.
	if !c.NetworkIsolation {
		out = append(out, "network_isolation")
	}
	if !c.FilesystemIsolation {
		out = append(out, "filesystem_isolation")
	}
	if !c.InputReadOnly && slices.Contains(m.Permissions.Filesystem, "granted-input:read") {
		out = append(out, "input_read_only")
	}
	return out
}

// InputFile is copied into in/<Name> before spawn; the child sees only copies.
// A Path input with SHA256 set must still have that digest when copied, so a
// file changed after the job was written is refused instead of passed on.
type InputFile struct {
	Name   string
	Data   []byte
	Path   string
	SHA256 string
}

// RunSpec is one file-protocol invocation of a verified package entry.
type RunSpec struct {
	Package        Package
	Files          fs.FS
	Target         string
	Job            []byte
	Inputs         []InputFile
	Deadline       time.Time
	BaseDir        string
	MaxInputBytes  int64
	MaxOutputBytes int64
	MaxOutputFiles int64
	MaxResultBytes int64
	MaxStderrBytes int64
	// OutputDir, when set, receives verified out/ files other than result.json.
	OutputDir string
	// ValidateResult, when set, must accept result.json before any output is
	// delivered; a rejection is a runtime fault and nothing leaves the run dir.
	ValidateResult func([]byte) error
}

// RunResult carries the observation; result bytes are unparsed business data.
type RunResult struct {
	Observation execution.Invocation
	Result      []byte
	Files       []File
	Stderr      []byte
	Failure     string
}

const (
	maxJobBytes           = 64 << 10
	defaultResultBytes    = 1 << 20
	defaultStderrBytes    = 64 << 10
	stopGrace             = 5 * time.Second
	hostDirMode           = 0o700
	resultName            = "result.json"
	outputVerificationErr = "undeclared_or_mismatched_output"
)

// EntryPath returns the entry for the current platform, or rejects a package
// whose entry artifact was built for a different platform.
func EntryPath(m Manifest, target string) (File, error) {
	t, ok := m.Targets[target]
	if !ok {
		return File{}, failure(errcode.ExtensionPointUnsupported, "target_missing")
	}
	for _, a := range m.PlatformArtifacts {
		if a.Path == t.Entry && a.Platform == Platform() {
			return a.File, nil
		}
	}
	return File{}, failure(errcode.ExtensionPointUnsupported, "platform_artifact_missing")
}

// RunOneShot spawns a registered entry exactly once under the file protocol:
// job.json, read-only in/, private out/ and a verified pkg/ copy. The child
// receives the job path as its only argument, an allowlisted environment and no
// core credentials. It returns an error only when nothing was dispatched.
func RunOneShot(ctx context.Context, spec RunSpec) (RunResult, error) {
	var out RunResult
	entry, err := EntryPath(spec.Package.Manifest, spec.Target)
	if err != nil {
		return out, err
	}
	if len(spec.Job) == 0 || len(spec.Job) > maxJobBytes || !json.Valid(spec.Job) {
		return out, failure(errcode.SchemaInvalid, "job_document_invalid")
	}
	if !spec.Deadline.After(time.Now()) {
		return out, errcode.New(errcode.LeaseStale, "invocation deadline expired before dispatch")
	}
	if spec.MaxResultBytes <= 0 {
		spec.MaxResultBytes = defaultResultBytes
	}
	if spec.MaxStderrBytes <= 0 {
		spec.MaxStderrBytes = defaultStderrBytes
	}
	if spec.MaxOutputBytes <= 0 {
		spec.MaxOutputBytes = spec.Package.Manifest.ResourceLimits.MaxOutputBytes
	}
	if spec.MaxOutputFiles < 0 {
		return out, failure(errcode.SchemaInvalid, "output_limit_invalid")
	}
	if spec.MaxInputBytes <= 0 {
		spec.MaxInputBytes = spec.Package.Manifest.ResourceLimits.MaxInputBytes
	}
	dir, err := os.MkdirTemp(spec.BaseDir, "lantai-ext-")
	if err != nil {
		return out, err
	}
	defer removeRunDir(dir)
	for _, d := range []string{"in", "out", "pkg", "tmp"} {
		if err = os.Mkdir(filepath.Join(dir, d), hostDirMode); err != nil {
			return out, err
		}
	}
	if err = copyPackage(spec.Files, spec.Package.Manifest, filepath.Join(dir, "pkg"), entry.Path); err != nil {
		return out, err
	}
	if err = writeInputs(filepath.Join(dir, "in"), spec.Inputs, spec.MaxInputBytes); err != nil {
		return out, err
	}
	jobPath := filepath.Join(dir, "job.json")
	if err = os.WriteFile(jobPath, spec.Job, 0o400); err != nil {
		return out, err
	}
	argv := []string{filepath.Join(dir, "pkg", filepath.FromSlash(entry.Path)), jobPath}
	obs, stderr, err := spawn(ctx, dir, argv, oneShotEnv(dir), spec.Deadline, spec.MaxStderrBytes)
	out.Observation, out.Stderr = obs, stderr
	if err != nil {
		out.Failure = "spawn_failed"
		return out, err
	}
	if !obs.StopConfirmed || obs.Cancelled || obs.TimedOut || !obs.Exited || obs.ExitCode != 0 {
		return out, nil
	}
	out.Result, out.Files, out.Failure = collectOutputs(filepath.Join(dir, "out"), spec)
	if out.Failure == "" && spec.ValidateResult != nil && spec.ValidateResult(out.Result) != nil {
		out.Result, out.Files, out.Failure = nil, nil, "result_invalid"
	}
	out.Observation.ResultValid = out.Failure == ""
	if out.Failure == "" && spec.OutputDir != "" {
		if err := deliverOutputs(filepath.Join(dir, "out"), spec.OutputDir, out.Files); err != nil {
			out.Observation.ResultValid = false
			out.Failure = "output_delivery_failed"
			out.Files = nil
		}
	}
	return out, nil
}

// oneShotEnv is the complete child environment. PATH is deliberately absent:
// the host never searches for executables and the child gets no secrets.
func oneShotEnv(dir string) []string {
	tmp := filepath.Join(dir, "tmp")
	return []string{"TMPDIR=" + tmp, "TMP=" + tmp, "TEMP=" + tmp, "LANTAI_PROTOCOL=lantai.processor/v1"}
}

// spawn starts argv in dir and returns once the whole process tree is stopped
// or its stop is known to be unconfirmed. Timeout and cancellation kill the tree
// without relying on any cooperative Stop RPC.
func spawn(ctx context.Context, dir string, argv, env []string, deadline time.Time, maxStderr int64) (execution.Invocation, []byte, error) {
	var obs execution.Invocation
	logPath := filepath.Join(dir, "stderr.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return obs, nil, err
	}
	defer logFile.Close()
	tree, err := startTree(dir, argv, env, logFile)
	if err != nil {
		return obs, nil, errcode.Wrap(errcode.UnsupportedCapability, "one-shot entry could not start", err)
	}
	obs.Dispatched = true
	done := make(chan waitResult, 1)
	go func() { done <- tree.wait() }()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var res waitResult
	select {
	case res = <-done:
	case <-timer.C:
		obs.TimedOut = true
	case <-ctx.Done():
		obs.Cancelled = true
	}
	if obs.TimedOut || obs.Cancelled {
		_ = tree.kill()
		select {
		case res = <-done:
		case <-time.After(stopGrace):
			// Stop is unknown: keep the observation unresolved for reconciliation.
			tree.release()
			return obs, readTail(logPath, maxStderr), nil
		}
	}
	// Reap descendants that outlived a normal exit as well.
	_ = tree.kill()
	tree.release()
	obs.StopConfirmed = true
	obs.Exited = res.exited && !obs.TimedOut && !obs.Cancelled
	obs.ExitCode = res.code
	return obs, readTail(logPath, maxStderr), nil
}

type waitResult struct {
	exited bool
	code   int
}

func readTail(p string, limit int64) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	if info.Size() > limit {
		if _, err = f.Seek(info.Size()-limit, io.SeekStart); err != nil {
			return nil
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, limit))
	return b
}

func copyPackage(src fs.FS, m Manifest, dst, entry string) error {
	if src == nil {
		return failure(errcode.SchemaInvalid, "package_files_required")
	}
	for _, f := range m.Files {
		if err := pathrule.Check(f.Path); err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(target), hostDirMode); err != nil {
			return err
		}
		mode := os.FileMode(0o400)
		if f.Path == entry {
			mode = 0o500
		}
		if err := copyVerified(src, f, target, mode); err != nil {
			return err
		}
	}
	return nil
}

// copyVerified hashes while copying so the executed bytes are exactly the
// manifest bytes; replacing the package store after verification cannot swap them.
func copyVerified(src fs.FS, f File, target string, mode os.FileMode) error {
	in, err := src.Open(f.Path)
	if err != nil {
		return failure(errcode.HashMismatch, "package_file_missing")
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := digest.NewHasher()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, f.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != f.Size || h.Hex() != f.SHA256 {
		return failure(errcode.HashMismatch, "entry_or_package_digest_mismatch")
	}
	return os.Chmod(target, mode)
}

func writeInputs(dir string, inputs []InputFile, limit int64) error {
	total := int64(0)
	seen := map[string]bool{}
	for _, in := range inputs {
		if err := pathrule.Check(in.Name); err != nil || strings.Contains(in.Name, "/") || seen[strings.ToLower(in.Name)] {
			return failure(errcode.SchemaInvalid, "input_name_invalid")
		}
		seen[strings.ToLower(in.Name)] = true
		target := filepath.Join(dir, in.Name)
		if in.Path != "" {
			n, err := copyInput(in.Path, target, limit-total, in.SHA256)
			if err != nil {
				return err
			}
			total += n
		} else {
			total += int64(len(in.Data))
			if total > limit {
				return failure(errcode.QuotaExceeded, "input_limit_exceeded")
			}
			if err := os.WriteFile(target, in.Data, 0o400); err != nil {
				return err
			}
		}
	}
	return os.Chmod(dir, 0o500)
}

func copyInput(src, target string, remaining int64, want string) (int64, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, failure(errcode.SchemaInvalid, "input_not_regular_file")
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	h := digest.NewHasher()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, remaining+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if n > remaining {
		return n, failure(errcode.QuotaExceeded, "input_limit_exceeded")
	}
	if want != "" && h.Hex() != want {
		return n, failure(errcode.HashMismatch, "input_changed_after_hashing")
	}
	return n, os.Chmod(target, 0o400)
}

// collectOutputs rejects symlinks, non-regular files, traversal, quota overflow
// and any output file not declared with the same digest in result.json.
func collectOutputs(dir string, spec RunSpec) ([]byte, []File, string) {
	actual := []File{}
	var result []byte
	total := int64(0)
	failureReason := ""
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == dir {
			return nil
		}
		rel, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			failureReason = "output_symlink"
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			failureReason = "output_not_regular"
			return fs.SkipAll
		}
		if pathrule.Check(rel) != nil {
			failureReason = "output_path_invalid"
			return fs.SkipAll
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if rel == resultName {
			if info.Size() > spec.MaxResultBytes {
				failureReason = "result_too_large"
				return fs.SkipAll
			}
			b, e := readBounded(p, spec.MaxResultBytes)
			if e != nil {
				return e
			}
			result = b
			return nil
		}
		total += info.Size()
		if int64(len(actual)+1) > spec.MaxOutputFiles || total > spec.MaxOutputBytes {
			failureReason = "output_limit_exceeded"
			return fs.SkipAll
		}
		h, n, e := hashFile(p)
		if e != nil {
			return e
		}
		actual = append(actual, File{Path: rel, SHA256: h, Size: n})
		return nil
	})
	if err != nil {
		return nil, nil, "output_unreadable"
	}
	if failureReason != "" {
		return nil, nil, failureReason
	}
	if result == nil {
		return nil, nil, "result_missing"
	}
	var declared struct {
		Files []File `json:"files"`
	}
	if json.Unmarshal(result, &declared) != nil {
		return nil, nil, "result_malformed"
	}
	cmp := func(a, b File) int { return strings.Compare(a.Path, b.Path) }
	slices.SortFunc(actual, cmp)
	slices.SortFunc(declared.Files, cmp)
	if !slices.Equal(actual, declared.Files) || pathrule.CheckSet(pathsOf(actual)) != nil {
		return nil, nil, outputVerificationErr
	}
	return result, actual, ""
}

func pathsOf(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func readBounded(p string, limit int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("extensions: bounded read exceeded")
	}
	return b, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := digest.NewHasher()
	n, err := io.Copy(h, f)
	return h.Hex(), n, err
}

// deliverOutputs copies verified files into an empty, caller-chosen directory
// and re-verifies each copy before the caller reports success.
func deliverOutputs(src, dst string, files []File) error {
	info, err := os.Lstat(dst)
	if err != nil || !info.IsDir() {
		return errors.New("extensions: output directory must exist")
	}
	entries, err := os.ReadDir(dst)
	if err != nil || len(entries) != 0 {
		return errors.New("extensions: output directory must be empty")
	}
	for _, f := range files {
		target := filepath.Join(dst, filepath.FromSlash(f.Path))
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err = copyVerified(os.DirFS(src), f, target, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// removeRunDir restores owner permissions before removal; read-only inputs and
// package copies must not survive a failed cleanup as executable residue.
func removeRunDir(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, e error) error {
		if e == nil && d.Type()&fs.ModeSymlink == 0 {
			if d.IsDir() {
				_ = os.Chmod(p, hostDirMode)
			} else {
				_ = os.Chmod(p, 0o600)
			}
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
