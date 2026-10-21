package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// This is a storage/application acceptance, not a native-format parser test.
// The optional generated corpus carries native-generation limitations explicitly.
func assetRealTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func assetAPIErrorCode(err error) errcode.Code {
	var remote *client.APIError
	if errors.As(err, &remote) {
		return remote.Body.Code
	}
	return errcode.CodeOf(err)
}

func assetRemoteServers(t *testing.T, a *application.App) *application.HTTPServers {
	t.Helper()
	cfg := a.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: true}
	// Use the normal configured HTTP JSON budget; remoteServers intentionally
	// constrains unrelated transfer boundary tests to 2 KiB.
	s, err := a.StartHTTP(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestAssetManagementProfileFindAndWorkset(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	_, maker := e.agent("asset-maker@synthetic", identity.RoleContributor)
	_, outsider := e.agent("asset-outsider@synthetic", "")
	a := reopenApplication(t, e)
	servers := assetRemoteServers(t, a)
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, AllowHTTP: true, SessionToken: maker.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	work := assetRealTempDir(t)
	source := filepath.Join(work, "source")
	if err = os.MkdirAll(filepath.Join(source, "textures"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{"README.md": "# Synthetic model\n", "textures/material.txt": "synthetic texture recipe v1\n"} {
		if err = os.WriteFile(filepath.Join(source, filepath.FromSlash(path)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	in := client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "synthetic/workset-model", Content: client.ContentInput{
		AssetType: "model", Files: []client.InputFile{{Path: "README.md", Role: "doc"}, {Path: "textures/material.txt", Role: "recipe"}}, Rights: mustJSON(rightsOwned()),
	}}
	in, err = client.ApplyAssetProfile(in, client.AssetProfile{Characters: []string{"hero-a"}, Source: "synthetic", BusinessState: "active", LegacyState: "awaiting-human-review", LegacyVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := client.PreparePush(t.Context(), in, source)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(work, "state-v1.json")
	first, err := c.Push(t.Context(), prepared, source, state, false)
	if err != nil {
		t.Fatal(err)
	}
	var v1 struct {
		AssetID            string `json:"asset_id"`
		VersionID          string `json:"version_id"`
		VersionNumber      int64  `json:"version_number"`
		DescriptionPending bool   `json:"description_pending"`
	}
	if err = first.Decode(&v1); err != nil || v1.AssetID == "" || v1.VersionID == "" || v1.DescriptionPending {
		t.Fatalf("v1 receipt: %+v %v", v1, err)
	}
	if _, err = c.Push(t.Context(), prepared, source, state, false); err != nil {
		t.Fatal(err)
	}
	if err = a.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	found, err := c.FindAssets(t.Context(), client.AssetFilter{ProjectID: string(e.project.ProjectID), Characters: []string{"hero-a"}, BusinessState: "active", Source: "synthetic"}, client.AssetFindOptions{})
	if err != nil || !found.Complete || len(found.Items) != 1 || found.Items[0].VersionID != v1.VersionID {
		t.Fatalf("find: %+v %v", found, err)
	}
	miss, err := c.FindAssets(t.Context(), client.AssetFilter{ProjectID: string(e.project.ProjectID), Characters: []string{"hero-b"}, BusinessState: "active"}, client.AssetFindOptions{})
	if err != nil || !miss.Complete || len(miss.Items) != 0 {
		t.Fatalf("AND filter returned non-matching item: %+v %v", miss, err)
	}
	// A new version must not change the previously selected working set.
	if err = os.WriteFile(filepath.Join(source, "textures/material.txt"), []byte("synthetic texture recipe v2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	in = client.PushInput{ProjectID: string(e.project.ProjectID), AssetID: v1.AssetID, BaseVersionID: v1.VersionID, Content: client.ContentInput{
		AssetType: "model", VersionNote: "synthetic continuation", Rights: mustJSON(rightsOwned()),
		Files: []client.InputFile{{Path: "README.md", Role: "doc"}, {Path: "textures/material.txt", Role: "recipe"}},
	}}
	in, err = client.ApplyAssetProfile(in, client.AssetProfile{Characters: []string{"hero-a"}, Source: "synthetic", BusinessState: "active", LegacyVersion: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = client.PreparePush(t.Context(), in, source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Push(t.Context(), prepared, source, filepath.Join(work, "state-v2.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	var v2 struct {
		AssetID       string `json:"asset_id"`
		VersionID     string `json:"version_id"`
		VersionNumber int64  `json:"version_number"`
	}
	if err = second.Decode(&v2); err != nil || v2.AssetID != v1.AssetID || v2.VersionID == v1.VersionID || v2.VersionNumber != 2 {
		t.Fatalf("v2: %+v %v", v2, err)
	}
	ws := client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: v1.AssetID, VersionID: v1.VersionID, Destination: "model"}}, Entrypoints: []string{"model/README.md"}, Dependencies: []client.WorksetDependency{{From: "model/README.md", Requires: []string{"model/textures/material.txt"}}}}
	target := filepath.Join(work, "working-copy")
	if _, err = c.PullWorkset(t.Context(), ws, target, "archive_review"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.PullWorkset(t.Context(), ws, target, "archive_review"); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(target, "model", "textures", "material.txt"))
	if err != nil || string(old) != "synthetic texture recipe v1\n" {
		t.Fatalf("working set drifted to latest: %q %v", old, err)
	}
	// A different local file is never overwritten to make a retry look successful.
	if err = os.WriteFile(filepath.Join(target, "model", "textures", "material.txt"), []byte("local edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = c.PullWorkset(t.Context(), ws, target, "archive_review"); err == nil {
		t.Fatal("conflicting local edit was accepted")
	}
	out, err := client.New(client.Config{BaseURL: c.Origin(), AllowHTTP: true, SessionToken: outsider.Token})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err = out.Do(t.Context(), http.MethodGet, "/api/v1/assets/"+v1.AssetID+"/versions/"+v1.VersionID, nil, client.Options{}); assetAPIErrorCode(err) != errcode.NotFound {
		t.Fatalf("outsider exact version: %v", err)
	}
}

// Set LANTAI_ASSET_FIXTURE_ROOT to a freshly generated, independently verified
// synthetic corpus. No developer's media directory is used by default.
func TestAssetManagementGeneratedFixturesStorageRoundTrip(t *testing.T) {
	root := os.Getenv("LANTAI_ASSET_FIXTURE_ROOT")
	if root == "" {
		t.Skip("set LANTAI_ASSET_FIXTURE_ROOT to the synthetic generator output")
	}
	raw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Contract string `json:"contract"`
		Assets   []struct {
			Key      string             `json:"asset_key"`
			Type     string             `json:"asset_type"`
			Coverage string             `json:"coverage_id"`
			Version  string             `json:"version_label"`
			Root     string             `json:"root"`
			Status   string             `json:"status"`
			Files    []client.InputFile `json:"files"`
			Rights   json.RawMessage    `json:"rights"`
			Missing  []string           `json:"missing_entrypoints"`
			Profile  struct {
				Characters    []string `json:"characters"`
				BusinessState string   `json:"business_state"`
				Source        string   `json:"source"`
			} `json:"expected_metadata"`
		} `json:"assets"`
	}
	if err = json.Unmarshal(raw, &corpus); err != nil || corpus.Contract != "lantai.synthetic-fixtures/v1" || len(corpus.Assets) == 0 {
		t.Fatalf("invalid synthetic corpus: %v", err)
	}
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	_, maker := e.agent("corpus-maker@synthetic", identity.RoleContributor)
	a := reopenApplication(t, e)
	servers := assetRemoteServers(t, a)
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, AllowHTTP: true, SessionToken: maker.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	work := assetRealTempDir(t)
	ws := client.WorksetInput{Schema: client.WorksetSchema}
	covered := map[string]bool{}
	type fixed struct{ AssetID, VersionID string }
	latest := map[string]fixed{}
	total := 0
	for i, asset := range corpus.Assets {
		if err = client.SafeRelative(asset.Root); err != nil {
			t.Fatal(err)
		}
		if len(asset.Files) == 0 {
			t.Fatalf("empty generated asset %s", asset.Key)
		}
		if asset.Status != "generated" || len(asset.Missing) > 0 {
			t.Logf("%s/%s: storage of supplied files only; native coverage incomplete: %v", asset.Coverage, asset.Version, asset.Missing)
		}
		dir := filepath.Join(root, filepath.FromSlash(asset.Root))
		in := client.PushInput{ProjectID: string(e.project.ProjectID), Content: client.ContentInput{AssetType: asset.Type, Files: asset.Files, Rights: asset.Rights}}
		prior, appended := latest[asset.Key]
		if appended {
			in.AssetID, in.BaseVersionID = prior.AssetID, prior.VersionID
		} else {
			in.Slug = "corpus/" + asset.Key
		}
		profile := client.AssetProfile{Characters: asset.Profile.Characters, Source: asset.Profile.Source, BusinessState: asset.Profile.BusinessState, LegacyVersion: asset.Version}
		in, err = client.ApplyAssetProfile(in, profile)
		if err != nil {
			t.Fatal(err)
		}
		in, err = client.PreparePush(t.Context(), in, dir)
		if err != nil {
			t.Fatal(err)
		}
		r, err := c.Push(t.Context(), in, dir, filepath.Join(work, fmt.Sprintf("push-%03d.json", i)), false)
		if err != nil {
			t.Fatalf("push %s/%s: %v", asset.Key, asset.Version, err)
		}
		var receipt struct {
			AssetID            string `json:"asset_id"`
			VersionID          string `json:"version_id"`
			DescriptionPending bool   `json:"description_pending"`
			VersionNumber      int64  `json:"version_number"`
		}
		if err = r.Decode(&receipt); err != nil || receipt.DescriptionPending || receipt.VersionID == "" {
			t.Fatalf("receipt: %+v %v", receipt, err)
		}
		if (!appended && receipt.VersionNumber != 1) || (appended && (receipt.VersionNumber != 2 || receipt.AssetID != prior.AssetID || receipt.VersionID == prior.VersionID)) {
			t.Fatalf("corpus version continuation: %s/%s %+v", asset.Key, asset.Version, receipt)
		}
		latest[asset.Key] = fixed{receipt.AssetID, receipt.VersionID}
		if appended {
			// Current descriptions change through their original conditional API;
			// frozen per-version profiles remain separate historical snapshots.
			patch, patchErr := client.ApplyAssetProfile(client.PushInput{}, profile)
			if patchErr != nil {
				t.Fatal(patchErr)
			}
			current, getErr := c.Do(t.Context(), http.MethodGet, "/api/v1/assets/"+receipt.AssetID+"?view=full", nil, client.Options{})
			if getErr != nil {
				t.Fatal(getErr)
			}
			if _, patchErr = c.Do(t.Context(), http.MethodPatch, "/api/v1/assets/"+receipt.AssetID+"/metadata", patch.Describe, client.Options{IfMatch: current.ETag, IdempotencyKey: fmt.Sprintf("corpus-profile-%03d", i)}); patchErr != nil {
				t.Fatal(patchErr)
			}
		}
		ws.Entries = append(ws.Entries, client.WorksetEntry{AssetID: receipt.AssetID, VersionID: receipt.VersionID, Destination: asset.Root})
		covered[asset.Coverage] = true
		total += len(asset.Files)
	}
	if len(covered) != 9 {
		t.Fatalf("corpus must explicitly describe nine groups, got %v", covered)
	}
	// Advance through the application's ordinary outbox/query coordinator;
	// search is an asynchronous projection, not the commit acknowledgement.
	if err = a.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	find, err := c.FindAssets(t.Context(), client.AssetFilter{ProjectID: string(e.project.ProjectID), Characters: []string{"hero-a"}, Source: "synthetic", BusinessState: "pending_review"}, client.AssetFindOptions{})
	if err != nil || !find.Complete || len(find.Items) != len(latest) {
		t.Fatalf("corpus find: %d/%d %+v %v", len(find.Items), len(latest), find, err)
	}
	for _, item := range find.Items {
		if item.VersionNumber != 2 {
			t.Fatalf("query did not select current v2: %+v", item)
		}
	}
	oldState, err := c.FindAssets(t.Context(), client.AssetFilter{ProjectID: string(e.project.ProjectID), Characters: []string{"hero-a"}, BusinessState: "active"}, client.AssetFindOptions{})
	if err != nil || !oldState.Complete || len(oldState.Items) != 0 {
		t.Fatalf("current description was not replaced conditionally: %+v %v", oldState, err)
	}
	for i, asset := range corpus.Assets {
		if asset.Coverage == "T08" {
			blocked := client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{ws.Entries[i]}}
			_, denyErr := c.PullWorkset(t.Context(), blocked, filepath.Join(work, fmt.Sprintf("denied-production-%d", i)), "production")
			if assetAPIErrorCode(denyErr) != errcode.UseRestricted {
				t.Fatalf("restricted source production use: %v", denyErr)
			}
		}
	}
	dst := filepath.Join(work, "downloaded")
	if retained := os.Getenv("LANTAI_ASSET_ROUNDTRIP_ROOT"); retained != "" {
		// Explicit opt-in retains only the synthetic round-trip corpus for an
		// independent native-software reopen. Never overwrite an existing root.
		absSource, sourceErr := filepath.Abs(root)
		absTarget, targetErr := filepath.Abs(retained)
		if sourceErr != nil || targetErr != nil {
			t.Fatal("invalid retained corpus root")
		}
		for _, pair := range [][2]string{{absSource, absTarget}, {absTarget, absSource}} {
			rel, relErr := filepath.Rel(pair[0], pair[1])
			if relErr != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				t.Fatal("retained corpus must be independent of source")
			}
		}
		if _, statErr := os.Lstat(absTarget); !os.IsNotExist(statErr) {
			t.Fatal("retained corpus root must not exist")
		}
		dst = absTarget
	}
	if _, err = c.PullWorkset(t.Context(), ws, dst, "archive_review"); err != nil {
		t.Fatal(err)
	}
	for _, asset := range corpus.Assets {
		for _, f := range asset.Files {
			got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(asset.Root), filepath.FromSlash(f.Path)))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(asset.Root), filepath.FromSlash(f.Path)))
			if err != nil || !bytes.Equal(got, want) || shaOf(got) != f.SHA256 || int64(len(got)) != f.Size {
				t.Fatalf("corpus byte mismatch: %s/%s/%s %v", asset.Key, asset.Version, f.Path, err)
			}
		}
	}
	if os.Getenv("LANTAI_ASSET_ROUNDTRIP_ROOT") != "" {
		if err = os.WriteFile(filepath.Join(dst, "manifest.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("synthetic storage round trip: %d declared groups, %d asset versions, %d supplied files; see native limitations above", len(covered), len(corpus.Assets), total)
}
