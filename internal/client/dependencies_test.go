package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSelectedDependenciesSharedBrowserFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/dependency-analysis/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name           string                        `json:"name"`
		Sources        []struct{ Path, Text string } `json:"sources"`
		ComfyInputRoot string                        `json:"comfy_input_root"`
		Complete       bool                          `json:"complete"`
		Expected       []struct {
			Format      string   `json:"format"`
			References  []string `json:"references"`
			NoticeCount int      `json:"notice_count"`
			Complete    bool     `json:"complete"`
			SHA256      string   `json:"sha256"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			sources := []DependencySource{}
			for _, s := range tc.Sources {
				sources = append(sources, DependencySource{Path: s.Path, Bytes: []byte(s.Text)})
			}
			out, err := AnalyzeSelectedDependencies(t.Context(), sources, tc.ComfyInputRoot)
			if err != nil {
				t.Fatal(err)
			}
			if out.Complete != tc.Complete || len(out.Files) != len(tc.Expected) {
				t.Fatal(out)
			}
			for i, want := range tc.Expected {
				got := out.Files[i]
				if got.Format != want.Format || got.SHA256 != want.SHA256 || got.Complete != want.Complete || len(got.Notices) != want.NoticeCount || !reflect.DeepEqual(got.References, want.References) {
					t.Fatalf("file%d mismatch: %+v want%+v", i, got, want)
				}
			}
		})
	}
}
func TestDependencySelectionLimitsPathsCancellationAndNoImplicitReads(t *testing.T) {
	valid := DependencySource{Path: "mesh.obj", Bytes: []byte("mtllib never-opened.mtl\n")}
	for _, sources := range [][]DependencySource{nil, make([]DependencySource, 129), {{Path: "../outside.obj"}}, {{Path: "mesh.obj", Bytes: make([]byte, MaxDependencyFileBytes+1)}}, {{Path: "A/a.obj"}, {Path: "a/b.obj"}}, {valid, valid}} {
		if _, err := AnalyzeSelectedDependencies(t.Context(), sources, ""); err == nil {
			t.Fatal("invalid dependency selection accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := AnalyzeSelectedDependencies(ctx, []DependencySource{valid}, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, valid.Path), valid.Bytes, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := AnalyzeDependencyFiles(t.Context(), DependencySelection{Paths: []string{valid.Path}}, root)
	if err != nil || !out.Complete || len(out.MissingSelectedFiles) != 1 || out.MissingSelectedFiles[0].Path != "never-opened.mtl" {
		t.Fatal("analysis opened or invented a referenced file", out, err)
	}
	if _, err := os.Stat(filepath.Join(root, "never-opened.mtl")); !os.IsNotExist(err) {
		t.Fatal("analysis created dependency", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.obj")
	if err := os.WriteFile(outside, valid.Bytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.obj")); err == nil {
		if _, err := AnalyzeDependencyFiles(t.Context(), DependencySelection{Paths: []string{"link.obj"}}, root); err == nil {
			t.Fatal("selected symlink read")
		}
	}
	for _, text := range []string{"mtllib x\x00.mtl", string([]byte{0xff, 0xfe}), strings.Repeat("v 0 0 0\n", 10001)} {
		out, err := AnalyzeSelectedDependencies(t.Context(), []DependencySource{{Path: "mesh.obj", Bytes: []byte(text)}}, "")
		if err != nil || out.Complete {
			t.Fatal("damaged/over-budget text became complete", out, err)
		}
	}
}

func TestDependencyAggregateGraphDepthAndDiagnosticBudgets(t *testing.T) {
	sources := []DependencySource{}
	for i := 0; i < 5; i++ {
		sources = append(sources, DependencySource{Path: fmt.Sprintf("selected%d.obj", i), Bytes: make([]byte, MaxDependencyFileBytes)})
	}
	if _, err := AnalyzeSelectedDependencies(t.Context(), sources, ""); err == nil {
		t.Fatal("aggregate4MiB budget ignored")
	}
	for _, source := range []DependencySource{
		{Path: "mesh.obj", Bytes: []byte(strings.Repeat("call unsupported\n", 1001))},
		{Path: "graph.json", Bytes: []byte(`{"nodes":[` + strings.Repeat(`{},`, 4096) + `{}]}`)},
		{Path: "graph.json", Bytes: []byte(`{"1":{"class_type":"SaveImage","ignored":` + strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65) + `}}`)},
	} {
		out, err := AnalyzeSelectedDependencies(t.Context(), []DependencySource{source}, "")
		if err != nil || out.Complete || len(out.Files) != 1 || len(out.Files[0].Notices) == 0 {
			t.Fatal("parser budget falsely completed", out, err)
		}
	}
}

func TestDependencyWorkspaceConfinesSelectedRootAndFiles(t *testing.T) {
	directory := t.TempDir()
	selected := filepath.Join(directory, "selected")
	outside := t.TempDir()
	for _, root := range []string{selected, outside} {
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "mesh.obj"), []byte("mtllib explicit.mtl\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	in := DependencySelection{Paths: []string{"mesh.obj"}}
	if out, err := AnalyzeDependencyWorkspace(t.Context(), in, workspace, "selected"); err != nil || len(out.Files) != 1 {
		t.Fatal(out, err)
	}
	for _, path := range []string{"../outside", outside, "selected/../selected"} {
		if _, err := AnalyzeDependencyWorkspace(t.Context(), in, workspace, path); err == nil {
			t.Fatal("outside or noncanonical selection accepted", path)
		}
	}
	if err := os.Symlink(outside, filepath.Join(directory, "linked")); err != nil {
		t.Skip("symlink unavailable", err)
	}
	if _, err := AnalyzeDependencyWorkspace(t.Context(), in, workspace, "linked"); err == nil {
		t.Fatal("linked selected directory accepted")
	}
	if err := os.Symlink(filepath.Join(outside, "mesh.obj"), filepath.Join(selected, "linked.obj")); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeDependencyWorkspace(t.Context(), DependencySelection{Paths: []string{"linked.obj"}}, workspace, "selected"); err == nil {
		t.Fatal("linked selected file accepted")
	}
}
