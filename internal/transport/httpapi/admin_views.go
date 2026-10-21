package httpapi

import (
	"context"
	"net/http"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

type versionHistoryReader interface {
	ListVersions(context.Context, authz.Context, ids.ID, string, int) (catalog.VersionPage, error)
}

type identityManagementReader interface {
	PrincipalPage(context.Context, authz.Context, ids.ID, int) (identity.PrincipalPage, error)
	CredentialPage(context.Context, authz.Context, ids.ID, ids.ID, int) (identity.CredentialPage, error)
}

func (h *Handler) adminViewRoutes(m *http.ServeMux) {
	h.route(m, "GET /api/v1/assets/{asset_id}/versions", false, h.versionHistory)
	h.route(m, "GET /api/v1/assets/{asset_id}/versions/{version_id}/rights", false, h.versionRights)
	h.route(m, "GET /api/v1/identity/principals", false, h.principalHistory)
	h.route(m, "GET /api/v1/identity/principals/{principal_id}/credentials", false, h.credentialHistory)
}

func (h *Handler) versionRights(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	asset, err := pathID(r, "asset_id")
	if err != nil {
		return err
	}
	version, err := pathID(r, "version_id")
	if err != nil {
		return err
	}
	if h.deps.Rights == nil {
		return errcode.New(errcode.UnsupportedCapability, "rights management view is unavailable")
	}
	view, err := h.deps.Rights.VersionRights(r.Context(), who, ids.PermanentRef{InstanceID: h.cfg.InstanceID, AssetID: asset, VersionID: version})
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, view)
}

func managementCursor(r *http.Request) (ids.ID, error) {
	v := ids.ID(r.URL.Query().Get("cursor"))
	if v != "" && !v.Valid() {
		return "", invalid("invalid identity management cursor")
	}
	return v, nil
}

func (h *Handler) principalHistory(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	n, err := limit(r)
	if err != nil {
		return err
	}
	cursor, err := managementCursor(r)
	if err != nil {
		return err
	}
	reader, ok := h.deps.Identity.(identityManagementReader)
	if !ok {
		return errcode.New(errcode.UnsupportedCapability, "identity management listing is unavailable")
	}
	p, err := reader.PrincipalPage(r.Context(), who, cursor, n)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, p)
}

func (h *Handler) credentialHistory(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	principal, err := pathID(r, "principal_id")
	if err != nil {
		return err
	}
	n, err := limit(r)
	if err != nil {
		return err
	}
	cursor, err := managementCursor(r)
	if err != nil {
		return err
	}
	reader, ok := h.deps.Identity.(identityManagementReader)
	if !ok {
		return errcode.New(errcode.UnsupportedCapability, "identity management listing is unavailable")
	}
	p, err := reader.CredentialPage(r.Context(), who, principal, cursor, n)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, p)
}

func (h *Handler) versionHistory(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	asset, err := pathID(r, "asset_id")
	if err != nil {
		return err
	}
	n, err := limit(r)
	if err != nil {
		return err
	}
	reader, ok := h.deps.Catalog.(versionHistoryReader)
	if !ok {
		return errcode.New(errcode.UnsupportedCapability, "version history is unavailable")
	}
	p, err := reader.ListVersions(r.Context(), who, asset, r.URL.Query().Get("cursor"), n)
	if err != nil {
		return err
	}
	out := struct {
		Items      []VersionView `json:"items"`
		NextCursor string        `json:"next_cursor,omitempty"`
	}{Items: make([]VersionView, 0, len(p.Items)), NextCursor: p.NextCursor}
	for _, v := range p.Items {
		out.Items = append(out.Items, versionView(v, h.cfg.InstanceID))
	}
	return respond(w, http.StatusOK, out)
}
