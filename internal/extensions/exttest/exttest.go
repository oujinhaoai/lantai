// Package exttest builds the synthetic one-shot fixture and writes digest-exact
// external packages for tests. It is test support only; production code never
// builds, discovers or trusts packages this way.
package exttest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/extensions"
)

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
	buildLog  []byte
)

// Fixture returns the path of the built tests/fixtures/public/oneshot program.
func Fixture(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lantai-oneshot-fixture-")
		if err != nil {
			buildErr = err
			return
		}
		root, err := moduleRoot()
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(dir, "oneshot"+exe())
		// The go command on PATH selects the go.mod toolchain itself.
		goBin, err := exec.LookPath("go")
		if err != nil {
			buildErr = err
			return
		}
		cmd := exec.Command(goBin, "build", "-o", buildPath, "./tests/fixtures/public/oneshot")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		buildLog, buildErr = cmd.CombinedOutput()
	})
	if buildErr != nil {
		t.Fatalf("build fixture: %v\n%s", buildErr, buildLog)
	}
	return buildPath
}

func exe() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// Spec selects package identity and targets.
type Spec struct {
	ID      string
	Version string
	Server  bool
	CLI     bool
	// Timeout caps each invocation; defaults to 30 seconds.
	TimeoutSeconds int64
	MaxOutputFiles int64
	MaxOutputBytes int64
	// Mutate may edit the manifest document before digests are frozen.
	Mutate func(map[string]any)
}

// EntryPath is the package-relative entry for this platform.
func EntryPath() string { return "bin/oneshot" + exe() }

// Write creates a package directory whose manifest lists exact file digests.
func Write(t testing.TB, dir string, s Spec) extensions.Package {
	t.Helper()
	if s.ID == "" {
		s.ID = "org.example.fixture"
	}
	if s.Version == "" {
		s.Version = "0.1.0"
	}
	if s.TimeoutSeconds == 0 {
		s.TimeoutSeconds = 30
	}
	if s.MaxOutputBytes == 0 {
		s.MaxOutputBytes = 1 << 20
	}
	if s.MaxOutputFiles == 0 && s.CLI {
		s.MaxOutputFiles = 16
	}
	files := map[string][]byte{
		"LICENSE":            []byte("Synthetic test fixture. Not for distribution.\n"),
		"config.schema.json": []byte(`{"type":"object","additionalProperties":false,"properties":{"mode":{"type":"string"},"sleep_ms":{"type":"integer","minimum":0},"heartbeat":{"type":"string"}}}`),
	}
	bin, err := os.ReadFile(Fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	files[EntryPath()] = bin
	listed := []map[string]any{}
	names := []string{}
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sum := sha256.Sum256(files[name])
		listed = append(listed, map[string]any{"path": name, "sha256": hex.EncodeToString(sum[:]), "size": len(files[name])})
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(p, files[name], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(bin)
	processor := map[string]any{"accepts": map[string]any{"extensions": []string{"json"}, "asset_types": []string{}}, "requires": []string{}, "timeout": "30s", "concurrency": 1, "produces": map[string]any{"records": []string{}, "files": s.CLI}}
	target := map[string]any{"runtime": "exec", "lifecycle": "oneshot", "entry": EntryPath(), "protocol": "lantai.processor/v1", "processor": processor}
	contributes := []map[string]any{}
	points := []map[string]any{}
	targets := map[string]any{}
	if s.Server {
		contributes = append(contributes, map[string]any{"id": s.ID + ".check", "point": "asset.validator", "target": "server", "input_schema": "lantai.check-input/v1", "output_schema": "lantai.check-result/v1"})
		points = append(points, map[string]any{"id": "asset.validator", "version": 1})
		targets["server"] = target
	}
	if s.CLI {
		contributes = append(contributes, map[string]any{"id": s.ID + ".copy", "point": "cli.command", "target": "cli", "input_schema": "lantai.cli-command-input/v1", "output_schema": "lantai.cli-command-result/v1"})
		points = append(points, map[string]any{"id": "cli.command", "version": 1})
		targets["cli"] = target
	}
	m := map[string]any{
		"contract": "lantai.extension/v1", "id": s.ID, "version": s.Version,
		"compatibility": map[string]any{"host_api": ">=1.0.0 <2.0.0", "required_points": points},
		"contributes":   contributes, "targets": targets,
		"platform_artifacts": []map[string]any{{"platform": extensions.Platform(), "path": EntryPath(), "sha256": hex.EncodeToString(sum[:]), "size": len(bin)}},
		"files":              listed,
		"permissions":        map[string]any{"api": []string{}, "network": []string{}, "filesystem": []string{"granted-input:read", "run-output:write"}, "secret_refs": []string{}},
		"resource_limits":    map[string]any{"max_input_bytes": 64 << 20, "max_output_bytes": s.MaxOutputBytes, "max_output_files": s.MaxOutputFiles, "memory_bytes": 256 << 20, "cpu_seconds": 30, "timeout_seconds": s.TimeoutSeconds},
		"dependencies":       []string{}, "config_schema": "config.schema.json",
		"license": map[string]any{"spdx": "LicenseRef-Synthetic", "files": []string{"LICENSE"}},
	}
	if s.Mutate != nil {
		s.Mutate(m)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "extension.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := extensions.ReadPackage(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// CopyFile is a small helper for tests that replace package bytes.
func CopyFile(t testing.TB, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err = out.Close(); err != nil {
		t.Fatal(err)
	}
}
