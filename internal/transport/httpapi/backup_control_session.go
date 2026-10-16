package httpapi

import (
	"net/http"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
)

const backupControlCookie = "lantai_backup_control"
const backupControlPath = "/api/v1/admin/backups"

func (h *Handler) controlSessionRoutes(m *http.ServeMux) {
	h.route(m, "POST "+backupControlPath+"/control-session", true, h.controlLogin)
	h.route(m, "GET "+backupControlPath+"/control-session", true, h.controlSession)
	h.route(m, "DELETE "+backupControlPath+"/control-session", true, h.controlLogout)
}
func controlCookie(token string, expiry time.Time) *http.Cookie {
	return &http.Cookie{Name: backupControlCookie, Value: token, Path: backupControlPath, Expires: expiry, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}
func (h *Handler) controlLogin(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
	if e := h.guard.CheckBrowserOrigin(r); e != nil {
		return e
	}
	if h.deps.Human == nil || h.deps.BackupScope == nil {
		return errcode.New(errcode.UnsupportedCapability, "backup reauthentication is unavailable")
	}
	if r.Header.Get("Authorization") != "" {
		return invalid("backup reauthentication requires browser credentials")
	}
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if e := h.decode(w, r, &in); e != nil {
		return e
	}
	if len(in.Name) < 1 || len(in.Name) > 32 || len(in.Password) < 1 || len(in.Password) > 4096 || len(in.Code) > 64 {
		return invalid("invalid backup reauthentication fields")
	}
	out, e := h.deps.Human.LoginBackupControl(r.Context(), in.Name, in.Password, in.Code, source(r), h.deps.BackupScope)
	if e != nil {
		return e
	}
	http.SetCookie(w, controlCookie(out.Token, out.ExpiresAt))
	return respond(w, 201, out)
}
func (h *Handler) controlSession(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
	err := h.withControl(r, false, func(v identity.BackupControlSession) error { return respond(w, 200, v) })
	clearFailedControl(w, r, err)
	return err
}
func (h *Handler) controlLogout(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
	if e := h.guard.CheckBrowserOrigin(r); e != nil {
		return e
	}
	c := controlCookie("", time.Unix(0, 0))
	c.MaxAge = -1
	http.SetCookie(w, c)
	cookie, e := r.Cookie(backupControlCookie)
	if e == nil && h.deps.Human != nil {
		if e = h.deps.Human.LogoutBackupControl(cookie.Value, r.Header.Get(httpauth.CSRFHeader)); e != nil {
			return e
		}
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
func (h *Handler) withControl(r *http.Request, write bool, run func(identity.BackupControlSession) error) error {
	cookie, e := r.Cookie(backupControlCookie)
	if e != nil || cookie.Value == "" {
		return errcode.New(errcode.AuthRequired, "backup control authentication is required")
	}
	if h.deps.Human == nil || h.deps.BackupScope == nil {
		return errcode.New(errcode.UnsupportedCapability, "backup reauthentication is unavailable")
	}
	if r.Header.Get("Authorization") != "" {
		return errcode.New(errcode.Forbidden, "ambiguous backup control credentials")
	}
	if write {
		if e = h.guard.CheckBrowserOrigin(r); e != nil {
			return e
		}
	}
	return h.deps.Human.WithBackupControl(r.Context(), cookie.Value, r.Header.Get(httpauth.CSRFHeader), write, h.deps.BackupScope, run)
}

// Backup-only handlers select the restricted authority explicitly. A control
// token is never converted to an ordinary authz.Context or accepted by Guard.
func (h *Handler) backupReadRoute(m *http.ServeMux, pattern string, fn endpoint) {
	m.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		var who authz.Context
		if _, e := r.Cookie(backupControlCookie); e != nil {
			c, e := h.guard.Authenticate(r)
			if e != nil {
				writeError(w, r, e)
				return
			}
			who = c.Context
		}
		if e := fn(w, r, who); e != nil {
			clearFailedControl(w, r, e)
			writeError(w, r, e)
		}
	})
}

// An obsolete restricted cookie must not shadow a later ordinary login.
func clearFailedControl(w http.ResponseWriter, r *http.Request, err error) {
	code := errcode.CodeOf(err)
	if code != errcode.TokenExpired && code != errcode.TokenRevoked && code != errcode.AuthRequired {
		return
	}
	if _, e := r.Cookie(backupControlCookie); e != nil {
		return
	}
	c := controlCookie("", time.Unix(0, 0))
	c.MaxAge = -1
	http.SetCookie(w, c)
}
