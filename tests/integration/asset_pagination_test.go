package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// The public HTTP limit is 100, while query.Service permits 200 internally.
// Keep this inexpensive real-route check in ordinary CI so a mock accepting the
// broader internal limit cannot hide a client/HTTP integration regression.
func TestAssetFindDefaultPageSizeUsesPublicHTTP(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	a := reopenApplication(t, e)
	servers := remoteServers(t, a, true)
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, AllowHTTP: true, SessionToken: e.admin.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	for _, options := range []client.AssetFindOptions{{}, {PageSize: 100}} {
		result, err := c.FindAssets(t.Context(), client.AssetFilter{}, options)
		if err != nil || !result.Complete || result.Truncated || len(result.Items) != 0 || result.Pages != 1 || result.Scanned != 0 {
			t.Fatalf("default/explicit public page size failed through the real route: %+v %v", result, err)
		}
	}
}

// This uses real identity, storage, catalog, ledger, index rebuilding and HTTP.
// The first 1025 asset IDs belong to a project the reader cannot access. No SQL
// or mock query inserts bypass the committed catalog or current authorization.
// Run explicitly with LANTAI_ACCEPT_ASSET_PAGINATION=1 and go test -timeout=30m.
// Ordinary CI skips this durable 1025-asset fixture without claiming acceptance.
func TestAssetFindContinuesRealAuthorizationFilteredEmptyPage(t *testing.T) {
	if os.Getenv("LANTAI_ACCEPT_ASSET_PAGINATION") != "1" {
		t.Skip("NOT RUN: real 1025-asset authorization pagination acceptance requires LANTAI_ACCEPT_ASSET_PAGINATION=1 and go test -timeout=30m")
	}
	started := time.Now()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	hiddenProject := e.project
	hiddenIDs := make([]string, 0, 1025)
	var lastHidden string
	data := []byte("synthetic pagination payload")
	var original catalog.VersionResult
	for n := 0; n < 1025; n++ {
		// The fixture's ID generator otherwise has a fixed fake time and random
		// entropy. Advancing it proves every hidden asset sorts before the tail.
		e.clk.Advance(time.Millisecond)
		var v catalog.VersionResult
		if n == 0 {
			v = e.ingest(e.admin.Context, fmt.Sprintf("hidden-%04d", n), data, *rightsOwned())
			original = v
		} else {
			// Reuse bytes through real current source authorization, avoiding
			// 1024 pointless retransmissions. Every asset still takes the normal
			// catalog install/commit/description path and durable filesystem I/O.
			u, err := e.storage.CreateUpload(t.Context(), storage.CreateUploadRequest{Who: e.admin.Context, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = e.storage.GrantFromSource(t.Context(), storage.SourceGrantRequest{Who: e.admin.Context, UploadID: u.UploadID, Source: original.Ref, Path: "content.txt", Purpose: authz.PurposeProduction}); err != nil {
				t.Fatal(err)
			}
			v, err = e.catalog.CommitVersion(t.Context(), catalog.VersionRequest{Who: e.admin.Context, IdempotencyKey: e.key(), UploadID: u.UploadID, Slug: fmt.Sprintf("hidden-%04d", n), Content: catalog.ContentInput{AssetType: manifest.TypeDoc, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "content.txt", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}}})
			if err != nil || v.DescriptionPending {
				t.Fatalf("commit synthetic hidden asset %d: %+v %v", n, v, err)
			}
		}
		if string(v.AssetID) <= lastHidden {
			t.Fatal("fixture failed to establish increasing hidden asset IDs")
		}
		lastHidden = string(v.AssetID)
		hiddenIDs = append(hiddenIDs, lastHidden)
		if (n+1)%256 == 0 {
			t.Logf("committed %d real hidden assets in %s", n+1, time.Since(started))
		}
	}
	created := time.Now()
	visibleProject, err := e.catalog.CreateProject(t.Context(), catalog.ProjectRequest{Who: e.admin.Context, IdempotencyKey: e.key(), Key: "visible-tail", Name: "Synthetic visible tail"})
	if err != nil || visibleProject.DescriptionPending {
		t.Fatalf("create visible project: %+v %v", visibleProject, err)
	}
	e.project = visibleProject
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	reader, session := e.agent("pagination-reader@synthetic", identity.RoleViewer)
	e.clk.Advance(time.Millisecond)
	visible := e.ingest(e.admin.Context, "visible-tail", []byte("synthetic pagination payload"), *rightsOwned())
	if string(visible.AssetID) <= lastHidden {
		t.Fatal("visible asset must sort after all hidden assets")
	}
	adapted, err := client.ApplyAssetProfile(client.PushInput{}, client.AssetProfile{Characters: []string{"hero-a"}, BusinessState: "active", Source: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	var patch catalog.AssetPatch
	if err = json.Unmarshal(adapted.Describe, &patch); err != nil {
		t.Fatal(err)
	}
	info, err := e.catalog.GetAsset(t.Context(), e.admin.Context, visible.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.catalog.PatchAsset(t.Context(), e.admin.Context, e.key(), visible.AssetID, info.Description.Revision, patch); err != nil {
		t.Fatal(err)
	}
	a := reopenApplication(t, e)
	servers := remoteServers(t, a, true)
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, AllowHTTP: true, SessionToken: session.Token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	opened := time.Now()
	first, err := c.Do(t.Context(), http.MethodGet, "/api/v1/assets?limit=100&view=brief", nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var page query.Page
	if err = first.Decode(&page); err != nil || len(page.Items) != 0 || page.NextCursor == "" {
		t.Fatalf("real 1024-candidate boundary did not yield an empty continuation page: %+v %v", page, err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(first.Body, &fields); err != nil || len(fields) != 3 || fields["items"] == nil || fields["state"] == nil || fields["next_cursor"] == nil {
		t.Fatalf("unexpected empty-page fields or candidate count: %s %v", first.Body, err)
	}
	assertNoHidden := func(raw []byte) {
		t.Helper()
		if strings.Contains(string(raw), string(hiddenProject.ProjectID)) || strings.Contains(string(raw), "hidden-") {
			t.Fatal("hidden project or slug leaked from the authorized HTTP/query result")
		}
		for _, hiddenID := range hiddenIDs {
			if strings.Contains(string(raw), hiddenID) {
				t.Fatal("a hidden asset ID leaked from the authorized HTTP/query result")
			}
		}
	}
	assertNoHidden(first.Body)
	filter := client.AssetFilter{Characters: []string{"hero-a"}, BusinessState: "active", Source: "synthetic"}
	result, err := c.FindAssets(t.Context(), filter, client.AssetFindOptions{})
	if err != nil || !result.Complete || result.Truncated || result.Pages != 2 || result.Scanned != 1 || len(result.Items) != 1 || result.Items[0].AssetID != string(visible.AssetID) || result.Items[0].VersionID != string(visible.VersionID) || result.Items[0].ProjectID != string(visibleProject.ProjectID) {
		t.Fatalf("client failed to traverse the real authorization-filtered empty page: %+v %v", result, err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	assertNoHidden(raw)
	queried := time.Now()
	// Revoke the actual role through identity's HumanGrant path. Reuse the
	// same HTTP client/session; it must fetch a new scope, not reuse old items.
	e.setRole(reader.ID, identity.RoleViewer, false)
	after, err := c.FindAssets(t.Context(), filter, client.AssetFindOptions{})
	if err != nil || !after.Complete || after.Truncated || after.Pages != 2 || after.Scanned != 0 || len(after.Items) != 0 {
		t.Fatalf("a fresh query after real revocation retained old results: %+v %v", after, err)
	}
	if _, err = c.Do(t.Context(), http.MethodGet, "/api/v1/assets/"+string(visible.AssetID)+"?view=full", nil, client.Options{}); err == nil {
		t.Fatal("revoked reader could still read the previously returned asset")
	} else {
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.Body.Code != errcode.NotFound {
			t.Fatalf("revoked exact read did not use hidden-object semantics: %v", err)
		}
	}
	t.Logf("real pagination fixture: hidden_assets=1025 visible_assets=1; ingest=%s reopen_and_metadata=%s query=%s revoke_and_requery=%s total=%s", created.Sub(started), opened.Sub(created), queried.Sub(opened), time.Since(queried), time.Since(started))
}
