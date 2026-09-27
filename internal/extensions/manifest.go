// Package extensions 实现 M1 统一包清单与官方静态登记，不加载外部代码。
package extensions

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
)

const Contract = "lantai.extension/v1"
const HostAPI = "1.0.0"
const MaxPackageBytes int64 = 32 << 20

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Artifact struct {
	Platform string `json:"platform"`
	File
}
type Contribution struct {
	ID           string `json:"id"`
	Point        string `json:"point"`
	Target       string `json:"target"`
	InputSchema  string `json:"input_schema"`
	OutputSchema string `json:"output_schema"`
}
type Requirement struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}
type Processor struct {
	Accepts struct {
		Extensions []string `json:"extensions"`
		AssetTypes []string `json:"asset_types"`
	} `json:"accepts"`
	Requires    []string `json:"requires"`
	Timeout     string   `json:"timeout"`
	Concurrency int      `json:"concurrency"`
	Produces    struct {
		Records []string `json:"records"`
		Files   bool     `json:"files"`
	} `json:"produces"`
}
type Target struct {
	Runtime   string    `json:"runtime"`
	Lifecycle string    `json:"lifecycle"`
	Entry     string    `json:"entry"`
	Protocol  string    `json:"protocol"`
	Processor Processor `json:"processor"`
}
type Manifest struct {
	Contract      string `json:"contract"`
	ID            string `json:"id"`
	Version       string `json:"version"`
	Compatibility struct {
		HostAPI        string        `json:"host_api"`
		RequiredPoints []Requirement `json:"required_points"`
	} `json:"compatibility"`
	Contributes       []Contribution    `json:"contributes"`
	Targets           map[string]Target `json:"targets"`
	PlatformArtifacts []Artifact        `json:"platform_artifacts"`
	Files             []File            `json:"files"`
	Permissions       struct {
		API        []string `json:"api"`
		Network    []string `json:"network"`
		Filesystem []string `json:"filesystem"`
		SecretRefs []string `json:"secret_refs"`
	} `json:"permissions"`
	ResourceLimits struct {
		MaxInputBytes  int64 `json:"max_input_bytes"`
		MaxOutputBytes int64 `json:"max_output_bytes"`
		MaxOutputFiles int64 `json:"max_output_files"`
		MemoryBytes    int64 `json:"memory_bytes"`
		CPUSeconds     int64 `json:"cpu_seconds"`
		TimeoutSeconds int64 `json:"timeout_seconds"`
	} `json:"resource_limits"`
	Dependencies []struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	} `json:"dependencies"`
	ConfigSchema string `json:"config_schema"`
	License      struct {
		SPDX  string   `json:"spdx"`
		Files []string `json:"files"`
	} `json:"license"`
}

// Point fixes the core owner and input/output contracts. Supported means static
// M1 registration, never runtime activation or permission to execute.
type Point struct {
	ID           string `json:"id"`
	Version      int    `json:"version"`
	Owner        string `json:"owner"`
	InputSchema  string `json:"input_schema,omitempty"`
	OutputSchema string `json:"output_schema,omitempty"`
	Supported    bool   `json:"m1_registration"`
}

func Points() []Point {
	return []Point{
		{"asset.validator", 1, "ledger", "lantai.check-input/v1", "lantai.check-result/v1", true},
		{"artifact.processor", 1, "jobs", "lantai.processor-input/v1", "lantai.processor-result/v1", true},
		{"asset.type", 1, "catalog", "", "", false}, {"metadata.extractor", 1, "provenance", "", "", false}, {"ingest.importer", 1, "catalog", "", "", false}, {"export.connector", 1, "ledger", "", "", false}, {"search.provider", 1, "query", "", "", false}, {"workflow.template", 1, "workflow", "", "", false}, {"agent.backend", 1, "agent_execution", "", "", false}, {"notification.channel", 1, "events", "", "", false}, {"event.consumer", 1, "events", "", "", false}, {"service.api", 1, "extensions", "", "", false}, {"storage.driver", 1, "storage", "", "", false}, {"file.install", 1, "storage", "", "", false}, {"db.driver", 1, "operations", "", "", false},
		{"ui.asset.preview", 1, "web", "", "", false}, {"ui.asset.inspector", 1, "web", "", "", false}, {"ui.asset.action", 1, "web", "", "", false}, {"ui.task.result", 1, "web", "", "", false}, {"ui.navigation", 1, "web", "", "", false}, {"ui.page", 1, "web", "", "", false}, {"ui.settings", 1, "web", "", "", false}, {"ui.dashboard", 1, "web", "", "", false}, {"ui.annotation", 1, "web", "", "", false},
	}
}
func failure(code errcode.Code, reason string) error {
	return errcode.New(code, "").WithDetails(errcode.Detail{Reason: reason})
}
func validate(contract string, v any) error {
	r, e := schema.Default()
	if e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if e = r.ValidateJSON(contract, b); e != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "extension contract is invalid", e)
	}
	return nil
}
func Parse(raw []byte) (Manifest, error) {
	var m Manifest
	if int64(len(raw)) > MaxPackageBytes {
		return m, failure(errcode.QuotaExceeded, "manifest_too_large")
	}
	b, e := yamljson.ToJSON(raw)
	if e != nil {
		return m, errcode.Wrap(errcode.SchemaInvalid, "extension manifest is invalid", e)
	}
	r, e := schema.Default()
	if e != nil {
		return m, e
	}
	if e = r.ValidateJSON(Contract, b); e != nil {
		return m, errcode.Wrap(errcode.SchemaInvalid, "extension manifest does not satisfy v1", e)
	}
	e = json.Unmarshal(b, &m)
	return m, e
}

// HostTool is a trusted administrator-owned capability declaration, not a plugin
// dependency and not a PATH lookup. M1 only checks metadata; it never probes it.
type HostTool struct {
	ID, Version, Path string
	Digest            digest.Digest
}

func CheckM1(m Manifest, tools []HostTool) error {
	if err := validate(Contract, m); err != nil {
		return err
	}
	if m.Compatibility.HostAPI != "1.0.0" && m.Compatibility.HostAPI != ">=1.0.0 <2.0.0" {
		return failure(errcode.ExtensionPointUnsupported, "host_api_incompatible")
	}
	if len(m.Dependencies) > 0 {
		return failure(errcode.ExtensionPointUnsupported, "plugin_dependencies_unsupported")
	}
	points := map[string]Point{}
	for _, p := range Points() {
		points[p.ID] = p
	}
	required := map[string]bool{}
	for _, r := range m.Compatibility.RequiredPoints {
		p, ok := points[r.ID]
		if !ok || !p.Supported || p.Version != r.Version {
			return failure(errcode.ExtensionPointUnsupported, "point_version_unsupported")
		}
		if required[r.ID] {
			return failure(errcode.IdempotencyConflict, "duplicate_required_point")
		}
		required[r.ID] = true
	}
	knownTools := map[string]bool{}
	for _, t := range tools {
		if t.ID == "" || t.Version == "" || !filepath.IsAbs(t.Path) || !t.Digest.Valid() || knownTools[t.ID] {
			return failure(errcode.SchemaInvalid, "invalid_host_tool")
		}
		knownTools[t.ID] = true
	}
	for name, t := range m.Targets {
		if name != "server" && name != "node" {
			return failure(errcode.ExtensionPointUnsupported, "target_reserved")
		}
		if t.Runtime != "builtin" || t.Lifecycle != "builtin" || t.Protocol != "lantai.processor/v1" {
			return failure(errcode.ExtensionPointUnsupported, "external_runtime_unsupported")
		}
		for _, need := range t.Processor.Requires {
			if !knownTools[need] {
				return failure(errcode.ExtensionPointUnsupported, "host_tool_unregistered")
			}
		}
		for _, s := range t.Processor.Produces.Records {
			r, _ := schema.Default()
			if err := r.Check(s); err != nil {
				return failure(errcode.ExtensionPointUnsupported, "output_schema_unregistered")
			}
		}
	}
	if len(m.Permissions.API) > 0 || len(m.Permissions.Network) > 0 || len(m.Permissions.SecretRefs) > 0 {
		return failure(errcode.ExtensionPointUnsupported, "builtin_permission_unsupported")
	}
	seen := map[string]bool{}
	usedTargets := map[string]bool{}
	for _, c := range m.Contributes {
		if seen[c.ID] {
			return failure(errcode.IdempotencyConflict, "duplicate_contribution_id")
		}
		seen[c.ID] = true
		if !strings.HasPrefix(c.ID, m.ID+".") {
			return failure(errcode.SchemaInvalid, "contribution_namespace_mismatch")
		}
		p, ok := points[c.Point]
		if !ok || !p.Supported {
			return failure(errcode.ExtensionPointUnsupported, "point_unsupported")
		}
		if c.InputSchema != p.InputSchema || c.OutputSchema != p.OutputSchema || !required[c.Point] {
			return failure(errcode.ExtensionPointUnsupported, "point_contract_mismatch")
		}
		if _, ok := m.Targets[c.Target]; !ok {
			return failure(errcode.SchemaInvalid, "contribution_target_missing")
		}
		usedTargets[c.Target] = true
	}
	if len(usedTargets) != len(m.Targets) {
		return failure(errcode.SchemaInvalid, "target_without_contribution")
	}
	return nil
}

// Package is verified immutable bytes, not an activation or executable handle.
type Package struct {
	Manifest       Manifest
	ManifestJSON   json.RawMessage
	ManifestDigest digest.Digest
	Digest         digest.Digest
}

func ReadPackage(fsys fs.FS) (Package, error) {
	var out Package
	count := 0
	err := fs.WalkDir(fsys, ".", func(p string, de fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if de.IsDir() {
			return nil
		}
		count++
		if count > 10001 {
			return failure(errcode.QuotaExceeded, "too_many_package_files")
		}
		if !de.Type().IsRegular() {
			return failure(errcode.SchemaInvalid, "package_non_regular_file")
		}
		return pathrule.Check(p)
	})
	if err != nil {
		return out, err
	}
	manifestFile, err := fsys.Open("extension.yaml")
	if err != nil {
		return out, failure(errcode.SchemaInvalid, "extension_manifest_required")
	}
	raw, err := io.ReadAll(io.LimitReader(manifestFile, MaxPackageBytes+1))
	manifestFile.Close()
	if err != nil {
		return out, failure(errcode.SchemaInvalid, "extension_manifest_required")
	}
	for _, name := range []string{"plugin.yaml", "module.yaml"} {
		if _, e := fs.Stat(fsys, name); e == nil {
			return out, failure(errcode.SchemaInvalid, "legacy_or_dual_manifest")
		}
	}
	m, err := Parse(raw)
	if err != nil {
		return out, err
	}
	expected := map[string]File{}
	paths := []string{"extension.yaml"}
	for _, f := range m.Files {
		if f.Path == "extension.yaml" || f.Path == "plugin.yaml" || f.Path == "module.yaml" || expected[f.Path].Path != "" {
			return out, failure(errcode.SchemaInvalid, "duplicate_or_recursive_package_file")
		}
		expected[f.Path] = f
		paths = append(paths, f.Path)
	}
	if err = pathrule.CheckSet(paths); err != nil {
		return out, err
	}
	actual := []File{}
	total := int64(0)
	err = fs.WalkDir(fsys, ".", func(p string, de fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if de.IsDir() {
			return nil
		}
		if !de.Type().IsRegular() {
			return failure(errcode.SchemaInvalid, "package_non_regular_file")
		}
		if p != "extension.yaml" && expected[p].Path == "" {
			return failure(errcode.SchemaInvalid, "undeclared_package_file")
		}
		f, e := fsys.Open(p)
		if e != nil {
			return e
		}
		h := digest.NewHasher()
		n, e := io.Copy(h, io.LimitReader(f, MaxPackageBytes-total+1))
		f.Close()
		if e != nil {
			return e
		}
		total += n
		if total > MaxPackageBytes {
			return failure(errcode.QuotaExceeded, "package_too_large")
		}
		entry := File{p, h.Hex(), n}
		if p != "extension.yaml" && entry != expected[p] {
			return failure(errcode.HashMismatch, "package_file_mismatch")
		}
		actual = append(actual, entry)
		return nil
	})
	if err != nil {
		return out, err
	}
	if len(actual) != len(expected)+1 {
		return out, failure(errcode.HashMismatch, "package_file_missing")
	}
	for _, a := range m.PlatformArtifacts {
		f, ok := expected[a.Path]
		if !ok || f != a.File {
			return out, failure(errcode.HashMismatch, "platform_artifact_mismatch")
		}
	}
	for _, t := range m.Targets {
		found := false
		for _, a := range m.PlatformArtifacts {
			if a.Path == t.Entry {
				found = true
			}
		}
		if !found {
			return out, failure(errcode.HashMismatch, "entry_not_in_platform_artifacts")
		}
	}
	if expected[m.ConfigSchema].Path == "" {
		return out, failure(errcode.SchemaInvalid, "config_schema_missing")
	}
	config, err := fs.ReadFile(fsys, m.ConfigSchema)
	if err != nil {
		return out, err
	}
	configDocument, err := canonjson.Decode(config)
	if err != nil {
		return out, failure(errcode.SchemaInvalid, "config_schema_invalid")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(noSchemaLoader{})
	if err = compiler.AddResource("https://lantai.invalid/package/config", configDocument); err != nil {
		return out, failure(errcode.SchemaInvalid, "config_schema_invalid")
	}
	if _, err = compiler.Compile("https://lantai.invalid/package/config"); err != nil {
		return out, failure(errcode.SchemaInvalid, "config_schema_invalid")
	}
	for _, p := range m.License.Files {
		if expected[p].Path == "" {
			return out, failure(errcode.SchemaInvalid, "license_evidence_missing")
		}
	}
	slices.SortFunc(actual, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	b, err := canonjson.CanonicalizeValue(actual)
	if err != nil {
		return out, err
	}
	canonical, err := canonjson.CanonicalizeValue(m)
	if err != nil {
		return out, err
	}
	return Package{m, canonical, digest.Of(canonical), digest.Of(b)}, nil
}

type noSchemaLoader struct{}

func (noSchemaLoader) Load(string) (any, error) {
	return nil, errors.New("extensions: config schema reference is not embedded")
}
