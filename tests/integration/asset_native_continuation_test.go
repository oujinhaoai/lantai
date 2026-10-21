package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Explicit native acceptance only: the same running core serves v1, receives a
// genuine Blender edit of its pulled working copy as v2, and serves both again.
func TestAssetNativeEditRecommitAndReopen(t *testing.T) {
	testAssetNativeEditRecommit(t, false)
}
func TestAssetRecoveryBlenderEditRecommitAndReopen(t *testing.T) {
	testAssetNativeEditRecommit(t, true)
}
func testAssetNativeEditRecommit(t *testing.T, restored bool) {
	if os.Getenv("LANTAI_ACCEPT_NATIVE_RECOMMIT") != "1" {
		t.Skip("NOT RUN: set LANTAI_ACCEPT_NATIVE_RECOMMIT=1, LANTAI_ASSET_FIXTURE_ROOT and LANTAI_BLENDER for native edit/recommit acceptance")
	}
	fixtureRoot, blender := os.Getenv("LANTAI_ASSET_FIXTURE_ROOT"), os.Getenv("LANTAI_BLENDER")
	if fixtureRoot == "" || blender == "" {
		t.Fatal("explicit synthetic corpus and installed Blender are required")
	}
	var corpus struct {
		Contract string `json:"contract"`
		Assets   []struct {
			Key     string             `json:"asset_key"`
			Version string             `json:"version_label"`
			Root    string             `json:"root"`
			Type    string             `json:"asset_type"`
			Files   []client.InputFile `json:"files"`
			Rights  json.RawMessage    `json:"rights"`
		} `json:"assets"`
	}
	raw, err := os.ReadFile(filepath.Join(fixtureRoot, "manifest.json"))
	if err != nil || json.Unmarshal(raw, &corpus) != nil || corpus.Contract != "lantai.synthetic-fixtures/v1" {
		t.Fatal("invalid explicit synthetic corpus")
	}
	var initial client.PushInput
	var originalScene client.InputFile
	var source string
	for _, asset := range corpus.Assets {
		if asset.Key == "scene" && asset.Version == "v1" {
			if client.SafeRelative(asset.Root) != nil || asset.Type != "scene" {
				t.Fatal("invalid scene fixture")
			}
			source = filepath.Join(fixtureRoot, filepath.FromSlash(asset.Root))
			initial = client.PushInput{Slug: "synthetic/native-continuation", Content: client.ContentInput{AssetType: asset.Type, Files: asset.Files, Rights: asset.Rights}}
			for _, file := range asset.Files {
				if file.Path == "scene.blend" {
					originalScene = file
				}
			}
		}
	}
	if source == "" || originalScene.SHA256 == "" {
		t.Fatal("complete native v1 scene is required")
	}
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	principal, maker := e.agent("native-maker@synthetic", identity.RoleContributor)
	a := reopenApplication(t, e)
	server := assetRemoteServers(t, a)
	c, err := client.New(client.Config{BaseURL: "http://" + server.Addresses.API, AllowHTTP: true, SessionToken: maker.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	work := availabilityWork(t)
	initial.ProjectID = string(e.project.ProjectID)
	initial, err = client.ApplyAssetProfile(initial, client.AssetProfile{Characters: []string{"hero-a"}, Source: "synthetic", BusinessState: "active", LegacyVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	initial, err = client.PreparePush(t.Context(), initial, source)
	if err != nil {
		t.Fatal(err)
	}
	type receipt struct {
		AssetID            string `json:"asset_id"`
		VersionID          string `json:"version_id"`
		VersionNumber      int64  `json:"version_number"`
		DescriptionPending bool   `json:"description_pending"`
	}
	push := func(in client.PushInput, root, state string) receipt {
		t.Helper()
		response, pushErr := c.Push(t.Context(), in, root, filepath.Join(work, state), false)
		var r receipt
		if pushErr != nil || response.Decode(&r) != nil || r.DescriptionPending || r.AssetID == "" || r.VersionID == "" {
			t.Fatalf("native commit: %+v %v", r, pushErr)
		}
		return r
	}
	v1 := push(initial, source, "v1.push.json")
	if restored {
		a = restoreAvailability(t, e, a, server)
		maker = availabilitySession(t, e, principal.ID, []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize})
		c = jointClient(t, a, maker.Token)
	}
	pull := func(r receipt, root string, entrypoint string) string {
		t.Helper()
		ws := client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: r.AssetID, VersionID: r.VersionID, Destination: "scene"}}, Entrypoints: []string{"scene/" + entrypoint}, Dependencies: []client.WorksetDependency{{From: "scene/" + entrypoint, Requires: []string{"scene/lib/prop-library.blend", "scene/textures/checker.png"}}}}
		if _, pullErr := c.PullWorkset(t.Context(), ws, root, "archive_review"); pullErr != nil {
			t.Fatal(pullErr)
		}
		return filepath.Join(root, "scene")
	}
	working := pull(v1, filepath.Join(work, "working"), "scene.blend")
	nativeScript, err := filepath.Abs("../fixtures/asset-management/blender_native.py")
	if err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(work, "native-acceptance.py")
	const python = `import json,runpy,sys
from pathlib import Path
script,root,mode=sys.argv[sys.argv.index("--")+1:]
root=Path(root)
m=runpy.run_path(script)
if mode=="edit":
    result=m["verify_scene"](root,1)
else:
    bpy=m["bpy"]
    bpy.ops.wm.open_mainfile(filepath=str(root/"continued.blend"))
    assert abs(bpy.data.objects["SyntheticHero"].location.x-0.25)<0.0001
    assert any(m["check_relative"](lib.filepath,root)=="lib/prop-library.blend" for lib in bpy.data.libraries)
    for image in bpy.data.images:
        if image.source=="FILE" and image.filepath:
            m["check_relative"](image.filepath,root)
            image.reload()
            assert len(image.pixels)>0
    bpy.context.scene.render.filepath="//reopened-preview.png"
    bpy.ops.render.render(write_still=True)
    assert (root/"reopened-preview.png").stat().st_size>0
    result={"edited_location_x":0.25,"reopened":True,"rendered":True}
(root/"native-acceptance.json").write_text(json.dumps(result)+"\n")
`
	if err = os.WriteFile(driver, []byte(python), 0600); err != nil {
		t.Fatal(err)
	}
	runNative := func(root, mode string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), blender, "--background", "--factory-startup", "--disable-autoexec", "--python-exit-code", "1", "--python", driver, "--", nativeScript, root, mode)
		cmd.Env = append(os.Environ(), "BLENDER_USER_CONFIG="+filepath.Join(work, "blender-config"), "BLENDER_USER_SCRIPTS="+filepath.Join(work, "blender-scripts"))
		output, nativeErr := cmd.CombinedOutput()
		if writeErr := os.WriteFile(filepath.Join(work, "blender-"+mode+".log"), output, 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
		if nativeErr != nil {
			t.Fatalf("Blender %s: %v\n%s", mode, nativeErr, output)
		}
		proof, readErr := os.ReadFile(filepath.Join(root, "native-acceptance.json"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		t.Logf("Blender %s proof: %s", mode, proof)
	}
	runNative(working, "edit")
	if hash, _, hashErr := client.HashFile(t.Context(), filepath.Join(working, "scene.blend")); hashErr != nil || hash != originalScene.SHA256 {
		t.Fatal("native edit replaced v1 source instead of saving a continuation")
	}
	next := client.PushInput{ProjectID: initial.ProjectID, AssetID: v1.AssetID, BaseVersionID: v1.VersionID, Content: client.ContentInput{AssetType: "scene", VersionNote: "Blender edited the actual v1 working copy by +0.25 and saved a new project", Rights: initial.Content.Rights, Files: []client.InputFile{{Path: "continued.blend", Role: "primary"}, {Path: "continued-preview.png", Role: "preview"}, {Path: "lib/prop-library.blend", Role: "source"}, {Path: "textures/checker.png", Role: "texture"}, {Path: "LICENSE.txt", Role: "doc"}}}}
	next, err = client.ApplyAssetProfile(next, client.AssetProfile{Characters: []string{"hero-a"}, Source: "synthetic", BusinessState: "pending_review", LegacyVersion: "native-v2"})
	if err != nil {
		t.Fatal(err)
	}
	next, err = client.PreparePush(t.Context(), next, working)
	if err != nil {
		t.Fatal(err)
	}
	v2 := push(next, working, "v2.push.json")
	if v2.AssetID != v1.AssetID || v2.VersionID == v1.VersionID || v1.VersionNumber != 1 || v2.VersionNumber != 2 {
		t.Fatalf("continuation identity changed: %+v %+v", v1, v2)
	}
	reopened := pull(v2, filepath.Join(work, "reopened-v2"), "continued.blend")
	for _, file := range next.Content.Files {
		got, size, hashErr := client.HashFile(t.Context(), filepath.Join(reopened, filepath.FromSlash(file.Path)))
		if hashErr != nil || got != file.SHA256 || size != file.Size {
			t.Fatalf("continued v2 bytes differ: %s %v", file.Path, hashErr)
		}
	}
	runNative(reopened, "reopen")
	old := pull(v1, filepath.Join(work, "old-v1-after-v2"), "scene.blend")
	if hash, _, hashErr := client.HashFile(t.Context(), filepath.Join(old, "scene.blend")); hashErr != nil || hash != originalScene.SHA256 {
		t.Fatal("v1 changed after real native v2 commit")
	}
	t.Logf("real native continuation complete: restored=%v, same asset %s, v1=%s v2=%s; old version intact", restored, v1.AssetID, v1.VersionID, v2.VersionID)
}
