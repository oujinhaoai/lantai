package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// NameReleaseRequest binds the exact current claim, independently from trash or
// purge. Ordinary deletion never implies permission to reuse a name.
type NameReleaseRequest struct {
	ProjectID        ids.ID `json:"project_id"`
	AssetID          ids.ID `json:"asset_id"`
	Slug             string `json:"slug"`
	Generation       int64  `json:"generation"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (l *Lifecycle) NameReleaseHumanAction(ctx context.Context, in NameReleaseRequest) (identity.HumanAction, error) {
	var out identity.HumanAction
	if !in.ProjectID.Valid() || !in.AssetID.Valid() || in.Generation < 1 || in.ExpectedRevision < 1 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 {
		return out, invalid("precise namespace claim and reason required")
	}
	if err := pathrule.CheckSlug(in.Slug); err != nil {
		return out, err
	}
	a, err := l.ledger.Asset(ctx, in.AssetID)
	if err != nil {
		return out, err
	}
	if a.ProjectID != in.ProjectID {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return out, err
	}
	return identity.HumanAction{Action: identity.ActReleaseName, ProjectID: in.ProjectID, ResourceID: in.AssetID, ResourceRevision: in.ExpectedRevision, Request: raw}, nil
}
func (l *Lifecycle) validateNameReleaseTarget(ctx context.Context, action identity.HumanAction) error {
	var in NameReleaseRequest
	if err := json.Unmarshal(action.Request, &in); err != nil {
		return err
	}
	want, err := l.NameReleaseHumanAction(ctx, in)
	if err != nil {
		return err
	}
	a, err := canonjson.CanonicalizeValue(action)
	if err != nil {
		return err
	}
	b, err := canonjson.CanonicalizeValue(want)
	if err != nil {
		return err
	}
	if string(a) != string(b) {
		return errcode.New(errcode.HumanGrantMismatch, "")
	}
	return nil
}
func (l *Lifecycle) ReleaseNameHuman(ctx context.Context, who authz.Context, in NameReleaseRequest, grant, child ids.ID, human HumanCommands) (commands.Receipt, error) {
	if human == nil {
		return commands.Receipt{}, errcode.New(errcode.HumanProofRequired, "")
	}
	action, err := l.NameReleaseHumanAction(ctx, in)
	if err != nil {
		return commands.Receipt{}, err
	}
	return human.AcceptHumanItem(ctx, who, grant, child, action, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}, Assets: []string{string(in.AssetID)}}, &humanNameRelease{l, in})
}

type humanNameRelease struct {
	l  *Lifecycle
	in NameReleaseRequest
}

func (h *humanNameRelease) Receipt(ctx context.Context, op ids.ID) (commands.Receipt, error) {
	r, err := h.l.ledger.store.ReceiptByOperation(ctx, h.l.ledger.db, op)
	if err != nil {
		return commands.Receipt{}, err
	}
	return *r, nil
}
func (h *humanNameRelease) Commit(ctx context.Context, cmd commands.Context, _ identity.HumanAction) (commands.Receipt, error) {
	s := h.l.ledger
	in := h.in
	if prior, err := s.lookup(ctx, s.db, cmd); err != nil {
		return commands.Receipt{}, err
	} else if prior != nil {
		return *prior, nil
	}
	asset, err := s.Asset(ctx, in.AssetID)
	if err != nil {
		return commands.Receipt{}, err
	}
	state, err := s.AssetControl(ctx, in.AssetID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if state.PendingOperationID != "" {
		return commands.Receipt{}, errcode.New(errcode.ResourceBusy, "")
	}
	// A live asset cannot lose its current name. A retained historical alias may
	// be explicitly released without rewriting its immutable allocation record.
	if pathrule.Key(asset.Slug) == pathrule.Key(in.Slug) && state.Lifecycle != "trashed" && state.Lifecycle != "purged" {
		return commands.Receipt{}, errcode.New(errcode.InvalidStateTransition, "live asset still occupies this name")
	}
	if err = s.checkPathLock(ctx, in.ProjectID, in.AssetID, in.Slug); err != nil {
		return commands.Receipt{}, err
	}
	result, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		claim, err := readJSON[commit.Claim](ctx, tx, `SELECT record FROM ledger_namespace_claims WHERE project_id=? AND slug_key=?`, in.ProjectID, pathrule.Key(in.Slug))
		if err != nil {
			return commands.Result{}, err
		}
		if claim.AssetID != in.AssetID || claim.Generation != in.Generation || claim.Revision != in.ExpectedRevision || claim.State != commit.ClaimActive {
			return commands.Result{}, errcode.New(errcode.PreconditionFailed, "namespace claim changed")
		}
		claim.State = commit.ClaimReleased
		claim.Revision++
		claim.OperationID = cmd.OperationID
		if _, err = tx.ExecContext(ctx, `UPDATE ledger_namespace_claims SET record=? WHERE project_id=? AND slug_key=?`, encoded(claim), in.ProjectID, pathrule.Key(in.Slug)); err != nil {
			return commands.Result{}, err
		}
		ev, err := s.event(cmd, "ledger.name_released", "asset", in.AssetID, claim.Revision, map[string]any{"asset_id": in.AssetID, "generation": in.Generation, "reason": in.Reason, "human_grant_id": cmd.HumanGrantID})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: claim, Events: []event.Envelope{ev}}, nil
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *result.Receipt, nil
}
