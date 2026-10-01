package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// 项目不可见时，缺 scope 也不能成为探测已提交版本存在性的旁路。
// 成员身份只决定项目可见性，不授予读取 scope；每次仍复验当前角色与会话。
func TestProjectVisibilityPrecedesReadScope(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	_, maker := e.agent("visibility-maker@node", identity.RoleContributor)
	version := e.ingest(maker.Context, "visibility/doc", []byte("synthetic visibility fixture"), *rightsOwned())
	principal, parent := e.agent("visibility-probe@node", "")
	narrow := func(projects []ids.ID) identity.IssuedSession {
		t.Helper()
		session, err := e.id.NarrowSession(ctx, parent.Context, identity.SessionRequest{Scopes: []identity.Scope{identity.ScopeIngest}, Projects: projects, Channel: identity.ChannelCLI})
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	blind := narrow(nil)
	probes := []struct {
		name string
		call func(authz.Context, ids.ID) error
	}{
		{"read grant", func(w authz.Context, v ids.ID) error {
			_, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: w, AssetID: version.AssetID, VersionID: v, Path: "content.txt", Purpose: authz.PurposeProduction})
			return err
		}},
		{"files", func(w authz.Context, v ids.ID) error {
			_, err := e.storage.VersionFiles(ctx, w, version.AssetID, v)
			return err
		}},
		{"catalog", func(w authz.Context, v ids.ID) error {
			_, err := e.catalog.GetVersion(ctx, w, version.AssetID, v)
			return err
		}},
		{"execution", func(w authz.Context, v ids.ID) error {
			_, err := e.catalog.ExecutionVersion(ctx, w, version.AssetID, v)
			return err
		}},
		{"permanent ref", func(w authz.Context, v ids.ID) error {
			_, err := e.catalog.ResolvePermanent(ctx, w, ids.PermanentRef{InstanceID: e.inst.InstanceID(), AssetID: version.AssetID, VersionID: v})
			return err
		}},
	}
	hidden := func(label string, who authz.Context) {
		t.Helper()
		for _, p := range probes {
			t.Run(label+"/"+p.name, func(t *testing.T) {
				existing, absent := p.call(who, version.VersionID), p.call(who, ids.New())
				if errcode.CodeOf(existing) != errcode.NotFound || errcode.CodeOf(absent) != errcode.NotFound || existing.Error() != absent.Error() {
					t.Fatalf("existing=%v absent=%v", existing, absent)
				}
			})
		}
	}
	hidden("nonmember missing scope", blind.Context)
	hidden("nonmember with scope", parent.Context)
	e.setRole(principal.ID, identity.RoleViewer, true)
	d, err := e.id.Authorize(ctx, blind.Context, storage.ActionReadContent, authz.Resource{ProjectID: e.project.ProjectID})
	if err != nil || d.Allowed || d.Code != errcode.Forbidden {
		t.Fatalf("member without scope: %+v %v", d, err)
	}
	for _, p := range probes {
		if err := p.call(blind.Context, version.VersionID); err == nil {
			t.Errorf("%s granted read without scope", p.name)
		}
	}
	grant, err := e.storage.IssueReadGrant(ctx, storage.ReadRequest{Who: parent.Context, AssetID: version.AssetID, VersionID: version.VersionID, Path: "content.txt", Purpose: authz.PurposeProduction})
	if err != nil {
		t.Fatal(err)
	}
	// 会话收窄到另一个项目，即使仍有目标项目成员身份也不能探测它。
	other, err := e.catalog.CreateProject(ctx, catalog.ProjectRequest{Who: e.admin.Context, IdempotencyKey: e.key(), Key: "scope-other", Name: "Scope other"})
	if err != nil {
		t.Fatal(err)
	}
	scoped := narrow([]ids.ID{other.ProjectID})
	hidden("outside session projects", scoped.Context)
	e.setRole(principal.ID, identity.RoleViewer, false)
	hidden("membership revoked missing scope", blind.Context)
	hidden("membership revoked with scope", parent.Context)
	if r := e.http("GET", grant.URL, parent.Token, nil, nil, 0); r.status != 404 {
		t.Fatalf("old grant after role revocation: %d", r.status)
	}
	if err := e.id.EndSession(ctx, parent.Context, parent.Context.SessionID); err != nil {
		t.Fatal(err)
	}
	for _, who := range []authz.Context{parent.Context, blind.Context, scoped.Context} {
		d, err := e.id.Authorize(ctx, who, storage.ActionReadContent, authz.Resource{ProjectID: e.project.ProjectID})
		if err != nil || d.Allowed || d.Code != errcode.TokenRevoked {
			t.Fatalf("revoked session must precede visibility: %+v %v", d, err)
		}
	}
}

// 两种监听模式都经真实 HTTP 认证与路由验证状态码和机器错误码。
func TestRemoteProjectVisibilityPrecedesReadScope(t *testing.T) {
	for _, merged := range []bool{true, false} {
		t.Run(fmt.Sprintf("merged=%t", merged), func(t *testing.T) {
			e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
			app := reopenApplication(t, e)
			server := remoteServers(t, app, merged)
			ctx := t.Context()
			_, maker := e.agent("http-maker@node", identity.RoleContributor)
			version := e.ingest(maker.Context, "scope/doc", []byte("synthetic HTTP fixture"), *rightsOwned())
			principal, parent := e.agent("http-probe@node", "")
			blind, err := e.id.NarrowSession(ctx, parent.Context, identity.SessionRequest{Scopes: []identity.Scope{identity.ScopeIngest}, Channel: identity.ChannelCLI})
			if err != nil {
				t.Fatal(err)
			}
			request := func(token string, asset, version ids.ID, grant bool) (int, errcode.Code, string) {
				t.Helper()
				path := fmt.Sprintf("http://%s/api/v1/assets/%s/versions/%s", server.Addresses.API, asset, version)
				method := http.MethodGet
				body := []byte(nil)
				if grant {
					method = http.MethodPost
					path += "/read-grants"
					body = []byte(`{"path":"content.txt","purpose":"production"}`)
				}
				req, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				var out struct {
					Error struct {
						Code    errcode.Code `json:"code"`
						Message string       `json:"message"`
					} `json:"error"`
				}
				if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
					t.Fatal(err)
				}
				return res.StatusCode, out.Error.Code, out.Error.Message
			}
			hidden := func(label string) {
				t.Helper()
				for _, grant := range []bool{false, true} {
					status, code, message := request(blind.Token, version.AssetID, version.VersionID, grant)
					for _, ref := range [][2]ids.ID{{version.AssetID, ids.New()}, {ids.New(), ids.New()}} {
						absentStatus, absentCode, absentMessage := request(blind.Token, ref[0], ref[1], grant)
						if status != 404 || code != errcode.NotFound || status != absentStatus || code != absentCode || message != absentMessage {
							t.Fatalf("%s grant=%t: existing=%d/%s/%s absent=%d/%s/%s", label, grant, status, code, message, absentStatus, absentCode, absentMessage)
						}
					}
				}
			}
			hidden("nonmember")
			e.setRole(principal.ID, identity.RoleViewer, true)
			if status, code, _ := request(blind.Token, version.AssetID, version.VersionID, true); status != 403 || code != errcode.Forbidden {
				t.Fatalf("member missing scope=%d/%s", status, code)
			}
			if status, _, _ := request(parent.Token, version.AssetID, version.VersionID, true); status != 201 {
				t.Fatalf("member with scope=%d", status)
			}
			e.setRole(principal.ID, identity.RoleViewer, false)
			hidden("revoked membership")
			if err := e.id.EndSession(ctx, parent.Context, parent.Context.SessionID); err != nil {
				t.Fatal(err)
			}
			for _, ref := range [][2]ids.ID{{version.AssetID, version.VersionID}, {ids.New(), ids.New()}} {
				for _, grant := range []bool{false, true} {
					if status, code, _ := request(blind.Token, ref[0], ref[1], grant); status != 401 || code != errcode.TokenRevoked {
						t.Fatalf("revoked HTTP session=%d/%s", status, code)
					}
				}
			}
		})
	}
}
