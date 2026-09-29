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
	"github.com/oujinhaoai/lantai/internal/identity"
)

// PreviewTrashBatch returns one fixed request per independent item. The 100-item
// bound matches identity's HumanGrant batch; it is never an implicit truncation.
func (l *Lifecycle) PreviewTrashBatch(ctx context.Context, who authz.Context, selectors []TrashSelector) ([]TrashRequest, error) {
	if len(selectors) < 1 || len(selectors) > 100 {
		return nil, invalid("trash batch must contain 1..100 items")
	}
	project := selectors[0].ProjectID
	for _, s := range selectors {
		if err := s.validate(); err != nil {
			return nil, err
		}
		if s.ProjectID != project {
			return nil, invalid("trash batch must have one project")
		}
	}
	ctx, h, err := l.ledger.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(project)}})
	if err != nil {
		return nil, err
	}
	defer h.Release()
	return l.previewTrashBatch(ctx, who, selectors)
}
func (l *Lifecycle) previewTrashBatch(ctx context.Context, who authz.Context, selectors []TrashSelector) ([]TrashRequest, error) {
	out := make([]TrashRequest, 0, len(selectors))
	seen := map[ids.ID]bool{}
	for _, selector := range selectors {
		snapshot, err := l.snapshot(ctx, who, selector)
		if err != nil {
			return nil, err
		}
		for _, target := range allTrashTargets(snapshot.request) {
			if seen[target.VersionID] {
				return nil, invalid("overlapping trash batch items")
			}
			seen[target.VersionID] = true
		}
		out = append(out, snapshot.request)
	}
	return out, nil
}

// PreviewDirectoryTrash expands the current namespace under the same guard used
// for snapshots. Newly created children after confirmation are not added to the
// frozen batch. Already trashed/purged assets are outside this live directory.
func (l *Lifecycle) PreviewDirectoryTrash(ctx context.Context, who authz.Context, project ids.ID, directory, reason string) ([]TrashRequest, error) {
	if !project.Valid() || strings.TrimSpace(reason) == "" || len(reason) > 4096 {
		return nil, invalid("directory project and reason required")
	}
	if err := pathrule.CheckSlug(directory); err != nil {
		return nil, err
	}
	ctx, h, err := l.ledger.gate.Acquire(ctx, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(project)}})
	if err != nil {
		return nil, err
	}
	defer h.Release()
	if err = l.ledger.authorize(ctx, who, "catalog.read", project, "project", project); err != nil {
		return nil, err
	}
	rows, err := l.ledger.db.QueryContext(ctx, `SELECT a.asset_id,a.slug FROM ledger_assets a LEFT JOIN ledger_asset_controls c ON c.asset_id=a.asset_id WHERE a.project_id=? AND a.record IS NOT NULL AND (c.asset_id IS NULL OR c.lifecycle NOT IN ('trashed','purged')) ORDER BY a.asset_id`, project)
	if err != nil {
		return nil, err
	}
	selectors := []TrashSelector{}
	key := pathrule.Key(directory)
	for rows.Next() {
		var asset ids.ID
		var slug string
		if err = rows.Scan(&asset, &slug); err != nil {
			rows.Close()
			return nil, err
		}
		candidate := pathrule.Key(slug)
		if candidate == key || strings.HasPrefix(candidate, key+"/") {
			selectors = append(selectors, TrashSelector{ProjectID: project, AssetID: asset, WholeAsset: true, Directory: directory, Reason: reason})
			if len(selectors) > 100 {
				rows.Close()
				return nil, errcode.New(errcode.QuotaExceeded, "directory exceeds 100-item confirmation batch; choose explicit subdirectories or assets")
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(selectors) == 0 {
		return nil, errcode.New(errcode.NotFound, "")
	}
	return l.previewTrashBatch(ctx, who, selectors)
}

func (l *Lifecycle) TrashBatchHumanActions(ctx context.Context, requests []TrashRequest, force bool) ([]identity.HumanAction, error) {
	if len(requests) < 1 || len(requests) > 100 {
		return nil, invalid("trash batch must contain 1..100 items")
	}
	out := make([]identity.HumanAction, 0, len(requests))
	seen := map[ids.ID]bool{}
	for _, request := range requests {
		if request.ProjectID != requests[0].ProjectID {
			return nil, invalid("trash batch must have one project")
		}
		for _, target := range allTrashTargets(request) {
			if seen[target.VersionID] {
				return nil, invalid("overlapping trash batch items")
			}
			seen[target.VersionID] = true
		}
		action, err := l.TrashHumanAction(ctx, request, force)
		if err != nil {
			return nil, err
		}
		out = append(out, action)
	}
	return out, nil
}

type HumanBatchCommands interface {
	HumanCommands
	DomainItems(context.Context, authz.Context, ids.ID) ([]identity.HumanGrantItem, error)
}
type TrashBatchItemResult = HumanBatchItemResult

// TrashHumanBatch executes exactly the items stored by identity, not a caller's
// replacement list. Each child has its own current authorization, transaction and
// receipt. Failure in one item does not invent success or undo another item.
func (l *Lifecycle) TrashHumanBatch(ctx context.Context, who authz.Context, grant ids.ID, human HumanBatchCommands) ([]TrashBatchItemResult, error) {
	if human == nil {
		return nil, errcode.New(errcode.HumanProofRequired, "")
	}
	items, err := human.DomainItems(ctx, who, grant)
	if err != nil {
		return nil, err
	}
	if len(items) < 1 || len(items) > 100 {
		return nil, invalid("invalid stored trash batch")
	}
	requests := make([]TrashRequest, len(items))
	force := items[0].Action.Action == "ledger.force_trash"
	for i, item := range items {
		if item.Action.Action != "ledger.trash" && item.Action.Action != "ledger.force_trash" {
			return nil, errcode.New(errcode.HumanGrantMismatch, "grant is not a trash batch")
		}
		if item.Action.Action != items[0].Action.Action {
			return nil, errcode.New(errcode.HumanGrantMismatch, "")
		}
		if err = json.Unmarshal(item.Action.Request, &requests[i]); err != nil {
			return nil, err
		}
	}
	actions, err := l.TrashBatchHumanActions(ctx, requests, force)
	if err != nil {
		return nil, err
	}
	for i, action := range actions {
		want, err := canonjson.CanonicalizeValue(action)
		if err != nil {
			return nil, err
		}
		actual, err := canonjson.CanonicalizeValue(items[i].Action)
		if err != nil {
			return nil, err
		}
		if string(want) != string(actual) {
			return nil, errcode.New(errcode.HumanGrantMismatch, "")
		}
	}
	out := make([]TrashBatchItemResult, 0, len(items))
	for i, item := range items {
		result := TrashBatchItemResult{OperationID: item.OperationID, ResourceID: item.Action.ResourceID}
		receipt, err := l.TrashHuman(ctx, who, requests[i], force, grant, item.OperationID, human)
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

func allTrashTargets(request TrashRequest) []TrashVersionTarget {
	out := append([]TrashVersionTarget{}, request.Targets...)
	for _, history := range request.History {
		out = append(out, history.TrashVersionTarget)
	}
	return out
}
