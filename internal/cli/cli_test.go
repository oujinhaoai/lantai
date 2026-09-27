package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func invoke(t *testing.T, server string, args ...string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	args = append(args, "--server", server, "--allow-http", "--json")
	code := Run(context.Background(), args, &out, &err)
	return code, out.String(), err.String()
}
func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestLoginPersistsPrivateCredentialWithoutOutput(t *testing.T) {
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	dir := t.TempDir()
	creds, session := filepath.Join(dir, "credentials.json"), filepath.Join(dir, "session.json")
	if err := client.WritePrivate(creds, []byte(`{"name":"H-test","password":"secret-password","code":"123456"}`)); err != nil {
		t.Fatal(err)
	}
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/sessions/login":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["password"] != "secret-password" || in["channel"] != "cli" {
				t.Error("invalid login request")
			}
			respond(w, map[string]any{"token": "issued-secret", "session": map[string]string{"session_id": "session1"}})
		case "/api/v1/whoami":
			if r.Header.Get("Authorization") != "Bearer issued-secret" {
				t.Error("missing stored credential")
			}
			respond(w, map[string]string{"principal_id": "principal1"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	code, out, stderr := invoke(t, url, "login", "--credentials-file", creds, "--session-file", session)
	if code != 0 || stderr != "" || strings.Contains(out, "issued-secret") || !strings.Contains(out, "session1") {
		t.Fatalf("login=%d %s %s", code, out, stderr)
	}
	b, err := client.ReadPrivate(session)
	if err != nil || !bytes.Contains(b, []byte("issued-secret")) {
		t.Fatalf("saved=%q %v", b, err)
	}
	code, out, stderr = invoke(t, url, "whoami", "--session-file", session)
	if code != 0 || !strings.Contains(out, "principal1") {
		t.Fatalf("whoami=%d %s %s", code, out, stderr)
	}
	other := serve(t, func(w http.ResponseWriter, r *http.Request) { t.Error("sent credential to different origin") })
	if code, _, _ := invoke(t, other, "whoami", "--session-file", session); code != 2 {
		t.Fatalf("origin code=%d", code)
	}
}
func TestStableExitsAndIfMatch(t *testing.T) {
	t.Setenv("LANTAI_SESSION_TOKEN", "session")
	input := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(input, []byte(`{"title":"new"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var headers http.Header
	calls := 0
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		headers = r.Header.Clone()
		w.WriteHeader(412)
		respond(w, errcode.New(errcode.PreconditionFailed, "revision changed").Envelope("request"))
	})
	code, out, stderr := invoke(t, url, "metadata", "set", "--asset", "asset1", "--input", input, "--if-match", `"3"`, "--idempotency-key", "stable-key")
	if code != 4 || out != "" || !strings.Contains(stderr, "PRECONDITION_FAILED") || headers.Get("If-Match") != `"3"` || headers.Get("Idempotency-Key") != "stable-key" {
		t.Fatalf("patch=%d %s %s headers=%v", code, out, stderr, headers)
	}
	if calls != 1 {
		t.Fatal("silently retried conflict")
	}
	if code, _, _ := invoke(t, url, "metadata", "set", "--asset", "asset1", "--input", input); code != 2 {
		t.Fatalf("missing precondition=%d", code)
	}
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	if code, _, _ := invoke(t, url, "whoami"); code != 5 {
		t.Fatalf("auth=%d", code)
	}
	for _, tc := range []struct {
		code errcode.Code
		exit int
	}{{errcode.Forbidden, 1}, {errcode.SchemaInvalid, 2}, {errcode.AuthRequired, 5}, {errcode.QuotaExceeded, 6}, {errcode.RateLimited, 6}} {
		var b bytes.Buffer
		if got := writeError(&b, errcode.New(tc.code, "example")); got != tc.exit {
			t.Errorf("%s exit=%d", tc.code, got)
		}
	}
	var b bytes.Buffer
	if got := writeError(&b, context.Canceled); got != 130 {
		t.Errorf("cancellation exit=%d", got)
	}
}
func TestMetaNeedsNoSessionAndPullRejectsGrantMismatch(t *testing.T) {
	t.Setenv("LANTAI_SESSION_TOKEN", "")
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/meta" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		respond(w, map[string]string{"instance_id": "instance1"})
	})
	if code, _, err := invoke(t, url, "meta"); code != 0 {
		t.Fatalf("meta=%d %s", code, err)
	}
	t.Setenv("LANTAI_SESSION_TOKEN", "session")
	url = serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/xfer/"):
			t.Error("downloaded mismatched grant")
		case strings.HasSuffix(r.URL.Path, "/read-grants"):
			respond(w, client.DownloadGrant{URL: "/xfer/grant?private", Path: "other", SHA256: strings.Repeat("a", 64), Size: 3})
		default:
			if r.URL.Query().Get("view") != "full" {
				t.Error("pull did not ask full view")
			}
			respond(w, map[string]any{"manifest": map[string]any{"content": map[string]any{"files": []client.InputFile{{Path: "a", SHA256: strings.Repeat("a", 64), Size: 3}}}}})
		}
	})
	if code, _, err := invoke(t, url, "pull", "--asset", "asset1", "--version", "version1", "--directory", t.TempDir()); code != 3 || !strings.Contains(err, "read grant") {
		t.Fatalf("pull=%d %s", code, err)
	}
}

func TestInvalidGatewayErrorIsProtocolExit(t *testing.T) {
	t.Setenv("LANTAI_SESSION_TOKEN", "session")
	url := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	if code, _, err := invoke(t, url, "whoami"); code != 3 || !strings.Contains(err, "INTERNAL") {
		t.Fatalf("gateway=%d %s", code, err)
	}
}

func TestBusinessMetadataIsNotRedactedAsCredential(t *testing.T) {
	t.Setenv("LANTAI_SESSION_TOKEN", "session")
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]any{"description": map[string]any{"extra": map[string]string{"code": "catalog-code", "url": "https://reference.example/item"}}})
	})
	code, out, err := invoke(t, url, "show", "--asset", "asset1", "--view", "full")
	if code != 0 || !strings.Contains(out, "catalog-code") || !strings.Contains(out, "reference.example") {
		t.Fatalf("metadata changed: %d %s %s", code, out, err)
	}
}
