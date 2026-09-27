package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestProjectListingFiltersBeforePagination(t *testing.T) {
	e := newEnv(t, storage.Config{}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	original := e.project
	hidden, err := e.catalog.CreateProject(t.Context(), catalog.ProjectRequest{Who: e.admin.Context, IdempotencyKey: e.key(), Key: "hidden", Name: "Hidden"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.catalog.CreateProject(t.Context(), catalog.ProjectRequest{Who: e.admin.Context, IdempotencyKey: e.key(), Key: "second", Name: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	e.project = second
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	e.project = original
	who := e.login().Context
	page, err := e.catalog.ListProjects(t.Context(), who, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("first page: %+v", page)
	}
	next, err := e.catalog.ListProjects(t.Context(), who, ids.ID(page.NextCursor), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 1 || next.NextCursor != "" || next.Items[0].ProjectID == page.Items[0].ProjectID {
		t.Fatalf("next page: %+v", next)
	}
	for _, p := range []catalog.ProjectPage{page, next} {
		raw, _ := json.Marshal(p)
		if strings.Contains(string(raw), string(hidden.ProjectID)) {
			t.Fatal("hidden project leaked through pagination")
		}
	}
	e.clk.Advance(24 * time.Hour)
	if _, err = e.catalog.ListProjects(t.Context(), who, "", 1); errcode.CodeOf(err) != errcode.TokenExpired {
		t.Fatalf("expired session became empty success: %v", err)
	}
}
