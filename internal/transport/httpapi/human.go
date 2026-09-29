package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/provenance"
)

type HumanIntent struct {
	Kind    string          `json:"kind"`
	Request json.RawMessage `json:"request"`
}
type HumanPrepareRequest struct {
	Items []HumanIntent `json:"items"`
}
type HumanExecuteRequest struct {
	GrantID     ids.ID `json:"grant_id"`
	OperationID ids.ID `json:"operation_id"`
}

func decodeIntent[T any](b json.RawMessage) (T, error) {
	var v T
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(&v); e != nil {
		return v, invalid("invalid human request fields")
	}
	return v, nil
}
func (h *Handler) humanRoutes(m *http.ServeMux) {
	h.route(m, "POST /api/v1/human/prepare", false, h.prepareHuman)
	h.route(m, "POST /api/v1/human/rechallenge", false, h.rechallengeHuman)
	h.route(m, "GET /api/v1/human/grants/{id}", false, readEndpoint(h.deps.Human.DomainItems))
	h.route(m, "POST /api/v1/human/execute", false, h.executeHuman)
}
func (h *Handler) intent(ctx context.Context, who authz.Context, i HumanIntent) (identity.HumanAction, error) {
	switch i.Kind {
	case "review":
		if h.deps.Reviews != nil {
			in, e := decodeIntent[ledger.ReviewDecision](i.Request)
			if e != nil {
				return identity.HumanAction{}, e
			}
			return h.deps.Reviews.HumanAction(ctx, in)
		}
	case "control":
		if h.deps.Ledger != nil {
			in, e := decodeIntent[ledger.ControlMutation](i.Request)
			if e != nil {
				return identity.HumanAction{}, e
			}
			return h.deps.Ledger.ControlHumanAction(ctx, in)
		}
	case "trash", "force_trash":
		if h.deps.Lifecycle != nil {
			in, e := decodeIntent[ledger.TrashSelector](i.Request)
			if e != nil {
				return identity.HumanAction{}, e
			}
			snapshot, e := h.deps.Lifecycle.PreviewTrash(ctx, who, in)
			if e != nil {
				return identity.HumanAction{}, e
			}
			return h.deps.Lifecycle.TrashHumanAction(ctx, snapshot, i.Kind == "force_trash")
		}
	case "trash_mutation":
		if h.deps.Lifecycle != nil {
			in, e := decodeIntent[ledger.TrashMutation](i.Request)
			if e != nil {
				return identity.HumanAction{}, e
			}
			return h.deps.Lifecycle.MutationHumanAction(ctx, in)
		}
	case "release_name":
		if h.deps.Lifecycle != nil {
			in, e := decodeIntent[ledger.NameReleaseRequest](i.Request)
			if e != nil {
				return identity.HumanAction{}, e
			}
			return h.deps.Lifecycle.NameReleaseHumanAction(ctx, in)
		}
	case "rights":
		if h.deps.Rights != nil {
			in, e := decodeIntent[provenance.AssertionRequest](i.Request)
			if e != nil {
				return identity.HumanAction{}, e
			}
			return h.deps.Rights.AssertionHumanAction(ctx, in)
		}
	}
	return identity.HumanAction{}, errcode.New(errcode.UnsupportedCapability, "human action is not enabled")
}
func (h *Handler) prepareHuman(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in HumanPrepareRequest
	if e := h.decode(w, r, &in); e != nil {
		return e
	}
	if len(in.Items) < 1 || len(in.Items) > 100 {
		return invalid("1-100 human items required")
	}
	actions := make([]identity.HumanAction, 0, len(in.Items))
	for _, i := range in.Items {
		a, e := h.intent(r.Context(), who, i)
		if e != nil {
			return e
		}
		actions = append(actions, a)
	}
	challenge, e := h.deps.Human.CreateDomainChallenge(r.Context(), who, actions, h.deps.HumanTargets)
	if e != nil {
		return e
	}
	return respond(w, 201, map[string]any{"challenge": challenge, "items": actions})
}
func (h *Handler) rechallengeHuman(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in struct {
		OperationID ids.ID `json:"operation_id"`
	}
	if e := h.decode(w, r, &in); e != nil {
		return e
	}
	out, e := h.deps.Human.RechallengeDomain(r.Context(), who, in.OperationID, h.deps.HumanTargets)
	if e != nil {
		return e
	}
	return respond(w, 201, out)
}
func (h *Handler) executeHuman(w http.ResponseWriter, r *http.Request, who authz.Context) error {
	var in HumanExecuteRequest
	if e := h.decode(w, r, &in); e != nil {
		return e
	}
	if !in.GrantID.Valid() || !in.OperationID.Valid() {
		return invalid("grant and child operation required")
	}
	items, e := h.deps.Human.DomainItems(r.Context(), who, in.GrantID)
	if e != nil {
		return e
	}
	for _, item := range items {
		if item.OperationID == in.OperationID {
			out, e := h.executeHumanItem(r.Context(), who, in, item.Action)
			if e != nil {
				return e
			}
			return respond(w, 200, out)
		}
	}
	return errcode.New(errcode.HumanGrantMismatch, "")
}

// Execution consumes the server-stored item verbatim. The client cannot replace
// a frozen target, inventory, revision, request, or batch child after approval.
func (h *Handler) executeHumanItem(ctx context.Context, who authz.Context, i HumanExecuteRequest, a identity.HumanAction) (any, error) {
	switch a.Action {
	case identity.ActRecordReview, identity.ActRevokeReview:
		in, e := decodeIntent[ledger.ReviewDecision](a.Request)
		if e != nil {
			return nil, e
		}
		return h.deps.Reviews.Record(ctx, who, in, i.GrantID, i.OperationID, h.deps.Human)
	case "ledger.trash", "ledger.force_trash":
		in, e := decodeIntent[ledger.TrashRequest](a.Request)
		if e != nil {
			return nil, e
		}
		return h.deps.Lifecycle.TrashHuman(ctx, who, in, a.Action == "ledger.force_trash", i.GrantID, i.OperationID, h.deps.Human)
	case identity.ActHold, identity.ActUnhold, identity.ActPurge:
		in, e := decodeIntent[ledger.TrashMutation](a.Request)
		if e != nil {
			return nil, e
		}
		return h.deps.Lifecycle.MutateTrashHuman(ctx, who, in, i.GrantID, i.OperationID, h.deps.Human)
	case identity.ActReleaseName:
		in, e := decodeIntent[ledger.NameReleaseRequest](a.Request)
		if e != nil {
			return nil, e
		}
		return h.deps.Lifecycle.ReleaseNameHuman(ctx, who, in, i.GrantID, i.OperationID, h.deps.Human)
	case identity.ActReleaseRestriction:
		in, e := decodeIntent[provenance.AssertionRequest](a.Request)
		if e != nil {
			return nil, e
		}
		return h.deps.Rights.ApplyHumanAssertion(ctx, who, in, i.GrantID, i.OperationID, h.deps.Human)
	case "ledger.unlock", "ledger.disable_version", "ledger.enable_version", "ledger.suspend", "ledger.archive", "ledger.unarchive":
		in, e := decodeIntent[ledger.ControlMutation](a.Request)
		if e != nil {
			return nil, e
		}
		return h.deps.Ledger.ChangeControl(ctx, who, in, i.GrantID, i.OperationID, h.deps.Human, h.deps.Flows)
	}
	return nil, errcode.New(errcode.UnsupportedCapability, "human action is not enabled")
}
