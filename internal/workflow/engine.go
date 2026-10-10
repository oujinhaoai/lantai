package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

type note struct {
	typ     string
	payload map[string]any
}

// facts 是事务外读取的权威事实；事务内的推进只依据它们与持久流程状态。
type facts struct {
	produce    *tasks.View
	task       *tasks.View
	version    *commit.Committed
	vcRevision int64
	qaEvidence ids.ID
	review     *ledger.ReviewFact
	target     *ledger.ReviewTarget
}

type taskCancel struct {
	TaskID ids.ID `json:"task_id"`
	Reason string `json:"reason"`
}
type taskAuthority struct {
	TaskID               ids.ID               `json:"task_id"`
	AuthorityOperationID ids.ID               `json:"authority_operation_id"`
	Reason               string               `json:"reason,omitempty"`
	Checkouts            []tasks.CheckoutSpec `json:"checkouts,omitempty"`
}
type publishPayload struct {
	AssetID   ids.ID `json:"asset_id"`
	VersionID ids.ID `json:"version_id"`
	ReviewID  ids.ID `json:"review_id"`
}

// prefetch 读取推进可能需要的制作任务、固定输出版本与审定修订。
func (w *Service) prefetch(ctx context.Context, r *flowRow, trigger *tasks.View) (facts, error) {
	var f facts
	f.task = trigger
	if r.meta.ProduceTaskID != "" {
		v, err := w.d.Tasks.Snapshot(ctx, r.meta.ProduceTaskID)
		if err != nil {
			return f, err
		}
		f.produce = &v
	}
	subject := ids.PermanentRef{}
	if r.meta.Subject != nil {
		subject = r.meta.Subject.Ref
	} else if f.produce != nil && f.produce.Task.State == "submitted" && len(f.produce.Task.OutputRefs) == 1 {
		subject = f.produce.Task.OutputRefs[0]
	}
	if subject.VersionID != "" {
		v, err := w.d.Ledger.VersionByID(ctx, subject.VersionID)
		if err != nil {
			return f, err
		}
		f.version = &v
		vc, err := w.d.Ledger.VersionControl(ctx, v.VersionID)
		if err != nil {
			return f, err
		}
		f.vcRevision = vc.Revision
	}
	if trigger != nil && trigger.Task.Type == "qa" && trigger.Meta.Completion != nil {
		id, err := w.d.Ledger.EvidenceIDByOperation(ctx, trigger.Meta.Completion.AuthorityOperationID)
		if err != nil {
			return f, err
		}
		f.qaEvidence = id
	}
	return f, nil
}

type plan struct {
	flow     ids.ID
	revision int64
	apply    func(context.Context, *sql.Tx, *flowRow) ([]note, error)
}

// CatchUp 以事务消费者处理一页事件：流程推进、待发命令、去重与水位同事务
// 提交。权威读取在 SQL 事务之外完成；流程修订在事务内复核，并发变化按瞬时
// 故障退避重试。不启动后台消费。
func (w *Service) CatchUp(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, invalid("invalid workflow batch")
	}
	if err := w.d.Events.RegisterConsumer(ctx, consumerName, true); err != nil {
		return 0, err
	}
	offset, err := w.consumer.Offset(ctx)
	if err != nil {
		return 0, err
	}
	page, err := w.d.Events.Read(ctx, offset, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, entry := range page.Entries {
		plans, prepareErr := w.prepare(ctx, entry.Envelope)
		applied, err := w.consumer.Apply(ctx, entry, func(ctx context.Context, tx *sql.Tx, e events.Entry) ([]events.Command, error) {
			if prepareErr != nil {
				return nil, prepareErr
			}
			for _, p := range plans {
				if err := w.applyPlan(ctx, tx, e.Envelope, p); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		if err != nil {
			return n, err
		}
		if applied {
			n++
		}
	}
	offset, err = w.consumer.Offset(ctx)
	if err != nil {
		return n, err
	}
	return n, w.d.Events.AcknowledgeConsumer(ctx, consumerName, offset)
}

func (w *Service) applyPlan(ctx context.Context, tx *sql.Tx, e event.Envelope, p plan) error {
	r, err := loadFlow(ctx, tx, p.flow)
	if err != nil {
		return err
	}
	if r.flow.Revision != p.revision {
		return &events.Failure{Kind: events.Transient, Cause: fmt.Errorf("workflow: flow %s changed during event preparation", p.flow)}
	}
	notes, err := p.apply(ctx, tx, &r)
	if err != nil {
		if code := errcode.CodeOf(err); code == errcode.SchemaInvalid {
			return &events.Failure{Kind: events.SchemaIncompatible, Cause: err}
		}
		return err
	}
	if len(notes) == 0 && !r.dirty() {
		return nil
	}
	if err = w.persist(ctx, tx, &r); err != nil {
		return err
	}
	op, err := ids.DeriveChild(e.OperationID, "workflow:"+string(e.EventID)+":flow:"+string(r.flow.ID))
	if err != nil {
		return err
	}
	var evs []event.Envelope
	for _, n := range notes {
		ev, err := event.New(w.d.IDs, w.d.Clock, event.Params{EventType: n.typ, SchemaVersion: 1, AggregateType: "flow", AggregateID: r.flow.ID, AggregateRevision: r.flow.Revision, ActorID: e.ActorID, SessionID: e.SessionID, ProjectID: r.flow.ProjectID, OperationID: op, CorrelationID: e.CorrelationID, CausationID: e.EventID, Payload: n.payload})
		if err != nil {
			return err
		}
		evs = append(evs, ev)
	}
	return w.store.AppendEvents(ctx, tx, op, evs)
}

func (r *flowRow) dirty() bool {
	return encode(r.flow)+encode(r.meta) != r.orig || slices.ContainsFunc(r.steps, func(s stepRow) bool { return s.snapshot() != s.orig })
}

// prepare 把事件映射到受影响的流程与推进动作；无关事件返回空计划。
func (w *Service) prepare(ctx context.Context, e event.Envelope) ([]plan, error) {
	switch {
	case strings.HasPrefix(e.EventType, "task.") && e.AggregateType == "task":
		return w.prepareTask(ctx, e.AggregateID)
	case e.EventType == "review.submitted" || e.EventType == "review.withdrawn" || e.EventType == "review.recorded":
		var p struct {
			Target ids.ID `json:"target_id"`
			Review ids.ID `json:"review_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil || !p.Target.Valid() {
			return nil, &events.Failure{Kind: events.SchemaIncompatible, Cause: fmt.Errorf("workflow: invalid review event payload")}
		}
		return w.prepareReview(ctx, e.EventType, p.Target, p.Review)
	case e.EventType == "publication.changed" && e.AggregateType == "asset":
		return w.preparePublication(ctx, e.AggregateID, e.OperationID)
	}
	return nil, nil
}

func (w *Service) prepareTask(ctx context.Context, task ids.ID) ([]plan, error) {
	binds, err := bound(ctx, w.d.DB, "task", task)
	if err != nil || len(binds) == 0 {
		return nil, err
	}
	view, err := w.d.Tasks.Snapshot(ctx, task)
	if err != nil {
		return nil, err
	}
	var out []plan
	seen := map[ids.ID]bool{}
	for _, b := range binds {
		if seen[b[0]] {
			continue
		}
		seen[b[0]] = true
		r, err := loadFlow(ctx, w.d.DB, b[0])
		if err != nil {
			return nil, err
		}
		if r.flow.State == "completed" {
			continue
		}
		f, err := w.prefetch(ctx, &r, &view)
		if err != nil {
			return nil, err
		}
		steps := []ids.ID{}
		for _, x := range binds {
			if x[0] == b[0] {
				steps = append(steps, x[1])
			}
		}
		out = append(out, plan{flow: r.flow.ID, revision: r.flow.Revision, apply: func(ctx context.Context, tx *sql.Tx, r *flowRow) ([]note, error) {
			for _, id := range steps {
				s := r.step(id)
				if s == nil || s != r.latest(s.run.StepKey) {
					continue
				}
				return w.onTask(ctx, tx, r, s, view, f)
			}
			return nil, nil
		}})
	}
	return out, nil
}

func (w *Service) prepareReview(ctx context.Context, typ string, targetID, reviewID ids.ID) ([]plan, error) {
	reviews, err := w.reviewService()
	if err != nil {
		return nil, err
	}
	target, err := reviews.TargetFact(ctx, targetID)
	if err != nil {
		return nil, err
	}
	var review *ledger.ReviewFact
	if typ == "review.recorded" {
		rf, err := reviews.ReviewByID(ctx, reviewID)
		if err != nil {
			return nil, err
		}
		review = &rf
	}
	binds, err := bound(ctx, w.d.DB, "task", target.Flow.TaskID)
	if err != nil {
		return nil, err
	}
	var out []plan
	seen := map[ids.ID]bool{}
	for _, b := range binds {
		if seen[b[0]] {
			continue
		}
		seen[b[0]] = true
		r, err := loadFlow(ctx, w.d.DB, b[0])
		if err != nil {
			return nil, err
		}
		if r.meta.Subject == nil || target.Flow != r.meta.Subject.Flow || target.VersionID != r.meta.Subject.Ref.VersionID || !activeFlow(r.flow.State) && r.flow.State != "paused" {
			continue
		}
		f, err := w.prefetch(ctx, &r, nil)
		if err != nil {
			return nil, err
		}
		f.target, f.review = &target, review
		out = append(out, plan{flow: r.flow.ID, revision: r.flow.Revision, apply: func(ctx context.Context, tx *sql.Tx, r *flowRow) ([]note, error) {
			return w.onReview(ctx, tx, r, typ, f)
		}})
	}
	return out, nil
}

func (w *Service) preparePublication(ctx context.Context, asset, op ids.ID) ([]plan, error) {
	binds, err := bound(ctx, w.d.DB, "asset", asset)
	if err != nil || len(binds) == 0 {
		return nil, err
	}
	control, err := w.d.Ledger.AssetControl(ctx, asset)
	if err != nil {
		return nil, err
	}
	var out []plan
	seen := map[ids.ID]bool{}
	for _, b := range binds {
		if seen[b[0]] {
			continue
		}
		seen[b[0]] = true
		r, err := loadFlow(ctx, w.d.DB, b[0])
		if err != nil {
			return nil, err
		}
		if r.meta.ApprovedVersion == "" || !activeFlow(r.flow.State) && r.flow.State != "paused" {
			continue
		}
		out = append(out, plan{flow: r.flow.ID, revision: r.flow.Revision, apply: func(ctx context.Context, tx *sql.Tx, r *flowRow) ([]note, error) {
			return w.onPublished(ctx, tx, r, control, op)
		}})
	}
	return out, nil
}

func activeFlow(s tc.FlowState) bool { return s == "running" || s == "waiting" }

func (w *Service) produceKey(r *flowRow) string { return r.meta.Definition.Steps[0].Key }

// onTask 按绑定步骤与任务当前状态推进；只有步骤最新轮次会被推进。
func (w *Service) onTask(ctx context.Context, tx *sql.Tx, r *flowRow, s *stepRow, view tasks.View, f facts) ([]note, error) {
	def := r.meta.Definition.Steps[s.meta.Ordinal]
	t := view.Task
	if t.State == "cancelled" {
		if !open(s.run.State) {
			return nil, nil
		}
		if err := toTerminal(s, "cancelled"); err != nil {
			return nil, err
		}
		// 流程已结束（取消或已因业务结论失败）时，任务取消只是传播结果。
		if r.flow.State == "cancelled" || r.flow.State == "failed" {
			return nil, nil
		}
		return w.fail(r, def.Key+"_task_cancelled"), nil
	}
	if s.meta.Ordinal == 0 {
		return w.onProduce(ctx, tx, r, s, view, f)
	}
	switch t.State {
	case "claimed", "blocked", "reconciling", "todo", "rework":
		if s.run.State == "ready" {
			return nil, transition(s, "running")
		}
	case "submitted":
		if s.run.State == "running" {
			return nil, transition(s, "waiting")
		}
	case "done":
		if !open(s.run.State) || view.Meta.Completion == nil {
			return nil, nil
		}
		op := view.Meta.Completion.AuthorityOperationID
		if s.run.State == "ready" {
			if err := transition(s, "running"); err != nil {
				return nil, err
			}
		}
		if view.Meta.Completion.Verdict == "pass" && f.qaEvidence != "" {
			s.run.AuthorityOperationID = op
			if err := transition(s, "completed"); err != nil {
				return nil, err
			}
			r.meta.QAEvidenceID = f.qaEvidence
			return w.startNext(ctx, tx, r, def.Key, f)
		}
		s.meta.Outcome = "qa_" + view.Meta.Completion.Verdict
		if err := transition(s, "failed"); err != nil {
			return nil, err
		}
		return w.rework(ctx, tx, r, op, "qa_"+view.Meta.Completion.Verdict, f)
	}
	return nil, nil
}

func (w *Service) onProduce(ctx context.Context, tx *sql.Tx, r *flowRow, s *stepRow, view tasks.View, f facts) ([]note, error) {
	t := view.Task
	switch {
	case t.State == "rework" && s.run.State == "waiting" && view.Meta.Round == r.meta.ProductionRound+1:
		// 返工：本轮以退回结束，新轮次沿用同一任务，输入与历史保留。
		s.meta.Outcome = "returned"
		if err := transition(s, "failed"); err != nil {
			return nil, err
		}
		r.meta.Reworks++
		r.meta.ProductionRound = view.Meta.Round
		r.meta.Subject, r.meta.CheckEvidence, r.meta.QAEvidenceID, r.meta.ReviewTargetID = nil, []ids.ID{}, "", ""
		next, err := w.newStep(ctx, tx, r, s.run.StepKey, "running")
		if err != nil {
			return nil, err
		}
		next.run.TaskIDs = []ids.ID{t.ID}
		if err := bind(ctx, tx, "task", t.ID, r.flow.ID, next.run.ID); err != nil {
			return nil, err
		}
		return []note{{"flow.reworked", map[string]any{"production_round": r.meta.ProductionRound}}}, nil
	case t.State == "submitted" && s.run.State == "running" && view.Meta.Round == r.meta.ProductionRound:
		if len(t.OutputRefs) != 1 || view.Attempt == nil {
			return w.block(s, r, "production_output_count"), nil
		}
		out := t.OutputRefs[0]
		if r.meta.SubjectAssetID != "" && out.AssetID != r.meta.SubjectAssetID {
			return w.block(s, r, "production_output_asset_mismatch"), nil
		}
		if f.version == nil || f.version.VersionID != out.VersionID {
			return nil, &events.Failure{Kind: events.Transient, Cause: fmt.Errorf("workflow: delivered version not prefetched")}
		}
		r.meta.SubjectAssetID = out.AssetID
		r.meta.Subject = &tasks.Subject{Flow: ledger.ReviewFlow{TaskID: t.ID, AttemptID: view.Attempt.ID, Round: int64(view.Meta.Round), Fence: view.Attempt.Fence.LeaseFence}, Ref: out}
		s.run.OutputRefs = []ids.PermanentRef{out}
		if err := transition(s, "waiting"); err != nil {
			return nil, err
		}
		if err := bind(ctx, tx, "asset", out.AssetID, r.flow.ID, s.run.ID); err != nil {
			return nil, err
		}
		notes, err := w.startNext(ctx, tx, r, s.run.StepKey, f)
		return append([]note{{"flow.delivered", map[string]any{"production_round": r.meta.ProductionRound, "version_id": out.VersionID}}}, notes...), err
	case t.State == "done" && open(s.run.State) && view.Meta.Completion != nil:
		if s.run.State == "running" {
			if err := transition(s, "waiting"); err != nil {
				return nil, err
			}
		}
		s.run.AuthorityOperationID = view.Meta.Completion.AuthorityOperationID
		if err := transition(s, "completed"); err != nil {
			return nil, err
		}
		return w.maybeComplete(r), nil
	case s.run.State == "ready":
		return nil, transition(s, "running")
	}
	return nil, nil
}

// onReview 只接受绑定本轮交付（任务、Attempt、轮次、fence 与版本）的审定事实。
func (w *Service) onReview(ctx context.Context, tx *sql.Tx, r *flowRow, typ string, f facts) ([]note, error) {
	key := ""
	for _, d := range r.meta.Definition.Steps {
		if d.Kind == "review" {
			key = d.Key
		}
	}
	s := r.latest(key)
	if s == nil || !open(s.run.State) || s.meta.ProductionRound != r.meta.ProductionRound {
		return nil, nil
	}
	switch typ {
	case "review.submitted":
		r.meta.ReviewTargetID, s.meta.TargetID = f.target.ID, f.target.ID
		if s.run.State == "ready" {
			if err := transition(s, "running"); err != nil {
				return nil, err
			}
		}
		if s.run.State == "blocked" {
			return nil, nil
		}
		return nil, transition(s, "waiting")
	case "review.withdrawn":
		if f.target.ID != r.meta.ReviewTargetID {
			return nil, nil
		}
		return w.block(s, r, "review_withdrawn"), nil
	}
	rv := f.review.Review
	if s.run.State == "ready" || s.run.State == "blocked" {
		return nil, nil
	}
	switch rv.Verdict {
	case "approve":
		s.run.AuthorityOperationID = rv.OperationID
		if err := transition(s, "completed"); err != nil {
			return nil, err
		}
		r.meta.ReviewID, r.meta.ApprovedVersion = rv.ID, f.target.VersionID
		notes, err := w.startNext(ctx, tx, r, key, f)
		return append([]note{{"flow.approved", map[string]any{"review_id": rv.ID, "version_id": f.target.VersionID}}}, notes...), err
	case "return":
		s.meta.Outcome = "returned"
		if err := transition(s, "failed"); err != nil {
			return nil, err
		}
		return w.rework(ctx, tx, r, rv.OperationID, "review_returned", f)
	default:
		s.meta.Outcome = "rejected"
		if err := transition(s, "failed"); err != nil {
			return nil, err
		}
		r.meta.Rounds = append(r.meta.Rounds, ProductionRecord{Round: r.meta.ProductionRound, Subject: r.meta.Subject, Outcome: "rejected", Authority: rv.OperationID, Evidence: r.meta.CheckEvidence, At: clock.Format(w.now())})
		if _, err := w.enqueue(ctx, tx, r, r.latest(w.produceKey(r)), "tasks.cancel", "cancel:rejected", taskCancel{TaskID: r.meta.ProduceTaskID, Reason: "review rejected"}); err != nil {
			return nil, err
		}
		return w.fail(r, "review_rejected"), nil
	}
}

// onPublished 仅当台账当前发布指针指向本流程批准的版本时完成发布步骤。
func (w *Service) onPublished(ctx context.Context, tx *sql.Tx, r *flowRow, control ledger.AssetControl, op ids.ID) ([]note, error) {
	key := r.meta.Definition.Steps[len(r.meta.Definition.Steps)-1].Key
	s := r.latest(key)
	if s == nil || !open(s.run.State) || s.meta.ProductionRound != r.meta.ProductionRound {
		return nil, nil
	}
	if control.PublicationState != "published" || control.PublishedVersionID != r.meta.ApprovedVersion {
		return nil, nil
	}
	if s.run.State == "ready" {
		if err := transition(s, "running"); err != nil {
			return nil, err
		}
	}
	// A downstream task depends on this production task. Keep it submitted
	// until the ledger really published the approved version; approval alone
	// must not release that dependency in a manual publication flow.
	reviewStep := r.latest(r.meta.Definition.Steps[len(r.meta.Definition.Steps)-2].Key)
	if reviewStep == nil || !reviewStep.run.AuthorityOperationID.Valid() {
		return nil, invalid("published flow has no accepted review authority")
	}
	authority := reviewStep.run.AuthorityOperationID
	if _, err := w.enqueue(ctx, tx, r, r.latest(w.produceKey(r)), "tasks.complete", "complete:"+string(authority), taskAuthority{TaskID: r.meta.ProduceTaskID, AuthorityOperationID: authority}); err != nil {
		return nil, err
	}
	s.run.AuthorityOperationID = op
	s.run.OutputRefs = []ids.PermanentRef{r.meta.Subject.Ref}
	if err := toTerminal(s, "completed"); err != nil {
		return nil, err
	}
	return append([]note{{"flow.published", map[string]any{"version_id": r.meta.ApprovedVersion}}}, w.maybeComplete(r)...), nil
}

// toTerminal 从 blocked 等不能直接结束的状态经合法边结束步骤。
func toTerminal(s *stepRow, to tc.StepState) error {
	if s.run.State == "blocked" && to != "cancelled" {
		if err := transition(s, "ready"); err != nil {
			return err
		}
	}
	if s.run.State == "ready" && to != "cancelled" {
		if err := transition(s, "running"); err != nil {
			return err
		}
	}
	return transition(s, to)
}

func (w *Service) maybeComplete(r *flowRow) []note {
	p := r.latest(w.produceKey(r))
	pub := r.latest(r.meta.Definition.Steps[len(r.meta.Definition.Steps)-1].Key)
	if p == nil || pub == nil || p.run.State != "completed" || pub.run.State != "completed" || !activeFlow(r.flow.State) {
		return nil
	}
	r.meta.Rounds = append(r.meta.Rounds, ProductionRecord{Round: r.meta.ProductionRound, Subject: r.meta.Subject, Outcome: "published", Authority: pub.run.AuthorityOperationID, Evidence: r.meta.CheckEvidence, At: clock.Format(w.now())})
	r.flow.State = "completed"
	return []note{{"flow.completed", map[string]any{"version_id": r.meta.ApprovedVersion}}}
}

func (w *Service) fail(r *flowRow, reason string) []note {
	r.meta.Failure = reason
	if tc.FlowTransition(r.flow.State, "failed") {
		r.flow.State = "failed"
	}
	return []note{{"flow.failed", map[string]any{"reason": reason}}}
}

func (w *Service) block(s *stepRow, r *flowRow, reason string) []note {
	if s.run.State == "ready" {
		_ = transition(s, "running")
	}
	if transition(s, "blocked") == nil {
		s.meta.Failure = reason
	}
	r.meta.Failure = reason
	return []note{{"flow.blocked", map[string]any{"step_key": s.run.StepKey, "reason": reason}}}
}

// rework 以退回依据开启制作任务的新轮次；超过定义的返工上限则流程失败。
func (w *Service) rework(ctx context.Context, tx *sql.Tx, r *flowRow, authority ids.ID, reason string, f facts) ([]note, error) {
	r.meta.Rounds = append(r.meta.Rounds, ProductionRecord{Round: r.meta.ProductionRound, Subject: r.meta.Subject, Outcome: reason, Authority: authority, Evidence: slices.Clone(r.meta.CheckEvidence), At: clock.Format(w.now())})
	if r.meta.Reworks >= r.meta.Definition.maxRework() {
		return w.fail(r, "rework_limit"), nil
	}
	first := r.meta.Definition.Steps[0]
	payload := taskAuthority{TaskID: r.meta.ProduceTaskID, AuthorityOperationID: authority, Reason: reason}
	if r.meta.Definition.Kind != "modify" && r.meta.SubjectAssetID != "" && first.Task.Checkout != "" && first.Task.Checkout != "none" {
		payload.Checkouts = []tasks.CheckoutSpec{{AssetID: r.meta.SubjectAssetID, Mode: first.Task.Checkout}}
	}
	if _, err := w.enqueue(ctx, tx, r, r.latest(first.Key), "tasks.rework", fmt.Sprintf("rework:%d", r.meta.ProductionRound), payload); err != nil {
		return nil, err
	}
	return []note{{"flow.returned", map[string]any{"reason": reason, "production_round": r.meta.ProductionRound}}}, nil
}

func (w *Service) startNext(ctx context.Context, tx *sql.Tx, r *flowRow, after string, f facts) ([]note, error) {
	i := r.meta.Definition.index(after) + 1
	if i <= 0 || i >= len(r.meta.Definition.Steps) {
		return nil, nil
	}
	_, err := w.startStep(ctx, tx, r, r.meta.Definition.Steps[i].Key, f)
	return nil, err
}

// startStep 创建步骤新轮次并持久化其启动命令；命令载荷在此固定。
func (w *Service) startStep(ctx context.Context, tx *sql.Tx, r *flowRow, key string, f facts) (*stepRow, error) {
	s, err := w.newStep(ctx, tx, r, key, "ready")
	if err != nil {
		return nil, err
	}
	def := r.meta.Definition.Steps[s.meta.Ordinal]
	switch def.Kind {
	case "task":
		req := w.taskRequest(r, def, s)
		if s.meta.TaskSpec, err = tasks.SpecDigest(req); err != nil {
			return nil, err
		}
		_, err = w.enqueue(ctx, tx, r, s, "tasks.create", "create", req)
	case "job":
		if r.meta.Subject == nil || f.version == nil {
			return nil, &events.Failure{Kind: events.Transient, Cause: fmt.Errorf("workflow: job input not prefetched")}
		}
		_, err = w.enqueue(ctx, tx, r, s, "jobs.start", "start", JobRequest{ProjectID: r.flow.ProjectID, FlowID: r.flow.ID, StepRunID: s.run.ID, Processor: def.Job.Processor, Target: r.meta.Subject.Ref, Flow: r.meta.Subject.Flow, ManifestDigest: string(f.version.ManifestDigest)})
	case "review":
		if r.meta.Subject == nil || f.vcRevision < 1 {
			return nil, &events.Failure{Kind: events.Transient, Cause: fmt.Errorf("workflow: review input not prefetched")}
		}
		evidence := slices.Clone(r.meta.CheckEvidence)
		if r.meta.QAEvidenceID != "" {
			evidence = append(evidence, r.meta.QAEvidenceID)
		}
		_, err = w.enqueue(ctx, tx, r, s, "ledger.submit_review", "submit", ledger.SubmitReview{ProjectID: r.flow.ProjectID, VersionID: r.meta.Subject.Ref.VersionID, ExpectedRevision: f.vcRevision, ProfileRef: r.meta.ProfileRef, EvidenceIDs: evidence, Flow: r.meta.Subject.Flow})
	case "publish":
		_, err = w.enqueue(ctx, tx, r, s, "ledger.run_publication", "publish", publishPayload{AssetID: r.meta.SubjectAssetID, VersionID: r.meta.ApprovedVersion, ReviewID: r.meta.ReviewID})
		if err == nil {
			err = bind(ctx, tx, "asset", r.meta.SubjectAssetID, r.flow.ID, s.run.ID)
		}
	}
	return s, err
}

// taskRequest 从定义与流程模板生成步骤任务规格（摘要由 tasks 端复核）。
func (w *Service) taskRequest(r *flowRow, def StepDef, s *stepRow) tasks.CreateRequest {
	t := r.meta.Template
	priority := t.Priority
	if def.Task.Priority != "" {
		priority = def.Task.Priority
	}
	req := tasks.CreateRequest{ProjectID: r.flow.ProjectID, Type: def.Task.Type, Title: t.Title, Description: t.Description, Priority: priority, AcceptanceCriteria: t.AcceptanceCriteria,
		Role: def.Task.Role, DueAt: t.DueAt, FlowID: r.flow.ID, StepRunID: s.run.ID}
	if def.Task.Type == "qa" {
		req.Title = "QA: " + t.Title
		req.Subject = r.meta.Subject
		return req
	}
	req.InputRefs = r.flow.Input.Refs
	req.ExpectedOutputs = t.ExpectedOutputs
	req.ContextAssetType = t.ContextAssetType
	if t.AssigneeID != "" || t.Role != "" {
		req.AssigneeID, req.Role = t.AssigneeID, t.Role
	}
	if r.meta.Definition.Kind == "modify" {
		req.Checkouts = []tasks.CheckoutSpec{{AssetID: r.meta.SubjectAssetID, Mode: def.Task.Checkout}}
	}
	return req
}

// SyncJobs 读取 T06 作业权威结果并推进检查步骤：全部证据通过才继续；合法的
// 检查失败开启返工；宿主故障使流程失败，可由 Retry 重跑作业。调用者须对流程
// 项目有 workflow.operate；只由显式调用触发。
func (w *Service) SyncJobs(ctx context.Context, who authz.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, invalid("invalid job sync batch")
	}
	if w.d.Jobs == nil {
		return 0, nil
	}
	rows, err := w.d.DB.QueryContext(ctx, `SELECT s.step_run_id,s.flow_id,f.project_id FROM workflow_step_runs s JOIN workflow_flows f ON f.flow_id=s.flow_id WHERE s.state='running' AND json_extract(s.record,'$.kind')='job' ORDER BY s.step_run_id LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	type due struct{ step, flow, project ids.ID }
	var list []due
	for rows.Next() {
		var d due
		if err = rows.Scan(&d.step, &d.flow, &d.project); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range list {
		if err := w.authorize(ctx, who, "workflow.operate", d.project, "flow", d.flow); err != nil {
			continue
		}
		done, err := w.syncJob(ctx, who, d.project, d.flow, d.step)
		if err != nil {
			return n, err
		}
		if done {
			n++
		}
	}
	return n, nil
}

func (w *Service) syncJob(ctx context.Context, who authz.Context, project, flowID, stepID ids.ID) (bool, error) {
	ctx, release, err := w.lock(ctx, project)
	if err != nil {
		return false, err
	}
	defer release()
	r, err := loadFlow(ctx, w.d.DB, flowID)
	if err != nil {
		return false, err
	}
	s := r.step(stepID)
	if s == nil || s.run.State != "running" || len(s.run.JobIDs) == 0 || !activeFlow(r.flow.State) {
		return false, nil
	}
	res, err := w.d.Jobs.JobResult(ctx, s.run.JobIDs[len(s.run.JobIDs)-1])
	if err != nil {
		return false, err
	}
	if res.State != "succeeded" && res.State != "failed" {
		return false, nil
	}
	f, err := w.prefetch(ctx, &r, nil)
	if err != nil {
		return false, err
	}
	op, err := ids.DeriveChild(s.run.OperationID, "job-result")
	if err != nil {
		return false, err
	}
	err = inTx(ctx, w.d.DB, func(tx *sql.Tx) error {
		cur, err := loadFlow(ctx, tx, flowID)
		if err != nil {
			return err
		}
		if cur.flow.Revision != r.flow.Revision {
			return errcode.New(errcode.PreconditionFailed, "flow changed")
		}
		s := cur.step(stepID)
		var notes []note
		switch {
		case res.State == "failed":
			s.meta.Outcome, s.meta.Failure = "job_failed", res.Failure
			if err := transition(s, "failed"); err != nil {
				return err
			}
			notes = w.fail(&cur, "job_failed")
		default:
			var failing ids.ID
			evidence := make([]ids.ID, 0, len(res.Evidence))
			for _, e := range res.Evidence {
				if !e.EvidenceID.Valid() || !e.OperationID.Valid() {
					return invalid("job evidence requires accepted record and operation")
				}
				evidence = append(evidence, e.EvidenceID)
				if e.Verdict != "pass" && failing == "" {
					failing = e.OperationID
				}
			}
			cur.meta.CheckEvidence = evidence
			if failing == "" && len(evidence) > 0 {
				if err := transition(s, "completed"); err != nil {
					return err
				}
				notes, err = w.startNext(ctx, tx, &cur, s.run.StepKey, f)
				if err != nil {
					return err
				}
			} else {
				s.meta.Outcome = "check_fail"
				if err := transition(s, "failed"); err != nil {
					return err
				}
				if failing == "" {
					notes = w.fail(&cur, "check_without_evidence")
				} else if notes, err = w.rework(ctx, tx, &cur, failing, "check_fail", f); err != nil {
					return err
				}
			}
		}
		if err := w.persist(ctx, tx, &cur); err != nil {
			return err
		}
		var evs []event.Envelope
		for _, n := range notes {
			e, err := w.flowEvent(who.PrincipalID, who.SessionID, op, n.typ, cur.flow, n.payload)
			if err != nil {
				return err
			}
			evs = append(evs, e)
		}
		return w.store.AppendEvents(ctx, tx, op, evs)
	})
	return err == nil, err
}

func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
