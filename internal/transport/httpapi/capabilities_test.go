package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/oujinhaoai/lantai/api"
)

func TestAgentOperationHTTPPages(t *testing.T) {
	f := newFixture(t)
	seen := map[string]bool{}
	cursor := ""
	for {
		w := call(f.h.API(), http.MethodGet, "/api/v1/capabilities?limit=3&cursor="+url.QueryEscape(cursor), "", nil)
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		var p api.AgentOperationPage
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Contract != api.AgentOperationsContract || len(p.Items) > 3 {
			t.Fatal(p)
		}
		for _, op := range p.Items {
			if seen[op.ID] {
				t.Fatal("duplicate operation", op.ID)
			}
			seen[op.ID] = true
		}
		if p.Complete {
			break
		}
		if p.NextCursor <= cursor {
			t.Fatal("non-advancing cursor")
		}
		cursor = p.NextCursor
	}
	if len(seen) < 100 {
		t.Fatal("incomplete inventory")
	}
	for _, q := range []string{"limit=0", "limit=21", "limit=text", "cursor=unknown"} {
		w := call(f.h.API(), http.MethodGet, "/api/v1/capabilities?"+q, "", nil)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatal(q, w.Code, w.Body.String())
		}
	}
}
