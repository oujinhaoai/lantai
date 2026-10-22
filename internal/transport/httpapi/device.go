package httpapi

import (
	"context"
	"net/http"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	dc "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type DeviceAPI interface {
	DeviceClaim(context.Context, authz.Context, string, dc.Claim) (dc.ClaimResponse, error)
	DeviceRead(context.Context, authz.Context, ids.ID) (dc.Device, error)
	DeviceList(context.Context, authz.Context, ids.ID, ids.ID, int) (dc.DevicePage, error)
	DeviceApprove(context.Context, authz.Context, string, ids.ID, dc.ApproveDevice) (dc.Device, error)
	DeviceRevoke(context.Context, authz.Context, string, ids.ID, dc.Control) (dc.Device, error)
	DeviceJobCreate(context.Context, authz.Context, string, ids.ID, dc.CreateJob) (dc.JobView, error)
	DeviceJobs(context.Context, authz.Context, ids.ID, ids.ID, int) (dc.JobPage, error)
	DeviceJob(context.Context, authz.Context, ids.ID) (dc.JobView, error)
	DeviceJobInspect(context.Context, authz.Context, string, ids.ID, ids.ID, dc.InspectRequest) (dc.JobView, error)
	DeviceJobApprove(context.Context, authz.Context, string, ids.ID, ids.ID, dc.ApproveJob) (dc.JobView, error)
	DevicePoll(context.Context, authz.Context, string, ids.ID) (dc.Poll, error)
	DeviceJobHeartbeat(context.Context, authz.Context, string, ids.ID, ids.ID, dc.LeaseRequest) (dc.Poll, error)
	DeviceJobCancel(context.Context, authz.Context, string, ids.ID, ids.ID, dc.Control) (dc.JobView, error)
	DeviceJobResult(context.Context, authz.Context, string, ids.ID, ids.ID, dc.ResultRequest) (dc.JobView, error)
}

func deviceInput[I, O any](h *Handler, fn func(context.Context, authz.Context, string, ids.ID, I) (O, error)) endpoint {
	return func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		id, err := pathID(r, "device_id")
		if err != nil {
			return err
		}
		var in I
		if err = h.decode(w, r, &in); err != nil {
			return err
		}
		key, err := key(r)
		if err != nil {
			return err
		}
		out, err := fn(r.Context(), who, key, id, in)
		if err != nil {
			return err
		}
		return respond(w, 200, out)
	}
}
func jobDeviceInput[I, O any](h *Handler, fn func(context.Context, authz.Context, string, ids.ID, ids.ID, I) (O, error)) endpoint {
	return func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		id, err := pathID(r, "device_id")
		if err != nil {
			return err
		}
		job, err := pathID(r, "job_id")
		if err != nil {
			return err
		}
		var in I
		if err = h.decode(w, r, &in); err != nil {
			return err
		}
		key, err := key(r)
		if err != nil {
			return err
		}
		out, err := fn(r.Context(), who, key, id, job, in)
		if err != nil {
			return err
		}
		return respond(w, 200, out)
	}
}
func (h *Handler) deviceRoutes(m *http.ServeMux) {
	s, ok := h.deps.Operations.(DeviceAPI)
	if !ok {
		return
	}
	h.route(m, "POST /api/v1/devices/claims", false, commandEndpoint(h, s.DeviceClaim))
	h.route(m, "GET /api/v1/devices", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		project := ids.ID(r.URL.Query().Get("project_id"))
		after := ids.ID(r.URL.Query().Get("after"))
		n, err := limit(r)
		if err != nil {
			return err
		}
		out, err := s.DeviceList(r.Context(), who, project, after, n)
		if err != nil {
			return err
		}
		return respond(w, 200, out)
	})
	h.route(m, "GET /api/v1/devices/{device_id}", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		id, err := pathID(r, "device_id")
		if err != nil {
			return err
		}
		out, err := s.DeviceRead(r.Context(), who, id)
		if err != nil {
			return err
		}
		return respond(w, 200, out)
	})
	h.route(m, "POST /api/v1/devices/{device_id}/approve", false, deviceInput(h, s.DeviceApprove))
	h.route(m, "POST /api/v1/devices/{device_id}/revoke", false, deviceInput(h, s.DeviceRevoke))
	h.route(m, "POST /api/v1/devices/{device_id}/jobs", false, deviceInput(h, s.DeviceJobCreate))
	h.route(m, "GET /api/v1/devices/{device_id}/jobs", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		id, err := pathID(r, "device_id")
		if err != nil {
			return err
		}
		after := ids.ID(r.URL.Query().Get("after"))
		n, err := limit(r)
		if err != nil {
			return err
		}
		out, err := s.DeviceJobs(r.Context(), who, id, after, n)
		if err != nil {
			return err
		}
		return respond(w, 200, out)
	})
	h.route(m, "GET /api/v1/devices/{device_id}/jobs/{job_id}", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		id, err := pathID(r, "device_id")
		if err != nil {
			return err
		}
		job, err := pathID(r, "job_id")
		if err != nil {
			return err
		}
		out, err := s.DeviceJob(r.Context(), who, job)
		if err != nil {
			return err
		}
		if out.DeviceID != id {
			return errcode.New(errcode.NotFound, "")
		}
		return respond(w, 200, out)
	})
	h.route(m, "POST /api/v1/devices/{device_id}/poll", false, deviceInput(h, func(ctx context.Context, w authz.Context, key string, id ids.ID, _ struct{}) (dc.Poll, error) {
		return s.DevicePoll(ctx, w, key, id)
	}))
	h.route(m, "POST /api/v1/devices/{device_id}/jobs/{job_id}/inspect", false, jobDeviceInput(h, s.DeviceJobInspect))
	h.route(m, "POST /api/v1/devices/{device_id}/jobs/{job_id}/approve", false, jobDeviceInput(h, s.DeviceJobApprove))
	h.route(m, "POST /api/v1/devices/{device_id}/jobs/{job_id}/heartbeat", false, jobDeviceInput(h, s.DeviceJobHeartbeat))
	h.route(m, "POST /api/v1/devices/{device_id}/jobs/{job_id}/cancel", false, jobDeviceInput(h, s.DeviceJobCancel))
	h.route(m, "POST /api/v1/devices/{device_id}/jobs/{job_id}/result", false, jobDeviceInput(h, s.DeviceJobResult))
}
