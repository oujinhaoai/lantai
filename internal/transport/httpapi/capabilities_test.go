package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/api"
)

func TestAgentOperationHTTPPages(t *testing.T) {
	f := newFixture(t)
	seen := map[string]bool{}
	cursor := ""
	cursors := map[string]bool{}
	digest := ""
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
		if digest == "" {
			digest = p.CatalogSHA256
		}
		if len(digest) != 64 || p.CatalogSHA256 != digest {
			t.Fatal("inconsistent catalog digest")
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
		if p.NextCursor == "" || cursors[p.NextCursor] {
			t.Fatal("non-advancing cursor")
		}
		cursors[p.NextCursor] = true
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
	stale, _ := json.Marshal(map[string]string{"catalog_sha256": strings.Repeat("0", 64), "after": "listAgentOperations"})
	w := call(f.h.API(), http.MethodGet, "/api/v1/capabilities?cursor="+base64.RawURLEncoding.EncodeToString(stale), "", nil)
	if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), "CURSOR_EXPIRED") {
		t.Fatal("stale catalog cursor accepted", w.Code, w.Body.String())
	}
}
