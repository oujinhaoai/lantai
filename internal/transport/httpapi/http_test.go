package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/query"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

type identityStub struct {
	IdentityService
	who        authz.Context
	loginCalls int
	login      identity.LoginRequest
	executed   identity.Command
	grant      ids.ID
	key        string
	err        error
}

func (s *identityStub) AuthenticateToken(_ context.Context, token string) (authz.Context, error) {
	if token != "lts_test" {
		return authz.Context{}, errcode.New(errcode.TokenRevoked, "")
	}
	return s.who, s.err
}
func (s *identityStub) AuthenticateBrowser(_ context.Context, token, csrf string, write bool) (authz.Context, error) {
	if token != "browser" || write && csrf != "csrf" {
		return authz.Context{}, errcode.New(errcode.Forbidden, "")
	}
	return s.who, s.err
}
func (s *identityStub) Login(_ context.Context, in identity.LoginRequest) (identity.IssuedSession, error) {
	s.loginCalls++
	s.login = in
	return identity.IssuedSession{Token: "browser-secret", CSRFToken: "csrf", Session: identity.SessionInfo{ExpiresAt: time.Now().Add(time.Hour)}}, s.err
}
func (s *identityStub) WhoAmI(_ context.Context, who authz.Context) (identity.Identity, error) {
	return identity.Identity{Principal: identity.Principal{ID: who.PrincipalID}}, s.err
}
func (s *identityStub) Execute(_ context.Context, _ authz.Context, grant ids.ID, key string, c identity.Command) (identity.Result, error) {
	s.executed = c
	s.grant = grant
	s.key = key
	return identity.Result{OperationID: ids.New(), Summary: json.RawMessage(`{"credential_id":"test"}`), Secret: "once-only"}, s.err
}

type catalogStub struct {
	CatalogService
	asset    catalog.AssetInfo
	version  catalog.VersionInfo
	patched  bool
	who      authz.Context
	revision int64
	key      string
	patch    catalog.AssetPatch
	err      error
}

func (s *catalogStub) GetAsset(_ context.Context, who authz.Context, _ ids.ID) (catalog.AssetInfo, error) {
	s.who = who
	return s.asset, s.err
}
func (s *catalogStub) GetVersion(_ context.Context, who authz.Context, _, _ ids.ID) (catalog.VersionInfo, error) {
	s.who = who
	return s.version, s.err
}
func (s *catalogStub) PatchAsset(_ context.Context, who authz.Context, key string, _ ids.ID, rev int64, patch catalog.AssetPatch) (catalog.AssetDescription, error) {
	s.patched = true
	s.who = who
	s.key = key
	s.revision = rev
	s.patch = patch
	return catalog.AssetDescription{Revision: rev + 1}, s.err
}

type storageStub struct {
	StorageService
	upload  storage.Upload
	request storage.CreateUploadRequest
	called  bool
}

func (s *storageStub) CreateUpload(_ context.Context, in storage.CreateUploadRequest) (storage.Upload, error) {
	s.request = in
	s.called = true
	return s.upload, nil
}
func (s *storageStub) GetUpload(context.Context, authz.Context, ids.ID) (storage.Upload, error) {
	return s.upload, nil
}

type queryStub struct{ QueryService }
type operationsStub struct {
	who authz.Context
	id  ids.ID
	err error
}

func (s *operationsStub) Operation(_ context.Context, who authz.Context, id ids.ID) (*commands.View, error) {
	s.who = who
	s.id = id
	return nil, s.err
}

type fixture struct {
	h          *Handler
	identity   *identityStub
	catalog    *catalogStub
	storage    *storageStub
	operations *operationsStub
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	who := authz.Context{InstanceID: ids.New(), PrincipalID: ids.New(), SessionID: ids.New(), PrincipalKind: authz.Agent, AuthEpoch: 1, RecoveryEpoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
	i := &identityStub{who: who}
	c := &catalogStub{}
	s := &storageStub{}
	o := &operationsStub{err: errcode.New(errcode.NotFound, "")}
	h, err := New(Deps{Identity: i, Catalog: c, Storage: s, Query: &queryStub{}, Operations: o}, Config{InstanceID: who.InstanceID, AllowedOrigins: []string{"https://example.test"}, MaxJSONBytes: 1024, APITimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{h: h, identity: i, catalog: c, storage: s, operations: o}
}
func call(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func bearer() map[string]string { return map[string]string{"Authorization": "Bearer lts_test"} }
func code(t *testing.T, w *httptest.ResponseRecorder, want errcode.Code) {
	t.Helper()
	var out struct {
		Error struct {
			Code      errcode.Code `json:"code"`
			RequestID string       `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if out.Error.Code != want {
		t.Fatalf("want %s, got %s", want, w.Body.String())
	}
	if w.Header().Get("X-Request-Id") == "" {
		t.Fatal("request ID missing")
	}
}

func TestStrictJSONAndLimitsBeforeDomainCall(t *testing.T) {
	f := newFixture(t)
	for _, body := range []string{`null`, `{}`, `{"project_id":"x","project_id":"y"}`, `{"project_id":"x","unknown":"secret"}`, `{"project_id":1}`, `{"project_id":"` + strings.Repeat("s", 1100) + `"}`} {
		// Empty object is valid transport JSON: the real domain validates its fields.
		if body == `{}` {
			continue
		}
		h := bearer()
		h["Idempotency-Key"] = "a"
		w := call(f.h.API(), "POST", "/api/v1/uploads", body, h)
		code(t, w, errcode.SchemaInvalid)
		if f.storage.called {
			t.Fatal("invalid request reached domain")
		}
		if strings.Contains(w.Body.String(), "secret") {
			t.Fatal("input leaked")
		}
	}
	w := call(f.h.API(), "POST", "/api/v1/uploads", `{"project_id":"x"}`, bearer())
	code(t, w, errcode.SchemaInvalid)
	w = call(f.h.API(), "POST", "/api/v1/uploads", `{}`, map[string]string{"Idempotency-Key": "a"})
	code(t, w, errcode.AuthRequired)
}

func TestIdentityContextAndConditionalWrite(t *testing.T) {
	f := newFixture(t)
	id := ids.New()
	path := "/api/v1/assets/" + string(id) + "/metadata"
	h := bearer()
	h["Idempotency-Key"] = "metadata"
	w := call(f.h.API(), "PATCH", path, `{"title":"new title"}`, h)
	code(t, w, errcode.PreconditionRequired)
	for _, v := range []string{`W/"1"`, `*`, `"01"`, `"-1"`, `"1", "2"`} {
		h["If-Match"] = v
		code(t, call(f.h.API(), "PATCH", path, `{}`, h), errcode.SchemaInvalid)
	}
	h["If-Match"] = `"0"`
	h["X-Lantai-Principal"] = "forged"
	h["X-Lantai-Transfer-Class"] = "interactive"
	w = call(f.h.API(), "PATCH", path, `{"title":"new title"}`, h)
	if w.Code != 200 || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !f.catalog.patched || f.catalog.who.PrincipalID != f.identity.who.PrincipalID || f.catalog.who.TransferClass() != authz.Batch || f.catalog.revision != 0 || f.catalog.key != "metadata" {
		t.Fatalf("untrusted adapter context: %+v", f.catalog)
	}
	f.catalog.err = errcode.New(errcode.PreconditionFailed, "")
	code(t, call(f.h.API(), "PATCH", path, `{}`, h), errcode.PreconditionFailed)
}

func TestRequestIDsErrorsAndUnsupportedContext(t *testing.T) {
	f := newFixture(t)
	h := bearer()
	h["X-Request-Id"] = "a.valid:request-1"
	f.identity.err = errors.New("private-path secret-token")
	w := call(f.h.API(), "GET", "/api/v1/whoami", "", h)
	code(t, w, errcode.Internal)
	if w.Header().Get("X-Request-Id") != h["X-Request-Id"] || strings.Contains(w.Body.String(), "private-path") {
		t.Fatal(w.Body.String())
	}
	f.identity.err = nil
	h["X-Request-Id"] = strings.Repeat("x", 129)
	w = call(f.h.API(), "GET", "/api/v1/whoami", "", h)
	code(t, w, errcode.SchemaInvalid)
	if w.Header().Get("X-Request-Id") == h["X-Request-Id"] {
		t.Fatal("invalid ID reflected")
	}
	delete(h, "X-Request-Id")
	h["X-Lantai-Task"] = "task"
	code(t, call(f.h.API(), "GET", "/api/v1/whoami", "", h), errcode.SchemaInvalid)
}

func TestBrowserLoginAndCookieWrites(t *testing.T) {
	f := newFixture(t)
	body := `{"name":"person","password":"not-real","code":"123456","channel":"browser"}`
	for _, origin := range []string{"", "null", "https://example.test.evil.invalid", "https://example.test/path"} {
		code(t, call(f.h.API(), "POST", "/api/v1/sessions/login", body, map[string]string{"Origin": origin}), errcode.Forbidden)
	}
	if f.identity.loginCalls != 0 {
		t.Fatal("unsafe login reached identity")
	}
	w := call(f.h.API(), "POST", "/api/v1/sessions/login", body, map[string]string{"Origin": "https://example.test", "X-Forwarded-For": "untrusted-source"})
	if w.Code != 201 || strings.Contains(w.Body.String(), "browser-secret") {
		t.Fatal(w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal(cookies)
	}
	if f.identity.login.Source == "untrusted-source" {
		t.Fatal("trusted forwarded source")
	}
	id := ids.New()
	headers := map[string]string{"Cookie": httpauth.CookieName + "=browser", "Idempotency-Key": "meta", "If-Match": `"0"`}
	code(t, call(f.h.API(), "PATCH", "/api/v1/assets/"+string(id)+"/metadata", `{}`, headers), errcode.Forbidden)
	headers["Origin"] = "https://example.test"
	headers["X-CSRF-Token"] = "csrf"
	if got := call(f.h.API(), "PATCH", "/api/v1/assets/"+string(id)+"/metadata", `{}`, headers); got.Code != 200 {
		t.Fatal(got.Body.String())
	}
	headers["Authorization"] = "Bearer lts_test"
	code(t, call(f.h.API(), "GET", "/api/v1/whoami", "", headers), errcode.Forbidden)
}

func TestBriefFullAndAuthorizedURLs(t *testing.T) {
	f := newFixture(t)
	a, v := ids.New(), ids.New()
	f.catalog.asset = catalog.AssetInfo{Asset: commit.Asset{AssetID: a}, Description: catalog.AssetDescription{AssetID: a, Revision: 2, Extra: map[string]any{"full_only": "value"}}}
	f.catalog.version = catalog.VersionInfo{Version: commit.Committed{AssetID: a, VersionID: v}, Manifest: manifest.Document{AssetID: a, VersionID: v}}
	path := "/api/v1/assets/" + string(a)
	if w := call(f.h.API(), "GET", path, "", bearer()); w.Code != 200 || strings.Contains(w.Body.String(), "full_only") || w.Header().Get("ETag") != `"2"` {
		t.Fatal(w.Body.String())
	}
	if w := call(f.h.API(), "GET", path+"?view=full", "", bearer()); !strings.Contains(w.Body.String(), "full_only") {
		t.Fatal(w.Body.String())
	}
	path += "/versions/" + string(v)
	if w := call(f.h.API(), "GET", path, "", bearer()); strings.Contains(w.Body.String(), `"manifest":`) {
		t.Fatal(w.Body.String())
	}
	if w := call(f.h.API(), "GET", path+"?view=full", "", bearer()); !strings.Contains(w.Body.String(), `"contract":"lantai.manifest/v1"`) {
		t.Fatal(w.Body.String())
	}
	u := ids.New()
	sha := strings.Repeat("a", 64)
	f.storage.upload = storage.Upload{UploadID: u, OperationID: ids.New(), PartsURL: storage.PartsURL(u), Files: []storage.UploadFile{{SHA256: sha, PartCount: 2, Received: []int{1}}}}
	w := call(f.h.API(), "GET", "/api/v1/uploads/"+string(u), "", bearer())
	var out UploadView
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 1 || out.Files[0].PartURLTemplate != storage.PartsURL(u)+sha+"/parts/{part_number}" || out.OperationID != f.storage.upload.OperationID || out.Files[0].Received[0] != 1 {
		t.Fatalf("%s", w.Body.String())
	}
}

func TestSensitiveCommandBoundariesAndOneTimeSecret(t *testing.T) {
	f := newFixture(t)
	grant := ids.New()
	h := bearer()
	h["Idempotency-Key"] = "issue"
	body := `{"action":"identity.issue_credential","grant_id":"` + string(grant) + `","command":{"principal_id":"` + string(ids.New()) + `","expected_revision":1,"scopes":["read"],"projects":[],"label":"test","ttl_seconds":60}}`
	w := call(f.h.API(), "POST", "/api/v1/identity/commands", body, h)
	if w.Code != 200 || f.identity.grant != grant || f.identity.key != "issue" || !strings.Contains(w.Body.String(), `"secret":"once-only"`) {
		t.Fatal(w.Body.String())
	}
	if _, ok := f.identity.executed.(*identity.IssueCredential); !ok {
		t.Fatal("wrong domain command")
	}
	code(t, call(f.h.API(), "POST", "/api/v1/identity/commands", strings.Replace(body, "identity.issue_credential", "identity.set_policy", 1), h), errcode.SchemaInvalid)
	code(t, call(f.h.API(), "POST", "/api/v1/identity/challenges", `{"action":"identity.grant_project_role","command":{"grant":false}}`, h), errcode.SchemaInvalid)
	op := ids.New()
	code(t, call(f.h.API(), "GET", "/api/v1/operations/"+string(op), "", h), errcode.NotFound)
	if f.operations.who.PrincipalID != f.identity.who.PrincipalID || f.operations.id != op {
		t.Fatal("operation authorization context not forwarded")
	}
}

func TestOpenAPIRoutesHaveHandlers(t *testing.T) {
	f := newFixture(t)
	// Enable all optional route groups. These zero-value owner handles must
	// never run: this test verifies authentication before domain dispatch.
	f.h.deps.Tasks = &tasks.Service{}
	f.h.deps.Flows = &workflow.Service{}
	f.h.deps.Execution = &ax.Service{}
	f.h.deps.Jobs = &jobs.Service{}
	f.h.deps.Nodes = &node.Service{}
	f.h.deps.Extensions = &extensions.Manager{}
	f.h.deps.Ledger = &ledger.Service{}
	f.h.deps.Reviews = &ledger.Reviews{}
	f.h.deps.Lifecycle = &ledger.Lifecycle{}
	f.h.deps.Discussions = &ledger.DiscussionObjects{}
	f.h.deps.Evidence = &ledger.FileReviewSources{}
	f.h.deps.Collaboration = &query.Collaboration{}
	f.h.deps.Rights = &provenance.Service{}
	f.h.deps.Human = &identity.Service{}
	f.h.deps.HumanTargets = f.h.deps.Ledger
	configured, setupErr := New(f.h.deps, f.h.cfg)
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	f.h = configured
	b, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := yamljson.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	paths := doc.(map[string]any)["paths"].(map[string]any)
	public := map[string]bool{"/api/v1/meta": true, "/api/v1/sessions/exchange": true, "/api/v1/sessions/login": true, "/api/v1/sessions/setup": true}
	for path, v := range paths {
		if !strings.HasPrefix(path, "/api/") || public[path] {
			continue
		}
		for method := range v.(map[string]any) {
			if method == "parameters" {
				continue
			}
			p := path
			for {
				start := strings.Index(p, "{")
				if start < 0 {
					break
				}
				end := strings.Index(p[start:], "}") + start
				p = p[:start] + string(ids.New()) + p[end+1:]
			}
			w := call(f.h.API(), strings.ToUpper(method), p, "", nil)
			if w.Code != 401 {
				t.Errorf("%s %s lacks guarded handler: %d %s", method, path, w.Code, w.Body.String())
			}
		}
	}
	if f.h.APIConfig("").WriteTimeout != 70*time.Second {
		t.Fatal("API timeout is not honored")
	}
	xfer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "transfer") })
	if w := call(f.h.Merged(xfer), "GET", "/xfer/test", "", nil); w.Body.String() != "transfer" {
		t.Fatal("merged handler did not reuse transfer")
	}
}
