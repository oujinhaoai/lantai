package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Real identity, authoritative ledger, projection and both public API listener
// compositions. No SQL fixture can fabricate a role or a committed version.
func TestAdminReadViewsHTTPRealPermissionsPaginationAndTags(t *testing.T) {
	for _, merged := range []bool{true, false} {
		t.Run(map[bool]string{true: "merged", false: "split"}[merged], func(t *testing.T) {
			e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
			_, maker := e.agent("admin-read-maker@node", identity.RoleContributor)
			a := e.ingest(maker.Context, "tagged/scene", []byte("synthetic v1"), *rightsOwned())
			b, err := e.commitDoc(maker.Context, "", a.AssetID, a.VersionID, []byte("synthetic v2"), nil)
			if err != nil {
				t.Fatal(err)
			}
			info, err := e.catalog.GetAsset(t.Context(), maker.Context, a.AssetID)
			if err != nil {
				t.Fatal(err)
			}
			tags := []string{"character:hero-a", "source:Studio", "state:active"}
			if _, err = e.catalog.PatchAsset(t.Context(), maker.Context, e.key(), a.AssetID, info.Description.Revision, catalog.AssetPatch{Tags: &tags}); err != nil {
				t.Fatal(err)
			}
			e.ingest(maker.Context, "other/doc", []byte("synthetic unrelated"), *rightsOwned())
			reader, session := e.agent("admin-read-viewer@node", identity.RoleViewer)
			app := reopenApplication(t, e)
			server := remoteServers(t, app, merged)
			get := func(token, path string) (int, []byte) {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+server.Addresses.API+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				raw, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatal(err)
				}
				return res.StatusCode, raw
			}
			history := "/api/v1/assets/" + string(a.AssetID) + "/versions?limit=1"
			status, raw := get(session.Token, history)
			if status != 200 {
				t.Fatalf("history %d %s", status, raw)
			}
			var first struct {
				Items []struct {
					VersionID     ids.ID `json:"version_id"`
					VersionNumber int64  `json:"version_number"`
				} `json:"items"`
				NextCursor string `json:"next_cursor"`
			}
			if err := json.Unmarshal(raw, &first); err != nil {
				t.Fatal(err)
			}
			if len(first.Items) != 1 || first.Items[0].VersionID != a.VersionID || first.NextCursor == "" {
				t.Fatalf("first history=%s", raw)
			}
			status, raw = get(session.Token, history+"&cursor="+url.QueryEscape(first.NextCursor))
			if status != 200 {
				t.Fatalf("next %d %s", status, raw)
			}
			var second struct {
				Items []struct {
					VersionID ids.ID `json:"version_id"`
				} `json:"items"`
			}
			if err := json.Unmarshal(raw, &second); err != nil {
				t.Fatal(err)
			}
			if len(second.Items) != 1 || second.Items[0].VersionID != b.VersionID {
				t.Fatalf("second=%s", raw)
			}
			status, raw = get(session.Token, "/api/v1/assets?tag=character%3Ahero-a&tag=source%3AStudio&limit=1")
			if status != 200 || !strings.Contains(string(raw), string(a.AssetID)) {
				t.Fatalf("AND %d %s", status, raw)
			}
			status, raw = get(session.Token, "/api/v1/assets?tag=character%3Ahero-a&tag=source%3Astudio&limit=1")
			if status != 200 || strings.Contains(string(raw), string(a.AssetID)) {
				t.Fatalf("case-sensitive AND %d %s", status, raw)
			}
			for _, path := range []string{"/api/v1/identity/principals", "/api/v1/identity/principals/" + string(reader.ID) + "/credentials"} {
				if status, raw = get(session.Token, path); status != 403 {
					t.Fatalf("ordinary principal saw instance data %d %s", status, raw)
				}
			}
			admin := e.login()
			status, raw = get(admin.Token, "/api/v1/identity/principals?limit=1")
			if status != 200 || !strings.Contains(string(raw), "next_cursor") {
				t.Fatalf("admin principals %d %s", status, raw)
			}
			status, raw = get(admin.Token, "/api/v1/identity/principals/"+string(reader.ID)+"/credentials?limit=1")
			if status != 200 || !strings.Contains(string(raw), "credential_id") || strings.Contains(string(raw), "secret_hash") || strings.Contains(string(raw), session.Token) {
				t.Fatalf("credential metadata %d %s", status, raw)
			}
			e.setRole(reader.ID, identity.RoleViewer, false)
			for _, path := range []string{history, history + "&cursor=" + url.QueryEscape(first.NextCursor), "/api/v1/assets/" + string(ids.New()) + "/versions"} {
				if status, raw = get(session.Token, path); status != 404 {
					t.Fatalf("revoked membership %d %s", status, raw)
				}
			}
			if err := e.id.EndSession(t.Context(), session.Context, session.Context.SessionID); err != nil {
				t.Fatal(err)
			}
			if status, raw = get(session.Token, history); status != 401 {
				t.Fatalf("revoked session %d %s", status, raw)
			}
		})
	}
}
