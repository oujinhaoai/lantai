package integration

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// A18 uses synthetic notes and real fixed-version receipts. Offline copy and
// rollback never send a server mutation or edit a source note in place.
func TestAssetReferencesCLIRealVersionsCopyRollback(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	_, maker := e.agent("reference-maker@synthetic", identity.RoleContributor)
	a := reopenApplication(t, e)
	servers := assetRemoteServers(t, a)
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, AllowHTTP: true, SessionToken: maker.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	root := assetRealTempDir(t)
	source, notes := filepath.Join(root, "source"), filepath.Join(root, "notes")
	for _, dir := range []string{source, notes} {
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	in := client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "synthetic/reference", Content: client.ContentInput{AssetType: "doc", Rights: mustJSON(rightsOwned()), Files: []client.InputFile{{Path: "recipe.txt", Role: "doc"}}}}
	type fixed struct {
		AssetID   string `json:"asset_id"`
		VersionID string `json:"version_id"`
	}
	push := func(body, state string) fixed {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, "recipe.txt"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		prepared, err := client.PreparePush(t.Context(), in, source)
		if err != nil {
			t.Fatal(err)
		}
		result, err := c.Push(t.Context(), prepared, source, filepath.Join(root, state), false)
		if err != nil {
			t.Fatal(err)
		}
		var f fixed
		if err = result.Decode(&f); err != nil || f.AssetID == "" || f.VersionID == "" {
			t.Fatalf("fixed receipt: %+v %v", f, err)
		}
		return f
	}
	v1 := push("synthetic original\n", "push-v1.json")
	metaResult, err := c.Do(t.Context(), http.MethodGet, "/api/v1/meta", nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		InstanceID string `json:"instance_id"`
	}
	if err = metaResult.Decode(&meta); err != nil {
		t.Fatal(err)
	}
	uri := func(f fixed) string {
		t.Helper()
		u, err := (ids.PermanentRef{InstanceID: ids.ID(meta.InstanceID), AssetID: ids.ID(f.AssetID), VersionID: ids.ID(f.VersionID)}).URI()
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	v1URI := uri(v1)
	before := "# Synthetic notebook\r\n[Recipe](../legacy/recipe.txt)\r\n[[Unrelated note]]\r\n[Tool](tools/tool.py)\r\n"
	if err = os.WriteFile(filepath.Join(notes, "note.md"), []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	inputPath, planPath := filepath.Join(root, "references.json"), filepath.Join(root, "reference-plan.json")
	input := client.ReferenceInput{Schema: client.ReferenceInputSchema, Notes: []string{"note.md"}, Mappings: []client.ReferenceMapping{{OldTarget: "../legacy/recipe.txt", FixedRef: v1URI}}, Preserve: []client.ReferencePreserve{{Target: "tools/tool.py", Kind: "tool", Reason: "synthetic executable remains local"}}}
	if err = os.WriteFile(inputPath, mustJSON(input), 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) []byte {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := cli.Run(t.Context(), append([]string{"asset", "references"}, args...), &out, &stderr); code != 0 {
			t.Fatalf("reference CLI %v: %d %s %s", args, code, out.Bytes(), stderr.String())
		}
		return out.Bytes()
	}
	t.Setenv("LANTAI_SERVER", "")
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	invoke("plan", "--input", inputPath, "--directory", notes, "--output", planPath)
	copyRoot := filepath.Join(root, "note-copy")
	invoke("copy", "--input", planPath, "--directory", notes, "--destination", copyRoot, "--output", filepath.Join(root, "copy-receipt.json"))
	after, err := os.ReadFile(filepath.Join(copyRoot, "note.md"))
	if err != nil || !strings.Contains(string(after), v1URI) || !strings.Contains(string(after), "[[Unrelated note]]\r\n[Tool](tools/tool.py)") {
		t.Fatalf("managed copy=%q %v", after, err)
	}
	// Work produced after cutover is a real second immutable version, recorded
	// explicitly for retention rather than discovered by a vault-wide scan.
	in = client.PushInput{ProjectID: string(e.project.ProjectID), AssetID: v1.AssetID, BaseVersionID: v1.VersionID, Content: client.ContentInput{AssetType: "doc", Rights: mustJSON(rightsOwned()), Files: []client.InputFile{{Path: "recipe.txt", Role: "doc"}}}}
	v2 := push("synthetic post-cutover work\n", "push-v2.json")
	v2URI := uri(v2)
	refs := filepath.Join(root, "post-cutover-refs.json")
	if err = os.WriteFile(refs, mustJSON([]string{v2URI}), 0600); err != nil {
		t.Fatal(err)
	}
	rollbackRoot := filepath.Join(root, "rollback-copy")
	rollback := invoke("rollback", "--input", planPath, "--directory", copyRoot, "--destination", rollbackRoot, "--post-cutover-refs", refs, "--output", filepath.Join(root, "rollback-receipt.json"))
	if !bytes.Contains(rollback, []byte(v2URI)) {
		t.Fatalf("new version omitted from rollback inventory: %s", rollback)
	}
	for _, dir := range []string{notes, rollbackRoot} {
		got, err := os.ReadFile(filepath.Join(dir, "note.md"))
		if err != nil || string(got) != before {
			t.Fatalf("original text at %s=%q %v", dir, got, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(copyRoot, "note.md"))
	if err != nil || string(got) != string(after) {
		t.Fatalf("rollback rewrote managed copy: %q %v", got, err)
	}
	for i, f := range []fixed{v1, v2} {
		dir := filepath.Join(root, []string{"retained-v1", "retained-v2"}[i])
		ws := client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: f.AssetID, VersionID: f.VersionID, Destination: "recipe"}}}
		if _, err = c.PullWorkset(t.Context(), ws, dir, "archive_review"); err != nil {
			t.Fatalf("retained version %d: %v", i+1, err)
		}
		want := "synthetic original\n"
		if i == 1 {
			want = "synthetic post-cutover work\n"
		}
		body, err := os.ReadFile(filepath.Join(dir, "recipe", "recipe.txt"))
		if err != nil || string(body) != want {
			t.Fatalf("retained bytes %d=%q %v", i+1, body, err)
		}
	}
}
