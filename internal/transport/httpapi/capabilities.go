package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/oujinhaoai/lantai/api"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func (h *Handler) agentOperations(w http.ResponseWriter, r *http.Request, _ authz.Context) error {
	n := 10
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		n, err = strconv.Atoi(raw)
		if err != nil {
			return invalid("capability limit must be 1-20")
		}
	}
	page, err := api.AgentOperationList(r.URL.Query().Get("cursor"), n)
	if err != nil {
		if errors.Is(err, api.ErrAgentOperationCursorExpired) {
			return errcode.New(errcode.CursorExpired, err.Error())
		}
		return invalid(err.Error())
	}
	return respond(w, http.StatusOK, page)
}
