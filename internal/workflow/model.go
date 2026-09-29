package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

type (
	Flow    = tc.Flow
	StepRun = tc.StepRun
)

// Template 是流程为制作任务固定的目标与验收条件。
type Template struct {
	Title              string                 `json:"title"`
	Description        string                 `json:"description,omitempty"`
	Priority           string                 `json:"priority"`
	AcceptanceCriteria []string               `json:"acceptance_criteria"`
	ExpectedOutputs    []tc.OutputRequirement `json:"expected_outputs"`
	ContextAssetType   string                 `json:"context_asset_type,omitempty"`
	AssigneeID         ids.ID                 `json:"assignee_id,omitempty"`
	Role               identity.Role          `json:"role,omitempty"`
	DueAt              string                 `json:"due_at,omitempty"`
}

// FlowMeta 是 lantai.flow/v1 之外由 workflow 维护的推进事实。
type FlowMeta struct {
	Definition      Definition         `json:"definition"`
	ProfileRef      ids.PermanentRef   `json:"profile_ref"`
	SubjectAssetID  ids.ID             `json:"subject_asset_id,omitempty"`
	Template        Template           `json:"template"`
	StartedBy       ids.ID             `json:"started_by"`
	ProductionRound int                `json:"production_round"`
	Reworks         int                `json:"reworks"`
	ProduceTaskID   ids.ID             `json:"produce_task_id,omitempty"`
	Subject         *tasks.Subject     `json:"subject,omitempty"`
	CheckEvidence   []ids.ID           `json:"check_evidence"`
	QAEvidenceID    ids.ID             `json:"qa_evidence_id,omitempty"`
	ReviewTargetID  ids.ID             `json:"review_target_id,omitempty"`
	ReviewID        ids.ID             `json:"review_id,omitempty"`
	ApprovedVersion ids.ID             `json:"approved_version_id,omitempty"`
	Failure         string             `json:"failure,omitempty"`
	PauseReason     string             `json:"pause_reason,omitempty"`
	CancelReason    string             `json:"cancel_reason,omitempty"`
	Rounds          []ProductionRecord `json:"rounds"`
}

// ProductionRecord 保留每一制作轮次的交付与结论，返工不覆盖历史。
type ProductionRecord struct {
	Round     int              `json:"round"`
	Subject   *tasks.Subject   `json:"subject,omitempty"`
	Outcome   string           `json:"outcome"`
	Authority ids.ID           `json:"authority_operation_id,omitempty"`
	Evidence  []ids.ID         `json:"evidence"`
	At        string           `json:"at"`
	Output    ids.PermanentRef `json:"output,omitzero"`
}

// StepMeta 是业务步骤轮次的引用与结论。
type StepMeta struct {
	Ordinal         int           `json:"ordinal"`
	ProductionRound int           `json:"production_round"`
	TaskSpec        digest.Digest `json:"task_spec,omitempty"`
	TargetID        ids.ID        `json:"target_id,omitempty"`
	Outcome         string        `json:"outcome,omitempty"`
	Failure         string        `json:"failure,omitempty"`
	CommandIDs      []ids.ID      `json:"command_ids"`
}

type stepRow struct {
	run  StepRun
	meta StepMeta
	orig string // 读取时的序列化形态；persist 只写入变化的轮次
}

func (s *stepRow) snapshot() string { return encode(s.run) + encode(s.meta) }

type flowRow struct {
	flow  Flow
	meta  FlowMeta
	steps []stepRow
	orig  string
}

func loadFlow(ctx context.Context, q commands.DBTX, id ids.ID) (flowRow, error) {
	var r flowRow
	var raw, meta string
	if err := q.QueryRowContext(ctx, `SELECT record,meta FROM workflow_flows WHERE flow_id=?`, id).Scan(&raw, &meta); err != nil {
		return r, missing(err)
	}
	if err := json.Unmarshal([]byte(raw), &r.flow); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(meta), &r.meta); err != nil {
		return r, err
	}
	r.orig = encode(r.flow) + encode(r.meta)
	rows, err := q.QueryContext(ctx, `SELECT record,meta FROM workflow_step_runs WHERE flow_id=? ORDER BY round,ordinal,step_run_id`, id)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var s stepRow
		if err = rows.Scan(&raw, &meta); err != nil {
			return r, err
		}
		if err = json.Unmarshal([]byte(raw), &s.run); err != nil {
			return r, err
		}
		if err = json.Unmarshal([]byte(meta), &s.meta); err != nil {
			return r, err
		}
		s.orig = s.snapshot()
		r.steps = append(r.steps, s)
	}
	return r, rows.Err()
}

// latest 返回某步骤键的最新轮次。
func (r *flowRow) latest(key string) *stepRow {
	var out *stepRow
	for i := range r.steps {
		if r.steps[i].run.StepKey == key && (out == nil || r.steps[i].run.Round > out.run.Round) {
			out = &r.steps[i]
		}
	}
	return out
}

func (r *flowRow) step(id ids.ID) *stepRow {
	for i := range r.steps {
		if r.steps[i].run.ID == id {
			return &r.steps[i]
		}
	}
	return nil
}

func open(s tc.StepState) bool {
	return s != "completed" && s != "failed" && s != "cancelled"
}

func (w *Service) saveFlow(ctx context.Context, tx *sql.Tx, r *flowRow) error {
	r.flow.Revision++
	if err := tc.ValidateShape("lantai.flow/v1", r.flow); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE workflow_flows SET revision=?,state=?,updated_at=?,record=?,meta=? WHERE flow_id=? AND revision=?`, r.flow.Revision, r.flow.State, clock.Millis(w.now()), encode(r.flow), encode(r.meta), r.flow.ID, r.flow.Revision-1)
	if err != nil {
		return err
	}
	return oneRow(res)
}

func (w *Service) saveStep(ctx context.Context, tx *sql.Tx, s *stepRow) error {
	if s.snapshot() == s.orig {
		return nil
	}
	s.run.Revision++
	if err := tc.ValidateShape("lantai.step-run/v1", s.run); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE workflow_step_runs SET revision=?,state=?,record=?,meta=? WHERE step_run_id=? AND revision=?`, s.run.Revision, s.run.State, encode(s.run), encode(s.meta), s.run.ID, s.run.Revision-1)
	if err != nil {
		return err
	}
	if err = oneRow(res); err != nil {
		return err
	}
	s.orig = s.snapshot()
	return nil
}

// persist 写入变化的步骤轮次与流程；流程修订每次推进递增一次。
func (w *Service) persist(ctx context.Context, tx *sql.Tx, r *flowRow) error {
	for i := range r.steps {
		if err := w.saveStep(ctx, tx, &r.steps[i]); err != nil {
			return err
		}
	}
	settleState(r)
	return w.saveFlow(ctx, tx, r)
}

// settleState 在运行中与等待之间切换：还有待发命令时为 running，否则等待外部事实。
func settleState(r *flowRow) {
	switch r.flow.State {
	case "running", "waiting":
		if len(r.flow.PendingOperationIDs) > 0 {
			r.flow.State = "running"
		} else {
			r.flow.State = "waiting"
		}
	}
}

func oneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errcode.New(errcode.PreconditionFailed, "concurrent flow change")
	}
	return nil
}

// transition 核对业务边后改变步骤状态。
func transition(s *stepRow, to tc.StepState) error {
	if s.run.State == to {
		return nil
	}
	if !tc.StepTransition(s.run.State, to) {
		return stateErr(fmt.Sprintf("step_%s_to_%s", s.run.State, to))
	}
	s.run.State = to
	return nil
}

var stepKinds = map[string]string{"task": "task", "job": "job", "review": "review", "publish": "publish"}

// newStep 创建某步骤的下一轮次；输入固定为流程输入或本轮交付的精确版本。
func (w *Service) newStep(ctx context.Context, tx *sql.Tx, r *flowRow, key string, state tc.StepState) (*stepRow, error) {
	i := r.meta.Definition.index(key)
	if i < 0 {
		return nil, invalid("unknown flow step")
	}
	round := 1
	if prev := r.latest(key); prev != nil {
		round = prev.run.Round + 1
		if open(prev.run.State) {
			return nil, stateErr("previous_step_round_open")
		}
	}
	id, err := w.d.IDs.New()
	if err != nil {
		return nil, err
	}
	op, err := ids.DeriveChild(r.flow.OperationID, fmt.Sprintf("step:%s:round:%d", key, round))
	if err != nil {
		return nil, err
	}
	input := r.flow.Input
	if r.meta.Subject != nil && i > 0 {
		refs := []ids.PermanentRef{r.meta.Subject.Ref}
		d, err := tc.SnapshotDigest(refs)
		if err != nil {
			return nil, err
		}
		input = tc.InputSnapshot{Refs: refs, Digest: d}
	}
	s := stepRow{run: StepRun{Contract: "lantai.step-run/v1", ID: id, FlowID: r.flow.ID, Revision: 1, OperationID: op, StepKey: key, Round: round, Kind: stepKinds[r.meta.Definition.Steps[i].Kind], State: state,
		Definition: r.flow.Definition, Input: input, TaskIDs: []ids.ID{}, JobIDs: []ids.ID{}, OutputRefs: []ids.PermanentRef{}},
		meta: StepMeta{Ordinal: i, ProductionRound: r.meta.ProductionRound, CommandIDs: []ids.ID{}}}
	if err := tc.ValidateShape("lantai.step-run/v1", s.run); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_step_runs(step_run_id,flow_id,step_key,round,ordinal,revision,state,record,meta) VALUES(?,?,?,?,?,?,?,?,?)`,
		s.run.ID, s.run.FlowID, s.run.StepKey, s.run.Round, i, s.run.Revision, s.run.State, encode(s.run), encode(s.meta)); err != nil {
		return nil, err
	}
	if len(r.flow.StepRunIDs) >= 10000 {
		return nil, errcode.New(errcode.QuotaExceeded, "flow step limit")
	}
	r.flow.StepRunIDs = append(r.flow.StepRunIDs, s.run.ID)
	s.orig = s.snapshot()
	r.steps = append(r.steps, s)
	return &r.steps[len(r.steps)-1], nil
}

func bind(ctx context.Context, tx *sql.Tx, kind string, ref ids.ID, flow, step ids.ID) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workflow_bindings(kind,ref_id,flow_id,step_run_id) VALUES(?,?,?,?)`, kind, ref, flow, step)
	return err
}

// bound 返回某任务/作业/资产绑定的流程与步骤。
func bound(ctx context.Context, q commands.DBTX, kind string, ref ids.ID) ([][2]ids.ID, error) {
	rows, err := q.QueryContext(ctx, `SELECT flow_id,step_run_id FROM workflow_bindings WHERE kind=? AND ref_id=? ORDER BY flow_id,step_run_id`, kind, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]ids.ID
	for rows.Next() {
		var f, s ids.ID
		if err = rows.Scan(&f, &s); err != nil {
			return nil, err
		}
		out = append(out, [2]ids.ID{f, s})
	}
	return out, rows.Err()
}

// enqueue 持久化一条待发命令；operation_id 从流程、步骤轮次与动作稳定派生，
// 重复推进不产生第二条命令。
func (w *Service) enqueue(ctx context.Context, tx *sql.Tx, r *flowRow, s *stepRow, typ, action string, payload any) (ids.ID, error) {
	op, err := ids.DeriveChild(s.run.OperationID, "cmd:"+action)
	if err != nil {
		return "", err
	}
	raw, err := canonjson.CanonicalizeValue(payload)
	if err != nil {
		return "", err
	}
	if len(raw) > 48<<10 {
		return "", invalid("flow command payload too large")
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workflow_commands(operation_id,flow_id,step_run_id,command_type,payload,status,created_at) VALUES(?,?,?,?,?,'pending',?)`, op, r.flow.ID, s.run.ID, typ, string(raw), clock.Millis(w.now()))
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if !slices.Contains(r.flow.PendingOperationIDs, op) {
			if len(r.flow.PendingOperationIDs) >= 1000 {
				return "", errcode.New(errcode.QuotaExceeded, "flow pending command limit")
			}
			r.flow.PendingOperationIDs = append(r.flow.PendingOperationIDs, op)
		}
		s.meta.CommandIDs = append(s.meta.CommandIDs, op)
	}
	return op, nil
}

func (r *flowRow) settle(op ids.ID) {
	r.flow.PendingOperationIDs = slices.DeleteFunc(r.flow.PendingOperationIDs, func(x ids.ID) bool { return x == op })
}
