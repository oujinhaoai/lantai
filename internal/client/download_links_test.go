package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestBackupLinkPreparationUsesTransferBudget(t *testing.T) {
	id := string(ids.New())
	digest := strings.Repeat("a", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer session" {
			t.Error("missing same-session authorization")
		}
		time.Sleep(80 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"download_token": strings.Repeat("b", 64), "expires_at": time.Now().Add(time.Minute), "package": map[string]any{"sha256": digest, "size": 42}})
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, AllowHTTP: true, SessionToken: "session", APITimeout: 10 * time.Millisecond, TransferTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.BackupDownloadLink(t.Context(), id, BackupDownloadInput{GrantID: ids.New(), SHA256: digest}, "stable")
	if err != nil {
		t.Fatal("backup inherited ordinary API timeout", err)
	}
	var link DownloadLink
	if err = out.Decode(&link); err != nil || link.URL != srv.URL+"/api/v1/admin/backups/"+id+"/package" || !link.SessionRequired || link.Headers["X-Lantai-Backup-Download"] == "" {
		t.Fatal("invalid link", err)
	}
	if strings.Contains(string(out.Body), "Bearer session") {
		t.Fatal("credential leaked")
	}
	if _, err = c.Do(t.Context(), "GET", "/api/v1/meta", nil, Options{}); err == nil {
		t.Fatal("ordinary API lost its timeout")
	}
}

func TestFileDownloadLinkRejectsUnboundDescriptor(t *testing.T) {
	asset, version := string(ids.New()), string(ids.New())
	for _, field := range []string{"url", "asset_id", "version_id", "path", "method", "sha256"} {
		t.Run(field, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				v := map[string]any{"url": "/xfer/v1/reads/test?sig=private", "asset_id": asset, "version_id": version, "path": "content.txt", "method": "GET", "sha256": strings.Repeat("a", 64), "size": 3, "expires_at": time.Now().Add(time.Minute)}
				v[field] = "https://untrusted.invalid/private-token"
				_ = json.NewEncoder(w).Encode(v)
			}))
			defer srv.Close()
			c, err := New(Config{BaseURL: srv.URL, AllowHTTP: true})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err = c.FileDownloadLink(t.Context(), asset, version, "content.txt", "archive_review"); err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatal("accepted or exposed an unbound descriptor", err)
			}
		})
	}
}
