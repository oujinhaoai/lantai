package catalog

import (
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
)

func TestProfilePatchPreservesUnknownServerJCSValuesWithoutBrowserRoundtrip(t *testing.T) {
	extra, err := cleanExtra(map[string]any{"unknown_decimal": json.Number("0.12345678901234567890"), "unknown_nested": map[string]any{"identifier": "9007199254740993", "list": []any{true, "keep", json.Number("1e30")}}, "asset_profile": map[string]any{"schema": "lantai.asset-profile/v1", "business_state": "unknown"}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := canonjson.CanonicalizeValue(extra)
	profile := json.RawMessage(`{"schema":"lantai.asset-profile/v1","characters":["synthetic-hero"],"source":"synthetic","business_state":"pending_review","legacy_state":"waiting","legacy_version":"v007","source_paths":["/synthetic/inert"],"notes":["generated"]}`)
	out, err := (AssetPatch{Profile: profile}).apply(AssetDescription{Title: "Synthetic", Sensitivity: "normal", Extra: extra})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"unknown_decimal", "unknown_nested"} {
		a, _ := canonjson.CanonicalizeValue(extra[key])
		b, _ := canonjson.CanonicalizeValue(out.Extra[key])
		if string(a) != string(b) {
			t.Fatalf("server JCS value changed: %s", key)
		}
	}
	after, _ := canonjson.CanonicalizeValue(extra)
	if string(before) != string(after) {
		t.Fatal("patch mutated its input map")
	}
	if out.Sensitivity != "normal" {
		t.Fatal("descriptive profile altered sensitivity")
	}
	if _, err = (AssetPatch{Profile: profile, Extra: &extra}).apply(AssetDescription{Title: "Synthetic", Extra: extra}); err == nil {
		t.Fatal("profile and full extra replacement accepted together")
	}
}

func TestProfilePatchRejectsSecurityUnknownAndUnsupportedStates(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"schema":"lantai.asset-profile/v1","rights":{}}`, `{"schema":"lantai.asset-profile/v1","noai":false}`, `{"schema":"lantai.asset-profile/v1","business_state":"approved"}`, `{"schema":"lantai.asset-profile/v1","source":"bad\u202etext"}`} {
		if _, err := (AssetPatch{Profile: json.RawMessage(raw)}).apply(AssetDescription{Title: "Synthetic"}); err == nil {
			t.Fatalf("profile accepted invalid input %s", raw)
		}
	}
}
