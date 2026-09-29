package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// ProjectActivity must include flows/tasks without an asset binding. A per-asset
// idle check alone cannot authorize a project lifecycle change.
type ProjectActivity interface {
	RequireProjectIdle(context.Context, ids.ID) error
}

type ProjectArchiveTarget struct {
	AssetID         ids.ID `json:"asset_id"`
	Revision        int64  `json:"revision"`
	LatestVersionID ids.ID `json:"latest_version_id"`
}
type ProjectArchiveRequest struct {
	Kind             string                 `json:"kind"`
	Action           authz.Action           `json:"action"`
	ProjectID        ids.ID                 `json:"project_id"`
	ExpectedRevision int64                  `json:"expected_revision"`
	Targets          []ProjectArchiveTarget `json:"targets"`
	Reason           string                 `json:"reason"`
}

func (a *Archival) PreviewProject(ctx context.Context, who authz.Context, project ids.ID, archive bool, reason string) (ProjectArchiveRequest, error) {
	action := authz.Action("ledger.archive")
	if !archive {
		action = "ledger.unarchive"
	}
	ctx, h, err := a.s.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(project)}})
	if err != nil {
		return ProjectArchiveRequest{}, err
	}
	defer h.Release()
	return a.projectSnapshot(ctx, who, project, action, reason)
}

func (a *Archival) projectSnapshot(ctx context.Context, who authz.Context, project ids.ID, action authz.Action, reason string) (ProjectArchiveRequest, error) {
	out := ProjectArchiveRequest{Kind: "project", Action: action, ProjectID: project, Targets: []ProjectArchiveTarget{}, Reason: reason}
	if !project.Valid() || strings.TrimSpace(reason) == "" || len(reason) > 4096 || action != "ledger.archive" && action != "ledger.unarchive" {
		return out, invalid("precise project archival required")
	}
	if err := a.s.authorize(ctx, who, "catalog.read", project, "project", project); err != nil {
		return out, err
	}
	p, err := a.s.Project(ctx, project)
	if err != nil {
		return out, err
	}
	if (action == "ledger.archive" && p.State != commit.ProjectActive) || (action == "ledger.unarchive" && p.State != commit.ProjectArchived) {
		return out, errcode.New(errcode.InvalidStateTransition, "project lifecycle already differs")
	}
	if err = a.s.db.QueryRowContext(ctx, `SELECT control_revision FROM ledger_projects WHERE project_id=?`, project).Scan(&out.ExpectedRevision); err != nil {
		return out, err
	}
	rows, err := a.s.db.QueryContext(ctx, `SELECT asset_id FROM ledger_assets WHERE project_id=? AND record IS NOT NULL ORDER BY asset_id`, project)
	if err != nil {
		return out, err
	}
	var assets []ids.ID
	for rows.Next() {
		var id ids.ID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		assets = append(assets, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, id := range assets {
		state, err := a.s.AssetControl(ctx, id)
		if err != nil {
			return out, err
		}
		if state.PendingOperationID != "" {
			return out, errcode.New(errcode.ResourceBusy, "project contains a pending lifecycle operation")
		}
		if state.Lifecycle == "trashed" || state.Lifecycle == "purged" {
			continue
		}
		if err = a.checkAsset(ctx, who, project, id); err != nil {
			return out, err
		}
		v, err := a.s.LatestVersion(ctx, id)
		if err != nil {
			return out, err
		}
		out.Targets = append(out.Targets, ProjectArchiveTarget{AssetID: id, Revision: state.Revision, LatestVersionID: v.VersionID})
		// Keep the exact target set within identity's 64 KiB action contract.
		if len(out.Targets) > 256 {
			return out, errcode.New(errcode.QuotaExceeded, "project archival exceeds 256 exact assets")
		}
	}
	return out, nil
}

func (a *Archival) ProjectHumanAction(ctx context.Context, in ProjectArchiveRequest) (identity.HumanAction, error) {
	var out identity.HumanAction
	if in.Kind != "project" || !in.ProjectID.Valid() || in.ExpectedRevision < 1 || len(in.Targets) > 256 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 || in.Action != "ledger.archive" && in.Action != "ledger.unarchive" {
		return out, invalid("fixed project archive target required")
	}
	var revision int64
	if err := a.s.db.QueryRowContext(ctx, `SELECT control_revision FROM ledger_projects WHERE project_id=?`, in.ProjectID).Scan(&revision); err != nil {
		return out, missing(err)
	}
	if in.ExpectedRevision > revision {
		return out, errcode.New(errcode.PreconditionFailed, "")
	}
	var previous ids.ID
	for _, target := range in.Targets {
		if !target.AssetID.Valid() || target.AssetID <= previous || target.Revision < 1 || !target.LatestVersionID.Valid() {
			return out, invalid("project targets must be unique and ordered")
		}
		previous = target.AssetID
		v, err := a.s.Version(ctx, target.AssetID, target.LatestVersionID)
		if err != nil {
			return out, err
		}
		if v.ProjectID != in.ProjectID {
			return out, errcode.New(errcode.RefMismatch, "")
		}
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return out, err
	}
	return identity.HumanAction{Action: in.Action, ProjectID: in.ProjectID, ResourceID: in.ProjectID, ResourceRevision: in.ExpectedRevision, Request: raw}, nil
}

func (a *Archival) validateProjectTarget(ctx context.Context, action identity.HumanAction) error {
	var in ProjectArchiveRequest
	if err := json.Unmarshal(action.Request, &in); err != nil {
		return err
	}
	expected, err := a.ProjectHumanAction(ctx, in)
	if err != nil {
		return err
	}
	want, err := canonjson.CanonicalizeValue(expected)
	if err != nil {
		return err
	}
	got, err := canonjson.CanonicalizeValue(action)
	if err != nil {
		return err
	}
	if string(want) != string(got) {
		return errcode.New(errcode.HumanGrantMismatch, "")
	}
	return nil
}

func (a *Archival) ChangeProject(ctx context.Context, who authz.Context, in ProjectArchiveRequest, grant, child ids.ID, human HumanCommands) (commands.Receipt, error) {
	if human == nil {
		return commands.Receipt{}, errcode.New(errcode.HumanProofRequired, "")
	}
	action, err := a.ProjectHumanAction(ctx, in)
	if err != nil {
		return commands.Receipt{}, err
	}
	return human.AcceptHumanItem(ctx, who, grant, child, action, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(in.ProjectID)}}, &humanArchiveProject{a, who, in})
}

type humanArchiveProject struct {
	a   *Archival
	who authz.Context
	in  ProjectArchiveRequest
}

func (h *humanArchiveProject) Receipt(ctx context.Context, op ids.ID) (commands.Receipt, error) {
	r, err := h.a.s.store.ReceiptByOperation(ctx, h.a.s.db, op)
	if err != nil {
		return commands.Receipt{}, err
	}
	return *r, nil
}
func (h *humanArchiveProject) Commit(ctx context.Context, cmd commands.Context, _ identity.HumanAction) (commands.Receipt, error) {
	in, a := h.in, h.a
	snapshot, err := a.projectSnapshot(ctx, h.who, in.ProjectID, in.Action, in.Reason)
	if err != nil {
		return commands.Receipt{}, err
	}
	want, err := canonjson.CanonicalizeValue(snapshot)
	if err != nil {
		return commands.Receipt{}, err
	}
	got, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return commands.Receipt{}, err
	}
	if string(want) != string(got) {
		return commands.Receipt{}, errcode.New(errcode.PreconditionFailed, "project archival target set changed")
	}
	projectActivity, ok := a.activity.(ProjectActivity)
	if !ok {
		return commands.Receipt{}, errcode.New(errcode.AssetInUse, "T05 project activity authority required")
	}
	if err = projectActivity.RequireProjectIdle(ctx, in.ProjectID); err != nil {
		return commands.Receipt{}, err
	}
	assets := make([]ids.ID, 0, len(in.Targets))
	for _, target := range in.Targets {
		assets = append(assets, target.AssetID)
	}
	if err = a.activity.RequireIdle(ctx, in.ProjectID, assets); err != nil {
		return commands.Receipt{}, err
	}
	response, err := a.s.store.Execute(ctx, a.s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		p, err := readJSON[commit.Project](ctx, tx, `SELECT record FROM ledger_projects WHERE project_id=?`, in.ProjectID)
		if err != nil {
			return commands.Result{}, err
		}
		p.State = commit.ProjectArchived
		if in.Action == "ledger.unarchive" {
			p.State = commit.ProjectActive
		}
		result, err := tx.ExecContext(ctx, `UPDATE ledger_projects SET record=?,control_revision=control_revision+1 WHERE project_id=? AND control_revision=?`, encoded(p), in.ProjectID, in.ExpectedRevision)
		if err != nil {
			return commands.Result{}, err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return commands.Result{}, errcode.New(errcode.PreconditionFailed, "project revision changed")
		}
		summary := map[string]any{"project_id": in.ProjectID, "state": p.State, "revision": in.ExpectedRevision + 1, "target_count": len(in.Targets)}
		e, err := a.s.event(cmd, "ledger.control_changed", "project", in.ProjectID, in.ExpectedRevision+1, map[string]any{"action": in.Action, "reason": in.Reason, "human_grant_id": cmd.HumanGrantID})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 200, Summary: summary, Events: []event.Envelope{e}}, nil
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *response.Receipt, nil
}
