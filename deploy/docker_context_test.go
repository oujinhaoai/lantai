package deploy_test

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The deployment recipe copies whole source roots. Check that its COPY list
// and restrictive context allowlist cover the actual command dependency graph.
// This does not substitute for building and starting an image with Docker.
func TestDockerContextCoversCommandDependencies(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "-json", "./cmd/lantai")
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("command dependency graph: %v", err)
	}
	roots := map[string]bool{"go.mod": true, "go.sum": true}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	for {
		var pkg struct {
			Dir    string
			Module *struct{ Main bool }
		}
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if pkg.Module == nil || !pkg.Module.Main {
			continue
		}
		rel, err := filepath.Rel(root, pkg.Dir)
		if err != nil {
			t.Fatal(err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		roots[strings.Split(filepath.ToSlash(rel), "/")[0]] = true
	}
	recipe, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	copied := map[string]bool{}
	for _, line := range strings.Split(string(recipe), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "COPY" || strings.HasPrefix(fields[1], "--from=") {
			continue
		}
		for _, source := range fields[1 : len(fields)-1] {
			copied[strings.TrimPrefix(source, "./")] = true
		}
	}
	ignore, err := os.ReadFile("Dockerfile.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, line := range strings.Split(string(ignore), "\n") {
		allowed[strings.TrimSpace(line)] = true
	}
	names := make([]string, 0, len(roots))
	for name := range roots {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !copied[name] {
			t.Errorf("command dependency root %q missing from Dockerfile COPY", name)
		}
		pattern := "!" + name + "/**"
		if name == "go.mod" || name == "go.sum" {
			pattern = "!" + name
		}
		if !allowed[pattern] {
			t.Errorf("command dependency root %q missing from Docker context allowlist", name)
		}
	}
}
