package client

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func importFixture(t *testing.T) (string, ImportInput) {
	t.Helper()
	root := t.TempDir()
	importWrite(t, root, "project/main.blend", "synthetic scene")
	importWrite(t, root, "project/textures/diffuse.png", "synthetic texture")
	importWrite(t, root, "unknown.bin", "not selected")
	importWrite(t, root, "cache/intermediate.bin", "not selected cache")
	return root, ImportInput{Schema: ImportInputSchema, Assets: []ImportAsset{{Key: "scene", Directory: "project", Classification: "final", Push: PushInput{ProjectID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Slug: "scene", Content: ContentInput{AssetType: "scene", Files: []InputFile{{Path: "main.blend", Role: "primary"}, {Path: "textures/diffuse.png", Role: "texture"}}}}}}, Excluded: []ImportExclusion{{Path: "cache", Reason: "reproducible intermediate cache"}}}
}
func importWrite(t *testing.T, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func importMustPlan(t *testing.T, root string, in ImportInput) ImportPlan {
	t.Helper()
	p, err := PrepareImport(context.Background(), in, root)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestImportPlanFreezesSelectionAndReportsUncovered(t *testing.T) {
	root, in := importFixture(t)
	p := importMustPlan(t, root, in)
	if p.SelectedFiles != 2 || p.SelectedBytes != 32 || len(p.Uncovered) != 1 || p.Uncovered[0].Path != "unknown.bin" || p.Uncovered[0].Classification != "unknown" {
		t.Fatalf("unexpected plan: %+v", p)
	}
	if in.Assets[0].Push.Content.Files[0].SHA256 != "" {
		t.Fatal("mutated caller input")
	}
	if err := ValidateImportPlan(context.Background(), p, root); err != nil {
		t.Fatal(err)
	}
	importWrite(t, root, "project/main.blend", "changed bytes!!")
	if err := ValidateImportPlan(context.Background(), p, root); err == nil {
		t.Fatal("changed source accepted")
	}
}
func TestImportPlanDiscoveryChangesNeedReview(t *testing.T) {
	root, in := importFixture(t)
	p := importMustPlan(t, root, in)
	importWrite(t, root, "new-final.png", "unreviewed")
	if err := ValidateImportPlan(context.Background(), p, root); err == nil {
		t.Fatal("new uncovered entry silently accepted")
	}
}
func TestImportPlanRejectsUnsafeSelections(t *testing.T) {
	for _, name := range []string{"unknown type", "duplicate key", "overlap file", "escape", "credential", "excluded selection", "missing exclusion reason", "classification", "reserved transfer"} {
		t.Run(name, func(t *testing.T) {
			root, in := importFixture(t)
			switch name {
			case "unknown type":
				in.Assets[0].Push.Content.AssetType = "document"
			case "duplicate key":
				a := in.Assets[0]
				a.Directory = "other"
				importWrite(t, root, "other/a", "a")
				a.Push.Content.Files = []InputFile{{Path: "a", Role: "primary"}}
				in.Assets = append(in.Assets, a)
			case "overlap file":
				a := in.Assets[0]
				a.Key = "other"
				in.Assets = append(in.Assets, a)
			case "escape":
				in.Assets[0].Directory = "../escape"
			case "credential":
				importWrite(t, root, "project/credentials.json", "secret")
				in.Assets[0].Push.Content.Files = []InputFile{{Path: "credentials.json", Role: "primary"}}
			case "excluded selection":
				in.Excluded = append(in.Excluded, ImportExclusion{Path: "project", Reason: "excluded"})
			case "missing exclusion reason":
				in.Excluded[0].Reason = " "
			case "classification":
				in.Assets[0].Classification = "approved"
			case "reserved transfer":
				in.Assets[0].Push.Content.Files[0].Path = "a.lantai-part"
			}
			if _, err := PrepareImport(context.Background(), in, root); err == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
}
func TestImportPlanNeverFollowsSourceSymlinks(t *testing.T) {
	root, in := importFixture(t)
	outside := t.TempDir()
	importWrite(t, outside, "private.txt", "outside bytes")
	if err := os.Symlink(filepath.Join(outside, "private.txt"), filepath.Join(root, "project/link.txt")); err != nil {
		t.Skip(err)
	}
	p := importMustPlan(t, root, in)
	if len(p.Uncovered) != 2 || p.Uncovered[0].Kind != "symlink" {
		t.Fatalf("link metadata absent: %+v", p.Uncovered)
	}
	in.Assets[0].Push.Content.Files = append(in.Assets[0].Push.Content.Files, InputFile{Path: "link.txt", Role: "primary"})
	if _, err := PrepareImport(context.Background(), in, root); err == nil {
		t.Fatal("selected escaping link accepted")
	}
	in.Assets[0].Push.Content.Files = in.Assets[0].Push.Content.Files[:2]
	in.Excluded = append(in.Excluded, ImportExclusion{Path: "project/link.txt", Reason: "symlink is not imported"})
	_ = importMustPlan(t, root, in)
}
func TestImportInputStrictJSONAndPlanTamper(t *testing.T) {
	for _, raw := range []string{
		`{"schema":"lantai.asset-import-input/v1","assets":[],"exclude":[]}`,
		`{"schema":"lantai.asset-import-input/v1","assets":[{"key":"x","directory":".","classification":"final","push":{"project_id":"x","content":{"asset_type":"image","files":[],"filez":[]}}}]}`,
		`{"schema":"lantai.asset-import-input/v1","assets":[]} {}`,
	} {
		var in ImportInput
		if json.Unmarshal([]byte(raw), &in) == nil {
			t.Fatal("unknown field or second document accepted")
		}
	}
	root, in := importFixture(t)
	p := importMustPlan(t, root, in)
	p.SelectedBytes++
	if ValidateImportPlan(context.Background(), p, root) == nil {
		t.Fatal("tampered plan accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareImport(ctx, in, root); err == nil {
		t.Fatal("canceled hashing continued")
	}
}
func TestImportPlanHardLimitDoesNotTruncate(t *testing.T) {
	root, in := importFixture(t)
	// Too many explicitly selected files is rejected before attempting a walk.
	files := make([]InputFile, MaxImportFiles+1)
	for i := range files {
		files[i] = InputFile{Path: "main.blend", Role: "primary"}
	}
	in.Assets[0].Push.Content.Files = files
	if _, err := PrepareImport(context.Background(), in, root); err == nil || !strings.Contains(err.Error(), "selected file limit") {
		t.Fatal("oversize/duplicate selection accepted")
	}
}

func importStateDir(t *testing.T) string {
	t.Helper()
	dir, err := importAbsolute(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := importPrivateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestImportPlanRejectsAssetsThatCannotBeVerifiedAsAWorkset(t *testing.T) {
	root, in := importFixture(t)
	in.Assets[0].Push.Content.Files = make([]InputFile, MaxWorksetFiles+1)
	if _, err := PrepareImport(context.Background(), in, root); err == nil || !strings.Contains(err.Error(), "per-asset verification limit") {
		t.Fatalf("unverifiable asset planned: %v", err)
	}
}
