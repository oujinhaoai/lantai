package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

type (
	Task    = tc.Task
	Seat    = tc.Seat
	Attempt = tc.Attempt
)

// CheckoutSpec 声明任务领取时签出的现有资产。exclusive 拒绝其他执行轮次的
// 追加版本；advisory 只标记，冲突仍由台账基线检查兜底。
type CheckoutSpec struct {
	AssetID ids.ID `json:"asset_id"`
	Mode    string `json:"mode"`
}

// ContextSnapshot 是任务创建时固定的生效上下文/决议版本与组合摘要。
type ContextSnapshot struct {
	AssetType string             `json:"asset_type"`
	Refs      []ids.PermanentRef `json:"refs"`
	Digest    digest.Digest      `json:"digest"`
}

// Subject 是 qa/review 任务评估的制作轮次与精确版本，等同台账 ReviewFlow。
type Subject struct {
	Flow ledger.ReviewFlow `json:"flow"`
	Ref  ids.PermanentRef  `json:"ref"`
}

// RoundOutcome 保留每一轮的结束依据；返工不覆盖旧轮次。
type RoundOutcome struct {
	Round                int                `json:"round"`
	AttemptID            ids.ID             `json:"attempt_id,omitempty"`
	OutputRefs           []ids.PermanentRef `json:"output_refs"`
	Outcome              string             `json:"outcome"`
	AuthorityOperationID ids.ID             `json:"authority_operation_id,omitempty"`
	Reason               string             `json:"reason,omitempty"`
	At                   string             `json:"at"`
}

// Completion 记录完成所依据的领域权威事实。
type Completion struct {
	Kind                 string `json:"kind"`
	AuthorityOperationID ids.ID `json:"authority_operation_id"`
	Verdict              string `json:"verdict,omitempty"`
	At                   string `json:"at"`
}

// Meta 是 lantai.task/v1 之外由 tasks 维护的指派、轮次与生命周期事实。
type Meta struct {
	Round             int              `json:"round"`
	Description       string           `json:"description,omitempty"`
	AssigneeID        ids.ID           `json:"assignee_id,omitempty"`
	Role              identity.Role    `json:"role,omitempty"`
	DistinctFrom      []ids.ID         `json:"distinct_from"`
	Checkouts         []CheckoutSpec   `json:"checkouts"`
	Context           *ContextSnapshot `json:"context,omitempty"`
	Subject           *Subject         `json:"subject,omitempty"`
	CreatedBy         ids.ID           `json:"created_by"`
	CreatedAt         string           `json:"created_at"`
	StateSince        string           `json:"state_since"`
	ExpiryCount       int              `json:"expiry_count"`
	Stuck             bool             `json:"stuck,omitempty"`
	CancelRequested   bool             `json:"cancel_requested,omitempty"`
	CancelReason      string           `json:"cancel_reason,omitempty"`
	SubmitOperationID ids.ID           `json:"submit_operation_id,omitempty"`
	Completion        *Completion      `json:"completion,omitempty"`
	History           []RoundOutcome   `json:"history"`
}

type Checkout struct {
	TaskID        ids.ID `json:"task_id"`
	AssetID       ids.ID `json:"asset_id"`
	ProjectID     ids.ID `json:"project_id"`
	Mode          string `json:"mode"`
	AttemptID     ids.ID `json:"attempt_id"`
	BaseVersionID ids.ID `json:"base_version_id,omitempty"`
	State         string `json:"state"`
	AcquiredAt    string `json:"acquired_at"`
}

type Block struct {
	ID                ids.ID `json:"block_id"`
	TaskID            ids.ID `json:"task_id"`
	AttemptID         ids.ID `json:"attempt_id"`
	Round             int    `json:"round"`
	State             string `json:"state"`
	QuestionMessageID ids.ID `json:"question_message_id"`
	AskedBy           ids.ID `json:"asked_by"`
	AnswerMessageID   ids.ID `json:"answer_message_id,omitempty"`
	AnsweredBy        ids.ID `json:"answered_by,omitempty"`
	CreatedAt         string `json:"created_at"`
	AnsweredAt        string `json:"answered_at,omitempty"`
}

type Handoff struct {
	ID        ids.ID             `json:"handoff_id"`
	TaskID    ids.ID             `json:"task_id"`
	AttemptID ids.ID             `json:"attempt_id"`
	Round     int                `json:"round"`
	MessageID ids.ID             `json:"message_id"`
	DraftRefs []ids.PermanentRef `json:"draft_refs"`
	AuthorID  ids.ID             `json:"author_id"`
	CreatedAt string             `json:"created_at"`
}

// Result 是命令回执中保存的紧凑结果；完整状态经读取接口按当前权限返回。
type Result struct {
	OperationID ids.ID             `json:"operation_id"`
	TaskID      ids.ID             `json:"task_id"`
	Revision    int64              `json:"revision"`
	State       tc.TaskState       `json:"state"`
	Round       int                `json:"round"`
	Attempt     *Attempt           `json:"attempt,omitempty"`
	OutputRefs  []ids.PermanentRef `json:"output_refs,omitempty"`
	BlockID     ids.ID             `json:"block_id,omitempty"`
}

func resultOf(t Task, m Meta, a *Attempt) Result {
	return Result{TaskID: t.ID, Revision: t.Revision, State: t.State, Round: m.Round, Attempt: a, OutputRefs: t.OutputRefs}
}

// row 是一次事务内读取的任务、席位与当前 Attempt。M2 每个任务恰有一个席位。
type row struct {
	task    Task
	meta    Meta
	seat    Seat
	attempt *Attempt
}

func loadTask(ctx context.Context, q commands.DBTX, id ids.ID) (Task, Meta, error) {
	var t Task
	var m Meta
	var raw, meta string
	if err := q.QueryRowContext(ctx, `SELECT record,meta FROM tasks_tasks WHERE task_id=?`, id).Scan(&raw, &meta); err != nil {
		return t, m, missing(err)
	}
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return t, m, err
	}
	err := json.Unmarshal([]byte(meta), &m)
	return t, m, err
}

func loadSeat(ctx context.Context, q commands.DBTX, id ids.ID) (Seat, error) {
	var s Seat
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT record FROM tasks_seats WHERE seat_id=?`, id).Scan(&raw); err != nil {
		return s, missing(err)
	}
	err := json.Unmarshal([]byte(raw), &s)
	return s, err
}

func loadAttempt(ctx context.Context, q commands.DBTX, id ids.ID) (Attempt, error) {
	var a Attempt
	var raw string
	if err := q.QueryRowContext(ctx, `SELECT record FROM tasks_attempts WHERE attempt_id=?`, id).Scan(&raw); err != nil {
		return a, missing(err)
	}
	err := json.Unmarshal([]byte(raw), &a)
	return a, err
}

func loadRow(ctx context.Context, q commands.DBTX, id ids.ID) (row, error) {
	var r row
	var err error
	if r.task, r.meta, err = loadTask(ctx, q, id); err != nil {
		return r, err
	}
	if len(r.task.SeatIDs) != 1 {
		return r, errcode.New(errcode.UnsupportedCapability, "M2 tasks have exactly one seat")
	}
	if r.seat, err = loadSeat(ctx, q, r.task.SeatIDs[0]); err != nil {
		return r, err
	}
	if r.seat.CurrentAttemptID != "" {
		a, err := loadAttempt(ctx, q, r.seat.CurrentAttemptID)
		if err != nil {
			return r, err
		}
		r.attempt = &a
	}
	return r, nil
}

func (s *Service) saveTask(ctx context.Context, tx *sql.Tx, t *Task, m *Meta, stateChanged bool) error {
	t.Revision++
	if stateChanged {
		m.StateSince = clock.Format(s.now())
	}
	if t.OutputRefs == nil {
		t.OutputRefs = []ids.PermanentRef{}
	}
	if err := tc.ValidateShape("lantai.task/v1", *t); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks_tasks SET revision=?,state=?,updated_at=?,record=?,meta=? WHERE task_id=? AND revision=?`, t.Revision, t.State, clock.Millis(s.now()), encode(t), encode(m), t.ID, t.Revision-1)
	if err != nil {
		return err
	}
	return oneRow(res)
}

func (s *Service) saveSeat(ctx context.Context, tx *sql.Tx, st *Seat) error {
	st.Revision++
	if err := tc.ValidateShape("lantai.seat/v1", *st); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks_seats SET revision=?,state=?,record=? WHERE seat_id=? AND revision=?`, st.Revision, st.State, encode(st), st.ID, st.Revision-1)
	if err != nil {
		return err
	}
	return oneRow(res)
}

func (s *Service) saveAttempt(ctx context.Context, tx *sql.Tx, a *Attempt) error {
	a.Revision++
	if err := a.Validate(); err != nil {
		return err
	}
	expires, err := clock.Parse(a.ExpiresAt)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks_attempts SET revision=?,state=?,expires_at=?,record=? WHERE attempt_id=? AND revision=?`, a.Revision, a.State, clock.Millis(expires), encode(a), a.ID, a.Revision-1)
	if err != nil {
		return err
	}
	return oneRow(res)
}

func oneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errcode.New(errcode.PreconditionFailed, "concurrent task change")
	}
	return nil
}

func checkouts(ctx context.Context, q commands.DBTX, task ids.ID, activeOnly bool) ([]Checkout, error) {
	query := `SELECT task_id,asset_id,project_id,mode,attempt_id,base_version_id,state,acquired_at FROM tasks_checkouts WHERE task_id=?`
	if activeOnly {
		query += ` AND state='active'`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY asset_id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Checkout{}
	for rows.Next() {
		var c Checkout
		var at int64
		if err := rows.Scan(&c.TaskID, &c.AssetID, &c.ProjectID, &c.Mode, &c.AttemptID, &c.BaseVersionID, &c.State, &at); err != nil {
			return nil, err
		}
		c.AcquiredAt = clock.Format(clock.FromMillis(at))
		out = append(out, c)
	}
	return out, rows.Err()
}

// releaseCheckouts 在任务回到待领、完成或取消时解除签出。
func (s *Service) releaseCheckouts(ctx context.Context, tx *sql.Tx, task ids.ID) error {
	_, err := tx.ExecContext(ctx, `UPDATE tasks_checkouts SET state='released',released_at=? WHERE task_id=? AND state='active'`, clock.Millis(s.now()), task)
	return err
}

func putRefs(ctx context.Context, tx *sql.Tx, task ids.ID, role string, refs []ids.PermanentRef) error {
	for _, r := range refs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tasks_refs(task_id,asset_id,version_id,role) VALUES(?,?,?,?)`, task, r.AssetID, r.VersionID, role); err != nil {
			return err
		}
	}
	return nil
}

func blocks(ctx context.Context, q commands.DBTX, task ids.ID) ([]Block, error) {
	rows, err := q.QueryContext(ctx, `SELECT record FROM tasks_blocks WHERE task_id=? ORDER BY block_id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Block{}
	for rows.Next() {
		var raw string
		var b Block
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func openBlock(ctx context.Context, q commands.DBTX, task ids.ID) (*Block, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT record FROM tasks_blocks WHERE task_id=? AND state='open'`, task).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Block
	err = json.Unmarshal([]byte(raw), &b)
	return &b, err
}

func handoffs(ctx context.Context, q commands.DBTX, task ids.ID) ([]Handoff, error) {
	rows, err := q.QueryContext(ctx, `SELECT record FROM tasks_handoffs WHERE task_id=? ORDER BY handoff_id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Handoff{}
	for rows.Next() {
		var raw string
		var h Handoff
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
