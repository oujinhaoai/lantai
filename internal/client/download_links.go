package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// DownloadLink contains no session credential. Callers must authenticate the
// GET with the same session used to obtain it, and keep temporary headers private.
type DownloadLink struct {
	URL             string            `json:"url"`
	Method          string            `json:"method"`
	Headers         map[string]string `json:"headers"`
	ExpiresAt       time.Time         `json:"expires_at"`
	SHA256          string            `json:"sha256"`
	Size            int64             `json:"size"`
	Path            string            `json:"path"`
	Range           bool              `json:"range"`
	SessionRequired bool              `json:"session_required"`
	Sensitive       bool              `json:"sensitive"`
}

func linkResponse(v DownloadLink) (Response, error) {
	raw, err := json.Marshal(v)
	return Response{Status: http.StatusOK, Body: raw}, err
}

func (c *Client) FileDownloadLink(ctx context.Context, asset, version, path, purpose string) (Response, error) {
	if !ids.ID(asset).Valid() || !ids.ID(version).Valid() || SafeRelative(path) != nil {
		return Response{}, errors.New("client: exact asset, version and safe file path required")
	}
	r, err := c.Do(ctx, http.MethodPost, "/api/v1/assets/"+asset+"/versions/"+version+"/read-grants", map[string]string{"path": path, "purpose": purpose}, Options{})
	if err != nil {
		return r, err
	}
	var v struct {
		DownloadLink
		AssetID   string `json:"asset_id"`
		VersionID string `json:"version_id"`
	}
	if err = r.Decode(&v); err != nil {
		return Response{}, err
	}
	u, err := c.authorizedURL(v.URL)
	if err != nil || v.AssetID != asset || v.VersionID != version || v.Path != path || v.Method != "GET" || !digest.ValidHex(v.SHA256) || v.Size < 0 || v.ExpiresAt.IsZero() {
		return Response{}, errors.New("client: invalid file download descriptor")
	}
	v.URL, v.Headers, v.SessionRequired = u.String(), map[string]string{}, true
	return linkResponse(v.DownloadLink)
}

type BackupDownloadInput struct {
	GrantID ids.ID `json:"grant_id"`
	SHA256  string `json:"package_sha256"`
}

// BackupDownloadLink redeems only an explicit, resource-bound human approval.
// The server still rejects Agent, delegated and non-administrator sessions.
func (c *Client) BackupDownloadLink(ctx context.Context, job string, in BackupDownloadInput, key string) (Response, error) {
	if !ids.ID(job).Valid() || !in.GrantID.Valid() || !digest.ValidHex(in.SHA256) || key == "" {
		return Response{}, errors.New("client: backup job, approval, package digest and idempotency key required")
	}
	path := "/api/v1/admin/backups/" + job + "/package"
	r, err := c.Do(ctx, http.MethodPost, "/api/v1/admin/backups/"+job+"/download-request", in, Options{IdempotencyKey: key})
	if err != nil {
		return r, err
	}
	var v struct {
		Token     string    `json:"download_token"`
		ExpiresAt time.Time `json:"expires_at"`
		URL       string    `json:"url"`
		Package   struct {
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"package"`
	}
	if err = r.Decode(&v); err != nil {
		return Response{}, err
	}
	// Old servers returned no URL; this is their one fixed public package route.
	if v.URL == "" {
		v.URL = path
	}
	if v.URL != path || !digest.ValidHex(v.Token) || v.Package.SHA256 != in.SHA256 || v.Package.Size < 0 || v.ExpiresAt.IsZero() {
		return Response{}, errors.New("client: invalid backup download descriptor")
	}
	return linkResponse(DownloadLink{URL: c.base.ResolveReference(&url.URL{Path: path}).String(), Method: "GET", Headers: map[string]string{"X-Lantai-Backup-Download": v.Token}, ExpiresAt: v.ExpiresAt, SHA256: v.Package.SHA256, Size: v.Package.Size, Path: "lantai-" + job + ".lantai", Range: true, SessionRequired: true, Sensitive: true})
}

func backupLinkRequest(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	const prefix = "/api/v1/admin/backups/"
	const suffix = "/download-request"
	if len(path) != len(prefix)+26+len(suffix) {
		return false
	}
	return path[:len(prefix)] == prefix && path[len(path)-len(suffix):] == suffix && ids.ID(path[len(prefix):len(prefix)+26]).Valid()
}
