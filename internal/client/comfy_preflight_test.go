package client

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func comfyFixture(t *testing.T, raw string, includeGraph bool) (string, ComfyPreflightInput) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "scene"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scene", "workflow.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	in := ComfyPreflightInput{
		Workflow: "scene/workflow.json", ComfyInputRoot: "input",
		ModelRoots: map[string]string{"checkpoints": "model/checkpoints", "loras": "model/loras", "vae": "model/vae"},
		Workset:    WorksetReceipt{Schema: WorksetReceiptSchema, State: "completed"},
	}
	instance := ids.New()
	add := func(dst string, items map[string]string) {
		t.Helper()
		e := WorksetPlannedEntry{WorksetEntry: WorksetEntry{AssetID: string(ids.New()), VersionID: string(ids.New()), Destination: dst},
			InstanceID: string(instance), ManifestDigest: digest.Of([]byte(dst))}
		for name, data := range items {
			e.Files = append(e.Files, WorksetFile{SourcePath: name, Path: dst + "/" + name, Size: int64(len(data)),
				SHA256: importSHA([]byte(data)), State: "verified"})
			in.Workset.FileCount++
			in.Workset.TotalBytes += int64(len(data))
		}
		in.Workset.Entries = append(in.Workset.Entries, e)
	}
	add("input", map[string]string{"hero.png": "not existing locally", "Case/sub.png": "other"})
	add("model", map[string]string{"checkpoints/same.safetensors": "a", "loras/same.safetensors": "b", "vae/piece.safetensors": "c"})
	if includeGraph {
		add("scene", map[string]string{"workflow.json": raw})
	}
	bindComfyFixture(t, &in)
	return root, in
}

func bindComfyFixture(t *testing.T, in *ComfyPreflightInput) {
	t.Helper()
	def := WorksetInput{Schema: WorksetSchema, Dependencies: in.Workset.Dependencies, Entrypoints: in.Workset.Entrypoints}
	for _, e := range in.Workset.Entries {
		def.Entries = append(def.Entries, e.WorksetEntry)
	}
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	in.Workset.InputDigest = digest.Of(canonical)
}

func TestComfyPreflightMatchesOnlyReceipt(t *testing.T) {
	jsonGraph := `{"9":{"class_type":"LoadImage","inputs":{"image":"hero.png"}},"1":{"class_type":"LoadImage","inputs":{"image":"hero.png"}},"2":{"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"same.safetensors"}},"3":{"class_type":"LoraLoader","inputs":{"lora_name":"same.safetensors"}},"4":{"class_type":"VAELoader","inputs":{"vae_name":"missing.safetensors"}},"5":{"class_type":"LoadImage","inputs":{"image":"case/sub.png"}},"6":{"class_type":"LoaderAddedByPlugin","inputs":{"filename":"/etc/passwd"}},"7":{"class_type":"LoadImage","inputs":{"image":["12",0]}},"8":{"class_type":"LoadImage","inputs":{"image":"hero.png/child"}}}`
	root, in := comfyFixture(t, jsonGraph, true)
	// None of the present files exist on disk. A missing receipt item actually
	// exists as a symlink pointing out of the workspace and must not change result.
	if err := os.MkdirAll(filepath.Join(root, "model", "vae"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "model", "vae", "missing.safetensors")); err != nil {
		t.Skipf("fixture requires symlink support: %v", err)
	}
	got, err := AnalyzeComfyPreflight(t.Context(), in, root)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WorkflowInReceipt || got.Scope != "fixed-workset-receipt-only" || got.Format != "comfy-api" || len(got.Findings) != 9 {
		t.Fatalf("unexpected report: %+v", got)
	}
	wantStatus := []string{"present", "present", "present", "missing", "manual", "manual", "manual", "manual", "present"}
	wantReason := []string{"", "", "", "", "path_spelling_or_prefix_conflict", "unsupported_node", "not_literal", "path_spelling_or_prefix_conflict", ""}
	for i, f := range got.Findings {
		if f.Node != i+1 || f.Status != wantStatus[i] || f.Reason != wantReason[i] {
			t.Fatalf("finding %d: %+v", i, f)
		}
	}
	if got.Findings[1].Path == got.Findings[2].Path || got.Findings[1].Match.SHA256 == got.Findings[2].Match.SHA256 {
		t.Fatal("did not honor distinct model roots")
	}
	if len(got.DraftEntries) != 2 || len(got.DraftDependencies) != 1 ||
		got.DraftDependencies[0].From != in.Workflow || len(got.DraftDependencies[0].Requires) != 3 {
		t.Fatalf("expected a deduplicated exact draft plus all node findings: %+v", got)
	}
	a, _ := json.Marshal(got)
	again, err := AnalyzeComfyPreflight(t.Context(), in, root)
	b, _ := json.Marshal(again)
	if err != nil || !bytes.Equal(a, b) || bytes.Contains(a, []byte("/etc/passwd")) {
		t.Fatalf("noncanonical result or echoed unsupported input: %v", err)
	}
}

func TestComfyPreflightUIAndReadBoundary(t *testing.T) {
	root, in := comfyFixture(t, `{"nodes":[{"type":"LoadImage","widgets_values":["hero.png"]},{"type":"LoadImage","widgets_values":["hero.png"],"inputs":[{"name":"image","link":4}]},{"type":"CheckpointLoaderSimple","widgets_values":["same.safetensors"]},{"type":"LoadImage","widgets_values":["../private.txt"]}]}`, false)
	got, err := AnalyzeComfyPreflight(t.Context(), in, root)
	if err != nil || got.Format != "comfy-ui" || got.WorkflowInReceipt || len(got.Findings) != 4 {
		t.Fatalf("unexpected report %+v %v", got, err)
	}
	want := []string{"", "not_literal", "ui_model_widget_unresolved", "unsafe_or_annotated_path"}
	for i, f := range got.Findings {
		if f.Reason != want[i] {
			t.Errorf("node %d: %+v", i, f)
		}
	}
	in.Workflow = "../outside.json"
	if _, err := AnalyzeComfyPreflight(t.Context(), in, root); err == nil {
		t.Fatal("source traversal accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"1":{"class_type":"SaveImage"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	in.Workflow = "link.json"
	if err := os.Symlink(outside, filepath.Join(root, in.Workflow)); err != nil {
		t.Skipf("fixture requires symlink support: %v", err)
	}
	if _, err := AnalyzeComfyPreflight(t.Context(), in, root); err == nil {
		t.Fatal("selected symlink was followed")
	}
}

func TestComfyPreflightRejectsMalformedOrUnboundInput(t *testing.T) {
	for _, raw := range []string{
		`{"1":{"class_type":"LoadImage","class_type":"SaveImage","inputs":{"image":"a"}}}`,
		`{"1":{"class_type":"LoadImage","inputs":{"image":"a","image":"b"}}}`,
		`{"1":null,"1":{}}`, `{"nodes":[]}`,
	} {
		root, in := comfyFixture(t, raw, false)
		if _, err := AnalyzeComfyPreflight(context.Background(), in, root); err == nil {
			t.Errorf("accepted invalid graph %s", raw)
		}
	}
	for name, tamper := range map[string]func(*ComfyPreflightInput){
		"partial":          func(i *ComfyPreflightInput) { i.Workset.State = "partial" },
		"bad totals":       func(i *ComfyPreflightInput) { i.Workset.FileCount++ },
		"bad input digest": func(i *ComfyPreflightInput) { i.Workset.InputDigest = digest.Of([]byte("different")) },
		"mixed instances":  func(i *ComfyPreflightInput) { i.Workset.Entries[1].InstanceID = string(ids.New()) },
		"casefold collisions": func(i *ComfyPreflightInput) {
			e := &i.Workset.Entries[0]
			f := e.Files[0]
			f.SourcePath, f.Path = "case/other.png", "input/case/other.png"
			e.Files = append(e.Files, f)
			i.Workset.FileCount++
			i.Workset.TotalBytes += f.Size
		},
		"non NFC": func(i *ComfyPreflightInput) {
			f := &i.Workset.Entries[0].Files[0]
			f.SourcePath, f.Path = "Cafe\u0301.png", "input/Cafe\u0301.png"
		},
		"wrong workset workflow": func(i *ComfyPreflightInput) {
			i.Workset.Entries[2].Files[0].SHA256 = importSHA([]byte("different"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, in := comfyFixture(t, `{"1":{"class_type":"LoadImage","inputs":{"image":"hero.png"}}}`, true)
			tamper(&in)
			if _, err := AnalyzeComfyPreflight(t.Context(), in, root); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
}
