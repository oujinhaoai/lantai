package tasks

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// CreateRequest 固定任务目标、输入版本与验收条件。FlowID/StepRunID 仅由
// workflow 设置，并经 Steps 端口核对步骤仍在推进且规格一致。
type CreateRequest struct {
	ProjectID          ids.ID                 `json:"project_id"`
	Type               string                 `json:"type"`
	Title              string                 `json:"title"`
	Description        string                 `json:"description,omitempty"`
	Priority           string                 `json:"priority"`
	AcceptanceCriteria []string               `json:"acceptance_criteria"`
	InputRefs          []ids.PermanentRef     `json:"input_refs"`
	ExpectedOutputs    []tc.OutputRequirement `json:"expected_outputs"`
	DependencyIDs      []ids.ID               `json:"dependency_ids"`
	AssigneeID         ids.ID                 `json:"assignee_id,omitempty"`
	Role               identity.Role          `json:"role,omitempty"`
	DistinctFrom       []ids.ID               `json:"distinct_from"`
	Checkouts          []CheckoutSpec         `json:"checkouts"`
	ContextAssetType   string                 `json:"context_asset_type,omitempty"`
	Subject            *Subject               `json:"subject,omitempty"`
	DueAt              string                 `json:"due_at,omitempty"`
	FlowID             ids.ID                 `json:"flow_id,omitempty"`
	StepRunID          ids.ID                 `json:"step_run_id,omitempty"`
}

// 各任务类型允许领取的项目角色；review 不发制作租约。
var claimRoles = map[string][]identity.Role{
	"produce":  {identity.RoleOwner, identity.RoleContributor},
	"ingest":   {identity.RoleOwner, identity.RoleContributor},
	"maintain": {identity.RoleOwner, identity.RoleContributor, identity.RoleCurator},
	"curate":   {identity.RoleOwner, identity.RoleCurator},
	"qa":       {identity.RoleOwner, identity.RoleChecker},
	"question": {identity.RoleOwner, identity.RoleCoordinator, identity.RoleContributor, identity.RoleCurator, identity.RoleChecker},
}

func (in *CreateRequest) normalize() error {
	if in.InputRefs == nil {
		in.InputRefs = []ids.PermanentRef{}
	}
	if in.ExpectedOutputs == nil {
		in.ExpectedOutputs = []tc.OutputRequirement{}
	}
	if in.DependencyIDs == nil {
		in.DependencyIDs = []ids.ID{}
	}
	if in.DistinctFrom == nil {
		in.DistinctFrom = []ids.ID{}
	}
	if in.Checkouts == nil {
		in.Checkouts = []CheckoutSpec{}
	}
	if in.Priority == "" {
		in.Priority = "P2"
	}
	if !in.ProjectID.Valid() || strings.TrimSpace(in.Title) == "" || len(in.Description) > 64<<10 {
		return invalid("project, title and bounded description required")
	}
	if _, ok := claimRoles[in.Type]; !ok && in.Type != "review" {
		return invalid("unsupported task type")
	}
	if (in.Type == "qa" || in.Type == "review") != (in.Subject != nil) {
		return invalid("qa and review tasks require exactly one subject; other types forbid it")
	}
	if in.Subject != nil {
		f := in.Subject.Flow
		if !f.TaskID.Valid() || !f.AttemptID.Valid() || f.Round < 1 || f.Fence < 1 || f.CandidateGroup != "" && !f.CandidateGroup.Valid() || in.Subject.Ref.Validate(true) != nil {
			return invalid("subject requires the exact production attempt and version")
		}
	}
	if in.Type == "review" && (len(in.Checkouts) > 0 || len(in.ExpectedOutputs) > 0) {
		return invalid("review tasks neither check out nor produce assets")
	}
	if in.AssigneeID != "" && !in.AssigneeID.Valid() || in.AssigneeID != "" && in.Role != "" {
		return invalid("assign to one principal or one role pool")
	}
	if in.Role != "" && !slices.Contains(identity.AllRoles, in.Role) {
		return invalid("unknown role")
	}
	if (in.FlowID == "") != (in.StepRunID == "") || in.FlowID != "" && (!in.FlowID.Valid() || !in.StepRunID.Valid()) {
		return invalid("flow and step must be given together")
	}
	seen := map[ids.ID]bool{}
	for _, c := range in.Checkouts {
		if !c.AssetID.Valid() || seen[c.AssetID] || (c.Mode != "exclusive" && c.Mode != "advisory") {
			return invalid("checkouts need distinct assets and exclusive/advisory mode")
		}
		seen[c.AssetID] = true
	}
	for _, id := range append(slices.Clone(in.DependencyIDs), in.DistinctFrom...) {
		if !id.Valid() {
			return invalid("invalid dependency or distinct principal")
		}
	}
	if in.DueAt != "" {
		if _, err := clock.Parse(in.DueAt); err != nil {
			return err
		}
	}
	if in.ContextAssetType != "" && !manifest.AssetType(in.ContextAssetType).Valid() {
		return invalid("unknown context asset type")
	}
	return nil
}

// SpecDigest 是流程步骤核对任务规格所用的摘要（不含 flow/step 字段本身）。
func SpecDigest(in CreateRequest) (digest.Digest, error) {
	in.FlowID, in.StepRunID = "", ""
	if err := in.normalize(); err != nil {
		return "", err
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return "", err
	}
	return digest.Of(raw), nil
}

func (s *Service) Create(ctx context.Context, who authz.Context, key string, in CreateRequest) (Result, error) {
	if err := in.normalize(); err != nil {
		return Result{}, err
	}
	ctx, release, err := s.lock(ctx, in.ProjectID, "")
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err = s.authorize(ctx, who, "tasks.create", in.ProjectID, "project", in.ProjectID); err != nil {
		return Result{}, err
	}
	cmd, err := s.command(ctx, who, key, "tasks.create", in.ProjectID, in)
	if err != nil {
		return Result{}, err
	}
	if r, err := s.replay(ctx, cmd); err != nil || r != nil {
		return deref(r), err
	}
	if in.FlowID != "" {
		steps, _ := s.wired()
		if steps == nil {
			return Result{}, errcode.New(errcode.FlowRequired, "workflow step authority is not configured")
		}
		spec, err := SpecDigest(in)
		if err != nil {
			return Result{}, err
		}
		if err = steps.VerifyStepTask(ctx, in.ProjectID, in.FlowID, in.StepRunID, spec); err != nil {
			return Result{}, err
		}
		// 一个业务步骤轮次只有一个任务：换操作者重试也返回已建任务。
		var existing ids.ID
		if err = s.d.DB.QueryRowContext(ctx, `SELECT task_id FROM tasks_tasks WHERE step_run_id=?`, in.StepRunID).Scan(&existing); err == nil {
			t, m, err := loadTask(ctx, s.d.DB, existing)
			if err != nil {
				return Result{}, err
			}
			out := resultOf(t, m, nil)
			out.OperationID = t.OperationID
			return out, nil
		} else if err != sql.ErrNoRows {
			return Result{}, err
		}
	}
	for _, r := range in.InputRefs {
		if err = s.readable(ctx, who, r, ""); err != nil {
			return Result{}, err
		}
	}
	for _, c := range in.Checkouts {
		a, err := s.d.Resources.Asset(ctx, c.AssetID)
		if err != nil {
			return Result{}, err
		}
		if a.ProjectID != in.ProjectID {
			return Result{}, errcode.New(errcode.NotFound, "")
		}
	}
	distinct := slices.Clone(in.DistinctFrom)
	if in.Subject != nil {
		// 质检与审定对象必须来自本项目的精确版本；制作者自动排除在质检之外。
		if err = s.readable(ctx, who, in.Subject.Ref, in.ProjectID); err != nil {
			return Result{}, err
		}
		if in.Type == "qa" {
			v, err := s.d.Resources.VersionByID(ctx, in.Subject.Ref.VersionID)
			if err != nil {
				return Result{}, err
			}
			distinct = append(distinct, v.CommittedBy)
		}
	}
	slices.Sort(distinct)
	distinct = slices.Compact(distinct)
	var snapshot *ContextSnapshot
	if in.ContextAssetType != "" {
		if s.d.Contexts == nil {
			return Result{}, errcode.New(errcode.InvalidStateTransition, "context authority is not configured")
		}
		b, err := s.d.Contexts.EffectiveContext(ctx, who, in.ProjectID, manifest.AssetType(in.ContextAssetType))
		if err != nil {
			return Result{}, err
		}
		snapshot = &ContextSnapshot{AssetType: in.ContextAssetType, Refs: []ids.PermanentRef{}, Digest: b.Digest}
		for _, d := range b.Documents {
			snapshot.Refs = append(snapshot.Refs, d.Ref)
		}
	}
	inputDigest, err := tc.SnapshotDigest(in.InputRefs)
	if err != nil {
		return Result{}, err
	}
	epoch, err := s.epoch(ctx)
	if err != nil {
		return Result{}, err
	}
	taskID, err := s.d.IDs.New()
	if err != nil {
		return Result{}, err
	}
	seatID, err := s.d.IDs.New()
	if err != nil {
		return Result{}, err
	}
	return s.execute(ctx, cmd, func(ctx context.Context, tx *sql.Tx) (Result, []event.Envelope, error) {
		state := tc.TaskState("todo")
		for _, dep := range in.DependencyIDs {
			var project ids.ID
			var depState string
			if err := tx.QueryRowContext(ctx, `SELECT project_id,state FROM tasks_tasks WHERE task_id=?`, dep).Scan(&project, &depState); err != nil {
				return Result{}, nil, missing(err)
			}
			if project != in.ProjectID {
				return Result{}, nil, errcode.New(errcode.NotFound, "")
			}
			if depState == "cancelled" {
				return Result{}, nil, stateErr("dependency_cancelled")
			}
			if depState != "done" {
				state = "waiting"
			}
		}
		now := clock.Format(s.now())
		t := Task{Contract: "lantai.task/v1", ID: taskID, ProjectID: in.ProjectID, Revision: 1, OperationID: cmd.OperationID, Type: in.Type, Title: in.Title, State: state, Priority: in.Priority,
			Input: tc.InputSnapshot{Refs: in.InputRefs, Digest: inputDigest}, AcceptanceCriteria: in.AcceptanceCriteria, ExpectedOutputs: in.ExpectedOutputs, SeatIDs: []ids.ID{seatID},
			DependencyIDs: in.DependencyIDs, OutputRefs: []ids.PermanentRef{}, FlowID: in.FlowID, StepRunID: in.StepRunID, DueAt: in.DueAt}
		if err := tc.ValidateShape("lantai.task/v1", t); err != nil {
			return Result{}, nil, err
		}
		m := Meta{Round: 1, Description: in.Description, AssigneeID: in.AssigneeID, Role: in.Role, DistinctFrom: distinct, Checkouts: in.Checkouts, Context: snapshot, Subject: in.Subject,
			CreatedBy: who.PrincipalID, CreatedAt: now, StateSince: now, History: []RoundOutcome{}}
		seat := Seat{Contract: "lantai.seat/v1", ID: seatID, TaskID: taskID, Revision: 1, Ordinal: 1, State: "open", RecoveryEpoch: epoch}
		if err := tc.ValidateShape("lantai.seat/v1", seat); err != nil {
			return Result{}, nil, err
		}
		at := clock.Millis(s.now())
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks_tasks(task_id,project_id,revision,state,type,flow_id,step_run_id,created_at,updated_at,record,meta) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			t.ID, t.ProjectID, t.Revision, t.State, t.Type, t.FlowID, t.StepRunID, at, at, encode(t), encode(m)); err != nil {
			return Result{}, nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks_seats(seat_id,task_id,revision,state,record) VALUES(?,?,?,?,?)`, seat.ID, seat.TaskID, seat.Revision, seat.State, encode(seat)); err != nil {
			return Result{}, nil, err
		}
		for _, dep := range in.DependencyIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO tasks_dependencies(task_id,depends_on) VALUES(?,?)`, t.ID, dep); err != nil {
				return Result{}, nil, err
			}
		}
		if err := putRefs(ctx, tx, t.ID, "input", in.InputRefs); err != nil {
			return Result{}, nil, err
		}
		if snapshot != nil {
			if err := putRefs(ctx, tx, t.ID, "context", snapshot.Refs); err != nil {
				return Result{}, nil, err
			}
		}
		if in.Subject != nil {
			if err := putRefs(ctx, tx, t.ID, "input", []ids.PermanentRef{in.Subject.Ref}); err != nil {
				return Result{}, nil, err
			}
		}
		for _, c := range in.Checkouts {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tasks_refs(task_id,asset_id,version_id,role) VALUES(?,?,'','checkout')`, t.ID, c.AssetID); err != nil {
				return Result{}, nil, err
			}
		}
		e, err := s.event(cmd, "task.created", t, map[string]any{"type": t.Type, "state": t.State, "flow_id": t.FlowID, "step_run_id": t.StepRunID})
		if err != nil {
			return Result{}, nil, err
		}
		return resultOf(t, m, nil), []event.Envelope{e}, nil
	})
}

// readable 核对当前主体能读取精确版本；project 非空时版本必须属于该项目。
func (s *Service) readable(ctx context.Context, who authz.Context, r ids.PermanentRef, project ids.ID) error {
	if err := r.Validate(true); err != nil || r.InstanceID != s.d.InstanceID {
		return invalid("exact local version reference required")
	}
	v, err := s.d.Resources.VersionByID(ctx, r.VersionID)
	if err != nil {
		return err
	}
	if v.AssetID != r.AssetID || project != "" && v.ProjectID != project {
		return errcode.New(errcode.RefMismatch, "")
	}
	if err = s.authorize(ctx, who, "catalog.read", v.ProjectID, "version", v.VersionID); err != nil {
		return err
	}
	return s.d.Resources.CheckVersionRead(ctx, v.AssetID, v.VersionID)
}

func deref(r *Result) Result {
	if r == nil {
		return Result{}
	}
	return *r
}
