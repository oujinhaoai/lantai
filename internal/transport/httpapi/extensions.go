package httpapi

import (
	"context"
	"net/http"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/extensions"
)

// extensionRoutes expose T09 governance. Import only registers verified bytes;
// enable/disable go through /api/v1/human (HumanGrant); probe reruns the
// restricted probe the enable grant authorized. No route installs, executes
// arbitrary entries or proxies to a plugin.
func (h *Handler) extensionRoutes(m *http.ServeMux) {
	x := h.deps.Extensions
	if x == nil {
		return
	}
	h.route(m, "POST /api/v1/extensions/packages", false, func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		var in extensions.ImportRequest
		if e := h.decode(w, r, &in); e != nil {
			return e
		}
		k, e := key(r)
		if e != nil {
			return e
		}
		out, e := x.Import(r.Context(), who, k, in)
		if e != nil {
			return e
		}
		return respond(w, 201, out)
	})
	h.route(m, "GET /api/v1/extensions/packages", false, listEndpoint(x.Packages))
	h.route(m, "GET /api/v1/extensions/enablements", false, listEndpoint(x.Enablements))
	h.route(m, "POST /api/v1/extensions/enablements/{id}/probe", false, readEndpoint(x.Probe))
	h.route(m, "GET /api/v1/extensions/commands", false, listEndpoint(x.CLICommands))
}

func listEndpoint[O any](fn func(context.Context, authz.Context) ([]O, error)) endpoint {
	return func(w http.ResponseWriter, r *http.Request, who authz.Context) error {
		out, e := fn(r.Context(), who)
		if e != nil {
			return e
		}
		return respond(w, 200, map[string]any{"items": out})
	}
}
