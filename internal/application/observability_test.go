package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/operations"
)

func TestOperationsReadinessAndUnavailableMetrics(t *testing.T) {
	r := operations.Readiness{Live: true, State: operations.StateRecovering, Reasons: []operations.Reason{{Code: operations.CodeRecoveryFailed, Message: "private/path/secret"}}}
	h := localOperationsHandler(func() operations.Readiness { return r }, func() bool { return r.Ready }, []collector{
		{"known", func(context.Context) ([]metric, error) { return []metric{gauge("test_known", 0)}, nil }},
		{"failed", func(context.Context) ([]metric, error) {
			return []metric{gauge("test_unknown", 0)}, errors.New("private credential")
		}},
	})
	for _, tc := range []struct {
		path   string
		status int
	}{{"/healthz", 200}, {"/readyz", 503}, {"/metrics", 503}, {"/debug/pprof/", 404}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status {
			t.Errorf("%s status=%d", tc.path, w.Code)
		}
		if strings.Contains(w.Body.String(), "private") {
			t.Fatal("private detail leaked")
		}
		if tc.path == "/metrics" && (!strings.Contains(w.Body.String(), `lantai_collector_available{collector="failed"} 0`) || strings.Contains(w.Body.String(), "test_unknown") || !strings.Contains(w.Body.String(), "lantai_test_known 0")) {
			t.Fatalf("misleading metrics: %s", w.Body.String())
		}
	}
	r.Ready = true
	r.State = operations.StateReady
	r.Reasons = nil
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	r.Live = false
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestReadinessGuardRejectsBeforeBusinessHandler(t *testing.T) {
	ready := false
	calls := 0
	h := readinessGuard(func() bool { return ready }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	for _, method := range []string{"GET", "POST", "PUT"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/meta", nil))
		if w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"MAINTENANCE_MODE"`) || calls != 0 {
			t.Fatalf("gate did not reject %s: %s", method, w.Body.String())
		}
	}
	ready = true
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/assets", nil))
	if w.Code != 204 || calls != 1 {
		t.Fatal("ready request rejected")
	}
}

func TestAccessLogContainsOnlySafeFieldsAndPreservesStreaming(t *testing.T) {
	var b bytes.Buffer
	l := accessLogger{out: &b}
	h := l.wrap("transfer", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "secret-cookie")
		w.WriteHeader(206)
		_, _ = w.Write([]byte("secret-response"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
	}))
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			r := httptest.NewRequest("GET", "https://example.test/xfer/v1/secret-path?sig=secret-query", strings.NewReader("secret-body"))
			r.Header.Set("Authorization", "Bearer secret-token")
			r.Header.Set("Cookie", "secret-cookie")
			r.Header.Set("X-Request-Id", "secret-request-id")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !w.Flushed || w.Code != 206 || w.Body.String() != "secret-response" {
				t.Error("stream changed")
			}
		})
	}
	wg.Wait()
	if strings.Contains(b.String(), "secret") {
		t.Fatalf("access log leaked: %s", b.String())
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 10 {
		t.Fatal(len(lines))
	}
	for _, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if len(row) != 9 || row["status"] != float64(206) || row["surface"] != "transfer" || len(row["observation_id"].(string)) != 32 {
			t.Fatalf("unexpected log row: %v", row)
		}
	}
	if safeMethod("secret") != "OTHER" {
		t.Fatal("untrusted method is logged")
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *deadlineRecorder) SetWriteDeadline(d time.Time) error { w.deadline = d; return nil }
func TestObservedWriterPreservesResponseController(t *testing.T) {
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	now := time.Now()
	if err := http.NewResponseController(&observedWriter{ResponseWriter: w}).SetWriteDeadline(now); err != nil || !w.deadline.Equal(now) {
		t.Fatalf("deadline lost: %v", err)
	}
}

func TestWALMissingIsZeroOnlyWhenDatabaseExists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "index.db")
	if _, err := walBytes(p); err == nil {
		t.Fatal("missing database reported as zero WAL")
	}
	if err := os.WriteFile(p, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := walBytes(p); err != nil || n != 0 {
		t.Fatalf("absent WAL: %d %v", n, err)
	}
	if err := os.WriteFile(p+"-wal", []byte("1234"), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := walBytes(p); err != nil || n != 4 {
		t.Fatalf("WAL size: %d %v", n, err)
	}
	if err := os.Remove(p + "-wal"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p+"-wal", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := walBytes(p); err == nil {
		t.Fatal("nonregular WAL accepted")
	}
}
