package query

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestExactTagsANDNormalizationAndCursorScope(t *testing.T) {
	f := setup(t)
	var refs []ids.ID
	for i := 0; i < 3; i++ {
		a := f.add(f.project, "tagged")
		a.Description.Tags = []string{"character:é", "source:Studio", "state:active"}
		f.source.assets[a.Asset.AssetID] = a
		refs = append(refs, a.Asset.AssetID)
	}
	other := f.add(f.project, "substring not exact")
	other.Description.Tags = []string{"character:é-other", "source:Studio"}
	f.source.assets[other.Asset.AssetID] = other
	f.build()
	p, err := f.s.Search(t.Context(), SearchRequest{Who: f.who, Filter: Filter{Tags: []string{"source:Studio", "character:e\u0301", "source:Studio"}}, Limit: 1})
	if err != nil || len(p.Items) != 1 || p.NextCursor == "" {
		t.Fatalf("first=%+v %v", p, err)
	}
	seen := []ids.ID{p.Items[0].AssetID}
	for p.NextCursor != "" {
		p, err = f.s.Search(t.Context(), SearchRequest{Who: f.who, Filter: Filter{Tags: []string{"character:é", "source:Studio"}}, Limit: 1, Cursor: p.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range p.Items {
			seen = append(seen, i.AssetID)
		}
	}
	slices.Sort(refs)
	if !slices.Equal(refs, seen) {
		t.Fatalf("exact AND=%v want %v", seen, refs)
	}
	snapshot, err := f.s.Snapshot(t.Context(), f.who, Filter{Tags: []string{"source:Studio", "character:é"}})
	if err != nil || len(snapshot.Items) != 3 || !snapshot.ReplaceScope {
		t.Fatalf("tagged snapshot=%+v %v", snapshot, err)
	}
	if got := f.search(Filter{Tags: []string{"source:studio"}}); len(got.Items) != 0 {
		t.Fatal("exact tags became case insensitive")
	}
	p, err = f.s.Search(t.Context(), SearchRequest{Who: f.who, Filter: Filter{Tags: []string{"source:Studio"}}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Search(t.Context(), SearchRequest{Who: f.who, Filter: Filter{Tags: []string{"state:active"}}, Limit: 1, Cursor: p.NextCursor})
	code(t, err, errcode.CursorExpired)
}

func TestExactTagsEmptyAuthorizedPageAndRevocation(t *testing.T) {
	f := setup(t)
	var ordered []ids.ID
	for i := 0; i < 1030; i++ {
		a := f.add(f.project, "synthetic")
		ordered = append(ordered, a.Asset.AssetID)
	}
	slices.Sort(ordered)
	for i, id := range ordered {
		a := f.source.assets[id]
		a.Description.Tags = []string{"other"}
		if i >= 1024 {
			a.Description.Tags = []string{"selected"}
		}
		f.source.assets[id] = a
	}
	hidden := f.add(ids.New(), "hidden-matching")
	hidden.Description.Tags = []string{"selected"}
	f.source.assets[hidden.Asset.AssetID] = hidden
	f.build()
	p, err := f.s.Search(t.Context(), SearchRequest{Who: f.who, Filter: Filter{Tags: []string{"selected"}}, Limit: 100})
	if err != nil || len(p.Items) != 0 || p.NextCursor == "" {
		t.Fatalf("bounded empty page=%+v %v", p, err)
	}
	p, err = f.s.Search(t.Context(), SearchRequest{Who: f.who, Filter: Filter{Tags: []string{"selected"}}, Limit: 100, Cursor: p.NextCursor})
	if err != nil || len(p.Items) != 6 || p.NextCursor != "" {
		t.Fatalf("next page=%+v %v", p, err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), string(hidden.Asset.AssetID)) {
		t.Fatal("hidden matching asset leaked")
	}
	f.az.Revoke(f.who.PrincipalID, f.project)
	f.who = f.az.OpenSession(f.who.PrincipalID, time.Hour)
	cursor := ""
	for {
		p, err = f.s.Search(t.Context(), SearchRequest{Who: f.who, Filter: Filter{Tags: []string{"selected"}}, Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) != 0 {
			t.Fatal("revoked role kept tagged data")
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
}

func TestExactTagValidationIsBounded(t *testing.T) {
	f := setup(t)
	for _, tags := range [][]string{{""}, {" \t "}, {"bad\nvalue"}, {strings.Repeat("雪", 65)}, make([]string, 65), {string([]byte{0xff})}} {
		_, err := f.s.Search(t.Context(), SearchRequest{Who: f.who, Filter: Filter{Tags: tags}, Limit: 1})
		code(t, err, errcode.SchemaInvalid)
	}
}
