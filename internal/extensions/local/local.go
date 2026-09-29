// Package local is the client-side host for explicitly installed cli.command
// contributions (lantai ext). It keeps a private registry of verified package
// directories, re-verifies every package and entry digest before a run, asks
// the server whether the exact package is still enabled for the caller, and
// runs the entry once under the file protocol. It never searches PATH or the
// working directory, never passes the session token or a HumanGrant to the
// child, and is local trust, not a sandbox.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/storage"
)

const RegistrySchema = "lantai.local-extensions/v1"

// Registry is the private local install list. It is bound to one server
// origin; an entry is trust in a directory's exact bytes, not a permission.
type Registry struct {
	Schema  string  `json:"schema"`
	Origin  string  `json:"origin"`
	Entries []Entry `json:"entries"`
}

type Entry struct {
	ExtensionID      string        `json:"extension_id"`
	ExtensionVersion string        `json:"extension_version"`
	PackageDigest    digest.Digest `json:"package_digest"`
	PackageDir       string        `json:"package_dir"`
	Platform         string        `json:"platform"`
	Entry            string        `json:"entry"`
	EntryDigest      digest.Digest `json:"entry_digest"`
	Commands         []string      `json:"commands"`
	InstalledAt      string        `json:"installed_at"`
}

// Command mirrors GET /api/v1/extensions/commands.
type Command struct {
	EnablementID     ids.ID           `json:"enablement_id"`
	Generation       int64            `json:"generation"`
	ExtensionID      string           `json:"extension_id"`
	ExtensionVersion string           `json:"extension_version"`
	PackageDigest    digest.Digest    `json:"package_digest"`
	ContributionID   string           `json:"contribution_id"`
	Command          string           `json:"command"`
	Entry            string           `json:"entry"`
	EntryDigest      digest.Digest    `json:"entry_digest"`
	Config           json.RawMessage  `json:"config"`
	ConfigDigest     digest.Digest    `json:"config_digest"`
	Producer         storage.Producer `json:"producer"`
}

func failure(code errcode.Code, reason string) error {
	return errcode.New(code, "").WithDetails(errcode.Detail{Reason: reason})
}

// Load reads a registry; a missing file is an empty registry for this origin.
func Load(path, origin string) (Registry, error) {
	r := Registry{Schema: RegistrySchema, Origin: origin, Entries: []Entry{}}
	if path == "" || !filepath.IsAbs(path) {
		return r, failure(errcode.SchemaInvalid, "absolute_registry_path_required")
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err := client.LoadState(path, &r); err != nil {
		return r, err
	}
	if r.Schema != RegistrySchema || r.Origin != origin {
		return r, failure(errcode.SchemaInvalid, "registry_belongs_to_another_server")
	}
	return r, nil
}

// Enabled fetches the commands the server currently enables for this caller.
func Enabled(ctx context.Context, c *client.Client) ([]Command, error) {
	r, err := c.Do(ctx, http.MethodGet, "/api/v1/extensions/commands", nil, client.Options{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []Command `json:"items"`
	}
	return out.Items, r.Decode(&out)
}

// verify re-reads the package directory and returns its verified identity.
func verify(dir string) (extensions.Package, extensions.File, error) {
	p, err := extensions.ReadPackage(os.DirFS(dir))
	if err != nil {
		return p, extensions.File{}, err
	}
	if err = extensions.CheckExternal(p.Manifest); err != nil {
		return p, extensions.File{}, err
	}
	entry, err := extensions.EntryPath(p.Manifest, "cli")
	return p, entry, err
}

// Install records an explicit local trust decision for one package directory
// whose exact digest the server currently enables for the caller as a CLI
// target. Nothing is executed; there is no postinstall.
func Install(ctx context.Context, c *client.Client, registryPath, packageDir string) (Entry, error) {
	var out Entry
	abs, err := filepath.Abs(packageDir)
	if err != nil {
		return out, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return out, err
	}
	p, entry, err := verify(resolved)
	if err != nil {
		return out, err
	}
	enabled, err := Enabled(ctx, c)
	if err != nil {
		return out, err
	}
	out = Entry{ExtensionID: p.Manifest.ID, ExtensionVersion: p.Manifest.Version, PackageDigest: p.Digest, PackageDir: resolved, Platform: extensions.Platform(), Entry: entry.Path, EntryDigest: digest.Digest("sha256:" + entry.SHA256), Commands: []string{}, InstalledAt: clock.Format(time.Now())}
	for _, cmd := range enabled {
		if cmd.PackageDigest == p.Digest && cmd.ExtensionID == p.Manifest.ID {
			if cmd.EntryDigest != out.EntryDigest {
				return Entry{}, failure(errcode.HashMismatch, "server_enabled_other_platform_entry")
			}
			out.Commands = append(out.Commands, cmd.ContributionID)
		}
	}
	if len(out.Commands) == 0 {
		return Entry{}, failure(errcode.ExtensionActivationStale, "package_not_enabled_for_caller")
	}
	slices.Sort(out.Commands)
	reg, err := Load(registryPath, c.Origin())
	if err != nil {
		return Entry{}, err
	}
	reg.Entries = slices.DeleteFunc(reg.Entries, func(e Entry) bool { return e.ExtensionID == out.ExtensionID })
	reg.Entries = append(reg.Entries, out)
	return out, client.SaveState(registryPath, reg)
}

// Remove forgets a local install; server enablement and history are untouched.
func Remove(registryPath, origin, extension string) error {
	reg, err := Load(registryPath, origin)
	if err != nil {
		return err
	}
	before := len(reg.Entries)
	reg.Entries = slices.DeleteFunc(reg.Entries, func(e Entry) bool { return e.ExtensionID == extension })
	if len(reg.Entries) == before {
		return errcode.New(errcode.NotFound, "")
	}
	return client.SaveState(registryPath, reg)
}

// RunRequest selects an installed command, explicit input files, verbatim
// arguments (never a shell string) and an existing empty output directory.
type RunRequest struct {
	ExtensionID string
	Command     string
	Args        []string
	Inputs      []string
	Output      string
}

type RunResult struct {
	OperationID ids.ID                      `json:"operation_id"`
	Command     string                      `json:"command"`
	Outcome     execution.InvocationOutcome `json:"outcome"`
	Status      string                      `json:"status,omitempty"`
	Files       []extensions.File           `json:"files"`
	Summary     map[string]any              `json:"summary,omitempty"`
	Failure     string                      `json:"failure,omitempty"`
	Producer    storage.Producer            `json:"producer"`
}

// Run executes one installed command once. The package bytes and entry must
// still match the install, and the server must still enable this exact
// package for the caller; otherwise nothing is spawned.
func Run(ctx context.Context, c *client.Client, registryPath string, in RunRequest) (RunResult, error) {
	var out RunResult
	reg, err := Load(registryPath, c.Origin())
	if err != nil {
		return out, err
	}
	i := slices.IndexFunc(reg.Entries, func(e Entry) bool { return e.ExtensionID == in.ExtensionID })
	if i < 0 {
		return out, failure(errcode.NotFound, "extension_not_installed")
	}
	e := reg.Entries[i]
	contribution := e.ExtensionID + "." + in.Command
	if !slices.Contains(e.Commands, contribution) {
		return out, failure(errcode.NotFound, "command_not_installed")
	}
	p, entry, err := verify(e.PackageDir)
	if err != nil {
		return out, err
	}
	if p.Digest != e.PackageDigest || digest.Digest("sha256:"+entry.SHA256) != e.EntryDigest || e.Platform != extensions.Platform() {
		return out, failure(errcode.HashMismatch, "installed_package_or_entry_changed")
	}
	enabled, err := Enabled(ctx, c)
	if err != nil {
		return out, err
	}
	j := slices.IndexFunc(enabled, func(x Command) bool {
		return x.ContributionID == contribution && x.PackageDigest == e.PackageDigest && x.EntryDigest == e.EntryDigest
	})
	if j < 0 {
		return out, failure(errcode.ExtensionActivationStale, "command_no_longer_enabled")
	}
	cmd := enabled[j]
	if len(in.Args) > 64 || len(in.Inputs) > 1000 || in.Output == "" || !filepath.IsAbs(in.Output) {
		return out, failure(errcode.SchemaInvalid, "run_request_invalid")
	}
	files := []extensions.InputFile{}
	inputs := []map[string]any{}
	seen := map[string]bool{}
	for _, path := range in.Inputs {
		name := filepath.Base(path)
		if seen[name] || !filepath.IsAbs(path) {
			return out, failure(errcode.SchemaInvalid, "input_names_must_be_unique_absolute_files")
		}
		seen[name] = true
		sum, size, err := client.HashFile(ctx, path)
		if err != nil {
			return out, err
		}
		files = append(files, extensions.InputFile{Name: name, Path: path, SHA256: sum})
		inputs = append(inputs, map[string]any{"name": name, "sha256": sum, "size": size})
	}
	limits := p.Manifest.ResourceLimits
	deadline := time.Now().Add(time.Duration(limits.TimeoutSeconds) * time.Second)
	out.OperationID = ids.New()
	args := in.Args
	if args == nil {
		args = []string{}
	}
	job := map[string]any{"contract": "lantai.cli-command-input/v1", "operation_id": out.OperationID, "command": contribution, "args": args, "inputs": inputs, "deadline": clock.Format(deadline), "producer": cmd.Producer, "config": cmd.Config}
	raw, err := json.Marshal(job)
	if err != nil {
		return out, err
	}
	reg2, err := schema.Default()
	if err != nil {
		return out, err
	}
	if err = reg2.ValidateJSON("lantai.cli-command-input/v1", raw); err != nil {
		return out, errcode.Wrap(errcode.SchemaInvalid, "command input is invalid", err)
	}
	var r struct {
		OperationID ids.ID           `json:"operation_id"`
		Command     string           `json:"command"`
		Status      string           `json:"status"`
		Summary     map[string]any   `json:"summary"`
		Producer    storage.Producer `json:"producer"`
	}
	// The result is validated before any output reaches the caller's directory.
	check := func(b []byte) error {
		if err := reg2.ValidateJSON("lantai.cli-command-result/v1", b); err != nil {
			return err
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if r.OperationID != out.OperationID || r.Command != contribution || r.Producer != cmd.Producer {
			return errors.New("result identity mismatch")
		}
		return nil
	}
	res, err := extensions.RunOneShot(ctx, extensions.RunSpec{Package: p, Files: os.DirFS(e.PackageDir), Target: "cli", Job: raw, Inputs: files, Deadline: deadline, MaxInputBytes: limits.MaxInputBytes, MaxOutputBytes: limits.MaxOutputBytes, MaxOutputFiles: limits.MaxOutputFiles, OutputDir: in.Output, ValidateResult: check})
	out.Command, out.Producer, out.Files, out.Failure = contribution, cmd.Producer, []extensions.File{}, res.Failure
	if err != nil {
		return out, err
	}
	if res.Observation.ResultValid {
		out.Status, out.Summary, out.Files = r.Status, r.Summary, res.Files
	}
	out.Outcome = execution.ClassifyInvocation(res.Observation)
	if out.Outcome != execution.InvocationCompleted && out.Failure == "" {
		out.Failure = string(out.Outcome)
	}
	return out, nil
}
