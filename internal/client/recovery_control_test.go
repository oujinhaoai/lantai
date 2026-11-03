package client

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRecoveryControlFixedClaimsAndNoRedirect(t *testing.T) {
	claim := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Error("claim forwarded to redirect target")
	}))
	defer target.Close()
	var origin string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != origin || r.Header.Get("Authorization") != "Bearer "+claim || r.URL.RawQuery != "" {
			t.Error("installation admission changed")
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1") || strings.Contains(r.URL.Path, "admin") {
			t.Error("ordinary/factor endpoint reached")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	origin = server.URL
	c, err := NewRecoveryControl(Config{BaseURL: origin, AllowHTTP: true}, claim)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Status(t.Context()); err == nil || strings.Contains(err.Error(), claim) {
		t.Fatal("redirect accepted or claim exposed", err)
	}
	if requests.Load() != 0 {
		t.Fatal("redirect followed")
	}
	for _, config := range []Config{{BaseURL: origin, AllowHTTP: true, SessionToken: "normal-agent-session"}, {BaseURL: origin}} {
		if x, err := NewRecoveryControl(config, claim); err == nil {
			x.Close()
			t.Fatal("ordinary session or HTTP downgrade accepted")
		}
	}
	if _, err := NewRecoveryControl(Config{BaseURL: origin, AllowHTTP: true}, "ordinary-agent-token"); err == nil {
		t.Fatal("non-installation claim accepted")
	}
	if _, err := c.Complete(t.Context(), RecoveryCompleteInput{}); err == nil {
		t.Fatal("implicit cutover confirmations")
	}
}
func TestRecoveryPackageHashAndReviewedDigest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "selected.lantai")
	if err := WritePrivate(path, []byte("selected synthetic bytes")); err != nil {
		t.Fatal(err)
	}
	hash, size, err := HashRecoveryPackage(t.Context(), path)
	if err != nil || size != 24 || len(hash) != 64 {
		t.Fatal(hash, size, err)
	}
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Error("wrong package digest reached target")
	}))
	defer s.Close()
	c, err := NewRecoveryControl(Config{BaseURL: s.URL, AllowHTTP: true}, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Upload(t.Context(), path, strings.Repeat("0", 64)); err == nil || requests.Load() != 0 {
		t.Fatal("unreviewed package uploaded", err)
	}
	link := filepath.Join(root, "link.lantai")
	if err := os.Symlink(path, link); err == nil {
		if _, _, err := HashRecoveryPackage(t.Context(), link); err == nil {
			t.Fatal("linked package accepted")
		}
	}
}

func TestRecoveryUnknownWritePreservesOriginalTargetWithoutRetry(t *testing.T) {
	claim := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/setup/recovery/restore" {
			writes.Add(1)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close() // original side effect happened; response lost
			return
		}
		if request.URL.Path != "/setup/recovery/status" || request.Method != http.MethodGet {
			t.Error("unexpected recovery action", request.Method, request.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"phase\":\"restored\",\"recovery_epoch\":2}"))
	}))
	defer server.Close()
	control, err := NewRecoveryControl(Config{BaseURL: server.URL, AllowHTTP: true}, claim)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	_, err = control.Restore(t.Context(), RecoveryRestoreInput{PackageSHA256: strings.Repeat("a", 64), ConfirmNewTarget: true})
	if err == nil || strings.Contains(err.Error(), claim) {
		t.Fatal("lost response accepted or secret disclosed", err)
	}
	if writes.Load() != 1 {
		t.Fatal("unknown recovery write retried", writes.Load())
	}
	status, err := control.Status(t.Context())
	if err != nil || !bytes.Contains(status.Body, []byte("restored")) || writes.Load() != 1 {
		t.Fatal("original target was not reconciled by read", status, err)
	}
}
