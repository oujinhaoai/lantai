package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBackupControlsFixedRoutesAndOriginalRequests(t *testing.T) {
	t.Setenv("LANTAI_SESSION_TOKEN", "synthetic-human-session")
	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	for _, tc := range []struct{ action, method, path, body string }{
		{"start", "POST", "/api/v1/admin/backups", `{"grant_id":"01ARZ3NDEKTSV4RRFFQ69G5FAW","timeout_seconds":0}`},
		{"resume", "POST", "/api/v1/admin/backups/" + id + "/resume", `{"grant_id":"01ARZ3NDEKTSV4RRFFQ69G5FAW","drain_seconds":15,"timeout_seconds":60}`},
		{"schedule-set", "PUT", "/api/v1/admin/backups/schedule", `{"grant_id":"01ARZ3NDEKTSV4RRFFQ69G5FAW","enabled":false,"timezone":"Asia/Shanghai","time":"02:30","revision":3}`},
		{"cancel", "POST", "/api/v1/admin/backups/" + id + "/cancel", `{}`},
		{"schedule-get", "GET", "/api/v1/admin/backups/schedule", ``},
	} {
		t.Run(tc.action, func(t *testing.T) {
			calls := 0
			origin := serve(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tc.method || r.URL.Path != tc.path || r.Header.Get("Authorization") != "Bearer synthetic-human-session" {
					t.Errorf("wrong method/path/auth: %s %s", r.Method, r.URL)
				}
				if tc.action == "start" || tc.action == "resume" || tc.action == "schedule-set" {
					if r.Header.Get("Idempotency-Key") != "original-key" {
						t.Error("original key lost")
					}
				}
				got, _ := io.ReadAll(r.Body)
				if tc.body != "" {
					var want, actual any
					if json.Unmarshal([]byte(tc.body), &want) != nil || json.Unmarshal(got, &actual) != nil || !reflect.DeepEqual(want, actual) {
						t.Errorf("original request fields changed: %s", got)
					}
				}
				respond(w, map[string]any{"accepted": true})
			})
			file := filepath.Join(t.TempDir(), "request.json")
			if tc.body != "" {
				assetDocumentRaw(t, file, tc.body)
			}
			args := []string{"backups", tc.action, "--id", id, "--input", file, "--idempotency-key", "original-key"}
			if exit, _, stderr := invoke(t, origin, args...); exit != 0 || stderr != "" || calls != 1 {
				t.Fatal(exit, stderr, calls)
			}
		})
	}
}

func assetDocumentRaw(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupControlsRejectLocalErrorsWithoutRequests(t *testing.T) {
	t.Setenv("LANTAI_SESSION_TOKEN", "synthetic-session")
	requests := 0
	origin := serve(t, func(w http.ResponseWriter, r *http.Request) { requests++; t.Error("invalid request reached server") })
	file := filepath.Join(t.TempDir(), "request.json")
	assetDocumentRaw(t, file, `{"grant_id":"01ARZ3NDEKTSV4RRFFQ69G5FAW","target":"/host/path"}`)
	for _, args := range [][]string{
		{"start", "--input", file},
		{"start", "--input", file, "--idempotency-key", "original"},
		{"resume", "--input", file, "--idempotency-key", "original", "--id", "../escape"},
		{"cancel", "--id", "../escape"},
		{"history", "--cursor", "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		{"history", "--explicit-completion", "--limit", "21"},
	} {
		if exit, _, stderr := invoke(t, origin, append([]string{"backups"}, args...)...); exit != 2 || !strings.Contains(stderr, "invalid") && !strings.Contains(stderr, "required") && !strings.Contains(stderr, "requires") && !strings.Contains(stderr, "unknown") {
			t.Fatal(args, exit, stderr)
		}
	}
	if requests != 0 {
		t.Fatal("local invalid requests were sent")
	}
}
