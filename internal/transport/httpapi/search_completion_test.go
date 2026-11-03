package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/query"
)

type completionQuery struct {
	QueryService
	page query.Page
}

func (q completionQuery) Search(context.Context, query.SearchRequest) (query.Page, error) {
	return q.page, nil
}

func TestSearchExplicitCompletionKeepsLegacyShape(t *testing.T) {
	for _, tc := range []struct {
		name, cursor, parameter string
		explicit                bool
		complete                bool
	}{
		{"legacy-empty", "", "", false, true},
		{"legacy-more", "opaque", "", false, false},
		{"explicit-empty", "", "&completion=explicit", true, true},
		{"explicit-empty-with-cursor", "opaque", "&completion=explicit", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			deps := f.h.deps
			deps.Query = completionQuery{page: query.Page{Items: []query.Item{}, NextCursor: tc.cursor}}
			h, err := New(deps, f.h.cfg)
			if err != nil {
				t.Fatal(err)
			}
			w := call(h.API(), "GET", "/api/v1/assets?limit=1"+tc.parameter, "", bearer())
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var page map[string]any
			if err = json.Unmarshal(w.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			complete, has := page["complete"]
			truncated, hasTruncated := page["truncated"]
			if has != tc.explicit || hasTruncated != tc.explicit {
				t.Fatal("legacy shape changed or explicit fields missing", page)
			}
			if tc.explicit && (complete != tc.complete || truncated != false) {
				t.Fatal("empty page was treated as terminal despite cursor", page)
			}
			validateResponse(t, apiCompiler(t), "QueryPage", w.Body.Bytes())
		})
	}
}
