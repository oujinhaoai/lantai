package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportVerifyExactVersionResumesAndChecksAllFiles(t *testing.T) {
	root, in := importFixture(t)
	plan := importMustPlan(t, root, in)
	c, peer := newImportPeer(t)
	state := importStateDir(t)
	report, err := c.ApplyImport(context.Background(), plan, root, state)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "verify")
	peer.truncateDownload = true
	verified, err := c.VerifyImport(context.Background(), plan, report, root, state, destination, "reference")
	if err == nil || verified.Complete {
		t.Fatal("truncated download verified")
	}
	verified, err = c.VerifyImport(context.Background(), plan, report, root, state, destination, "reference")
	if err != nil || !verified.Complete || verified.Results[0].Files != 2 || verified.Results[0].Bytes != 32 {
		t.Fatalf("verify failed: %+v %v", verified, err)
	}
	if len(peer.requestedRanges) != 1 || peer.requestedRanges[0] != "bytes=1-" {
		t.Fatalf("did not reuse Download range state: %v", peer.requestedRanges)
	}
	if _, err = c.VerifyImport(context.Background(), plan, report, root, state, destination, "reference"); err != nil {
		t.Fatal(err)
	}
	for _, f := range plan.Input.Assets[0].Push.Content.Files {
		digest, size, err := HashFile(context.Background(), filepath.Join(destination, "scene", filepath.FromSlash(f.Path)))
		if err != nil || digest != f.SHA256 || size != f.Size {
			t.Fatal("verification bytes differ")
		}
	}
	if _, err = os.Stat(filepath.Join(root, "project/main.blend")); err != nil {
		t.Fatal("source removed")
	}
	importWrite(t, destination, "scene/extra.txt", "unexpected payload")
	if _, err = c.VerifyImport(context.Background(), plan, report, root, state, destination, "reference"); err == nil {
		t.Fatal("extra file silently accepted")
	}
}
func TestImportVerifyRejectsConflictsAndChangedManifestBeforeDownloading(t *testing.T) {
	root, in := importFixture(t)
	plan := importMustPlan(t, root, in)
	c, peer := newImportPeer(t)
	state := importStateDir(t)
	report, err := c.ApplyImport(context.Background(), plan, root, state)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	importWrite(t, target, "scene/main.blend", "preserve this conflict")
	if _, err = c.VerifyImport(context.Background(), plan, report, root, state, target, "reference"); err == nil {
		t.Fatal("existing unbound destination accepted")
	}
	got, _ := os.ReadFile(filepath.Join(target, "scene/main.blend"))
	if string(got) != "preserve this conflict" || peer.grantCalls != 0 {
		t.Fatal("modified conflict or downloaded before validation")
	}
	peer.badManifest = true
	if _, err = c.VerifyImport(context.Background(), plan, report, root, state, t.TempDir(), "reference"); err == nil {
		t.Fatal("server manifest did not match plan")
	}
	if peer.grantCalls != 0 {
		t.Fatal("requested bytes from a mismatched manifest")
	}
	peer.badManifest = false
	for _, destination := range []string{root, filepath.Join(root, "verify"), state, filepath.Dir(root)} {
		if _, err = c.VerifyImport(context.Background(), plan, report, root, state, destination, "reference"); err == nil {
			t.Fatal("overlapping verification root accepted")
		}
	}
	report.Results[0].VersionID = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	if _, err = c.VerifyImport(context.Background(), plan, report, root, state, t.TempDir(), "reference"); err == nil {
		t.Fatal("edited receipt redirected verification")
	}
}
func TestImportVerifyRejectsBoundTargetSymlinkAndConflictingBytes(t *testing.T) {
	root, in := importFixture(t)
	plan := importMustPlan(t, root, in)
	c, _ := newImportPeer(t)
	state := importStateDir(t)
	report, err := c.ApplyImport(context.Background(), plan, root, state)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if _, err = c.VerifyImport(context.Background(), plan, report, root, state, target, "reference"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(target, "scene/main.blend")
	if err = os.Remove(file); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.blend")
	importWrite(t, filepath.Dir(outside), filepath.Base(outside), "outside")
	if err = os.Symlink(outside, file); err == nil {
		if _, err = c.VerifyImport(context.Background(), plan, report, root, state, target, "reference"); err == nil {
			t.Fatal("destination symlink accepted")
		}
		if err = os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	importWrite(t, target, "scene/main.blend", "changed local target")
	if _, err = c.VerifyImport(context.Background(), plan, report, root, state, target, "reference"); err == nil {
		t.Fatal("changed target overwritten")
	}
	data, _ := os.ReadFile(file)
	if string(data) != "changed local target" {
		t.Fatal("conflict was overwritten")
	}
}
func TestImportSyntheticNineClassesAndOldVersionAfterNewIngest(t *testing.T) {
	root := t.TempDir()
	in := ImportInput{Schema: ImportInputSchema}
	// Representative wrappers only: these bytes prove batch integrity and path
	// restoration, not that a DCC application accepts a fabricated binary file.
	fixtures := []struct{ key, typ, name string }{
		{"image", "image", "preview.png"}, {"video", "video", "final.mp4"}, {"audio", "audio", "voice.wav"},
		{"model", "model", "character.glb"}, {"motion", "motion", "walk.fbx"}, {"scene", "scene", "main.blend"},
		{"recipe", "config", "workflow.json"}, {"document", "doc", "notes.md"}, {"production", "production", "edit/project.json"},
	}
	for _, f := range fixtures {
		importWrite(t, root, f.key+"/"+f.name, "synthetic "+f.key)
		in.Assets = append(in.Assets, ImportAsset{Key: f.key, Directory: f.key, Classification: "final", Push: PushInput{ProjectID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Slug: f.key, Content: ContentInput{AssetType: f.typ, Files: []InputFile{{Path: f.name, Role: "primary"}}}}})
	}
	plan := importMustPlan(t, root, in)
	c, peer := newImportPeer(t)
	state := importStateDir(t)
	report, err := c.ApplyImport(context.Background(), plan, root, state)
	if err != nil || !report.Complete {
		t.Fatalf("nine-class apply: %+v %v", report, err)
	}
	if peer.commitCount != 9 {
		t.Fatal("missing representative type")
	}
	verified, err := c.VerifyImport(context.Background(), plan, report, root, state, t.TempDir(), "reference")
	if err != nil || !verified.Complete || len(verified.Results) != 9 {
		t.Fatalf("nine-class verification: %+v %v", verified, err)
	}
	// Continue work in a separate local copy, then append a new version. The
	// original source snapshot and old receipt remain usable and unchanged.
	working := t.TempDir()
	importWrite(t, working, "preview.png", "synthetic image v2")
	v2 := ImportInput{Schema: ImportInputSchema, Assets: []ImportAsset{{Key: "image-v2", Directory: ".", Classification: "final", Push: PushInput{ProjectID: in.Assets[0].Push.ProjectID, AssetID: report.Results[0].AssetID, BaseVersionID: report.Results[0].VersionID, Content: ContentInput{AssetType: "image", VersionNote: "continued synthetic work", Files: []InputFile{{Path: "preview.png", Role: "primary"}}}}}}}
	plan2 := importMustPlan(t, working, v2)
	second, err := c.ApplyImport(context.Background(), plan2, working, importStateDir(t))
	if err != nil || second.Results[0].AssetID != report.Results[0].AssetID || second.Results[0].VersionID == report.Results[0].VersionID {
		t.Fatalf("v2 ingest failed: %+v %v", second, err)
	}
	if _, err = c.VerifyImport(context.Background(), plan, report, root, state, t.TempDir(), "reference"); err != nil {
		t.Fatal("new version changed old verification", err)
	}
	old := peer.versions[report.Results[0].VersionID][0]
	if strings.Contains(string(peer.data[old.SHA256]), "v2") {
		t.Fatal("old bytes changed")
	}
}

func TestImportVerifyRejectsUnboundWorksetIdentityAndDigestBeforeDownload(t *testing.T) {
	for _, which := range []string{"version-asset", "version-id", "manifest-asset", "manifest-id", "instance", "digest", "digest-binding"} {
		t.Run(which, func(t *testing.T) {
			root, in := importFixture(t)
			plan := importMustPlan(t, root, in)
			c, peer := newImportPeer(t)
			state := importStateDir(t)
			report, err := c.ApplyImport(context.Background(), plan, root, state)
			if err != nil {
				t.Fatal(err)
			}
			peer.wrongManifestIdentity = which
			target := t.TempDir()
			verified, err := c.VerifyImport(context.Background(), plan, report, root, state, target, "reference")
			if err == nil || verified.Complete || len(verified.Results) != 1 || verified.Results[0].Status != "failed" {
				t.Fatalf("unbound manifest verified: %+v %v", verified, err)
			}
			if peer.grantCalls != 0 || peer.transferCalls != 0 {
				t.Fatal("unbound manifest caused read grants or download")
			}
			if _, err = os.Stat(filepath.Join(target, "scene")); !os.IsNotExist(err) {
				t.Fatal("unbound manifest produced an asset directory")
			}
		})
	}
}

func TestImportVerifyWorksetRejectsRootReplacementBeforeTransfer(t *testing.T) {
	root, in := importFixture(t)
	plan := importMustPlan(t, root, in)
	c, peer := newImportPeer(t)
	state := importStateDir(t)
	report, err := c.ApplyImport(context.Background(), plan, root, state)
	if err != nil {
		t.Fatal(err)
	}
	target, err := importAbsolute(filepath.Join(t.TempDir(), "verify"))
	if err != nil {
		t.Fatal(err)
	}
	outside, err := importAbsolute(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(t.TempDir(), "symlink-probe")
	if err = os.Symlink(outside, probe); err != nil {
		t.Skip("symlink creation unavailable", err)
	}
	if err = os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	replaced := false
	peer.onGrant = func() {
		peer.onGrant = nil
		if err := os.Rename(target, target+"-original"); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(outside, target); err != nil {
			t.Error(err)
			return
		}
		replaced = true
	}
	verified, err := c.VerifyImport(context.Background(), plan, report, root, state, target, "reference")
	if !replaced || err == nil || verified.Complete {
		t.Fatalf("root replacement not rejected: %+v %v", verified, err)
	}
	if peer.transferCalls != 0 {
		t.Fatal("root replacement reached a transfer")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("wrote outside pinned root: %v %v", entries, err)
	}
	if err = ValidateImportPlan(context.Background(), plan, root); err != nil {
		t.Fatal("source changed", err)
	}
}
