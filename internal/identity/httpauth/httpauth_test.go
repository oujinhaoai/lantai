package httpauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type fakeAuth struct {
	bearer, cookie, csrf string
	calls                []string
}

func (f *fakeAuth) AuthenticateToken(_ context.Context, token string) (authz.Context, error) {
	f.calls = append(f.calls, "bearer")
	if token != f.bearer {
		return authz.Context{}, errcode.New(errcode.AuthRequired, "")
	}
	return authz.Context{PrincipalID: ids.New(), PrincipalKind: authz.Agent}, nil
}

func (f *fakeAuth) AuthenticateBrowser(_ context.Context, token, csrf string, requireCSRF bool) (authz.Context, error) {
	f.calls = append(f.calls, "cookie")
	if token != f.cookie {
		return authz.Context{}, errcode.New(errcode.AuthRequired, "")
	}
	if requireCSRF && csrf != f.csrf {
		return authz.Context{}, errcode.New(errcode.Forbidden, "").WithDetails(errcode.Detail{Reason: "csrf_invalid"})
	}
	return authz.Context{PrincipalID: ids.New(), PrincipalKind: authz.Human}, nil
}

type reqSpec struct {
	method, bearer, cookie, origin, csrf, fetchSite string
}

func do(t *testing.T, g *Guard, r reqSpec) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	req := httptest.NewRequest(r.method, "https://lantai.example.test/api/v1/things", nil)
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: r.cookie})
	}
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	if r.csrf != "" {
		req.Header.Set(CSRFHeader, r.csrf)
	}
	if r.fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", r.fetchSite)
	}
	called := false
	rec := httptest.NewRecorder()
	g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if _, ok := FromContext(r.Context()); !ok {
			t.Error("caller missing from the request context")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)
	return rec, called
}

func errorReason(t *testing.T, rec *httptest.ResponseRecorder) (errcode.Code, string) {
	t.Helper()
	var env errcode.Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("error body: %v (%s)", err, rec.Body.String())
	}
	reason := ""
	if len(env.Error.Details) > 0 {
		reason = env.Error.Details[0].Reason
	}
	return env.Error.Code, reason
}

func TestOriginAndCSRFMatrix(t *testing.T) {
	fa := &fakeAuth{bearer: "lts_bearer", cookie: "lts_cookie", csrf: "ltc_ok"}
	g, err := NewGuard(fa, Policy{AllowedOrigins: []string{"https://lantai.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	const origin = "https://lantai.example.test"
	cases := []struct {
		name   string
		req    reqSpec
		status int
		reason string
	}{
		{"cookie write same origin", reqSpec{method: "POST", cookie: "lts_cookie", origin: origin, csrf: "ltc_ok", fetchSite: "same-origin"}, 204, ""},
		{"origin case-insensitive", reqSpec{method: "PATCH", cookie: "lts_cookie", origin: "HTTPS://Lantai.Example.Test", csrf: "ltc_ok"}, 204, ""},
		{"cross origin", reqSpec{method: "POST", cookie: "lts_cookie", origin: "https://evil.example.test", csrf: "ltc_ok"}, 403, "origin_not_allowed"},
		{"sibling port", reqSpec{method: "POST", cookie: "lts_cookie", origin: "https://lantai.example.test:8443", csrf: "ltc_ok"}, 403, "origin_not_allowed"},
		{"missing origin", reqSpec{method: "POST", cookie: "lts_cookie", csrf: "ltc_ok"}, 403, "origin_missing"},
		{"null origin", reqSpec{method: "DELETE", cookie: "lts_cookie", origin: "null", csrf: "ltc_ok"}, 403, "origin_missing"},
		{"forged origin with path", reqSpec{method: "POST", cookie: "lts_cookie", origin: origin + "/x", csrf: "ltc_ok"}, 403, "origin_not_allowed"},
		{"cross-site fetch", reqSpec{method: "POST", cookie: "lts_cookie", origin: origin, csrf: "ltc_ok", fetchSite: "cross-site"}, 403, "cross_site_request"},
		{"missing csrf", reqSpec{method: "POST", cookie: "lts_cookie", origin: origin}, 403, "csrf_invalid"},
		{"wrong csrf", reqSpec{method: "PUT", cookie: "lts_cookie", origin: origin, csrf: "ltc_bad"}, 403, "csrf_invalid"},
		{"cookie read needs no csrf", reqSpec{method: "GET", cookie: "lts_cookie"}, 204, ""},
		{"cookie read from anywhere", reqSpec{method: "HEAD", cookie: "lts_cookie", origin: "https://evil.example.test"}, 204, ""},
		{"bearer write without csrf", reqSpec{method: "POST", bearer: "lts_bearer"}, 204, ""},
		{"bearer ignores origin", reqSpec{method: "POST", bearer: "lts_bearer", origin: "https://evil.example.test"}, 204, ""},
		{"bad bearer", reqSpec{method: "GET", bearer: "lts_other"}, 401, ""},
		{"both credentials", reqSpec{method: "GET", bearer: "lts_bearer", cookie: "lts_cookie"}, 403, "ambiguous_credentials"},
		{"no credentials", reqSpec{method: "GET"}, 401, "credentials_missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, called := do(t, g, c.req)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d (%s)", rec.Code, c.status, rec.Body.String())
			}
			if called != (c.status == 204) {
				t.Fatalf("handler called = %v", called)
			}
			if c.status != 204 {
				_, reason := errorReason(t, rec)
				if c.reason != "" && reason != c.reason {
					t.Fatalf("reason %q, want %q", reason, c.reason)
				}
				if rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("errors must not be cached")
				}
			}
			if c.status == 401 && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
				t.Fatal("401 must carry WWW-Authenticate")
			}
		})
	}
}

func TestMalformedAuthorizationAndPolicy(t *testing.T) {
	g, _ := NewGuard(&fakeAuth{bearer: "x"}, Policy{})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if _, err := g.Authenticate(req); errcode.CodeOf(err) != errcode.AuthRequired {
		t.Fatalf("basic auth = %v", err)
	}
	for _, bad := range []string{"lantai.example.test", "https://lantai.example.test/app", "ftp://x", "https://user@x", "https://x?q=1"} {
		if _, err := NewGuard(&fakeAuth{}, Policy{AllowedOrigins: []string{bad}}); err == nil {
			t.Errorf("origin %q accepted", bad)
		}
	}
}

func TestCookieAttributesAndErrors(t *testing.T) {
	c := SessionCookie("lts_x", time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC))
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Domain != "" || c.Path != "/" {
		t.Fatalf("cookie = %+v", c)
	}
	if cl := ClearCookie(); cl.MaxAge >= 0 || cl.Value != "" {
		t.Fatalf("clear cookie = %+v", cl)
	}
	rec := httptest.NewRecorder()
	WriteError(rec, errcode.New(errcode.RateLimited, "slow down").WithRetryAfter(1500*time.Millisecond), "req-1")
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("rate limited = %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec = httptest.NewRecorder()
	WriteError(rec, context.DeadlineExceeded, "")
	code, _ := errorReason(t, rec)
	if rec.Code != 500 || code != errcode.Internal || strings.Contains(rec.Body.String(), "deadline") {
		t.Fatalf("internal errors must not leak details: %s", rec.Body.String())
	}
}
