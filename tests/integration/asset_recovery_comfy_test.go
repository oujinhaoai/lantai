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

func TestAssetRecoveryComfyEditRecommitAndReopen(t *testing.T) {
	if os.Getenv("LANTAI_ACCEPT_COMFY_RECOVERY") != "1" {
		t.Skip("explicit installed ComfyUI, Python and synthetic corpus required")
	}
	root, python, comfy := os.Getenv("LANTAI_ASSET_FIXTURE_ROOT"), os.Getenv("LANTAI_COMFY_PYTHON"), os.Getenv("LANTAI_COMFY_SOURCE")
	if root == "" || python == "" || comfy == "" {
		t.Fatal("explicit installed ComfyUI paths and synthetic corpus required")
	}
	var corpus struct {
		Assets []jointAsset `json:"assets"`
	}
	raw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil || json.Unmarshal(raw, &corpus) != nil {
		t.Fatal("synthetic corpus", err)
	}
	var original jointAsset
	for _, a := range corpus.Assets {
		if a.Key == "recipe" && a.Version == "v1" {
			original = a
		}
	}
	if original.Key == "" {
		t.Fatal("synthetic v1 recipe missing")
	}
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	principal, maker := e.agent("comfy-recovery@synthetic", identity.RoleContributor)
	a := reopenApplication(t, e)
	s := assetRemoteServers(t, a)
	c, err := client.New(client.Config{BaseURL: "http://" + s.Addresses.API, AllowHTTP: true, SessionToken: maker.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	work := availabilityWork(t)
	in := client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "synthetic/comfy-recovery", Content: client.ContentInput{AssetType: "config", Files: original.Files, Rights: original.Rights}}
	in, err = client.PreparePush(t.Context(), in, filepath.Join(root, filepath.FromSlash(original.Root)))
	if err != nil {
		t.Fatal(err)
	}
	type receipt struct {
		AssetID       string `json:"asset_id"`
		VersionID     string `json:"version_id"`
		VersionNumber int64  `json:"version_number"`
	}
	first, err := c.Push(t.Context(), in, filepath.Join(root, filepath.FromSlash(original.Root)), filepath.Join(work, "v1-state.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	var v1 receipt
	if err = first.Decode(&v1); err != nil || v1.VersionNumber != 1 {
		t.Fatal(v1, err)
	}
	a = restoreAvailability(t, e, a, s)
	maker = availabilitySession(t, e, principal.ID, []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize})
	c = jointClient(t, a, maker.Token)
	pull := func(v receipt, target string) {
		t.Helper()
		_, er := c.PullWorkset(t.Context(), client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: v.AssetID, VersionID: v.VersionID, Destination: "recipe"}}}, target, "archive_review")
		if er != nil {
			t.Fatal(er)
		}
	}
	pulled := filepath.Join(work, "recovered-v1")
	pull(v1, pulled)
	driver, err := filepath.Abs("../fixtures/asset-management/comfy_continue.py")
	if err != nil {
		t.Fatal(err)
	}
	run := func(source, destination, mode string, files []client.InputFile) {
		t.Helper()
		list := destination + "-files.json"
		if er := os.WriteFile(list, mustJSON(files), 0600); er != nil {
			t.Fatal(er)
		}
		cmd := exec.CommandContext(t.Context(), python, "-I", driver, "--comfy-source", comfy, "--workspace", source, "--files", list, "--output", destination, "--mode", mode)
		out, er := cmd.CombinedOutput()
		if writeErr := os.WriteFile(destination+".log", out, 0600); writeErr != nil {
			t.Fatal(writeErr)
		}
		if er != nil {
			t.Fatalf("Comfy %s: %v\n%s", mode, er, out)
		}
		proof, er := os.ReadFile(filepath.Join(destination, "validation.json"))
		if er != nil {
			t.Fatal(er)
		}
		t.Logf("Comfy %s proof: %s", mode, proof)
	}
	edited := filepath.Join(work, "edited")
	run(filepath.Join(pulled, "recipe"), edited, "edit", original.Files)
	editedRoot := filepath.Join(edited, "asset")
	var files []client.InputFile
	roles := map[string]string{}
	for _, f := range original.Files {
		roles[f.Path] = f.Role
	}
	err = filepath.WalkDir(editedRoot, func(path string, d os.DirEntry, er error) error {
		if er != nil {
			return er
		}
		if d.IsDir() {
			return nil
		}
		rel, er := filepath.Rel(editedRoot, path)
		if er != nil {
			return er
		}
		rel = filepath.ToSlash(rel)
		role := roles[rel]
		if role == "" {
			role = "preview"
		}
		files = append(files, client.InputFile{Path: rel, Role: role})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	next := client.PushInput{ProjectID: in.ProjectID, AssetID: v1.AssetID, BaseVersionID: v1.VersionID, Content: client.ContentInput{AssetType: "config", Files: files, Rights: in.Content.Rights}}
	next, err = client.PreparePush(t.Context(), next, editedRoot)
	if err != nil {
		t.Fatal(err)
	}
	// The native workflow, rather than a sidecar alone, must have changed.
	for _, f := range next.Content.Files {
		if f.Path == "workflow-api.json" {
			for _, old := range original.Files {
				if old.Path == f.Path && old.SHA256 == f.SHA256 {
					t.Fatal("recipe was not edited")
				}
			}
		}
	}
	second, err := c.Push(t.Context(), next, editedRoot, filepath.Join(work, "v2-state.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	var v2 receipt
	if err = second.Decode(&v2); err != nil || v2.AssetID != v1.AssetID || v2.VersionID == v1.VersionID || v2.VersionNumber != 2 {
		t.Fatal(v2, err)
	}
	repeated, err := c.Push(t.Context(), next, editedRoot, filepath.Join(work, "v2-state.json"), false)
	var again receipt
	if err != nil || repeated.Decode(&again) != nil || again != v2 {
		t.Fatal("native replay changed version", err)
	}
	reopened := filepath.Join(work, "recovered-v2")
	pull(v2, reopened)
	for _, f := range next.Content.Files {
		hash, size, er := client.HashFile(t.Context(), filepath.Join(reopened, "recipe", filepath.FromSlash(f.Path)))
		if er != nil || hash != f.SHA256 || size != f.Size {
			t.Fatal("v2 mismatch", f.Path, er)
		}
	}
	run(filepath.Join(reopened, "recipe"), filepath.Join(work, "reexecuted"), "reopen", next.Content.Files)
	old := filepath.Join(work, "unchanged-v1")
	pull(v1, old)
	for _, f := range original.Files {
		hash, size, er := client.HashFile(t.Context(), filepath.Join(old, "recipe", filepath.FromSlash(f.Path)))
		if er != nil || hash != f.SHA256 || size != f.Size {
			t.Fatal("v1 changed", f.Path, er)
		}
	}
	found, err := c.FindAssets(t.Context(), client.AssetFilter{ProjectID: in.ProjectID}, client.AssetFindOptions{})
	if err != nil || len(found.Items) != 1 || found.Items[0].VersionNumber != 2 || found.Items[0].VersionID != v2.VersionID {
		t.Fatal("native replay created v3", found, err)
	}
	t.Logf("recovered Comfy recipe edited and executed, v1=%s v2=%s; same asset, old bytes and native reproduction verified", v1.VersionID, v2.VersionID)
}
