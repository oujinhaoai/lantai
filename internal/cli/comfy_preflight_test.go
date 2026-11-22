package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestComfyPreflightCLIOffline(t *testing.T) {
	root, inputDir := t.TempDir(), t.TempDir()
	graph := []byte(`{"1":{"class_type":"LoadImage","inputs":{"image":"hero.png"}},"2":{"class_type":"SaveImage"}}`)
	if err := os.WriteFile(filepath.Join(root, "graph.json"), graph, 0600); err != nil {
		t.Fatal(err)
	}
	e := client.WorksetEntry{AssetID: string(ids.New()), VersionID: string(ids.New()), Destination: "input"}
	orig, _ := json.Marshal(client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{e}})
	canon, err := canonjson.Canonicalize(orig)
	if err != nil {
		t.Fatal(err)
	}
	d := digest.Of([]byte("synthetic"))
	in := client.ComfyPreflightInput{
		Workflow:       "graph.json",
		ComfyInputRoot: "input",
		Workset: client.WorksetReceipt{
			Schema: client.WorksetReceiptSchema, State: "completed", Origin: "https://example.invalid",
			InputDigest: digest.Of(canon), FileCount: 1, TotalBytes: 9,
			Entries: []client.WorksetPlannedEntry{{WorksetEntry: e, InstanceID: string(ids.New()), ManifestDigest: d,
				Files: []client.WorksetFile{{SourcePath: "hero.png", Path: "input/hero.png", SHA256: string(d)[7:], Size: 9, State: "verified"}}}},
		},
	}
	input := filepath.Join(inputDir, "request.json")
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LANTAI_SERVER", "https://not-a-server.invalid")
	var stdout, stderr bytes.Buffer
	args := []string{"comfy-preflight", "--input", input, "--directory", root}
	if code := runAssets(t.Context(), args, &stdout, &stderr); code != 0 {
		t.Fatalf("offline operation failed (%d): %s", code, stderr.String())
	}
	var got client.ComfyPreflightReport
	if err = json.Unmarshal(stdout.Bytes(), &got); err != nil || len(got.Findings) != 1 || got.Findings[0].Status != "present" {
		t.Fatalf("unexpected JSON report: %s (%v)", stdout.String(), err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runAssets(t.Context(), append(args, "--output", filepath.Join(root, "should-not-write.json")), &stdout, &stderr); code == 0 {
		t.Fatal("source-directory output was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-write.json")); !os.IsNotExist(err) {
		t.Fatal("unexpected source-directory output")
	}
}
