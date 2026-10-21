package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// BackupMaintenanceRequest is an exact, read/control-only exception to business
// admission. It does not bypass HTTP authentication, live admin authorization,
// Origin/CSRF validation, or the backup owner's mutex. In particular, login,
// grant creation/redemption, start, resume and schedule writes stay closed.
func BackupMaintenanceRequest(r *http.Request) bool {
	if r.URL.EscapedPath() != r.URL.Path {
		return false
	}
	path := r.URL.Path
	if path == backupControlPath+"/control-session" && (r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		return true
	}
	if r.Method == http.MethodGet {
		switch path {
		case "/api/v1/admin/backups/control-session", "/api/v1/whoami", "/api/v1/admin/backups", "/api/v1/admin/backups/history", "/api/v1/admin/backups/schedule":
			return true
		}
	}
	suffix := strings.TrimPrefix(path, "/api/v1/admin/backups/")
	if suffix == path {
		return false
	}
	if r.Method == http.MethodGet {
		return ids.ID(suffix).Valid()
	}
	if r.Method == http.MethodPost && strings.HasSuffix(suffix, "/cancel") {
		return ids.ID(strings.TrimSuffix(suffix, "/cancel")).Valid()
	}
	return false
}
func (h *Handler) backupRoutes(m *http.ServeMux) {
	h.route(m, "POST /api/v1/admin/backups/{id}/download-request", false, h.backupDownloadRequest)
	h.route(m, "GET /api/v1/admin/backups/{id}/package", false, h.backupDownloadPackage)
	h.controlSessionRoutes(m)
	h.backupReadRoute(m, "GET /api/v1/admin/backups", h.backupOverview)
	h.backupReadRoute(m, "GET /api/v1/admin/backups/history", h.backupHistory)
	h.backupReadRoute(m, "GET /api/v1/admin/backups/schedule", h.backupSchedule)
	h.backupReadRoute(m, "GET /api/v1/admin/backups/{id}", h.backupStatus)
	h.route(m, "POST /api/v1/admin/backups", false, h.backupStart)
	h.route(m, "POST /api/v1/admin/backups/{id}/resume", false, h.backupResume)
	h.backupReadRoute(m, "POST /api/v1/admin/backups/{id}/cancel", h.backupCancel)
	h.route(m, "PUT /api/v1/admin/backups/schedule", false, h.backupPutSchedule)
}
func backupError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, backupmgmt.ErrScopeStale):
		return errcode.New(errcode.Forbidden, "backup control session does not match the active attempt")
	case errors.Is(err, backupmgmt.ErrNotFound):
		return errcode.New(errcode.NotFound, "backup not found")
	case errors.Is(err, backupmgmt.ErrBusy):
		return errcode.New(errcode.InvalidStateTransition, "a backup is already active").WithDetails(errcode.Detail{Reason: "backup_busy"})
	case errors.Is(err, backupmgmt.ErrUnavailable):
		return errcode.New(errcode.StorageUnavailable, "backup directory is unavailable; review backup settings").WithDetails(errcode.Detail{Reason: "backup_unavailable"})
	case errors.Is(err, backupmgmt.ErrInvalid):
		return errcode.New(errcode.SchemaInvalid, "invalid backup request").WithDetails(errcode.Detail{Reason: "invalid_backup_request"})
	case errors.Is(err, backupmgmt.ErrIdempotencyConflict):
		return errcode.New(errcode.IdempotencyConflict, "")
	case errors.Is(err, backupmgmt.ErrRevisionConflict):
		return errcode.New(errcode.PreconditionFailed, "backup schedule changed; refresh before editing").WithDetails(errcode.Detail{Reason: "schedule_revision_conflict"})
	default:
		return err
	}
}
func (h *Handler) backupRead(r *http.Request, who authz.Context, action authz.Action, run func() error) error {
	if _, e := r.Cookie(backupControlCookie); e == nil {
		return h.withControl(r, action == identity.ActBackupCancel, func(identity.BackupControlSession) error { return backupError(run()) })
	}
	ctx := r.Context()
	if h.deps.Backups == nil || h.deps.Human == nil {
		return errcode.New(errcode.UnsupportedCapability, "backup management is unavailable")
	}
	// BeginRead takes only the existing security guard. AuthenticateBrowser and
	// current session/role checks are SQL reads; no heartbeat or token renewal is
	// permitted here while the backup owns the consistency barrier.
	return h.deps.Human.BeginRead(ctx, who, action, authz.Resource{}, func(context.Context, authz.Decision) error { return backupError(run()) })
}
func (h *Handler) backupOverview(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	return h.backupRead(r, who, identity.ActBackupRead, func() error {
		v, e := h.deps.Backups.Overview()
		if e != nil {
			return e
		}
		return respond(w, 200, v)
	})
}
func (h *Handler) backupHistory(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	return h.backupRead(r, who, identity.ActBackupRead, func() error {
		v, e := h.deps.Backups.History()
		if e != nil {
			return e
		}
		if v == nil {
			v = []backupmgmt.Job{}
		}
		return respond(w, 200, map[string]any{"items": v})
	})
}
func (h *Handler) backupStatus(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, e := pathID(r, "id")
	if e != nil {
		return e
	}
	return h.backupRead(r, who, identity.ActBackupRead, func() error {
		v, e := h.deps.Backups.Status(id)
		if e != nil {
			return e
		}
		return respond(w, 200, v)
	})
}
func (h *Handler) backupSchedule(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	return h.backupRead(r, who, identity.ActBackupRead, func() error {
		v, e := h.deps.Backups.Overview()
		if e != nil {
			return e
		}
		etag(w, v.Schedule.Revision)
		return respond(w, 200, v.Schedule)
	})
}
func (h *Handler) backupCancel(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	id, e := pathID(r, "id")
	if e != nil {
		return e
	}
	var in struct{}
	if e = h.decode(w, r, &in); e != nil {
		return e
	}
	if _, e := r.Cookie(backupControlCookie); e == nil {
		return h.withControl(r, true, func(v identity.BackupControlSession) error {
			if id != v.Scope.JobID {
				return errcode.New(errcode.Forbidden, "backup control session is bound to a different job")
			}
			scoped, ok := h.deps.Backups.(interface {
				CancelScoped(ids.ID, int) (backupmgmt.Job, error)
			})
			if !ok {
				return errcode.New(errcode.UnsupportedCapability, "scoped cancellation is unavailable")
			}
			out, e := scoped.CancelScoped(id, v.Scope.Attempt)
			if e != nil {
				return backupError(e)
			}
			return respond(w, 202, out)
		})
	}
	return h.backupRead(r, who, identity.ActBackupCancel, func() error {
		v, e := h.deps.Backups.Cancel(id)
		if e != nil {
			return e
		}
		return respond(w, 202, v)
	})
}

type backupRunRequest struct {
	GrantID        ids.ID `json:"grant_id"`
	DrainSeconds   int    `json:"drain_seconds,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

func (h *Handler) approveBackup(r *http.Request, who authz.Context, grant ids.ID, cmd *identity.ApproveBackup) (ids.ID, error) {
	if _, e := r.Cookie(backupControlCookie); e == nil {
		return "", errcode.New(errcode.Forbidden, "backup control session permits only status and cancellation")
	}
	if h.deps.Backups == nil || h.deps.Human == nil {
		return "", errcode.New(errcode.UnsupportedCapability, "backup management is unavailable")
	}
	if !grant.Valid() {
		return "", errcode.New(errcode.HumanProofRequired, "a backup approval is required")
	}
	k, e := key(r)
	if e != nil {
		return "", e
	}
	// Execute fully releases identity's transaction and locks before the owner
	// may close admission. Its operation ID is the owner's persisted replay key.
	v, e := h.deps.Identity.Execute(r.Context(), who, grant, k, cmd)
	return v.OperationID, e
}
func (h *Handler) backupStart(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	return h.backupTrigger(w, r, who, false)
}
func (h *Handler) backupResume(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	return h.backupTrigger(w, r, who, true)
}
func (h *Handler) backupTrigger(w http.ResponseWriter, r *http.Request, who authz.Context, resume bool) error {
	var in backupRunRequest
	if e := h.decode(w, r, &in); e != nil {
		return e
	}
	cmd := &identity.ApproveBackup{Kind: "start", DrainSeconds: in.DrainSeconds, TimeoutSeconds: in.TimeoutSeconds}
	req := backupmgmt.Request{DrainSeconds: in.DrainSeconds, TimeoutSeconds: in.TimeoutSeconds}
	if resume {
		id, e := pathID(r, "id")
		if e != nil {
			return e
		}
		cmd.Kind = "resume"
		cmd.JobID = id
		req.Resume = id
	}
	op, e := h.approveBackup(r, who, in.GrantID, cmd)
	if e != nil {
		return e
	}
	return h.backupRead(r, who, identity.ActBackupRead, func() error {
		out, e := h.deps.Backups.Trigger(req, string(op))
		if e != nil {
			return e
		}
		return respond(w, 202, out)
	})
}
func (h *Handler) backupPutSchedule(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in struct {
		GrantID ids.ID `json:"grant_id"`
		identity.BackupScheduleInput
	}
	if e := h.decode(w, r, &in); e != nil {
		return e
	}
	op, e := h.approveBackup(r, who, in.GrantID, &identity.ApproveBackup{Kind: "schedule", Schedule: &in.BackupScheduleInput})
	if e != nil {
		return e
	}
	return h.backupRead(r, who, identity.ActBackupRead, func() error {
		plan := in.Schedule()
		plan.AuthorizedBy = who.PrincipalID
		out, e := h.deps.Backups.SaveSchedule(plan, string(op))
		if e != nil {
			return e
		}
		etag(w, out.Revision)
		return respond(w, 200, out)
	})
}
