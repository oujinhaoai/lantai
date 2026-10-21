package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// This API fixture contains only synthetic bytes. It checks the exact HTTP
// version/grant binding while retaining the production Download implementation.
type worksetFixture struct {
	t           *testing.T
	mu          sync.Mutex
	c           *Client
	instance    string
	meta        any
	responses   map[string]map[string]any
	files       map[string][]InputFile
	data        map[string]string
	requests    []string
	ranges      []string
	shortOnce   bool
	deny        bool
	onGrant     func()
	onTransfer  func()
	changeGrant func(*DownloadGrant)
}

func newWorksetFixture(t *testing.T) *worksetFixture {
	f := &worksetFixture{t: t, instance: string(ids.New()), responses: map[string]map[string]any{}, files: map[string][]InputFile{}, data: map[string]string{}}
	f.meta = map[string]any{"api_version": "v1", "instance_id": f.instance}
	f.c = newTestClient(t, f.serve)
	return f
}

func (f *worksetFixture) entry(destination string, files map[string]string) WorksetEntry {
	e := WorksetEntry{AssetID: string(ids.New()), VersionID: string(ids.New()), Destination: destination}
	var specs []InputFile
	for p, data := range files {
		specs = append(specs, InputFile{Path: p, Role: "source", SHA256: sha(data), Size: int64(len(data))})
		f.data[sha(data)] = data
	}
	return f.entryWithSpecs(e, specs)
}

func (f *worksetFixture) entryWithSpecs(e WorksetEntry, specs []InputFile) WorksetEntry {
	content := map[string]any{"asset_type": "production", "type_schema": "lantai.asset-types/v1", "files": specs, "uses": []any{}, "metadata": map[string]any{},
		"rights": map[string]any{"usage": "production", "license": "CC0-1.0", "redistribute_raw": true, "noai": false, "sensitivity": "normal"}}
	raw, err := json.Marshal(content)
	if err != nil {
		f.t.Fatal(err)
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		f.t.Fatal(err)
	}
	d := digest.Of(canonical)
	key := "/api/v1/assets/" + e.AssetID + "/versions/" + e.VersionID
	f.responses[key] = map[string]any{
		"version":  map[string]any{"asset_id": e.AssetID, "version_id": e.VersionID, "manifest_digest": d},
		"manifest": map[string]any{"contract": "lantai.manifest/v1", "instance_id": f.instance, "asset_id": e.AssetID, "version_id": e.VersionID, "manifest_digest": d, "content": content},
	}
	f.files[key] = specs
	return e
}

func (f *worksetFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer private-session" {
		f.t.Error("missing authorization")
	}
	if r.Method == "GET" && r.URL.Path == "/api/v1/meta" {
		writeJSON(w, f.meta)
		return
	}
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/") {
		if r.URL.Query().Get("view") != "full" {
			f.t.Error("full exact manifest was not requested")
		}
		response, ok := f.responses[r.URL.Path]
		if !ok {
			f.t.Errorf("unexpected API read %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		writeJSON(w, response)
		return
	}
	if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/read-grants") {
		if f.deny {
			w.WriteHeader(403)
			writeJSON(w, map[string]any{})
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Error(err)
			return
		}
		if body["purpose"] != "production" {
			f.t.Errorf("purpose=%s", body["purpose"])
		}
		key := strings.TrimSuffix(r.URL.Path, "/read-grants")
		for _, spec := range f.files[key] {
			if spec.Path == body["path"] {
				g := DownloadGrant{URL: "/xfer/" + spec.SHA256, SHA256: spec.SHA256, Size: spec.Size, Path: spec.Path}
				if f.changeGrant != nil {
					f.changeGrant(&g)
				}
				if f.onGrant != nil {
					f.onGrant()
				}
				writeJSON(w, g)
				return
			}
		}
		f.t.Errorf("grant requested unbound path %s", body["path"])
		w.WriteHeader(404)
		return
	}
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/xfer/") {
		data, ok := f.data[strings.TrimPrefix(r.URL.Path, "/xfer/")]
		if !ok {
			f.t.Error("unknown transfer hash")
			w.WriteHeader(404)
			return
		}
		if f.onTransfer != nil {
			f.onTransfer()
		}
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		if r.Header.Get("Range") != "" {
			var n int
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &n); err != nil {
				f.t.Error(err)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", n, len(data)-1, len(data)))
			w.Header().Set("Content-Length", fmt.Sprint(len(data)-n))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, data[n:])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if f.shortOnce {
			f.shortOnce = false
			_, _ = io.WriteString(w, data[:len(data)/2])
			return
		}
		_, _ = io.WriteString(w, data)
		return
	}
	f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	w.WriteHeader(404)
}

func (f *worksetFixture) calls() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), append([]string(nil), f.ranges...)
}

func worksetTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir()) // macOS /var is an OS alias.
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func worksetInput(entries ...WorksetEntry) WorksetInput {
	return WorksetInput{Schema: WorksetSchema, Entries: entries}
}

func TestWorksetPlanAndPullFixedWorkingCopy(t *testing.T) {
	f := newWorksetFixture(t)
	project := f.entry("project", map[string]string{"scene.blend": "synthetic scene", "workflow/ui.json": "{\"nodes\":[]}"})
	texture := f.entry("textures", map[string]string{"color.png": "synthetic texture"})
	in := worksetInput(project, texture)
	in.Entrypoints = []string{"project/scene.blend", "project/workflow/ui.json"}
	in.Dependencies = []WorksetDependency{{From: "project/scene.blend", Requires: []string{"textures/color.png"}}}
	dir := filepath.Join(worksetTemp(t), "not-created")
	p, err := f.c.PlanWorkset(t.Context(), in, dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Schema != WorksetPlanSchema || p.State != "planned" || p.FileCount != 3 || !p.InputDigest.Valid() {
		t.Fatalf("plan=%+v", p)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("plan created root: %v", err)
	}
	requests, ranges := f.calls()
	if len(requests) != 3 || requests[0] != "GET /api/v1/meta" || len(ranges) != 0 {
		t.Fatalf("plan requests=%v ranges=%v", requests, ranges)
	}
	for _, e := range p.Entries {
		if !e.ManifestDigest.Valid() || e.InstanceID != f.instance {
			t.Fatal("missing manifest binding")
		}
	}
	r, err := f.c.PullWorkset(t.Context(), in, dir, "production")
	if err != nil || r.State != "completed" || r.Schema != WorksetReceiptSchema {
		t.Fatalf("receipt=%+v error=%v", r, err)
	}
	for _, e := range r.Entries {
		for _, file := range e.Files {
			actual, size, err := HashFile(t.Context(), filepath.Join(dir, filepath.FromSlash(file.Path)))
			if err != nil || actual != file.SHA256 || size != file.Size || file.State != "verified" {
				t.Fatalf("file=%+v error=%v", file, err)
			}
		}
	}
	p2, err := f.c.PlanWorkset(t.Context(), in, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p2.Entries {
		for _, file := range e.Files {
			if file.State != "present" {
				t.Fatal("existing file not verified in plan")
			}
		}
	}
	r2, err := f.c.PullWorkset(t.Context(), in, dir, "production")
	if err != nil || r2.InputDigest != r.InputDigest || !reflect.DeepEqual(r2.Entries, r.Entries) {
		t.Fatalf("rerun=%+v %v", r2, err)
	}
	_, ranges = f.calls()
	if len(ranges) != 3 {
		t.Fatalf("rerun redownloaded files: %v", ranges)
	}
	raw, _ := json.Marshal(r2)
	if strings.Contains(string(raw), "private-session") || strings.Contains(string(raw), "/xfer/") {
		t.Fatal("receipt contains credentials/grant URL")
	}
}

func TestWorksetStaticPreflightBeforeAnyNetwork(t *testing.T) {
	cases := map[string][]string{
		"same": {"output", "output"}, "case": {"output", "OUTPUT"}, "nested": {"output", "output/nested"},
		"unicode": {"café", "cafe\u0301"}, "directory-case": {"A/x", "a/y"}, "absolute": {"/outside"}, "dot": {"../outside"}, "backslash": {"a\\b"}, "reserved": {"COM0"},
	}
	for name, targets := range cases {
		t.Run(name, func(t *testing.T) {
			f := newWorksetFixture(t)
			var entries []WorksetEntry
			for _, target := range targets {
				entries = append(entries, f.entry(target, map[string]string{"file": "safe"}))
			}
			dir := filepath.Join(worksetTemp(t), "absent")
			if _, err := f.c.PullWorkset(t.Context(), worksetInput(entries...), dir, "production"); err == nil {
				t.Fatal("unsafe workset accepted")
			}
			requests, _ := f.calls()
			if len(requests) != 0 {
				t.Fatalf("static preflight sent requests: %v", requests)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("static preflight created root")
			}
		})
	}
}

func TestWorksetRejectsMalformedManifestBeforeWrites(t *testing.T) {
	cases := map[string][]string{
		"escape": {"../out"}, "absolute": {"/out"}, "backslash": {"a\\b"}, "nfd": {"cafe\u0301.txt"},
		"case": {"a.txt", "A.txt"}, "fold": {"Å.txt", "å.txt"}, "parent": {"a", "a/b"}, "suffix": {"a.lantai-part"}, "directory-case": {"A/x", "a/y"},
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			f := newWorksetFixture(t)
			files := map[string]string{}
			for _, p := range paths {
				files[p] = "safe"
			}
			e := f.entry("output", files)
			dir := filepath.Join(worksetTemp(t), "absent")
			if _, err := f.c.PullWorkset(t.Context(), worksetInput(e), dir, "production"); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
			requests, ranges := f.calls()
			if len(requests) != 2 || len(ranges) != 0 {
				t.Fatalf("download occurred before full preflight: %v", requests)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("bad manifest created root")
			}
		})
	}
}

func TestWorksetMissingDependencyAndEntrypoint(t *testing.T) {
	for _, which := range []string{"entrypoint", "from", "required"} {
		t.Run(which, func(t *testing.T) {
			f := newWorksetFixture(t)
			e := f.entry("project", map[string]string{"scene.blend": "scene"})
			in := worksetInput(e)
			switch which {
			case "entrypoint":
				in.Entrypoints = []string{"project/missing.blend"}
			case "from":
				in.Dependencies = []WorksetDependency{{From: "project/missing", Requires: []string{"project/scene.blend"}}}
			case "required":
				in.Dependencies = []WorksetDependency{{From: "project/scene.blend", Requires: []string{"textures/missing.png"}}}
			}
			dir := filepath.Join(worksetTemp(t), "absent")
			if _, err := f.c.PullWorkset(t.Context(), in, dir, "production"); err == nil || !strings.Contains(err.Error(), "required path is absent") {
				t.Fatalf("error=%v", err)
			}
			requests, _ := f.calls()
			if len(requests) != 2 {
				t.Fatalf("missing dependency requested a grant: %v", requests)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("missing dependency created root")
			}
		})
	}
}

func TestWorksetPreservesDifferentExistingContentBeforeAnyGrant(t *testing.T) {
	f := newWorksetFixture(t)
	a := f.entry("first", map[string]string{"file": "first"})
	b := f.entry("second", map[string]string{"file": "second"})
	dir := worksetTemp(t)
	if err := os.Mkdir(filepath.Join(dir, "second"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second/file"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.PullWorkset(t.Context(), worksetInput(a, b), dir, "production"); err == nil {
		t.Fatal("existing conflict accepted")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "second/file"))
	if string(data) != "keep" {
		t.Fatal("existing file overwritten")
	}
	if _, err := os.Stat(filepath.Join(dir, "first")); !os.IsNotExist(err) {
		t.Fatal("earlier asset written before later conflict was checked")
	}
	requests, _ := f.calls()
	if len(requests) != 3 {
		t.Fatalf("grant requested before full conflict check: %v", requests)
	}
}

func TestWorksetSymlinksAndExistingNameAliases(t *testing.T) {
	for _, kind := range []string{"root", "root-parent", "destination", "nested", "file", "part", "case", "nfd"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorksetFixture(t)
			e := f.entry("output", map[string]string{"nested/file": "safe"})
			base, outside := worksetTemp(t), worksetTemp(t)
			dir := filepath.Join(base, "work")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			symlink := func(target, name string) {
				if err := os.Symlink(target, name); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			switch kind {
			case "root":
				os.Remove(dir)
				symlink(outside, dir)
			case "root-parent":
				symlink(outside, filepath.Join(base, "alias"))
				dir = filepath.Join(base, "alias/work")
			case "destination":
				symlink(outside, filepath.Join(dir, "output"))
			case "nested":
				os.Mkdir(filepath.Join(dir, "output"), 0700)
				symlink(outside, filepath.Join(dir, "output/nested"))
			case "file", "part":
				os.MkdirAll(filepath.Join(dir, "output/nested"), 0700)
				name := "output/nested/file"
				if kind == "part" {
					name += ".lantai-part"
				}
				symlink(filepath.Join(outside, "secret"), filepath.Join(dir, name))
			case "case":
				os.Mkdir(filepath.Join(dir, "OUTPUT"), 0700)
			case "nfd":
				e.Destination = "café"
				os.Mkdir(filepath.Join(dir, "cafe\u0301"), 0700)
			}
			if _, err := f.c.PullWorkset(t.Context(), worksetInput(e), dir, "production"); err == nil {
				t.Fatal("symlink/alias accepted")
			}
			_, ranges := f.calls()
			if len(ranges) != 0 {
				t.Fatal("symlink/alias transferred bytes")
			}
			contents, err := os.ReadDir(outside)
			if err != nil || len(contents) != 0 {
				t.Fatalf("wrote outside destination: %v %v", contents, err)
			}
		})
	}
}

func TestWorksetResumeAndAuthorizationRecheckedOnRerun(t *testing.T) {
	f := newWorksetFixture(t)
	e := f.entry("output", map[string]string{"file": "0123456789"})
	in, dir := worksetInput(e), worksetTemp(t)
	f.shortOnce = true
	r, err := f.c.PullWorkset(t.Context(), in, dir, "production")
	if err == nil || r.State != "partial" {
		t.Fatalf("short response accepted: %+v %v", r, err)
	}
	partial, _ := os.ReadFile(filepath.Join(dir, "output/file.lantai-part"))
	if string(partial) != "01234" {
		t.Fatalf("partial=%q", partial)
	}
	f.mu.Lock()
	f.deny = true
	f.mu.Unlock()
	if _, err = f.c.PullWorkset(t.Context(), in, dir, "production"); err == nil {
		t.Fatal("withdrawn grant accepted")
	}
	f.mu.Lock()
	f.deny = false
	f.mu.Unlock()
	r, err = f.c.PullWorkset(t.Context(), in, dir, "production")
	if err != nil || r.State != "completed" {
		t.Fatalf("resume=%+v %v", r, err)
	}
	_, ranges := f.calls()
	if !reflect.DeepEqual(ranges, []string{"", "bytes=5-"}) {
		t.Fatalf("ranges=%v", ranges)
	}
	if _, err = os.Stat(filepath.Join(dir, "output/file.lantai-part")); !os.IsNotExist(err) {
		t.Fatal("completed partial retained")
	}
	if _, err = f.c.PullWorkset(t.Context(), in, dir, "production"); err != nil {
		t.Fatal(err)
	}
	_, ranges = f.calls()
	if len(ranges) != 2 {
		t.Fatal("completed file downloaded again")
	}
}

func TestWorksetRejectsVersionManifestAndGrantMismatch(t *testing.T) {
	for _, which := range []string{"version-asset", "version-id", "manifest-asset", "manifest-id", "manifest-digest", "content-digest", "missing", "grant-path", "grant-sha", "grant-size"} {
		t.Run(which, func(t *testing.T) {
			f := newWorksetFixture(t)
			e := f.entry("output", map[string]string{"file": "safe"})
			key := "/api/v1/assets/" + e.AssetID + "/versions/" + e.VersionID
			response := f.responses[key]
			v, m := response["version"].(map[string]any), response["manifest"].(map[string]any)
			switch which {
			case "version-asset":
				v["asset_id"] = string(ids.New())
			case "version-id":
				v["version_id"] = string(ids.New())
			case "manifest-asset":
				m["asset_id"] = string(ids.New())
			case "manifest-id":
				m["version_id"] = string(ids.New())
			case "manifest-digest":
				m["manifest_digest"] = digest.Of([]byte("other"))
			case "content-digest":
				m["content"].(map[string]any)["version_note"] = "changed"
			case "missing":
				delete(response, "manifest")
			case "grant-path":
				f.changeGrant = func(g *DownloadGrant) { g.Path = "other" }
			case "grant-sha":
				f.changeGrant = func(g *DownloadGrant) { g.SHA256 = sha("other") }
			case "grant-size":
				f.changeGrant = func(g *DownloadGrant) { g.Size++ }
			}
			dir := worksetTemp(t)
			if _, err := f.c.PullWorkset(t.Context(), worksetInput(e), dir, "production"); err == nil {
				t.Fatal("unbound manifest/grant accepted")
			}
			_, ranges := f.calls()
			if len(ranges) != 0 {
				t.Fatal("unbound data transferred")
			}
			if _, err := os.Stat(filepath.Join(dir, "output")); !os.IsNotExist(err) {
				t.Fatal("mismatch wrote output")
			}
		})
	}
}

func TestWorksetRejectsWrongOrMixedInstanceBeforeGrantsAndWrites(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
			f := newWorksetFixture(t)
			first := f.entry("first", map[string]string{"file": "first"})
			entries := []WorksetEntry{first}
			wrong := first
			if mixed {
				wrong = f.entry("second", map[string]string{"file": "second"})
				entries = append(entries, wrong)
			}
			key := "/api/v1/assets/" + wrong.AssetID + "/versions/" + wrong.VersionID
			// A different, fully valid ULID is still the wrong archive instance.
			f.responses[key]["manifest"].(map[string]any)["instance_id"] = string(ids.New())
			dir := filepath.Join(worksetTemp(t), "absent")
			if _, err := f.c.PullWorkset(t.Context(), worksetInput(entries...), dir, "production"); err == nil || !strings.Contains(err.Error(), "REF_MISMATCH") {
				t.Fatalf("wrong instance accepted: %v", err)
			}
			requests, ranges := f.calls()
			if len(requests) != len(entries)+1 || requests[0] != "GET /api/v1/meta" || len(ranges) != 0 {
				t.Fatalf("wrong instance reached grants/transfer: %v %v", requests, ranges)
			}
			for _, request := range requests {
				if !strings.HasPrefix(request, "GET ") {
					t.Fatalf("write/grant requested: %s", request)
				}
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("wrong instance created destination")
			}
		})
	}
}

func TestWorksetRejectsInvalidMetadataBeforeManifestOrWrites(t *testing.T) {
	validID := string(ids.New())
	cases := map[string]any{
		"null":                  nil,
		"array":                 []string{},
		"missing-instance":      map[string]any{"api_version": "v1"},
		"invalid-instance":      map[string]any{"api_version": "v1", "instance_id": "not-an-id"},
		"noncanonical-instance": map[string]any{"api_version": "v1", "instance_id": "01j00000000000000000000001"},
		"numeric-instance":      map[string]any{"api_version": "v1", "instance_id": 42},
		"missing-api":           map[string]any{"instance_id": validID},
		"wrong-api":             map[string]any{"api_version": "v2", "instance_id": validID},
		"duplicate-instance":    json.RawMessage(fmt.Sprintf(`{"api_version":"v1","instance_id":%q,"instance_id":%q}`, validID, validID)),
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			f := newWorksetFixture(t)
			f.meta = meta
			e := f.entry("output", map[string]string{"file": "safe"})
			dir := filepath.Join(worksetTemp(t), "absent")
			if _, err := f.c.PullWorkset(t.Context(), worksetInput(e), dir, "production"); err == nil {
				t.Fatal("invalid metadata accepted")
			}
			requests, ranges := f.calls()
			if !reflect.DeepEqual(requests, []string{"GET /api/v1/meta"}) || len(ranges) != 0 {
				t.Fatalf("invalid metadata continued: %v %v", requests, ranges)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("invalid metadata created destination")
			}
		})
	}
}

func TestWorksetRootAndDescendantReplacementCannotEscape(t *testing.T) {
	for _, when := range []string{"grant-root", "transfer-root", "grant-descendant"} {
		t.Run(when, func(t *testing.T) {
			f := newWorksetFixture(t)
			e := f.entry("output", map[string]string{"file": "safe"})
			base, outside := worksetTemp(t), worksetTemp(t)
			dir := filepath.Join(base, "work")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			replace := func() {
				source := dir
				if when == "grant-descendant" {
					source = filepath.Join(dir, "output")
					os.Mkdir(source, 0700)
				}
				if err := os.Rename(source, source+"-retained"); err != nil {
					t.Error(err)
					return
				}
				if err := os.Symlink(outside, source); err != nil {
					t.Error(err)
				}
			}
			if when == "transfer-root" {
				f.onTransfer = replace
			} else {
				f.onGrant = replace
			}
			if _, err := f.c.PullWorkset(t.Context(), worksetInput(e), dir, "production"); err == nil {
				t.Fatal("replaced destination reported success")
			}
			contents, err := os.ReadDir(outside)
			if err != nil || len(contents) != 0 {
				t.Fatalf("escaped pinned root: %v %v", contents, err)
			}
			if when == "transfer-root" {
				data, err := os.ReadFile(filepath.Join(base, "work-retained/output/file"))
				if err != nil || string(data) != "safe" {
					t.Fatalf("pinned original file=%q %v", data, err)
				}
			}
		})
	}
}

func TestWorksetLimitsAndStrictJSON(t *testing.T) {
	valid := `{"schema":"lantai.client-workset/v1","entries":[]}`
	for _, raw := range []string{valid + ` {}`, strings.Replace(valid, `"entries":[]`, `"entries":[],"unexpected":true`, 1), strings.Replace(valid, `"entries":[]`, `"entries":[],"entries":[]`, 1), strings.Repeat(" ", int(MaxJSONBytes)+1)} {
		if _, err := DecodeWorkset(strings.NewReader(raw)); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	if _, err := DecodeWorkset(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	f := newWorksetFixture(t)
	e := f.entry("output", map[string]string{"file": "safe"})
	in := worksetInput(e)
	in.Entries = make([]WorksetEntry, MaxWorksetEntries+1)
	if err := ValidateWorkset(in, worksetTemp(t)); err == nil {
		t.Fatal("entry limit ignored")
	}
	for _, which := range []string{"files", "bytes"} {
		t.Run(which, func(t *testing.T) {
			f := newWorksetFixture(t)
			e := WorksetEntry{AssetID: string(ids.New()), VersionID: string(ids.New()), Destination: "output"}
			specs := []InputFile{{Path: "large.bin", SHA256: sha(""), Size: MaxWorksetBytes + 1}}
			if which == "files" {
				specs = make([]InputFile, MaxWorksetFiles+1)
				for i := range specs {
					specs[i] = InputFile{Path: fmt.Sprintf("file-%05d", i), SHA256: sha(""), Size: 0}
				}
			}
			f.entryWithSpecs(e, specs)
			if _, err := f.c.PlanWorkset(context.Background(), worksetInput(e), worksetTemp(t)); err == nil {
				t.Fatal("workset budget ignored")
			}
			requests, _ := f.calls()
			if len(requests) != 2 {
				t.Fatalf("limit made extra requests: %v", requests)
			}
		})
	}
}
