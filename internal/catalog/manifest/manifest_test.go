package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
)

const (
	h1 = "1111111111111111111111111111111111111111111111111111111111111111"
	h2 = "2222222222222222222222222222222222222222222222222222222222222222"
)

func baseInput() Input {
	return Input{
		AssetType: TypeProduction,
		Files: []InputFile{
			{Path: "renders/shot 03.mp4", Role: "primary", SHA256: h1, Size: 10},
			{Path: "source/scene.blend", Role: "source", SHA256: h2, Size: 20},
		},
		Rights:   Rights{Usage: "production", License: "LicenseRef-Owned", Sensitivity: "normal"},
		Metadata: map[string]any{"chapter": "ch03", "fps": 24, "subjects": []any{"fourth-sister"}},
	}
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	e, ok := errcode.As(err)
	if !ok || len(e.Details) == 0 {
		t.Fatalf("want structured error with details, got %v", err)
	}
	return e.Details[0].Reason
}

func TestNormalizeSortsAndFreezes(t *testing.T) {
	in := baseInput()
	in.Files[0], in.Files[1] = in.Files[1], in.Files[0]
	in.Files[0].Path = "source/scéne.blend" // NFD，规范化后变成 NFC
	c, err := Normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	if c.Files[0].Path != "renders/shot 03.mp4" || c.Files[1].Path != "source/scéne.blend" {
		t.Fatalf("files = %+v", c.Files)
	}
	if c.TypeSchema != TypesContract || c.Uses == nil || len(c.Uses) != 0 {
		t.Fatalf("content = %+v", c)
	}
	d1, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	// 同一语义的输入（顺序、数字写法不同）得到同一摘要。
	again := baseInput()
	again.Files[1].Path = "source/scéne.blend"
	again.Metadata = map[string]any{"subjects": []any{"fourth-sister"}, "fps": 24.0, "chapter": "ch03"}
	c2, err := Normalize(again)
	if err != nil {
		t.Fatal(err)
	}
	if d2, _ := c2.Digest(); d2 != d1 {
		t.Fatalf("digest differs for the same content: %s vs %s", d1, d2)
	}
	changed := baseInput()
	changed.Files[1].Role = "recipe"
	c3, _ := Normalize(changed)
	if d3, _ := c3.Digest(); d3 == d1 {
		t.Fatal("a different role must change the digest")
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
		code   errcode.Code
		reason string
	}{
		{"unknown type", func(in *Input) { in.AssetType = "hologram" }, errcode.SchemaInvalid, "asset_type"},
		{"no files", func(in *Input) { in.Files = nil }, errcode.SchemaInvalid, "no_files"},
		{"traversal", func(in *Input) { in.Files[0].Path = "../x" }, errcode.SchemaInvalid, "dot_segment"},
		{"windows reserved", func(in *Input) { in.Files[0].Path = "renders/aux.mp4" }, errcode.SchemaInvalid, "reserved_name"},
		{"case twins", func(in *Input) {
			in.Files[1] = InputFile{Path: "Renders/Shot 03.mp4", Role: "preview", SHA256: h2, Size: 20}
		}, errcode.PathConflict, "same_name_after_normalization"},
		{"file and directory", func(in *Input) {
			in.Files[1] = InputFile{Path: "renders/shot 03.mp4/inner", Role: "preview", SHA256: h2, Size: 20}
		}, errcode.PathConflict, "file_directory_conflict"},
		{"unknown role", func(in *Input) { in.Files[0].Role = "thumbnail" }, errcode.SchemaInvalid, "role"},
		{"bad hash", func(in *Input) { in.Files[0].SHA256 = strings.Repeat("AB", 32) }, errcode.SchemaInvalid, "sha256"},
		{"negative size", func(in *Input) { in.Files[0].Size = -1 }, errcode.SchemaInvalid, "size"},
		{"license in metadata", func(in *Input) { in.Metadata["license"] = "CC0-1.0" }, errcode.SchemaInvalid, "reserved_metadata_field"},
		{"license in extra", func(in *Input) { in.Metadata["extra"] = map[string]any{"license": "CC0-1.0"} }, errcode.SchemaInvalid, "reserved_metadata_field"},
		{"invalid UTF-8", func(in *Input) { in.VersionNote = string([]byte{0xff}) }, errcode.SchemaInvalid, "invalid_utf8"},
		{"bad license", func(in *Input) { in.Rights.License = "MIT OR" }, errcode.SchemaInvalid, "license"},
		{"bad usage", func(in *Input) { in.Rights.Usage = "anything" }, errcode.SchemaInvalid, "usage"},
		{"bad sensitivity", func(in *Input) { in.Rights.Sensitivity = "secret" }, errcode.SchemaInvalid, "sensitivity"},
		{"unresolved use", func(in *Input) { in.Uses = []Use{{Relation: "uses"}} }, errcode.SchemaInvalid, "use_ref"},
		{"bad relation", func(in *Input) {
			in.Uses = []Use{{InstanceID: ids.New(), AssetID: ids.New(), VersionID: ids.New(), Relation: "likes"}}
		}, errcode.SchemaInvalid, "relation"},
	}
	for _, c := range cases {
		in := baseInput()
		c.mutate(&in)
		_, err := Normalize(in)
		if errcode.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", c.name, err, c.code)
			continue
		}
		if got := reasonOf(t, err); got != c.reason {
			t.Errorf("%s: reason %s, want %s", c.name, got, c.reason)
		}
	}
	// 类型元数据取值不合法：按类型 schema 拒绝。
	in := baseInput()
	in.Metadata["fps"] = -1
	if _, err := Normalize(in); errcode.CodeOf(err) != errcode.SchemaInvalid {
		t.Fatalf("invalid fps: %v", err)
	}
}

func TestFrozenMetadataDoesNotAliasInput(t *testing.T) {
	in := baseInput()
	in.Metadata["extra"] = map[string]any{"nested": map[string]any{"value": "original"}}
	c, err := Normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := c.Digest()
	in.Metadata["subjects"].([]any)[0] = "changed"
	in.Metadata["extra"].(map[string]any)["nested"].(map[string]any)["value"] = "changed"
	if after, _ := c.Digest(); after != before {
		t.Fatal("mutating the request changed the frozen metadata")
	}
}

// 未登记的元数据字段放进 extra，不拒收；与 extra 已有的不同值冲突时拒绝。
func TestUnknownMetadataGoesToExtra(t *testing.T) {
	in := baseInput()
	in.Metadata["mixamo_product"] = "abc"
	in.Metadata["extra"] = map[string]any{"keep": true}
	c, err := Normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	extra, _ := c.Metadata["extra"].(map[string]any)
	if extra["mixamo_product"] != "abc" || extra["keep"] != true || c.Metadata["mixamo_product"] != nil {
		t.Fatalf("metadata = %v", c.Metadata)
	}
	in = baseInput()
	in.Metadata["x"] = 1
	in.Metadata["extra"] = map[string]any{"x": 2}
	if _, err := Normalize(in); reasonOf(t, err) != "extra_conflict" {
		t.Fatal("conflicting extra accepted")
	}
	if got := KnownMetadata(TypeMotion); len(got) == 0 || got[0] != "body_model" {
		t.Fatalf("known motion metadata = %v", got)
	}
}

func TestUsesAreSortedAndDeduplicated(t *testing.T) {
	a, b := ids.MustParse("01J8Z3K4M5N6P7Q8R9S0T100ZA"), ids.MustParse("01J8Z3K4M5N6P7Q8R9S0T100ZB")
	inst := ids.New()
	in := baseInput()
	in.Uses = []Use{
		{InstanceID: inst, AssetID: b, VersionID: b, Relation: "uses"},
		{InstanceID: inst, AssetID: a, VersionID: a, Relation: "reference", Declared: "lib/x@v001"},
		{InstanceID: inst, AssetID: b, VersionID: b, Relation: "uses"},
	}
	c, err := Normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Uses) != 2 || c.Uses[0].AssetID != a || c.Uses[1].AssetID != b {
		t.Fatalf("uses = %+v", c.Uses)
	}
}

func TestDocumentRoundTrip(t *testing.T) {
	in := baseInput()
	in.VersionNote = "调整落点\n镜 3 机位后移  "
	in.Metadata["extra"] = map[string]any{"ratio": 0.5, "flag": "true", "code": "007"}
	c, err := Normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	dg, _ := c.Digest()
	doc := Document{InstanceID: ids.New(), ProjectID: ids.New(), AssetID: ids.New(), VersionID: ids.New(), VersionNumber: 3,
		OperationID: ids.New(), CreatedBy: ids.New(), ManifestDigest: dg, Content: c}
	raw, err := doc.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "contract: lantai.manifest/v1\n") {
		t.Fatalf("rendering does not lead with the contract:\n%s", raw)
	}
	again, err := doc.Render()
	if err != nil || string(again) != string(raw) {
		t.Fatal("rendering is not deterministic")
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if parsed.VersionID != doc.VersionID || parsed.ManifestDigest != dg || parsed.Content.VersionNote != in.VersionNote {
		t.Fatalf("parsed = %+v", parsed)
	}
	if d, _ := parsed.Content.Digest(); d != dg {
		t.Fatalf("digest after parse = %s, want %s", d, dg)
	}
	// 字符串形式的 "true" 与 "007" 保持为字符串。
	tree, _ := yamljson.Decode(raw)
	extra := tree.(map[string]any)["content"].(map[string]any)["metadata"].(map[string]any)["extra"].(map[string]any)
	if extra["flag"] != "true" || extra["code"] != "007" || extra["ratio"] != json.Number("0.5") {
		t.Fatalf("scalars changed type: %#v", extra)
	}
	// 改动内容后摘要对不上。
	tampered := strings.Replace(string(raw), "ch03", "ch04", 1)
	if _, err := Parse([]byte(tampered)); errcode.CodeOf(err) != errcode.HashMismatch {
		t.Fatalf("tampered manifest: %v", err)
	}
	bad := doc
	bad.ManifestDigest = "sha256:" + h1
	if _, err := bad.Render(); err == nil {
		t.Fatal("rendering a document with a wrong digest must fail")
	}
}

func TestCheckLicense(t *testing.T) {
	for _, ok := range []string{"MIT", "CC0-1.0", "LicenseRef-Mixamo", "Apache-2.0 OR MIT", "GPL-2.0+",
		"(MIT AND BSD-3-Clause) OR LicenseRef-Proprietary-Extracted", "GPL-2.0-only WITH Classpath-exception-2.0",
		"DocumentRef-spdx-tool-1.2:LicenseRef-MIT-Style-2"} {
		if err := CheckLicense(ok); err != nil {
			t.Errorf("CheckLicense(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "MIT OR", "AND MIT", "(MIT", "MIT)", "MIT and BSD", "MIT WITH", "MIT/BSD", "LicenseRef-a b c", strings.Repeat("M", 257)} {
		if err := CheckLicense(bad); err == nil {
			t.Errorf("CheckLicense(%q) accepted", bad)
		}
	}
}
