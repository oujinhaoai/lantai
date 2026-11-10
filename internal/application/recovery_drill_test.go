package application

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoveryDrillHTTPAllowlist(t *testing.T) {
	h := recoveryDrillHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }))
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{"GET", "/api/v1/assets/a/versions/v", true},
		{"POST", "/api/v1/assets/a/versions/v/read-grants", true},
		{"GET", "/xfer/v1/reads/r", true},
		{"HEAD", "/xfer/v1/reads/r", true},
		{"POST", "/api/v1/sessions/login", true},
		{"POST", "/api/v1/tasks/claim", true},
		{"POST", "/api/v1/task-runs/progress", true},
		{"POST", "/api/v1/task-runs/checkpoint", true},
		{"POST", "/api/v1/task-runs/tool", false},
		{"POST", "/api/v1/task-runs/candidate", false},
		{"POST", "/api/v1/jobs/run", false},
		{"POST", "/api/v1/flows/dispatch", false},
		{"POST", "/api/v1/backups", false},
		{"POST", "/api/v1/identity/commands", false},
		{"POST", "/api/v1/human/execute", false},
		{"POST", "/api/v1/uploads", false},
		{"PUT", "/xfer/v1/uploads/u/files/f/parts/0", false},
		{"GET", "/api/v1/new-dangerous-feature", false},
		{"GET", "/api/v1//assets/a", false},
		{"GET", "/api/v1/../assets/a", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://127.0.0.1:8080"+tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := http.StatusForbidden
			if tc.allowed {
				want = 299
			}
			if w.Code != want {
				t.Fatalf("status %d, want %d", w.Code, want)
			}
			if w.Header().Get("X-Lantai-Recovery-Drill") != "true" {
				t.Fatal("missing drill label")
			}
		})
	}
}

func TestRecoveryDrillRejectsDNSAndForwarding(t *testing.T) {
	h := recoveryDrillHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }))
	for _, host := range []string{"localhost:8080", "evil.example", "192.0.2.1:8080", "0.0.0.0:8080"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1/api/v1/whoami", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("accepted Host %s", host)
		}
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-Host", "X-Forwarded-For", "X-Forwarded-Proto", "X-Real-IP"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1/api/v1/whoami", nil)
		r.Header.Set(header, "")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("accepted %s", header)
		}
	}
}

func TestRecoveryDrillBrowserAndHealthBoundary(t *testing.T) {
	ready := true
	h := recoveryDrillHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) }), drillHTTPOptions{authority: "127.0.0.1:8080", ready: func() bool { return ready }})
	for _, tc := range []struct {
		host, origin, site string
		want               int
	}{
		{"127.0.0.1:8080", "", "", 299},
		{"127.0.0.1:8080", "http://127.0.0.1:8080", "same-origin", 299},
		{"127.0.0.1:9090", "", "", 403},
		{"127.0.0.2:8080", "", "", 403},
		{"127.0.0.1:8080", "https://production.example", "same-origin", 403},
		{"127.0.0.1:8080", "http://127.0.0.1:9090", "same-site", 403},
		{"127.0.0.1:8080", "", "cross-site", 403},
		{"127.0.0.1:8080", "null", "", 403},
	} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/v1/whoami", nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%+v: got %d", tc, w.Code)
		}
	}
	ready = false
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8080/readyz", nil))
	if w.Code != 503 {
		t.Fatalf("unready drill reported %d", w.Code)
	}
}
