package client

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAssetProfileProjectionPreservesMetadataAndRights(t *testing.T) {
	rights := json.RawMessage(`{"usage":"reference","license":"LicenseRef-Synthetic-Unknown","redistribute_raw":false,"noai":false,"sensitivity":"normal"}`)
	in := PushInput{
		ProjectID: "00000000000000000000000001", Slug: "hero-a",
		Content:  ContentInput{AssetType: "motion", Rights: rights, Metadata: json.RawMessage(`{"fps":30,"extra":{"frame_counter":9007199254740993,"custom":"kept"}}`)},
		Describe: json.RawMessage(`{"title":"Synthetic motion","summary":"Do not infer approval","tags":["freeform"],"subjects":["scene-a"],"extra":{"custom":"kept"}}`),
	}
	p := AssetProfile{Characters: []string{" hero-a ", "hero-a"}, Source: "mixamo", BusinessState: "pending_review", LegacyState: "awaiting-human-review", LegacyVersion: "v007", SourcePaths: []string{"synthetic/motion.fbx"}, Notes: []string{"synthetic-note.md"}}
	out, err := ApplyAssetProfile(in, p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Content.Rights, rights) || !strings.Contains(string(out.Content.Metadata), "9007199254740993") || string(in.Describe) == string(out.Describe) {
		t.Fatal("rights, exact numbers or input preservation failed")
	}
	var d struct {
		Title    string                 `json:"title"`
		Tags     []string               `json:"tags"`
		Subjects []string               `json:"subjects"`
		Extra    map[string]interface{} `json:"extra"`
	}
	if err := json.Unmarshal(out.Describe, &d); err != nil {
		t.Fatal(err)
	}
	if d.Title != "Synthetic motion" || d.Extra["custom"] != "kept" || !reflect.DeepEqual(d.Tags, []string{"character:hero-a", "freeform", "legacy-version:v007", "source:mixamo", "state:pending_review"}) || !reflect.DeepEqual(d.Subjects, []string{"hero-a", "scene-a"}) {
		t.Fatalf("unexpected description: %s", out.Describe)
	}
	var m struct {
		FPS   int `json:"fps"`
		Extra struct {
			Profile AssetProfile `json:"asset_profile"`
		} `json:"extra"`
	}
	if err := json.Unmarshal(out.Content.Metadata, &m); err != nil || m.FPS != 30 || m.Extra.Profile.LegacyState != "awaiting-human-review" || m.Extra.Profile.Schema != AssetProfileSchema {
		t.Fatalf("unexpected frozen snapshot: %s", out.Content.Metadata)
	}
	again, err := ApplyAssetProfile(out, p)
	if err != nil || !bytes.Equal(out.Describe, again.Describe) || !bytes.Equal(out.Content.Metadata, again.Content.Metadata) {
		t.Fatalf("profile replay drifted: %v", err)
	}
	// Committed bytes remain a committed version when initial description is
	// pending; the adapter must not encourage a new upload or grant privileges.
	notice := CommitNotice(Response{Body: json.RawMessage(`{"asset_id":"00000000000000000000000001","version_id":"00000000000000000000000002","committed_at":"2026-01-01T00:00:00Z","description_pending":true,"description_error":"FORBIDDEN"}`)})
	if !strings.Contains(notice, "initial metadata") && !strings.Contains(notice, "metadata is pending") {
		t.Fatalf("pending metadata was lost: %s", notice)
	}
}

func TestAssetProfileUnknownStatesAndStrictInput(t *testing.T) {
	for _, state := range []string{"active", "reference", "rejected", "superseded", "archived", "pending_review", "unknown"} {
		p, err := DecodeAssetProfile([]byte(`{"business_state":"` + state + `","legacy_state":"historic evidence"}`))
		if err != nil || p.BusinessState != state || p.LegacyState != "historic evidence" {
			t.Fatalf("descriptive state %q: %v", state, err)
		}
	}
	p, err := DecodeAssetProfile([]byte(`{"legacy_state":"some-unrecognized-spelling"}`))
	if err != nil || p.BusinessState != "unknown" {
		t.Fatalf("missing state must stay unknown: %+v %v", p, err)
	}
	for _, raw := range []string{
		`{"business_state":"approved"}`, `{"business_state":"published"}`, `{"business_state":"new-spelling"}`,
		`{"rights":{}}`, `{"license":"CC0-1.0"}`, `{"characters":["a"],"typo":1}`,
		`{"schema":"lantai.asset-profile/v99"}`, `{"characters":[""]}`, `{"characters":["bad\nvalue"]}`,
		`null`, `[]`, `{} {}`, `{"notes":["bad\u202evalue"]}`,
	} {
		if _, err := DecodeAssetProfile([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid profile: %s", raw)
		}
	}
}

func TestAssetProfileRejectsSecurityAndConflicts(t *testing.T) {
	p := AssetProfile{Characters: []string{"hero-a"}, Source: "synthetic", BusinessState: "active"}
	for _, in := range []PushInput{
		{Describe: json.RawMessage(`{"extra":null}`)},
		{Describe: json.RawMessage(`{"extra":[]}`)},
		{Content: ContentInput{Metadata: json.RawMessage(`{"extra":"x"}`)}},
		{Content: ContentInput{Metadata: json.RawMessage(`{"license":"CC0-1.0"}`)}},
		{Content: ContentInput{Metadata: json.RawMessage(`{"extra":{"record":{"noai":false}}}`)}},
		{Describe: json.RawMessage(`{"extra":{"rights":{}}}`)},
		{Describe: json.RawMessage(`{"sensitivity":"normal"}`)},
		{Describe: json.RawMessage(`{"review_state":"approved"}`)},
		{Describe: json.RawMessage(`{"tags":["state:rejected"]}`)},
		{Describe: json.RawMessage(`{"tags":["character:hero-b"]}`)},
		{Describe: json.RawMessage(`{"tags":null}`)},
		{Describe: json.RawMessage(`{"extra":{"asset_profile":{"business_state":"rejected"}}}`)},
		{Content: ContentInput{Metadata: json.RawMessage(`{"extra":{"asset_profile":{"source":"different"}}}`)}},
	} {
		before, _ := json.Marshal(in)
		if _, err := ApplyAssetProfile(in, p); err == nil {
			t.Fatalf("accepted conflict/security input: %s", before)
		}
		after, _ := json.Marshal(in)
		if !bytes.Equal(before, after) {
			t.Fatal("failed adapter changed original input")
		}
	}
}

func TestAssetProfileAllMaterialClassesAndFileRoles(t *testing.T) {
	// Opaque names are enough for this adapter test. Native format validation
	// and tool round trips belong to the workset/acceptance fixtures.
	for _, tc := range []struct{ name, kind, primary string }{
		{"image", "image", "hero.png"}, {"video", "video", "review.mp4"},
		{"audio", "audio", "tone.wav"}, {"recipe", "config", "workflow.json"},
		{"blender", "scene", "scene.blend"}, {"model", "model", "model.obj"},
		{"motion", "motion", "motion.fbx"}, {"engine", "model", "mesh.uasset"},
		{"package", "model", "release.zip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := []InputFile{{Path: tc.primary, Role: "primary"}, {Path: "README.md", Role: "doc"}, {Path: "preview.png", Role: "preview"}}
			out, err := ApplyAssetProfile(PushInput{Content: ContentInput{AssetType: tc.kind, Files: files}}, AssetProfile{Source: "synthetic", BusinessState: "reference"})
			if err != nil || !reflect.DeepEqual(files, out.Content.Files) || out.Content.AssetType != tc.kind || len(out.Content.Rights) != 0 {
				t.Fatalf("adapter modified file grouping, types or defaulted rights: %+v %v", out, err)
			}
		})
	}
}

func TestAssetProfileUnicodeAndCollectionLimits(t *testing.T) {
	p, err := (AssetProfile{Characters: []string{" cafe\u0301 ", "café"}}).Normalize()
	if err != nil || !reflect.DeepEqual(p.Characters, []string{"café"}) {
		t.Fatalf("NFC set mismatch: %+v %v", p, err)
	}
	if _, err := ApplyAssetProfile(PushInput{}, AssetProfile{Characters: []string{strings.Repeat("x", 55)}}); err == nil {
		t.Fatal("tag length was not bounded")
	}
	characters := make([]string, 64)
	for i := range characters {
		characters[i] = strings.Repeat("x", i/2+1) + string(rune('a'+i%2))
	}
	if _, err := ApplyAssetProfile(PushInput{}, AssetProfile{Characters: characters}); err == nil {
		t.Fatal("description total tag limit was not bounded")
	}
}

func TestAssetProfileAppendedVersionKeepsDescriptionSeparate(t *testing.T) {
	in := PushInput{AssetID: "00000000000000000000000001", BaseVersionID: "00000000000000000000000002", Content: ContentInput{AssetType: "image"}}
	out, err := ApplyAssetProfile(in, AssetProfile{BusinessState: "pending_review"})
	if err != nil || len(out.Describe) != 0 || !strings.Contains(string(out.Content.Metadata), `"business_state":"pending_review"`) || out.BaseVersionID != in.BaseVersionID {
		t.Fatalf("append description or version snapshot incorrect: %+v %v", out, err)
	}
	in.Describe = json.RawMessage(`{"title":"Do not silently patch the current asset"}`)
	if _, err := ApplyAssetProfile(in, AssetProfile{}); err == nil {
		t.Fatal("appended version accepted an initial description")
	}
	in.Describe, in.Slug = nil, "old-slug"
	if _, err := ApplyAssetProfile(in, AssetProfile{}); err == nil {
		t.Fatal("appended version accepted a creation slug")
	}
}
