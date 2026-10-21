package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func (h *Handler) managementRoutes(m *http.ServeMux) {
	h.route(m, "GET /api/v1/admin/instance", false, h.managementInfo)
	h.route(m, "POST /api/v1/management/batches/check", false, h.checkManagementBatch)
}

func (h *Handler) checkManagementBatch(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in struct {
		AssetID   ids.ID `json:"asset_id"`
		VersionID ids.ID `json:"version_id"`
	}
	if err := h.decode(w, r, &in); err != nil {
		return err
	}
	if !in.AssetID.Valid() || !in.VersionID.Valid() {
		return invalid("a fixed management record version is required")
	}
	source, ok := h.deps.Operations.(interface {
		CheckManagementBatch(context.Context, authz.Context, ids.ID, ids.ID) (json.RawMessage, error)
	})
	if !ok {
		return errcode.New(errcode.UnsupportedCapability, "batch acceptance is unavailable")
	}
	value, err := source.CheckManagementBatch(r.Context(), who, in.AssetID, in.VersionID)
	if err != nil {
		return err
	}
	return respond(w, http.StatusOK, value)
}

func (h *Handler) managementInfo(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	source, ok := h.deps.Operations.(interface {
		ManagementInfo(context.Context) (operations.ManagementInfo, error)
	})
	if h.deps.Human == nil || !ok {
		return errcode.New(errcode.UnsupportedCapability, "administrator instance view is unavailable")
	}
	return h.deps.Human.BeginRead(r.Context(), who, identity.ActBackupRead, authz.Resource{}, func(ctx context.Context, _ authz.Decision) error {
		value, err := source.ManagementInfo(ctx)
		if err != nil {
			return errcode.New(errcode.StorageUnavailable, "administrator instance view is unavailable")
		}
		return respond(w, http.StatusOK, value)
	})
}
