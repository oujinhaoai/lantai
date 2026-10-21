package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

type backupDownload struct {
	principal, session, job ids.ID
	digest                  string
	expires                 time.Time
}

func backupPackageRequest(r *http.Request) bool {
	if r.URL.Path != r.URL.EscapedPath() {
		return false
	}
	s := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/backups/")
	for _, tail := range []string{"/package", "/download-request"} {
		if strings.HasSuffix(s, tail) && ids.ID(strings.TrimSuffix(s, tail)).Valid() {
			return r.Method == http.MethodGet && tail == "/package" || r.Method == http.MethodPost && tail == "/download-request"
		}
	}
	return false
}
func downloadHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
func downloadCookie(id ids.ID) string { return "lantai_backup_download_" + string(id) }

func (h *Handler) backupDownloadRequest(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, e := pathID(r, "id")
	if e != nil {
		return e
	}
	var in struct {
		GrantID ids.ID `json:"grant_id"`
		SHA256  string `json:"package_sha256"`
	}
	if e = h.decode(w, r, &in); e != nil {
		return e
	}
	_, e = h.approveBackup(r, who, in.GrantID, &identity.ApproveBackup{Kind: "download", JobID: id, PackageSHA256: in.SHA256})
	if e != nil {
		return e
	}
	svc, ok := h.deps.Backups.(backupmgmt.PackageService)
	if !ok {
		return errcode.New(errcode.UnsupportedCapability, "complete recovery package unavailable")
	}
	reader, p, e := svc.OpenPackage(r.Context(), id, in.SHA256)
	if e != nil {
		return backupError(e)
	}
	_ = reader.Close()
	if e = h.deps.Human.BeginRead(r.Context(), who, identity.ActBackupRead, authz.Resource{}, func(context.Context, authz.Decision) error { return nil }); e != nil {
		return e
	}
	var token [32]byte
	if _, e = rand.Read(token[:]); e != nil {
		return e
	}
	raw := hex.EncodeToString(token[:])
	now := time.Now()
	expires := now.Add(15 * time.Minute)
	h.downloadMu.Lock()
	if h.downloads == nil {
		h.downloads = map[string]backupDownload{}
	}
	for k, v := range h.downloads {
		if !v.expires.After(now) {
			delete(h.downloads, k)
		}
	}
	if len(h.downloads) >= 1024 {
		h.downloadMu.Unlock()
		return errcode.New(errcode.StorageUnavailable, "download approval capacity reached")
	}
	h.downloads[downloadHash(raw)] = backupDownload{who.PrincipalID, who.SessionID, id, p.SHA256, expires}
	h.downloadMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: downloadCookie(id), Value: raw, Path: "/api/v1/admin/backups/" + string(id) + "/package", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 900})
	return respond(w, 200, map[string]any{"download_token": raw, "expires_at": expires, "package": p})
}

func (h *Handler) backupDownloadPackage(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	if _, e := r.Cookie(backupControlCookie); e == nil {
		return errcode.New(errcode.Forbidden, "maintenance control cannot download recovery material")
	}
	id, e := pathID(r, "id")
	if e != nil {
		return e
	}
	raw := r.Header.Get("X-Lantai-Backup-Download")
	cookies := 0
	for _, c := range r.Cookies() {
		if c.Name == downloadCookie(id) {
			cookies++
			if raw != "" || cookies > 1 {
				return errcode.New(errcode.Forbidden, "ambiguous download approval")
			}
			raw = c.Value
		}
	}
	if len(r.Header.Values("X-Lantai-Backup-Download")) > 1 {
		return errcode.New(errcode.Forbidden, "ambiguous download approval")
	}
	h.downloadMu.Lock()
	permit, ok := h.downloads[downloadHash(raw)]
	h.downloadMu.Unlock()
	if !ok || !permit.expires.After(time.Now()) || permit.principal != who.PrincipalID || permit.session != who.SessionID || permit.job != id {
		return errcode.New(errcode.Forbidden, "fresh package download approval required")
	}
	if h.deps.Human == nil {
		return errcode.New(errcode.Forbidden, "administrator required")
	}
	if e = h.deps.Human.BeginRead(r.Context(), who, identity.ActBackupRead, authz.Resource{}, func(context.Context, authz.Decision) error { return nil }); e != nil {
		return e
	}
	svc, ok := h.deps.Backups.(backupmgmt.PackageService)
	if !ok {
		return errcode.New(errcode.UnsupportedCapability, "complete recovery package unavailable")
	}
	f, p, e := svc.OpenPackage(r.Context(), id, permit.digest)
	if e != nil {
		return backupError(e)
	}
	defer f.Close()
	if e = h.deps.Human.BeginRead(r.Context(), who, identity.ActBackupRead, authz.Resource{}, func(context.Context, authz.Decision) error { return nil }); e != nil {
		return e
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="lantai-`+string(id)+`.lantai"`)
	w.Header().Set("ETag", `"`+p.SHA256+`"`)
	w.Header().Set("X-Lantai-Content-SHA256", p.SHA256)
	idleTransfer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeContent(w, r, "backup.lantai", time.Time{}, f) }), h.cfg.TransferIdleTimeout).ServeHTTP(w, r)
	return nil
}
