package httpapi

import (
	"context"
	"net/http"

	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

// commandEndpoint only decodes the public request and delegates to its owner.
// Authentication, CSRF, size and timeout enforcement remain the common guard.
func commandEndpoint[I, O any](h *Handler, fn func(context.Context, authz.Context, string, I) (O, error)) endpoint {
	return func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		var in I
		if e := h.decode(w, r, &in); e != nil {
			return e
		}
		k, e := key(r)
		if e != nil {
			return e
		}
		out, e := fn(r.Context(), who, k, in)
		if e != nil {
			return e
		}
		return respond(w, 200, out)
	}
}
func readEndpoint[O any](fn func(context.Context, authz.Context, ids.ID) (O, error)) endpoint {
	return func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		id, e := pathID(r, "id")
		if e != nil {
			return e
		}
		out, e := fn(r.Context(), who, id)
		if e != nil {
			return e
		}
		return respond(w, 200, out)
	}
}
func pageEndpoint[O any](fn func(context.Context, authz.Context, ids.ID, ids.ID, int) ([]O, error), filter string) endpoint {
	return func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		id := ids.ID(r.URL.Query().Get(filter))
		after := ids.ID(r.URL.Query().Get("after"))
		n, e := limit(r)
		if e != nil {
			return e
		}
		if !id.Valid() || after != "" && !after.Valid() {
			return invalid("valid filter and after IDs required")
		}
		out, e := fn(r.Context(), who, id, after, n)
		if e != nil {
			return e
		}
		return respond(w, 200, map[string]any{"items": out})
	}
}
func (h *Handler) executionRoutes(m *http.ServeMux) {
	if s := h.deps.Tasks; s != nil {
		h.route(m, "GET /api/v1/tasks", false, pageEndpoint(func(ctx context.Context, w authz.Context, p, a ids.ID, n int) ([]tasks.Task, error) {
			return s.List(ctx, w, p, a, n, false)
		}, "project_id"))
		h.route(m, "GET /api/v1/tasks/{id}", false, readEndpoint(s.Task))
		h.route(m, "GET /api/v1/tasks/{id}/attempts", false, readEndpoint(s.Attempts))
		h.route(m, "POST /api/v1/tasks", false, commandEndpoint(h, s.Create))
		h.route(m, "POST /api/v1/tasks/claim", false, commandEndpoint(h, s.Claim))
		h.route(m, "POST /api/v1/tasks/renew", false, commandEndpoint(h, s.Renew))
		h.route(m, "POST /api/v1/tasks/release", false, commandEndpoint(h, s.Release))
		h.route(m, "POST /api/v1/tasks/submit", false, commandEndpoint(h, s.Submit))
		h.route(m, "POST /api/v1/tasks/block", false, commandEndpoint(h, s.Block))
		h.route(m, "POST /api/v1/tasks/handoff", false, commandEndpoint(h, s.Handoff))
		h.route(m, "POST /api/v1/tasks/assign", false, commandEndpoint(h, s.Assign))
		h.route(m, "POST /api/v1/tasks/answer", false, commandEndpoint(h, s.Answer))
		h.route(m, "POST /api/v1/tasks/cancel", false, commandEndpoint(h, s.Cancel))
		h.route(m, "POST /api/v1/tasks/reconcile", false, commandEndpoint(h, s.Reconcile))
		h.route(m, "POST /api/v1/tasks/complete", false, commandEndpoint(h, s.Complete))
		h.route(m, "POST /api/v1/tasks/rework", false, commandEndpoint(h, s.Rework))
	}
	if s := h.deps.Flows; s != nil {
		h.route(m, "GET /api/v1/flows", false, pageEndpoint(s.List, "project_id"))
		h.route(m, "GET /api/v1/flows/{id}", false, readEndpoint(s.Flow))
		h.route(m, "POST /api/v1/flows", false, commandEndpoint(h, s.Start))
		h.route(m, "POST /api/v1/flows/pause", false, commandEndpoint(h, s.Pause))
		h.route(m, "POST /api/v1/flows/resume", false, commandEndpoint(h, s.Resume))
		h.route(m, "POST /api/v1/flows/cancel", false, commandEndpoint(h, s.Cancel))
		h.route(m, "POST /api/v1/flows/retry", false, commandEndpoint(h, s.Retry))
		h.route(m, "POST /api/v1/flows/retry-command", false, commandEndpoint(h, s.RetryCommand))
		h.route(m, "POST /api/v1/flows/dispatch", false, h.flowDispatch)
	}
	if s := h.deps.Execution; s != nil {
		h.route(m, "GET /api/v1/task-runs/capabilities", false, func(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
			return respond(w, 200, ax.Capabilities())
		})
		h.route(m, "GET /api/v1/task-runs", false, pageEndpoint(s.List, "task_id"))
		h.route(m, "GET /api/v1/task-runs/{id}", false, readEndpoint(s.Read))
		h.route(m, "POST /api/v1/task-runs", false, commandEndpoint(h, s.Start))
		h.route(m, "POST /api/v1/task-runs/progress", false, commandEndpoint(h, s.Progress))
		h.route(m, "POST /api/v1/task-runs/tool", false, commandEndpoint(h, s.Tool))
		h.route(m, "POST /api/v1/task-runs/candidate", false, commandEndpoint(h, s.Candidate))
		h.route(m, "POST /api/v1/task-runs/checkpoint", false, commandEndpoint(h, s.Checkpoint))
		h.route(m, "POST /api/v1/task-runs/ask", false, commandEndpoint(h, s.Ask))
		h.route(m, "POST /api/v1/task-runs/answer", false, commandEndpoint(h, s.Answer))
		h.route(m, "POST /api/v1/task-runs/cancel", false, commandEndpoint(h, s.Cancel))
		h.route(m, "POST /api/v1/task-runs/reconcile", false, commandEndpoint(h, s.Reconcile))
		h.route(m, "POST /api/v1/task-runs/seal", false, commandEndpoint(h, s.Seal))
	}
	if s := h.deps.Jobs; s != nil {
		h.route(m, "GET /api/v1/jobs", false, pageEndpoint(s.List, "project_id"))
		h.route(m, "GET /api/v1/jobs/{id}", false, readEndpoint(s.Read))
		h.route(m, "POST /api/v1/jobs/run", false, commandEndpoint(h, s.Run))
		h.route(m, "POST /api/v1/jobs/cancel", false, commandEndpoint(h, s.Cancel))
		h.route(m, "POST /api/v1/jobs/retry", false, commandEndpoint(h, s.Retry))
		h.route(m, "POST /api/v1/jobs/reconcile", false, commandEndpoint(h, s.Reconcile))
	}
	if s := h.deps.Nodes; s != nil {
		h.route(m, "POST /api/v1/nodes/observe", false, commandEndpoint(h, s.Observe))
	}
}

// Dispatch is explicitly requested; opening a server does not enable triggers.
func (h *Handler) flowDispatch(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in struct {
		Limit int `json:"limit"`
	}
	if e := h.decode(w, r, &in); e != nil {
		return e
	}
	if in.Limit < 1 || in.Limit > 100 {
		return invalid("limit must be 1-100")
	}
	if _, e := key(r); e != nil {
		return e
	}
	if _, e := h.deps.Flows.CatchUp(r.Context(), in.Limit); e != nil {
		return e
	}
	n, e := h.deps.Flows.Dispatch(r.Context(), who, in.Limit)
	if e != nil {
		return e
	}
	j, e := h.deps.Flows.SyncJobs(r.Context(), who, in.Limit)
	if e != nil {
		return e
	}
	return respond(w, 200, map[string]int{"dispatched": n, "jobs_synchronized": j})
}
