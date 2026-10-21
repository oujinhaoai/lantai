package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

type historyCatalogStub struct {
	CatalogService
	page   catalog.VersionPage
	who    authz.Context
	asset  ids.ID
	cursor string
	limit  int
	err    error
}

func (s *historyCatalogStub) ListVersions(_ context.Context, w authz.Context, a ids.ID, c string, n int) (catalog.VersionPage, error) {
	s.who, s.asset, s.cursor, s.limit = w, a, c, n
	return s.page, s.err
}

type managementIdentityStub struct {
	*identityStub
	who       authz.Context
	principal ids.ID
	cursor    ids.ID
	limit     int
	err       error
}

func (s *managementIdentityStub) PrincipalPage(_ context.Context, w authz.Context, c ids.ID, n int) (identity.PrincipalPage, error) {
	s.who, s.cursor, s.limit = w, c, n
	return identity.PrincipalPage{Items: []identity.Principal{}}, s.err
}
func (s *managementIdentityStub) CredentialPage(_ context.Context, w authz.Context, p, c ids.ID, n int) (identity.CredentialPage, error) {
	s.who, s.principal, s.cursor, s.limit = w, p, c, n
	return identity.CredentialPage{Items: []identity.CredentialInfo{}}, s.err
}

func adminViewsHandler(f fixture) http.Handler {
	mux := http.NewServeMux()
	f.h.adminViewRoutes(mux)
	return f.h.middleware(mux, true)
}
func TestVersionHistoryHTTPUsesCurrentIdentityAndSafeVersionViews(t *testing.T) {
	f := newFixture(t)
	a, v := ids.New(), ids.New()
	s := &historyCatalogStub{CatalogService: f.catalog, page: catalog.VersionPage{Items: []commit.Committed{{AssetID: a, VersionID: v, VersionNumber: 2}}, NextCursor: "opaque"}}
	f.h.deps.Catalog = s
	w := call(adminViewsHandler(f), "GET", "/api/v1/assets/"+string(a)+"/versions?limit=7&cursor=prior", "", bearer())
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if s.who.PrincipalID != f.identity.who.PrincipalID || s.asset != a || s.cursor != "prior" || s.limit != 7 {
		t.Fatalf("wrong domain inputs %+v", s)
	}
	var out struct {
		Items      []VersionView `json:"items"`
		NextCursor string        `json:"next_cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].VersionID != v || out.NextCursor != "opaque" || out.Items[0].URI == "" {
		t.Fatalf("response=%s", w.Body.String())
	}
	s.err = errcode.New(errcode.TokenRevoked, "")
	w = call(adminViewsHandler(f), "GET", "/api/v1/assets/"+string(a)+"/versions", "", bearer())
	code(t, w, errcode.TokenRevoked)
	s.err = nil
	w = call(adminViewsHandler(f), "GET", "/api/v1/assets/"+string(a)+"/versions?limit=101", "", bearer())
	code(t, w, errcode.SchemaInvalid)
	w = call(adminViewsHandler(f), "GET", "/api/v1/assets/"+string(a)+"/versions", "", nil)
	code(t, w, errcode.AuthRequired)
}
func TestManagementHTTPInputsAndDenials(t *testing.T) {
	f := newFixture(t)
	s := &managementIdentityStub{identityStub: f.identity}
	f.h.deps.Identity = s
	id := ids.New()
	cursor := ids.New()
	w := call(adminViewsHandler(f), "GET", "/api/v1/identity/principals/"+string(id)+"/credentials?limit=5&cursor="+string(cursor), "", bearer())
	if w.Code != 200 || s.principal != id || s.cursor != cursor || s.limit != 5 {
		t.Fatalf("%d %s %+v", w.Code, w.Body.String(), s)
	}
	s.err = errcode.New(errcode.Forbidden, "")
	w = call(adminViewsHandler(f), "GET", "/api/v1/identity/principals", "", bearer())
	code(t, w, errcode.Forbidden)
	if strings.Contains(w.Body.String(), "credential") {
		t.Fatal("denial leaked credential metadata")
	}
	w = call(adminViewsHandler(f), "GET", "/api/v1/identity/principals?cursor=invalid", "", bearer())
	code(t, w, errcode.SchemaInvalid)
}
