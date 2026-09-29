package tasks

import (
	"context"
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// View 是按当前读取权限返回的任务完整状态。
type View struct {
	Task      Task       `json:"task"`
	Seat      Seat       `json:"seat"`
	Attempt   *Attempt   `json:"current_attempt,omitempty"`
	Meta      Meta       `json:"meta"`
	Checkouts []Checkout `json:"checkouts"`
	Blocks    []Block    `json:"blocks"`
	Handoffs  []Handoff  `json:"handoffs"`
}

func (s *Service) Task(ctx context.Context, who authz.Context, id ids.ID) (View, error) {
	var out View
	r, err := loadRow(ctx, s.d.DB, id)
	if err != nil {
		return out, err
	}
	if err = s.authorize(ctx, who, "tasks.read", r.task.ProjectID, "task", id); err != nil {
		return out, err
	}
	out = View{Task: r.task, Seat: r.seat, Attempt: r.attempt, Meta: r.meta}
	if out.Checkouts, err = checkouts(ctx, s.d.DB, id, false); err != nil {
		return out, err
	}
	if out.Blocks, err = blocks(ctx, s.d.DB, id); err != nil {
		return out, err
	}
	out.Handoffs, err = handoffs(ctx, s.d.DB, id)
	return out, err
}

// Attempts 返回任务的全部执行轮次（按 fence 递增），旧轮次不会被覆盖。
func (s *Service) Attempts(ctx context.Context, who authz.Context, task ids.ID) ([]Attempt, error) {
	t, _, err := loadTask(ctx, s.d.DB, task)
	if err != nil {
		return nil, err
	}
	if err = s.authorize(ctx, who, "tasks.read", t.ProjectID, "task", task); err != nil {
		return nil, err
	}
	return s.AttemptHistory(ctx, task)
}

// List 按任务 ID 递增分页返回项目任务；openOnly 排除已完成与已取消。
func (s *Service) List(ctx context.Context, who authz.Context, project, after ids.ID, limit int, openOnly bool) ([]Task, error) {
	if !project.Valid() || limit < 1 || limit > 500 || after != "" && !after.Valid() {
		return nil, invalid("project and limit 1-500 required")
	}
	if err := s.authorize(ctx, who, "tasks.read", project, "project", project); err != nil {
		return nil, err
	}
	q := `SELECT record FROM tasks_tasks WHERE project_id=? AND task_id>?`
	if openOnly {
		q += ` AND state NOT IN ('done','cancelled')`
	}
	rows, err := s.d.DB.QueryContext(ctx, q+` ORDER BY task_id LIMIT ?`, project, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var raw string
		var t Task
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Operation 按 operation_id 读取本模块回执（恢复与对账）。
func (s *Service) Operation(ctx context.Context, who authz.Context, op ids.ID) (Result, error) {
	r, err := s.store.ReceiptByOperation(ctx, s.d.DB, op)
	if err != nil {
		return Result{}, errcode.New(errcode.NotFound, "")
	}
	if r.OwnerModule != Module || r.Key.ActorID != who.PrincipalID {
		return Result{}, errcode.New(errcode.NotFound, "")
	}
	var out Result
	err = json.Unmarshal(r.ResponseSummary, &out)
	return out, err
}

// Snapshot 是 T05 内部（workflow）使用的受信任读取，不做授权；调用者须已
// 按所属流程授权或只把结果用于核对权威事实。
func (s *Service) Snapshot(ctx context.Context, id ids.ID) (View, error) {
	r, err := loadRow(ctx, s.d.DB, id)
	if err != nil {
		return View{}, err
	}
	out := View{Task: r.task, Seat: r.seat, Attempt: r.attempt, Meta: r.meta}
	if out.Checkouts, err = checkouts(ctx, s.d.DB, id, false); err != nil {
		return out, err
	}
	if out.Blocks, err = blocks(ctx, s.d.DB, id); err != nil {
		return out, err
	}
	out.Handoffs, err = handoffs(ctx, s.d.DB, id)
	return out, err
}

// SubjectTasks 返回评估某制作任务的 qa/review 任务（受信任读取）。
func (s *Service) SubjectTasks(ctx context.Context, project, production ids.ID, typ string) ([]View, error) {
	rows, err := s.d.DB.QueryContext(ctx, `SELECT task_id FROM tasks_tasks WHERE project_id=? AND type=? AND json_extract(meta,'$.subject.flow.task_id')=? ORDER BY task_id`, project, typ, production)
	if err != nil {
		return nil, err
	}
	var list []ids.ID
	for rows.Next() {
		var id ids.ID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(list))
	for _, id := range list {
		v, err := s.Snapshot(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// AttemptHistory 是受信任的执行轮次历史读取（按 fence 递增），供 T05 内部核对。
func (s *Service) AttemptHistory(ctx context.Context, task ids.ID) ([]Attempt, error) {
	rows, err := s.d.DB.QueryContext(ctx, `SELECT record FROM tasks_attempts WHERE task_id=? ORDER BY lease_fence`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var raw string
		var a Attempt
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
