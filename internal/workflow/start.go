package workflow

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// StartRequest 在开始时把定义、输入与目标资产固定为精确版本；之后不再解析
// latest 或别名。修改流程必须给出目标资产，其当前最新版本成为固定输入。
type StartRequest struct {
	ProjectID          ids.ID                 `json:"project_id"`
	DefinitionRef      ids.PermanentRef       `json:"definition_ref"`
	ProfileRef         ids.PermanentRef       `json:"profile_ref"`
	TargetAssetID      ids.ID                 `json:"target_asset_id,omitempty"`
	InputRefs          []ids.PermanentRef     `json:"input_refs"`
	Title              string                 `json:"title"`
	Description        string                 `json:"description,omitempty"`
	Priority           string                 `json:"priority,omitempty"`
	AcceptanceCriteria []string               `json:"acceptance_criteria"`
	ExpectedOutputs    []tc.OutputRequirement `json:"expected_outputs"`
	ContextAssetType   string                 `json:"context_asset_type,omitempty"`
	AssigneeID         ids.ID                 `json:"assignee_id,omitempty"`
	Role               identity.Role          `json:"role,omitempty"`
	DueAt              string                 `json:"due_at,omitempty"`
}

// FlowRef 是按修订条件更新的流程管理命令目标。
type FlowRef struct {
	ProjectID        ids.ID `json:"project_id"`
	FlowID           ids.ID `json:"flow_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

func (r FlowRef) valid() error {
	if !r.ProjectID.Valid() || !r.FlowID.Valid() || r.ExpectedRevision < 1 || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 4096 {
		return invalid("flow, expected revision and reason required")
	}
	return nil
}

func (w *Service) Start(ctx context.Context, who authz.Context, key string, in StartRequest) (Result, error) {
	if in.InputRefs == nil {
		in.InputRefs = []ids.PermanentRef{}
	}
	if in.ExpectedOutputs == nil {
		in.ExpectedOutputs = []tc.OutputRequirement{}
	}
	if in.Priority == "" {
		in.Priority = "P2"
	}
	if !in.ProjectID.Valid() || strings.TrimSpace(in.Title) == "" || len(in.Title) > 200 || len(in.AcceptanceCriteria) == 0 || in.DefinitionRef.Validate(true) != nil || in.ProfileRef.Validate(true) != nil || in.TargetAssetID != "" && !in.TargetAssetID.Valid() {
		return Result{}, invalid("project, title, acceptance criteria, definition and profile versions required")
	}
	ctx, release, err := w.lock(ctx, in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err = w.authorize(ctx, who, "workflow.start", in.ProjectID, "project", in.ProjectID); err != nil {
		return Result{}, err
	}
	cmd, err := w.command(ctx, who, key, "workflow.start", in.ProjectID, in)
	if err != nil {
		return Result{}, err
	}
	if r, err := w.replay(ctx, cmd); err != nil || r != nil {
		return deref(r), err
	}
	def, defDigest, err := w.definition(ctx, who, in.DefinitionRef)
	if err != nil {
		return Result{}, err
	}
	if err = w.readable(ctx, who, in.ProfileRef, ""); err != nil {
		return Result{}, err
	}
	refs := slices.Clone(in.InputRefs)
	if def.Kind == "modify" {
		if in.TargetAssetID == "" {
			return Result{}, invalid("modify flows require a target asset")
		}
		a, err := w.d.Ledger.Asset(ctx, in.TargetAssetID)
		if err != nil {
			return Result{}, err
		}
		if a.ProjectID != in.ProjectID {
			return Result{}, errcode.New(errcode.NotFound, "")
		}
		v, err := w.d.Ledger.LatestVersion(ctx, in.TargetAssetID)
		if err != nil {
			return Result{}, err
		}
		refs = append([]ids.PermanentRef{v.Ref(w.d.InstanceID)}, refs...)
	} else if in.TargetAssetID != "" {
		return Result{}, invalid("only modify flows take a target asset")
	}
	for _, r := range refs {
		if err = w.readable(ctx, who, r, ""); err != nil {
			return Result{}, err
		}
	}
	inputDigest, err := tc.SnapshotDigest(refs)
	if err != nil {
		return Result{}, err
	}
	flowID, err := w.d.IDs.New()
	if err != nil {
		return Result{}, err
	}
	return w.execute(ctx, cmd, func(ctx context.Context, tx *sql.Tx) (Result, []event.Envelope, error) {
		r := flowRow{flow: Flow{Contract: "lantai.flow/v1", ID: flowID, ProjectID: in.ProjectID, Revision: 1, OperationID: cmd.OperationID, State: "running",
			Definition: tc.DefinitionRef{Ref: in.DefinitionRef, Digest: defDigest}, Input: tc.InputSnapshot{Refs: refs, Digest: inputDigest}, StepRunIDs: []ids.ID{}, PendingOperationIDs: []ids.ID{}},
			meta: FlowMeta{Definition: def, ProfileRef: in.ProfileRef, SubjectAssetID: in.TargetAssetID, StartedBy: who.PrincipalID, ProductionRound: 1, CheckEvidence: []ids.ID{}, Rounds: []ProductionRecord{},
				Template: Template{Title: in.Title, Description: in.Description, Priority: in.Priority, AcceptanceCriteria: in.AcceptanceCriteria, ExpectedOutputs: in.ExpectedOutputs, ContextAssetType: in.ContextAssetType, AssigneeID: in.AssigneeID, Role: in.Role, DueAt: in.DueAt}}}
		if _, err := w.startStep(ctx, tx, &r, def.Steps[0].Key, facts{}); err != nil {
			return Result{}, nil, err
		}
		for i := range r.steps {
			if err := w.saveStep(ctx, tx, &r.steps[i]); err != nil {
				return Result{}, nil, err
			}
		}
		settleState(&r)
		if err := tc.ValidateShape("lantai.flow/v1", r.flow); err != nil {
			return Result{}, nil, err
		}
		at := clock.Millis(w.now())
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_flows(flow_id,project_id,revision,state,created_at,updated_at,record,meta) VALUES(?,?,?,?,?,?,?,?)`,
			r.flow.ID, r.flow.ProjectID, r.flow.Revision, r.flow.State, at, at, encode(r.flow), encode(r.meta)); err != nil {
			return Result{}, nil, err
		}
		if in.TargetAssetID != "" {
			if err := bind(ctx, tx, "asset", in.TargetAssetID, r.flow.ID, r.steps[0].run.ID); err != nil {
				return Result{}, nil, err
			}
		}
		e, err := w.flowEvent(cmd.ActorID, cmd.SessionID, cmd.OperationID, "flow.started", r.flow, map[string]any{"definition_key": def.Key, "definition_version": def.Version, "kind": def.Kind})
		if err != nil {
			return Result{}, nil, err
		}
		return Result{FlowID: r.flow.ID, Revision: r.flow.Revision, State: string(r.flow.State)}, []event.Envelope{e}, nil
	})
}

// definition 读取 config 版本冻结清单中的 flow_definition，并核对当前可读、未停用。
func (w *Service) definition(ctx context.Context, who authz.Context, ref ids.PermanentRef) (Definition, digest.Digest, error) {
	if err := w.readable(ctx, who, ref, ""); err != nil {
		return Definition{}, "", err
	}
	v, err := w.d.Ledger.VersionByID(ctx, ref.VersionID)
	if err != nil {
		return Definition{}, "", err
	}
	state, err := w.d.Ledger.VersionControl(ctx, v.VersionID)
	if err != nil {
		return Definition{}, "", err
	}
	if state.Lifecycle != "active" || state.Availability != "" && state.Availability != "enabled" {
		return Definition{}, "", stateErr("definition_version_unavailable")
	}
	raw, err := w.d.Manifests.ReadManifest(ctx, v)
	if err != nil {
		return Definition{}, "", err
	}
	doc, err := manifest.Parse(raw)
	if err != nil {
		return Definition{}, "", err
	}
	if doc.VersionID != v.VersionID || doc.ManifestDigest != v.ManifestDigest || doc.Content.AssetType != manifest.TypeConfig {
		return Definition{}, "", errcode.New(errcode.RefMismatch, "flow definition must be a config version")
	}
	body, ok := doc.Content.Metadata["flow_definition"]
	if !ok {
		return Definition{}, "", invalid("config version has no flow_definition")
	}
	return ParseDefinition(body)
}

func (w *Service) readable(ctx context.Context, who authz.Context, r ids.PermanentRef, project ids.ID) error {
	if err := r.Validate(true); err != nil || r.InstanceID != w.d.InstanceID {
		return invalid("exact local version reference required")
	}
	v, err := w.d.Ledger.VersionByID(ctx, r.VersionID)
	if err != nil {
		return err
	}
	// 先按版本实际所属项目授权，再比较归属：无权读取时与不存在一样，
	// 不泄露版本是否存在。
	if err = w.authorize(ctx, who, "catalog.read", v.ProjectID, "version", v.VersionID); err != nil {
		return err
	}
	if v.AssetID != r.AssetID || project != "" && v.ProjectID != project {
		return errcode.New(errcode.RefMismatch, "")
	}
	return w.d.Ledger.CheckVersionRead(ctx, v.AssetID, v.VersionID)
}

type flowMutation func(context.Context, *sql.Tx, *flowRow) ([]note, error)

// manage 是流程管理命令的公共路径：锁 → 回执 → 授权 → 修订条件更新。
func (w *Service) manage(ctx context.Context, who authz.Context, key, typ string, in FlowRef, body any, prepare func(context.Context, flowRow) error, mutate flowMutation) (Result, error) {
	if err := in.valid(); err != nil {
		return Result{}, err
	}
	ctx, release, err := w.lock(ctx, in.ProjectID)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err = w.authorize(ctx, who, "workflow.read", in.ProjectID, "flow", in.FlowID); err != nil {
		return Result{}, err
	}
	cmd, err := w.command(ctx, who, key, typ, in.ProjectID, body)
	if err != nil {
		return Result{}, err
	}
	if r, err := w.replay(ctx, cmd); err != nil || r != nil {
		return deref(r), err
	}
	if err = w.authorize(ctx, who, "workflow.operate", in.ProjectID, "flow", in.FlowID); err != nil {
		return Result{}, err
	}
	current, err := loadFlow(ctx, w.d.DB, in.FlowID)
	if err != nil {
		return Result{}, err
	}
	if current.flow.ProjectID != in.ProjectID {
		return Result{}, errcode.New(errcode.NotFound, "")
	}
	if prepare != nil {
		if err = prepare(ctx, current); err != nil {
			return Result{}, err
		}
	}
	return w.execute(ctx, cmd, func(ctx context.Context, tx *sql.Tx) (Result, []event.Envelope, error) {
		r, err := loadFlow(ctx, tx, in.FlowID)
		if err != nil {
			return Result{}, nil, err
		}
		if err := tc.CheckRevision(in.ExpectedRevision, r.flow.Revision); err != nil {
			return Result{}, nil, err
		}
		notes, err := mutate(ctx, tx, &r)
		if err != nil {
			return Result{}, nil, err
		}
		if err := w.persist(ctx, tx, &r); err != nil {
			return Result{}, nil, err
		}
		var evs []event.Envelope
		for _, n := range notes {
			e, err := w.flowEvent(cmd.ActorID, cmd.SessionID, cmd.OperationID, n.typ, r.flow, n.payload)
			if err != nil {
				return Result{}, nil, err
			}
			evs = append(evs, e)
		}
		return Result{FlowID: r.flow.ID, Revision: r.flow.Revision, State: string(r.flow.State)}, evs, nil
	})
}

// Pause 停止派发新的流程命令，并使依赖流程授权的审定/自动发布被拒绝；已领取
// 的任务照常可以交付，恢复后按持久事实继续。
func (w *Service) Pause(ctx context.Context, who authz.Context, key string, in FlowRef) (Result, error) {
	return w.manage(ctx, who, key, "workflow.pause", in, in, nil, func(ctx context.Context, tx *sql.Tx, r *flowRow) ([]note, error) {
		if !tc.FlowTransition(r.flow.State, "paused") {
			return nil, stateErr("flow_not_active")
		}
		r.flow.State, r.meta.PauseReason = "paused", in.Reason
		return []note{{"flow.paused", map[string]any{"reason": in.Reason}}}, nil
	})
}

func (w *Service) Resume(ctx context.Context, who authz.Context, key string, in FlowRef) (Result, error) {
	return w.manage(ctx, who, key, "workflow.resume", in, in, nil, func(ctx context.Context, tx *sql.Tx, r *flowRow) ([]note, error) {
		if r.flow.State != "paused" {
			return nil, stateErr("flow_not_paused")
		}
		r.flow.State, r.meta.PauseReason = "running", ""
		return []note{{"flow.resumed", map[string]any{"reason": in.Reason}}}, nil
	})
}

// Cancel 结束流程：未结束的步骤取消，未派发的命令撤回，并为仍未结束的任务
// 追加取消命令（任务仍须按停止事实对账后才记为取消）。
func (w *Service) Cancel(ctx context.Context, who authz.Context, key string, in FlowRef) (Result, error) {
	return w.manage(ctx, who, key, "workflow.cancel", in, in, nil, func(ctx context.Context, tx *sql.Tx, r *flowRow) ([]note, error) {
		if !tc.FlowTransition(r.flow.State, "cancelled") {
			return nil, stateErr("flow_finished")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_commands SET status='cancelled' WHERE flow_id=? AND status IN ('pending','blocked') AND command_type<>'tasks.cancel'`, r.flow.ID); err != nil {
			return nil, err
		}
		r.flow.PendingOperationIDs = []ids.ID{}
		for i := range r.steps {
			s := &r.steps[i]
			if !open(s.run.State) {
				continue
			}
			for _, t := range s.run.TaskIDs {
				payload := taskCancel{TaskID: t, Reason: "flow cancelled: " + in.Reason}
				if _, err := w.enqueue(ctx, tx, r, s, "tasks.cancel", "cancel:"+string(t), payload); err != nil {
					return nil, err
				}
			}
			if err := transition(s, "cancelled"); err != nil {
				return nil, err
			}
		}
		r.flow.State, r.meta.CancelReason = "cancelled", in.Reason
		return []note{{"flow.cancelled", map[string]any{"reason": in.Reason}}}, nil
	})
}

// Retry 只重跑阻塞步骤或宿主故障失败的检查作业：旧轮次保留，新轮次使用新
// StepRun 与新命令。审定拒绝、返工上限等业务结论不能靠重试推翻。
func (w *Service) Retry(ctx context.Context, who authz.Context, key string, in FlowRef) (Result, error) {
	var f facts
	return w.manage(ctx, who, key, "workflow.retry", in, in, func(ctx context.Context, r flowRow) error {
		var err error
		f, err = w.prefetch(ctx, &r, nil)
		return err
	}, func(ctx context.Context, tx *sql.Tx, r *flowRow) ([]note, error) {
		var target *stepRow
		for i := range r.steps {
			s := &r.steps[i]
			if s != r.latest(s.run.StepKey) || s.meta.ProductionRound != r.meta.ProductionRound {
				continue
			}
			if s.run.State == "blocked" || s.run.State == "failed" && s.meta.Outcome == "job_failed" {
				target = s
			}
		}
		if target == nil || r.flow.State == "cancelled" || r.flow.State == "completed" || r.flow.State == "paused" {
			return nil, stateErr("flow_not_retryable")
		}
		if target.run.State == "blocked" {
			if err := transition(target, "cancelled"); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE workflow_commands SET status='cancelled' WHERE step_run_id=? AND status='blocked'`, target.run.ID); err != nil {
				return nil, err
			}
			for _, op := range target.meta.CommandIDs {
				r.settle(op)
			}
		}
		if r.flow.State == "failed" {
			r.flow.State = "running"
		}
		r.meta.Failure = ""
		if _, err := w.startStep(ctx, tx, r, target.run.StepKey, f); err != nil {
			return nil, err
		}
		return []note{{"flow.retried", map[string]any{"step_key": target.run.StepKey}}}, nil
	})
}

// CommandRequest 释放一条已阻塞的流程命令，保留原 operation_id 与载荷。
type CommandRequest struct {
	FlowRef
	OperationID ids.ID `json:"operation_id"`
}

func (w *Service) RetryCommand(ctx context.Context, who authz.Context, key string, in CommandRequest) (Result, error) {
	if !in.OperationID.Valid() {
		return Result{}, invalid("operation required")
	}
	return w.manage(ctx, who, key, "workflow.retry_command", in.FlowRef, in, nil, func(ctx context.Context, tx *sql.Tx, r *flowRow) ([]note, error) {
		var step ids.ID
		var typ, status string
		if err := tx.QueryRowContext(ctx, `SELECT step_run_id,command_type,status FROM workflow_commands WHERE operation_id=? AND flow_id=?`, in.OperationID, r.flow.ID).Scan(&step, &typ, &status); err != nil {
			return nil, missing(err)
		}
		if status != "blocked" || r.flow.State == "cancelled" && typ != "tasks.cancel" {
			return nil, stateErr("command_not_blocked")
		}
		// 任务命令按步骤唯一或状态后置条件幂等，可换派发者；台账命令保留原派发者。
		clear := ""
		if strings.HasPrefix(typ, "tasks.") {
			clear = ",actor_id=''"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_commands SET status='pending',retry_at=0,failure_code=''`+clear+` WHERE operation_id=?`, in.OperationID); err != nil {
			return nil, err
		}
		if !slices.Contains(r.flow.PendingOperationIDs, in.OperationID) {
			r.flow.PendingOperationIDs = append(r.flow.PendingOperationIDs, in.OperationID)
		}
		if s := r.step(step); s != nil && s.run.State == "blocked" {
			if err := transition(s, "ready"); err != nil {
				return nil, err
			}
			s.meta.Failure = ""
		}
		r.meta.Failure = ""
		return []note{{"flow.command_released", map[string]any{"operation_id": in.OperationID}}}, nil
	})
}

func deref(r *Result) Result {
	if r == nil {
		return Result{}
	}
	return *r
}
