// Package extension is the minimal Lantai one-shot plugin SDK for Go.
//
// A host starts the registered package entry with exactly one argument, the
// absolute path of job.json, inside a private run directory:
//
//	job.json   fixed job document (lantai.processor-input/v1 or lantai.cli-command-input/v1)
//	in/        read-only copies of granted inputs
//	out/       candidate files plus out/result.json
//
// The SDK only reads that job, exposes the inputs and writes a result whose
// file list exactly matches out/. It carries no Lantai credentials, cannot
// approve or commit anything, and is not a sandbox. It depends only on the Go
// standard library so plugin authors can vendor it.
package extension

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	ProcessorInputContract  = "lantai.processor-input/v1"
	ProcessorResultContract = "lantai.processor-result/v1"
	CommandInputContract    = "lantai.cli-command-input/v1"
	CommandResultContract   = "lantai.cli-command-result/v1"
	CheckResultContract     = "lantai.check-result/v1"
	maxJobBytes             = 64 << 10
	maxResultBytes          = 1 << 20
)

// Producer is copied from the job; a plugin must echo it unchanged.
type Producer struct {
	ExtensionID       string `json:"extension_id"`
	ExtensionVersion  string `json:"extension_version"`
	PackageDigest     string `json:"package_digest"`
	Source            string `json:"source"`
	ContributionID    string `json:"contribution_id,omitempty"`
	CoreReleaseDigest string `json:"core_release_digest,omitempty"`
}

type Ref struct {
	InstanceID string `json:"instance_id"`
	AssetID    string `json:"asset_id"`
	VersionID  string `json:"version_id"`
}

// ProcessorJob is lantai.processor-input/v1. Mode "probe" is a host-authorized
// synthetic call without business inputs; answer it without side effects.
type ProcessorJob struct {
	Contract    string          `json:"contract"`
	OperationID string          `json:"operation_id"`
	Fence       json.RawMessage `json:"job_fence"`
	InputRefs   []Ref           `json:"input_refs"`
	InputDigest string          `json:"input_digest"`
	Deadline    string          `json:"deadline"`
	Activation  json.RawMessage `json:"activation_snapshot,omitempty"`
	Producer    *Producer       `json:"producer,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	Mode        string          `json:"mode,omitempty"`
}

// CommandInput is one explicitly selected local file, copied read-only into in/.
type CommandInput struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// CommandJob is lantai.cli-command-input/v1.
type CommandJob struct {
	Contract    string          `json:"contract"`
	OperationID string          `json:"operation_id"`
	Command     string          `json:"command"`
	Args        []string        `json:"args"`
	Inputs      []CommandInput  `json:"inputs"`
	Deadline    string          `json:"deadline"`
	Producer    Producer        `json:"producer"`
	Config      json.RawMessage `json:"config,omitempty"`
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Check is lantai.check-result/v1. A legal "fail" is business evidence; crash
// or exit non-zero only for runtime faults, never to express a failing check.
type Check struct {
	Contract       string   `json:"contract"`
	Ref            Ref      `json:"ref"`
	ManifestDigest string   `json:"manifest_digest"`
	Verdict        string   `json:"verdict"`
	Findings       []string `json:"findings"`
	Producer       Producer `json:"producer"`
}

type Record struct {
	Schema  string         `json:"schema"`
	Payload map[string]any `json:"payload"`
}

type ProcessorResult struct {
	Contract    string   `json:"contract"`
	OperationID string   `json:"operation_id"`
	Status      string   `json:"status"`
	Files       []File   `json:"files"`
	Records     []Record `json:"records"`
	Checks      []Check  `json:"checks"`
	Producer    Producer `json:"producer"`
}

type CommandResult struct {
	Contract    string         `json:"contract"`
	OperationID string         `json:"operation_id"`
	Command     string         `json:"command"`
	Status      string         `json:"status"`
	Files       []File         `json:"files"`
	Summary     map[string]any `json:"summary"`
	Producer    Producer       `json:"producer"`
}

// Run is one invocation's private directory.
type Run struct {
	dir string
	raw []byte
}

// Open reads the job named by os.Args-style arguments (args[1]).
func Open(args []string) (*Run, error) {
	if len(args) != 2 || !filepath.IsAbs(args[1]) || filepath.Base(args[1]) != "job.json" {
		return nil, errors.New("extension: expected the absolute job.json path as the only argument")
	}
	f, err := os.Open(args[1])
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxJobBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxJobBytes {
		return nil, errors.New("extension: job document too large")
	}
	return &Run{dir: filepath.Dir(args[1]), raw: raw}, nil
}

// Contract reports which job document the host supplied.
func (r *Run) Contract() string {
	var head struct {
		Contract string `json:"contract"`
	}
	_ = json.Unmarshal(r.raw, &head)
	return head.Contract
}

func (r *Run) Processor() (ProcessorJob, error) {
	var j ProcessorJob
	if err := json.Unmarshal(r.raw, &j); err != nil {
		return j, err
	}
	if j.Contract != ProcessorInputContract {
		return j, fmt.Errorf("extension: job contract %q is not %s", j.Contract, ProcessorInputContract)
	}
	return j, nil
}

func (r *Run) Command() (CommandJob, error) {
	var j CommandJob
	if err := json.Unmarshal(r.raw, &j); err != nil {
		return j, err
	}
	if j.Contract != CommandInputContract {
		return j, fmt.Errorf("extension: job contract %q is not %s", j.Contract, CommandInputContract)
	}
	return j, nil
}

// Input reads a granted input copy, bounded by limit bytes.
func (r *Run) Input(name string, limit int64) ([]byte, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, errors.New("extension: input name must be a single file name")
	}
	f, err := os.Open(filepath.Join(r.dir, "in", name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("extension: input exceeds limit")
	}
	return b, nil
}

// Output creates parent directories for a portable relative path inside out/.
func (r *Run) Output(rel string) (string, error) {
	if rel == "" || path.IsAbs(rel) || strings.Contains(rel, `\`) || path.Clean(rel) != rel || strings.HasPrefix(rel, "../") || rel == ".." || rel == "result.json" {
		return "", errors.New("extension: output path must be relative, clean and not result.json")
	}
	p := filepath.Join(r.dir, "out", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	return p, nil
}

// Files hashes every file currently under out/ (except result.json).
func (r *Run) Files() ([]File, error) {
	root := filepath.Join(r.dir, "out")
	out := []File{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("extension: %s is not a regular file", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "result.json" {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		out = append(out, File{Path: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// FinishProcessor fills Files from out/ and writes out/result.json.
func (r *Run) FinishProcessor(res ProcessorResult) error {
	res.Contract = ProcessorResultContract
	if res.Records == nil {
		res.Records = []Record{}
	}
	if res.Checks == nil {
		res.Checks = []Check{}
	}
	files, err := r.Files()
	if err != nil {
		return err
	}
	res.Files = files
	return r.writeResult(res)
}

// FinishCommand fills Files from out/ and writes out/result.json.
func (r *Run) FinishCommand(res CommandResult) error {
	res.Contract = CommandResultContract
	if res.Summary == nil {
		res.Summary = map[string]any{}
	}
	files, err := r.Files()
	if err != nil {
		return err
	}
	res.Files = files
	return r.writeResult(res)
}

func (r *Run) writeResult(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxResultBytes {
		return errors.New("extension: result exceeds 1 MiB")
	}
	tmp := filepath.Join(r.dir, "result.json.partial")
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.dir, "out", "result.json"))
}

// Main runs fn and maps an error to exit status 1 (a runtime fault). Report a
// failing check through the result instead of returning an error.
func Main(fn func(*Run) error) {
	r, err := Open(os.Args)
	if err == nil {
		err = fn(r)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
