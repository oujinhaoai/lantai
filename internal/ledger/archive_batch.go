package ledger

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// Archival composes current resource visibility and the T05 activity authority.
// It owns neither a second task state machine nor a background executor.
type Archival struct {
	s        *Service
	rights   rights.RiskReader
	activity Activity
}

func (s *Service) NewArchival(r rights.RiskReader, activity Activity) (*Archival, error) {
	if r == nil || activity == nil {
		return nil, invalid("archive requires current rights and T05 activity authorities")
	}
	return &Archival{s, r, activity}, nil
}

func (a *Archival) checkAsset(ctx context.Context, who authz.Context, project, asset ids.ID) error {
	if err := a.s.authorize(ctx, who, "catalog.read", project, "asset", asset); err != nil {
		return err
	}
	rows, err := a.s.db.QueryContext(ctx, `SELECT v.version_id FROM ledger_versions v JOIN ledger_version_states s ON s.version_id=v.version_id WHERE v.asset_id=? AND s.lifecycle!='purged' ORDER BY v.version_number`, asset)
	if err != nil {
		return err
	}
	var versions []ids.ID
	for rows.Next() {
		var id ids.ID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		versions = append(versions, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(versions) == 0 {
		return a.s.authorize(ctx, who, "ledger.restore_others", project, "asset", asset)
	}
	for _, id := range versions {
		v, err := a.s.Version(ctx, asset, id)
		if err != nil {
			return err
		}
		if v.ProjectID != project {
			return errcode.New(errcode.RefMismatch, "")
		}
		d, err := a.rights.EvaluateRiskAccess(ctx, who, v.Ref(who.InstanceID))
		if err != nil {
			return err
		}
		if err = d.Err(); err != nil {
			return err
		}
	}
	return nil
}

// PreviewDirectory fixes an asset set and the latest committed version of each
// asset. A directory is a selector, never a standing grant for future children.
func (a *Archival) PreviewDirectory(ctx context.Context, who authz.Context, project ids.ID, directory string, archive bool, reason string) ([]ControlMutation, error) {
	if !project.Valid() || strings.TrimSpace(reason) == "" || len(reason) > 4096 {
		return nil, invalid("archive project and reason required")
	}
	if err := pathrule.CheckSlug(directory); err != nil {
		return nil, err
	}
	ctx, h, err := a.s.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(project)}})
	if err != nil {
		return nil, err
	}
	defer h.Release()
	if err = a.s.authorize(ctx, who, "catalog.read", project, "project", project); err != nil {
		return nil, err
	}
	rows, err := a.s.db.QueryContext(ctx, `SELECT asset_id,slug FROM ledger_assets WHERE project_id=? AND record IS NOT NULL ORDER BY asset_id`, project)
	if err != nil {
		return nil, err
	}
	var assets []ids.ID
	key := pathrule.Key(directory)
	for rows.Next() {
		var id ids.ID
		var slug string
		if err = rows.Scan(&id, &slug); err != nil {
			rows.Close()
			return nil, err
		}
		candidate := pathrule.Key(slug)
		if candidate == key || strings.HasPrefix(candidate, key+"/") {
			assets = append(assets, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	action, from := authz.Action("ledger.archive"), "active"
	if !archive {
		action, from = "ledger.unarchive", "archived"
	}
	out := []ControlMutation{}
	for _, id := range assets {
		state, err := a.s.AssetControl(ctx, id)
		if err != nil {
			return nil, err
		}
		if state.Lifecycle != from {
			continue
		}
		if state.PendingOperationID != "" {
			return nil, errcode.New(errcode.ResourceBusy, "asset has an unfinished lifecycle operation")
		}
		if err = a.checkAsset(ctx, who, project, id); err != nil {
			return nil, err
		}
		v, err := a.s.LatestVersion(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, ControlMutation{Action: action, ProjectID: project, Kind: "asset", ID: id, ExpectedRevision: state.Revision, LatestVersionID: v.VersionID, Reason: reason})
		if len(out) > 100 {
			return nil, errcode.New(errcode.QuotaExceeded, "archive directory exceeds 100-item human batch")
		}
	}
	if len(out) == 0 {
		return nil, errcode.New(errcode.NotFound, "")
	}
	return out, nil
}

func (a *Archival) BatchActions(ctx context.Context, requests []ControlMutation) ([]identity.HumanAction, error) {
	if len(requests) < 1 || len(requests) > 100 {
		return nil, invalid("archive batch requires 1..100 assets")
	}
	seen := map[ids.ID]bool{}
	out := make([]identity.HumanAction, 0, len(requests))
	for _, r := range requests {
		if r.Kind != "asset" || !r.LatestVersionID.Valid() || r.Action != "ledger.archive" && r.Action != "ledger.unarchive" || r.Action != requests[0].Action || r.ProjectID != requests[0].ProjectID || seen[r.ID] {
			return nil, invalid("archive batch must have unique fixed assets in one project and action")
		}
		seen[r.ID] = true
		action, err := a.s.ControlHumanAction(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, action)
	}
	return out, nil
}

func (a *Archival) ValidateHumanTarget(ctx context.Context, who authz.Context, in identity.HumanAction) error {
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(in.Request, &kind); err != nil {
		return err
	}
	if kind.Kind == "project" {
		return a.validateProjectTarget(ctx, in)
	}
	return a.s.ValidateHumanTarget(ctx, who, in)
}

// ChangeBatch consumes only identity's stored items and preserves independent
// child receipts. Failed items can be retried without expanding the target set.
func (a *Archival) ChangeBatch(ctx context.Context, who authz.Context, grant ids.ID, human HumanBatchCommands) ([]HumanBatchItemResult, error) {
	if human == nil {
		return nil, errcode.New(errcode.HumanProofRequired, "")
	}
	items, err := human.DomainItems(ctx, who, grant)
	if err != nil {
		return nil, err
	}
	requests := make([]ControlMutation, len(items))
	for i, item := range items {
		if err = json.Unmarshal(item.Action.Request, &requests[i]); err != nil {
			return nil, err
		}
	}
	actions, err := a.BatchActions(ctx, requests)
	if err != nil {
		return nil, err
	}
	for i, action := range actions {
		want, err := canonjson.CanonicalizeValue(action)
		if err != nil {
			return nil, err
		}
		got, err := canonjson.CanonicalizeValue(items[i].Action)
		if err != nil {
			return nil, err
		}
		if string(want) != string(got) {
			return nil, errcode.New(errcode.HumanGrantMismatch, "")
		}
	}
	out := make([]HumanBatchItemResult, 0, len(items))
	for i, item := range items {
		r := requests[i]
		result := HumanBatchItemResult{OperationID: item.OperationID, ResourceID: r.ID}
		domain := &humanArchiveAsset{humanControl: humanControl{s: a.s, who: who, request: r, activity: a.activity}, archival: a}
		receipt, err := human.AcceptHumanItem(ctx, who, grant, item.OperationID, item.Action, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(r.ProjectID)}}, domain)
		if err != nil {
			result.ErrorCode = errcode.CodeOf(err)
			if result.ErrorCode == "" {
				result.ErrorCode = errcode.Internal
			}
		} else {
			result.Receipt = &receipt
		}
		out = append(out, result)
	}
	return out, nil
}

type humanArchiveAsset struct {
	humanControl
	archival *Archival
}

func (h *humanArchiveAsset) Commit(ctx context.Context, cmd commands.Context, action identity.HumanAction) (commands.Receipt, error) {
	if err := h.archival.checkAsset(ctx, h.who, h.request.ProjectID, h.request.ID); err != nil {
		return commands.Receipt{}, err
	}
	return h.humanControl.Commit(ctx, cmd, action)
}
