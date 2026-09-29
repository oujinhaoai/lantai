package tasks

import (
	"context"
	"slices"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/query"
)

// 以下只读端口供 T01/T03/T04 调用。它们继承调用者已持有的锁，只读 runtime，
// 不再取锁、不写其他模块数据，也不把事件载荷当作当前事实。
var (
	_ query.CollaborationTasks    = (*Service)(nil)
	_ ledger.DiscussionTasks      = (*Service)(nil)
	_ identity.TaskProgressReader = (*Service)(nil)
)

var priorities = map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}

// Current 返回任务当前状态；无权或不存在时由调用者隐藏。
func (s *Service) Current(ctx context.Context, who authz.Context, ref query.ObjectRef) (query.ObjectState, error) {
	var out query.ObjectState
	t, m, err := loadTask(ctx, s.d.DB, ref.ID)
	if err != nil {
		return out, err
	}
	if t.ProjectID != ref.ProjectID || ref.Kind != "task" {
		return out, errcode.New(errcode.NotFound, "")
	}
	if err = s.authorize(ctx, who, "tasks.read", t.ProjectID, "task", t.ID); err != nil {
		return out, err
	}
	out = query.ObjectState{Ref: ref, Revision: t.Revision, Title: t.Title, State: string(t.State), Priority: priorities[t.Priority], DueAt: t.DueAt}
	if slices.Contains([]string{"todo", "rework", "blocked", "submitted", "reconciling"}, string(t.State)) {
		out.WaitingSince = m.StateSince
	}
	return out, nil
}

// Refs 按 ID 递增枚举项目任务（含已结束任务，供重同步替换）。
func (s *Service) Refs(ctx context.Context, project, after ids.ID, limit int) ([]ids.ID, error) {
	if !project.Valid() || limit < 1 || limit > 10000 {
		return nil, invalid("project and bounded limit required")
	}
	rows, err := s.d.DB.QueryContext(ctx, `SELECT task_id FROM tasks_tasks WHERE project_id=? AND task_id>? ORDER BY task_id LIMIT ?`, project, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ids.ID{}
	for rows.Next() {
		var id ids.ID
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Recipients 按当前指派展开待办对象：待领任务给指派人或角色池，执行中给
// 当前 Attempt 主体，阻塞/待验收/待对账给任务创建者；已结束任务无人待办。
func (s *Service) Recipients(ctx context.Context, ref query.ObjectRef) ([]ids.ID, error) {
	r, err := loadRow(ctx, s.d.DB, ref.ID)
	if err != nil {
		return nil, err
	}
	if r.task.ProjectID != ref.ProjectID {
		return nil, errcode.New(errcode.NotFound, "")
	}
	out := []ids.ID{}
	holder := func() {
		if r.attempt != nil && r.attempt.State == "active" {
			out = append(out, r.attempt.PrincipalID)
		}
	}
	switch r.task.State {
	case "todo", "rework":
		switch {
		case r.meta.AssigneeID != "":
			out = append(out, r.meta.AssigneeID)
		default:
			members, err := s.d.Members.ProjectRecipients(ctx, r.task.ProjectID)
			if err != nil {
				return nil, err
			}
			roles := claimRoles[r.task.Type]
			if r.task.Type == "review" {
				roles = []identity.Role{identity.RoleOwner, identity.RoleReviewer}
			}
			if r.meta.Role != "" {
				roles = []identity.Role{r.meta.Role}
			}
			for _, m := range members {
				if hasAnyRole(m.Roles, roles...) && !slices.Contains(r.meta.DistinctFrom, m.PrincipalID) {
					out = append(out, m.PrincipalID)
				}
			}
		}
	case "claimed":
		holder()
	case "blocked", "reconciling", "submitted":
		holder()
		out = append(out, r.meta.CreatedBy)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// CheckDiscussion 核对任务存在于该项目且调用者可读；台账另查讨论动作权限。
func (s *Service) CheckDiscussion(ctx context.Context, who authz.Context, target ledger.DiscussionTarget, _ bool) error {
	t, _, err := loadTask(ctx, s.d.DB, target.ID)
	if err != nil {
		return err
	}
	if t.ProjectID != target.ProjectID || target.Kind != "task" {
		return errcode.New(errcode.NotFound, "")
	}
	return s.authorize(ctx, who, "tasks.read", t.ProjectID, "task", t.ID)
}

// CheckDiscussionAnchor 只接受任务输入、输出、草稿、上下文版本或签出资产上的锚点。
func (s *Service) CheckDiscussionAnchor(ctx context.Context, who authz.Context, target ledger.DiscussionTarget, a ledger.Anchor) error {
	if err := s.CheckDiscussion(ctx, who, target, false); err != nil {
		return err
	}
	var n int
	if err := s.d.DB.QueryRowContext(ctx, `SELECT count(*) FROM tasks_refs WHERE task_id=? AND asset_id=? AND (version_id=? OR role='checkout')`, target.ID, a.Ref.AssetID, a.Ref.VersionID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errcode.New(errcode.RefMismatch, "anchor is outside the task's resources")
	}
	return nil
}

// MilestoneTasks 返回恰好所请求的本项目任务；缺失或跨项目即报错。
func (s *Service) MilestoneTasks(ctx context.Context, who authz.Context, project ids.ID, list []ids.ID) ([]Task, error) {
	if err := s.authorize(ctx, who, "tasks.read", project, "project", project); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(list))
	for _, id := range list {
		t, _, err := loadTask(ctx, s.d.DB, id)
		if err != nil {
			return nil, err
		}
		if t.ProjectID != project {
			return nil, errcode.New(errcode.NotFound, "")
		}
		out = append(out, t)
	}
	return out, nil
}

// Use 是一个仍未结束的任务对资产的在用关系。
type Use struct {
	TaskID   ids.ID `json:"task_id"`
	Revision int64  `json:"revision"`
	AssetID  ids.ID `json:"asset_id"`
	Role     string `json:"role"`
}

// OpenUses 返回引用这些资产（输入/输出/草稿/上下文或签出）且未结束的任务。
func (s *Service) OpenUses(ctx context.Context, assets []ids.ID) ([]Use, error) {
	out := []Use{}
	for _, a := range assets {
		rows, err := s.d.DB.QueryContext(ctx, `SELECT DISTINCT r.task_id,t.revision,r.role FROM tasks_refs r JOIN tasks_tasks t ON t.task_id=r.task_id WHERE r.asset_id=? AND t.state NOT IN ('done','cancelled') ORDER BY r.task_id,r.role`, a)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			u := Use{AssetID: a}
			if err = rows.Scan(&u.TaskID, &u.Revision, &u.Role); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, u)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		var n int
		if err = s.d.DB.QueryRowContext(ctx, `SELECT count(*) FROM tasks_checkouts WHERE asset_id=? AND state='active'`, a).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 && !slices.ContainsFunc(out, func(u Use) bool { return u.AssetID == a && u.Role == "checkout" }) {
			out = append(out, Use{AssetID: a, Role: "checkout"})
		}
	}
	return out, nil
}

// OpenInProject 报告项目内是否还有未结束的任务。
func (s *Service) OpenInProject(ctx context.Context, project ids.ID) (int, error) {
	var n int
	err := s.d.DB.QueryRowContext(ctx, `SELECT count(*) FROM tasks_tasks WHERE project_id=? AND state NOT IN ('done','cancelled')`, project).Scan(&n)
	return n, err
}

// LedgerAuthorities 把 T03 的审定记录与证据读取组合为 Authorities 端口。
type LedgerAuthorities struct {
	Reviews  *ledger.Reviews
	Evidence *ledger.FileReviewSources
}

func (a LedgerAuthorities) ReviewByOperation(ctx context.Context, op ids.ID) (ledger.ReviewFact, error) {
	return a.Reviews.ReviewByOperation(ctx, op)
}
func (a LedgerAuthorities) EvidenceByOperation(ctx context.Context, who authz.Context, op ids.ID) (ledger.AcceptedEvidence, error) {
	return a.Evidence.EvidenceByOperation(ctx, who, op)
}

// CatalogContexts 把 T02 的生效上下文包与 T03 审定集合组合为 Contexts 端口。
type CatalogContexts struct {
	Catalog *catalog.Service
	Reviews catalog.ContextReviews
}

func (c CatalogContexts) EffectiveContext(ctx context.Context, who authz.Context, project ids.ID, t manifest.AssetType) (catalog.ContextBundle, error) {
	return c.Catalog.EffectiveContext(ctx, who, project, t, c.Reviews)
}
