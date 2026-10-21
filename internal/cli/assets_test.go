package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
)

func assetInvoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	code := Run(context.Background(), append([]string{"asset"}, args...), &out, &err)
	return code, out.String(), err.String()
}

func assetDocument(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAssetClientGeneratedPlanJSONRoundTrip(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "data.txt"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	input := client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{
		Key: "scene", Directory: ".", Classification: "final",
		Push: client.PushInput{ProjectID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Slug: "scene", Content: client.ContentInput{
			AssetType: "doc", Files: []client.InputFile{{Path: "data.txt", Role: "primary"}},
			Metadata: json.RawMessage(`{"extra":{"z":1,"a":9007199254740991}}`),
		}},
	}}}
	plan, err := client.PrepareImport(t.Context(), input, source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "plan.json")
	raw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var decoded client.ImportPlan
	if err = decodeAssetInput(path, &decoded); err != nil {
		t.Fatal(err)
	}
	if err = client.ValidateImportPlan(t.Context(), decoded, source); err != nil {
		t.Fatal("a generated JSON plan changed while being read by the CLI", err)
	}
	decoded.SelectedBytes++
	if err = client.ValidateImportPlan(t.Context(), decoded, source); err == nil {
		t.Fatal("edited plan accepted")
	}
	if err = os.WriteFile(path, []byte(`{"schema":"one","schema":"two","assets":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = decodeAssetInput(path, &decoded); err == nil {
		t.Fatal("duplicate JSON keys accepted")
	}
}

func TestAssetOfflineProfileAndStrictInput(t *testing.T) {
	t.Setenv("LANTAI_SERVER", "")
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	dir := t.TempDir()
	in, output := filepath.Join(dir, "input.json"), filepath.Join(dir, "profile.json")
	assetDocument(t, in, map[string]any{
		"input":   client.PushInput{ProjectID: "synthetic-project", Content: client.ContentInput{AssetType: "doc"}},
		"profile": client.AssetProfile{Characters: []string{"hero-a"}, BusinessState: "active", LegacyState: "approved-in-old-spreadsheet", Source: "synthetic"},
	})
	code, out, stderr := assetInvoke(t, "profile", "--input", in, "--output", output, "--json")
	if code != 0 || stderr != "" || !strings.Contains(out, "character:hero-a") || !strings.Contains(out, "approved-in-old-spreadsheet") {
		t.Fatalf("profile=%d %s %s", code, out, stderr)
	}
	if _, err := client.ReadPrivate(output); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"input":{},"profile":{"business_state":"approved"}}`,
		`{"input":{},"profile":{"human_approved":true}}`,
		`{"input":{},"profile":null}`,
		`{"input":{},"profile":{"characters":null}}`,
		`{"input":{},"profile":{},"extra":true}`,
		`{"input":{},"profile":{}} {}`,
	} {
		if err := os.WriteFile(in, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if code, out, _ := assetInvoke(t, "profile", "--input", in); code == 0 || out != "" {
			t.Fatalf("unsafe profile accepted: %d %s", code, out)
		}
	}
}

func TestAssetImportPlanIsOfflineAndProtectsSource(t *testing.T) {
	t.Setenv("LANTAI_SERVER", "")
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "recipe.txt"), []byte("synthetic recipe\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "unselected.txt"), []byte("keep local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	in, plan := filepath.Join(dir, "input.json"), filepath.Join(dir, "plan.json")
	assetDocument(t, in, client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{Key: "recipe", Directory: ".", Classification: "final", Push: client.PushInput{ProjectID: "synthetic-project", Slug: "synthetic/recipe", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "recipe.txt", Role: "doc"}}}}}}})
	code, out, stderr := assetInvoke(t, "import", "plan", "--input", in, "--directory", src, "--output", plan)
	if code != 0 || stderr != "" || !strings.Contains(out, "unselected.txt") || !strings.Contains(out, "sha256") {
		t.Fatalf("plan=%d %s %s", code, out, stderr)
	}
	var frozen client.ImportPlan
	if err := client.LoadState(plan, &frozen); err != nil || frozen.SelectedFiles != 1 || frozen.SelectedBytes != 17 {
		t.Fatalf("plan state: %+v %v", frozen, err)
	}
	for _, output := range []string{in, filepath.Join(src, "result.json")} {
		if code, out, _ := assetInvoke(t, "import", "plan", "--input", in, "--directory", src, "--output", output); code == 0 || out != "" {
			t.Fatalf("unsafe output accepted: %s code=%d", output, code)
		}
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(src, alias); err != nil {
		t.Fatal(err)
	}
	if err := assetOutputOutside(filepath.Join(alias, "nested", "plan.json"), src); err == nil {
		t.Fatal("source alias output accepted")
	}
	if _, err := os.Stat(filepath.Join(src, "result.json")); !os.IsNotExist(err) {
		t.Fatalf("source was modified: %v", err)
	}
}

func TestAssetHelpAndLocalValidationBeforeAuth(t *testing.T) {
	t.Setenv("LANTAI_SERVER", "")
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	for _, args := range [][]string{{}, {"--help"}, {"profile", "--help"}, {"workset", "--help"}, {"import", "--help"}} {
		if code, out, stderr := assetInvoke(t, args...); code != 0 || !strings.Contains(out, "import apply") || stderr != "" {
			t.Fatalf("help %v = %d %s %s", args, code, out, stderr)
		}
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(in, []byte(`{"surprise":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"find", "--input", in}, {"workset", "plan", "--input", in, "--directory", filepath.Join(dir, "target")}} {
		if code, out, stderr := assetInvoke(t, args...); code != 2 || out != "" || !strings.Contains(stderr, "unknown field") || strings.Contains(stderr, "credential") {
			t.Fatalf("validation %v = %d %s %s", args, code, out, stderr)
		}
	}
}

func TestAssetFindBudgetOutputAndSessionOrigin(t *testing.T) {
	t.Setenv("LANTAI_SESSION_TOKEN", "synthetic-session")
	const asset = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	calls := 0
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/assets" || r.Header.Get("Authorization") != "Bearer synthetic-session" {
			t.Errorf("unexpected search request %s", r.URL.Path)
		}
		respond(w, map[string]any{
			"items":       []any{map[string]any{"asset_id": asset, "project_id": "01ARZ3NDEKTSV4RRFFQ69G5FAX", "version_id": "01ARZ3NDEKTSV4RRFFQ69G5FAW", "version_number": 1, "asset_type": "image", "slug": "synthetic/image", "title": "Synthetic image", "tags": []string{"character:hero-a"}, "subjects": []string{"hero-a"}, "revision": 1}},
			"next_cursor": "synthetic-next", "state": map[string]int{"generation": 1, "high_water": 0, "rebuild_start": 0},
		})
	})
	dir := t.TempDir()
	in, output := filepath.Join(dir, "find.json"), filepath.Join(dir, "result.json")
	assetDocument(t, in, map[string]any{"filter": client.AssetFilter{Characters: []string{"hero-a"}}, "options": client.AssetFindOptions{MaxPages: 1}})
	code, out, stderr := invoke(t, server, "asset", "find", "--input", in, "--output", output)
	var result client.AssetFindResult
	if json.Unmarshal([]byte(out), &result) != nil || code == 0 || result.Complete || !result.Truncated || len(result.Items) != 1 || result.Items[0].AssetID != asset || !strings.Contains(stderr, "incomplete") || calls != 1 {
		t.Fatalf("budget=%d %s %s calls=%d", code, out, stderr, calls)
	}
	if _, err := client.ReadPrivate(output); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(dir, "session.json")
	if err := client.SaveState(session, sessionFile{Schema: "lantai.client-session/v1", Origin: "http://127.0.0.1:1", Token: "wrong-origin-secret"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	code, out, stderr = invoke(t, server, "asset", "find", "--input", in, "--session-file", session)
	if code != 2 || out != "" || calls != 1 || strings.Contains(stderr, "wrong-origin-secret") {
		t.Fatalf("origin=%d %s %s calls=%d", code, out, stderr, calls)
	}
}

func TestAssetOutputsCannotOverwriteWorkingFilesOrReplayState(t *testing.T) {
	dir := t.TempDir()
	state, work := filepath.Join(dir, "state"), filepath.Join(dir, "working")
	for _, root := range []string{state, work} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	stateFile := filepath.Join(state, "scene.push.json")
	workFile := filepath.Join(work, "recipe.txt")
	for _, file := range []string{stateFile, workFile} {
		if err := os.WriteFile(file, []byte("retain exactly"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"import", "apply", "--input", "not-read.json", "--directory", filepath.Join(dir, "source"), "--state", state, "--output", stateFile},
		{"import", "verify", "--input", "not-read.json", "--destination", work, "--output", workFile},
		{"workset", "pull", "--input", "not-read.json", "--directory", work, "--output", workFile},
		{"workset", "plan", "--input", "not-read.json", "--directory", filepath.Join(dir, "future"), "--output", filepath.Join(dir, "future", "receipt.json")},
		{"workset", "pull", "--input", "not-read.json", "--directory", work, "--output", filepath.Join(dir, "WORKING", "recipe.txt")},
		{"import", "apply", "--input", "not-read.json", "--state", state, "--output", filepath.Join(dir, "STATE", "scene.push.json")},
		{"workset", "plan", "--input", "not-read.json", "--directory", filepath.Join(dir, "Caf\u00e9"), "--output", filepath.Join(dir, "Cafe\u0301", "result.json")},
	} {
		if code, out, stderr := assetInvoke(t, args...); code != 2 || out != "" || !strings.Contains(stderr, "outside") {
			t.Fatalf("unsafe output %v = %d %s %s", args, code, out, stderr)
		}
	}
	for _, file := range []string{stateFile, workFile} {
		raw, err := os.ReadFile(file)
		if err != nil || string(raw) != "retain exactly" {
			t.Fatalf("output replaced %s: %q %v", file, raw, err)
		}
	}
}
