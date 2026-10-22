package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const AssetProfileSchema = "lantai.asset-profile/v1"

// AssetProfile is a local input adapter, not a server manifest or approval.
// Characters identifies depicted characters; manifest files[].role has a
// different meaning. Source paths and notes are descriptive, never executable.
type AssetProfile struct {
	Schema        string   `json:"schema,omitempty"`
	Characters    []string `json:"characters,omitempty"`
	Source        string   `json:"source,omitempty"`
	BusinessState string   `json:"business_state,omitempty"`
	LegacyState   string   `json:"legacy_state,omitempty"`
	LegacyVersion string   `json:"legacy_version,omitempty"`
	SourcePaths   []string `json:"source_paths,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

// DecodeAssetProfile rejects misspelled and misplaced security fields instead
// of letting encoding/json silently discard them. Empty business_state becomes
// unknown; unknown historic spellings belong in legacy_state.
func DecodeAssetProfile(raw []byte) (AssetProfile, error) {
	var p AssetProfile
	if err := decodeAssetJSON(raw, &p); err != nil {
		return p, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return p, errors.New("client: profile must be a JSON object")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return p, errors.New("client: null profile fields are not supported")
		}
	}
	return p.Normalize()
}

func decodeAssetJSON(raw []byte, out any) error {
	if len(raw) == 0 || int64(len(raw)) > MaxJSONBytes || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("client: expected a non-null bounded JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return errors.New("client: invalid asset JSON document")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("client: trailing asset JSON data")
	}
	return nil
}

// Normalize only normalizes descriptive vocabulary. It never translates an
// imported state into Lantai review, lifecycle, publication or rights facts.
func (p AssetProfile) Normalize() (AssetProfile, error) {
	if p.Schema == "" {
		p.Schema = AssetProfileSchema
	}
	if p.Schema != AssetProfileSchema {
		return p, errors.New("client: unsupported asset profile schema")
	}
	var err error
	if p.Characters, err = assetStringSet(p.Characters, 64, 54); err != nil {
		return p, fmt.Errorf("client: invalid profile characters: %w", err)
	}
	if p.Source, err = assetText(p.Source, 57); err != nil {
		return p, errors.New("client: invalid profile source")
	}
	if p.BusinessState == "" {
		p.BusinessState = "unknown"
	}
	if !slices.Contains([]string{"active", "reference", "rejected", "superseded", "archived", "pending_review", "unknown"}, p.BusinessState) {
		return p, errors.New("client: unknown business_state; preserve its original value in legacy_state and choose unknown")
	}
	if p.LegacyState, err = assetText(p.LegacyState, 200); err != nil {
		return p, errors.New("client: invalid legacy_state")
	}
	if p.LegacyVersion, err = assetText(p.LegacyVersion, 49); err != nil {
		return p, errors.New("client: invalid legacy_version")
	}
	if p.SourcePaths, err = assetStringSet(p.SourcePaths, 256, 2048); err != nil {
		return p, errors.New("client: invalid source_paths")
	}
	if p.Notes, err = assetStringSet(p.Notes, 64, 4000); err != nil {
		return p, errors.New("client: invalid notes")
	}
	return p, nil
}

func assetText(s string, max int) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("invalid UTF-8")
	}
	s = strings.TrimSpace(norm.NFC.String(s))
	if utf8.RuneCountInString(s) > max || strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || r == '\u202a' || r == '\u202b' || r == '\u202c' || r == '\u202d' || r == '\u202e' || r == '\u2066' || r == '\u2067' || r == '\u2068' || r == '\u2069'
	}) {
		return "", errors.New("invalid text length or control characters")
	}
	return s, nil
}

func assetStringSet(in []string, count, width int) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		v, err := assetText(s, width)
		if err != nil || v == "" {
			return nil, errors.New("empty or invalid value")
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
			if len(out) > count {
				return nil, errors.New("too many values")
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (p AssetProfile) tags() []string {
	tags := []string{"state:" + p.BusinessState}
	for _, character := range p.Characters {
		tags = append(tags, "character:"+character)
	}
	if p.Source != "" {
		tags = append(tags, "source:"+p.Source)
	}
	if p.LegacyVersion != "" {
		tags = append(tags, "legacy-version:"+p.LegacyVersion)
	}
	sort.Strings(tags)
	return tags
}

// ApplyAssetProfile projects exact-search tags into the initial description and
// keeps a profile snapshot in frozen version metadata. Existing ordinary
// metadata is preserved, but conflicting profile namespaces are rejected.
// This prepares input only: Push/Commit may still return description_pending.
// It does not patch an existing description or grant organize permission.
// Appended versions only receive the frozen metadata snapshot; their current
// searchable description must be changed with a separate conditional PATCH.
func ApplyAssetProfile(in PushInput, p AssetProfile) (PushInput, error) {
	p, err := p.Normalize()
	if err != nil {
		return in, err
	}
	metadata, err := assetObject(in.Content.Metadata)
	if err != nil {
		return in, err
	}
	if err = installAssetProfile(metadata, p); err != nil {
		return in, err
	}
	m, err := json.Marshal(metadata)
	if err != nil {
		return in, err
	}
	if in.AssetID != "" {
		if in.Slug != "" || len(in.Describe) != 0 {
			return in, errors.New("client: appended versions cannot include slug or describe; use a separate conditional metadata patch")
		}
		in.Content.Metadata = m
		return in, nil
	}
	describe, err := assetObject(in.Describe)
	if err != nil {
		return in, err
	}
	for key := range describe {
		if !slices.Contains([]string{"title", "summary", "tags", "subjects", "extra"}, key) {
			return in, errors.New("client: profile description contains unsupported or security fields")
		}
	}
	tags, err := assetObjectStrings(describe, "tags")
	if err != nil {
		return in, err
	}
	for _, tag := range tags {
		for _, prefix := range []string{"character:", "state:", "source:", "legacy-version:"} {
			if strings.HasPrefix(tag, prefix) && !slices.Contains(p.tags(), tag) {
				return in, errors.New("client: existing profile tags conflict; review the complete profile instead of merging states")
			}
		}
	}
	tags, err = assetStringSet(append(tags, p.tags()...), 64, 64)
	if err != nil {
		return in, errors.New("client: profile tags exceed description limits")
	}
	subjects, err := assetObjectStrings(describe, "subjects")
	if err != nil {
		return in, err
	}
	subjects, err = assetStringSet(append(subjects, p.Characters...), 64, 64)
	if err != nil {
		return in, errors.New("client: profile subjects exceed description limits")
	}
	describe["tags"], describe["subjects"] = tags, subjects
	if err = installAssetProfile(describe, p); err != nil {
		return in, err
	}
	d, err := json.Marshal(describe)
	if err != nil {
		return in, err
	}
	in.Describe, in.Content.Metadata = d, m
	return in, nil
}

func assetObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := decodeAssetJSON(raw, &m); err != nil || m == nil {
		return nil, errors.New("client: profile target must be a JSON object")
	}
	if assetSecurityField(m) {
		return nil, errors.New("client: security fields belong in formal rights or dedicated commands, never profile metadata")
	}
	return m, nil
}

func assetObjectStrings(m map[string]any, key string) ([]string, error) {
	v, exists := m[key]
	if !exists {
		return nil, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, errors.New("client: tags and subjects must be arrays of strings")
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, ok := x.(string)
		if !ok {
			return nil, errors.New("client: tags and subjects must be arrays of strings")
		}
		out = append(out, s)
	}
	return assetStringSet(out, 64, 64)
}

func installAssetProfile(m map[string]any, p AssetProfile) error {
	extra := map[string]any{}
	if e, exists := m["extra"]; exists {
		var ok bool
		extra, ok = e.(map[string]any)
		if !ok || extra == nil {
			return errors.New("client: profile target extra must be an object")
		}
	}
	if previous, exists := extra["asset_profile"]; exists {
		raw, err := json.Marshal(previous)
		if err != nil {
			return errors.New("client: invalid existing asset profile")
		}
		old, err := DecodeAssetProfile(raw)
		if err != nil {
			return err
		}
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(p)
		if !bytes.Equal(a, b) {
			return errors.New("client: existing asset profile conflicts with input")
		}
	}
	extra["asset_profile"] = p
	m["extra"] = extra
	return nil
}

func assetSecurityField(v any) bool {
	switch v := v.(type) {
	case map[string]any:
		for key, value := range v {
			if slices.Contains([]string{"rights", "usage", "license", "sensitivity", "defaults", "redistribute_raw", "noai", "digital_source_type", "restricted", "restricted_source"}, strings.ToLower(key)) || assetSecurityField(value) {
				return true
			}
		}
	case []any:
		for _, value := range v {
			if assetSecurityField(value) {
				return true
			}
		}
	}
	return false
}
