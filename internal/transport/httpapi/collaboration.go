package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/query"
)

func sequence(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, e := strconv.ParseInt(v, 10, 64)
	if e != nil || n < 0 {
		return 0, invalid("nonnegative sequence required")
	}
	return n, nil
}
func projectID(r *http.Request) (ids.ID, error) {
	id := ids.ID(r.URL.Query().Get("project_id"))
	if !id.Valid() {
		return "", invalid("project_id required")
	}
	return id, nil
}
func (h *Handler) collaborationRoutes(m *http.ServeMux) {
	if h.deps.Ledger != nil && h.deps.Discussions != nil {
		h.route(m, "POST /api/v1/messages", false, commandEndpoint(h, func(ctx context.Context, w authz.Context, key string, in ledger.MessageInput) (ledger.Message, error) {
			return h.deps.Ledger.PostMessage(ctx, w, key, in, h.deps.Discussions)
		}))
		h.route(m, "GET /api/v1/messages", false, h.messages)
	}
	if s := h.deps.Collaboration; s != nil {
		h.route(m, "GET /api/v1/events", false, h.collaborationEvents)
		h.route(m, "GET /api/v1/resync", false, h.collaborationResync)
		h.route(m, "GET /api/v1/inbox", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
			p, e := projectID(r)
			if e != nil {
				return e
			}
			out, e := s.Inbox(r.Context(), who, p)
			if e != nil {
				return e
			}
			return respond(w, 200, out)
		})
		h.route(m, "POST /api/v1/inbox/read", false, h.inboxRead)
	}
	if s := h.deps.Reviews; s != nil {
		h.route(m, "POST /api/v1/review-targets", false, commandEndpoint(h, s.Submit))
		h.route(m, "GET /api/v1/review-targets/{id}", false, readEndpoint(s.Target))
		h.route(m, "POST /api/v1/publications", false, commandEndpoint(h, s.Publish))
		h.route(m, "GET /api/v1/context", false, h.effectiveContext)
	}
	if s := h.deps.Lifecycle; s != nil {
		h.route(m, "POST /api/v1/trash/preview", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
			var in ledger.TrashSelector
			if e := h.decode(w, r, &in); e != nil {
				return e
			}
			out, e := s.PreviewTrash(r.Context(), who, in)
			if e != nil {
				return e
			}
			return respond(w, 200, out)
		})
		h.route(m, "POST /api/v1/trash/own", false, commandEndpoint(h, s.TrashOwn))
		h.route(m, "POST /api/v1/trash/restore", false, commandEndpoint(h, s.Restore))
		h.route(m, "GET /api/v1/trash/{id}", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
			p, e := projectID(r)
			if e != nil {
				return e
			}
			id, e := pathID(r, "id")
			if e != nil {
				return e
			}
			out, e := s.Entry(r.Context(), who, p, id)
			if e != nil {
				return e
			}
			return respond(w, 200, out)
		})
	}
	if s := h.deps.Rights; s != nil {
		h.route(m, "POST /api/v1/rights/assertions", false, commandEndpoint(h, s.ApplyAssertion))
		h.route(m, "POST /api/v1/rights/cancel", false, commandEndpoint(h, s.CancelAssertion))
	}
	if h.deps.Evidence != nil {
		h.route(m, "POST /api/v1/evidence", false, commandEndpoint(h, h.deps.Evidence.AppendEvidence))
	}
	if h.deps.Human != nil && h.deps.HumanTargets != nil {
		h.humanRoutes(m)
	}
}
func (h *Handler) messages(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	p, e := projectID(r)
	if e != nil {
		return e
	}
	after, e := sequence(r, "after")
	if e != nil {
		return e
	}
	n, e := limit(r)
	if e != nil {
		return e
	}
	target := ledger.DiscussionTarget{ProjectID: p, Kind: r.URL.Query().Get("kind"), ID: ids.ID(r.URL.Query().Get("id"))}
	out, e := h.deps.Ledger.Messages(r.Context(), who, target, after, n, h.deps.Discussions)
	if e != nil {
		return e
	}
	return respond(w, 200, map[string]any{"items": out})
}
func (h *Handler) collaborationEvents(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	p, e := projectID(r)
	if e != nil {
		return e
	}
	after, e := sequence(r, "after")
	if e != nil {
		return e
	}
	n, e := limit(r)
	if e != nil {
		return e
	}
	wait, e := sequence(r, "wait_seconds")
	if e != nil {
		return e
	}
	if wait > 20 {
		return invalid("wait_seconds must be 0-20")
	}
	out, e := h.deps.Collaboration.Events(r.Context(), query.EventRequest{Who: who, ProjectID: p, After: after, Limit: n, Wait: time.Duration(wait) * time.Second})
	if e != nil {
		return e
	}
	return respond(w, 200, out)
}
func (h *Handler) collaborationResync(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	p, e := projectID(r)
	if e != nil {
		return e
	}
	n, e := limit(r)
	if e != nil {
		return e
	}
	out, e := h.deps.Collaboration.Resync(r.Context(), who, p, r.URL.Query().Get("cursor"), n)
	if e != nil {
		return e
	}
	return respond(w, 200, out)
}
func (h *Handler) inboxRead(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in struct {
		ProjectID ids.ID `json:"project_id"`
		Through   int64  `json:"through"`
	}
	if e := h.decode(w, r, &in); e != nil {
		return e
	}
	if e := h.deps.Collaboration.MarkRead(r.Context(), who, in.ProjectID, in.Through); e != nil {
		return e
	}
	return respond(w, 200, map[string]int64{"through": in.Through})
}
func (h *Handler) effectiveContext(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	p, e := projectID(r)
	if e != nil {
		return e
	}
	out, e := h.deps.ContextCatalog.EffectiveContext(r.Context(), who, p, manifest.AssetType(r.URL.Query().Get("asset_type")), h.deps.Reviews)
	if e != nil {
		return e
	}
	return respond(w, 200, out)
}
