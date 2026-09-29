package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

type pending struct {
	op, flow, step, project ids.ID
	typ                     string
	payload                 json.RawMessage
	attempts                int
}

// Dispatch 以调用者身份执行已持久化的流程命令。每个目标在最终接受时复验
// 调用者权限、修订与领域条件，并按稳定 operation 幂等。首个派发者被固定为
// 该命令的回执作用域；台账命令只能由同一主体重试，任务命令由步骤唯一性或
// 状态后置条件保证不重复生效。结果不明时先按后置条件对账，不换键重做。
func (w *Service) Dispatch(ctx context.Context, who authz.Context, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, invalid("dispatch limit must be 1-100")
	}
	n := 0
	skip := map[ids.ID]bool{}
	for n < limit {
		c, err := w.claim(ctx, who, skip)
		if err != nil || c == nil {
			return n, err
		}
		receipt, callErr := w.run(ctx, who, *c)
		if err = w.finish(ctx, who, *c, receipt, callErr); err != nil {
			return n, err
		}
		skip[c.op] = true
		n++
	}
	return n, nil
}

func (w *Service) claim(ctx context.Context, who authz.Context, skip map[ids.ID]bool) (*pending, error) {
	rows, err := w.d.DB.QueryContext(ctx, `SELECT c.operation_id,c.flow_id,c.step_run_id,c.command_type,c.payload,c.attempts,c.actor_id,f.project_id,f.state FROM workflow_commands c JOIN workflow_flows f ON f.flow_id=c.flow_id
		WHERE c.status='pending' AND c.retry_at<=? ORDER BY c.created_at,c.operation_id LIMIT 200`, clock.Millis(w.now()))
	if err != nil {
		return nil, err
	}
	var candidates []pending
	for rows.Next() {
		var p pending
		var raw, actor, state string
		if err = rows.Scan(&p.op, &p.flow, &p.step, &p.typ, &raw, &p.attempts, &actor, &p.project, &state); err != nil {
			rows.Close()
			return nil, err
		}
		p.payload = json.RawMessage(raw)
		if skip[p.op] || actor != "" && ids.ID(actor) != who.PrincipalID || state == "paused" || state == "cancelled" && p.typ != "tasks.cancel" {
			continue
		}
		candidates = append(candidates, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, p := range candidates {
		if w.authorize(ctx, who, "workflow.operate", p.project, "flow", p.flow) != nil {
			continue
		}
		lctx, h, err := w.d.Gate.Acquire(ctx, commands.Request{})
		if err != nil {
			return nil, err
		}
		res, err := w.d.DB.ExecContext(lctx, `UPDATE workflow_commands SET actor_id=?,attempts=attempts+1,retry_at=? WHERE operation_id=? AND status='pending' AND attempts=?`, who.PrincipalID, clock.Millis(w.now().Add(backoff(p.attempts+1))), p.op, p.attempts)
		h.Release()
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			p.attempts++
			return &p, nil
		}
	}
	return nil, nil
}

func backoff(attempts int) time.Duration {
	d := time.Second << min(attempts-1, 9)
	return min(d, 5*time.Minute)
}

// run 调用目标领域接口；它在流程事务与锁之外执行。
func (w *Service) run(ctx context.Context, who authz.Context, c pending) (any, error) {
	key := "wf-" + string(c.op)
	switch c.typ {
	case "tasks.create":
		var req tasks.CreateRequest
		if err := json.Unmarshal(c.payload, &req); err != nil {
			return nil, err
		}
		return w.d.Tasks.Create(ctx, who, key, req)
	case "tasks.complete", "tasks.rework", "tasks.cancel":
		return w.runTask(ctx, who, c)
	case "jobs.start":
		if w.d.Jobs == nil {
			return nil, errcode.New(errcode.UnsupportedCapability, "").WithDetails(errcode.Detail{Reason: "job_authority_not_configured"})
		}
		var req JobRequest
		if err := json.Unmarshal(c.payload, &req); err != nil {
			return nil, err
		}
		req.OperationID = c.op
		return w.d.Jobs.StartJob(ctx, who, req)
	case "ledger.submit_review":
		reviews, err := w.reviewService()
		if err != nil {
			return nil, err
		}
		var in ledger.SubmitReview
		if err := json.Unmarshal(c.payload, &in); err != nil {
			return nil, err
		}
		return reviews.Submit(ctx, who, key, in)
	case "ledger.run_publication":
		var p publishPayload
		if err := json.Unmarshal(c.payload, &p); err != nil {
			return nil, err
		}
		return w.runPublication(ctx, who, p)
	}
	return nil, errcode.New(errcode.SchemaInvalid, "unknown flow command")
}

// runTask 在当前修订上执行任务命令；原结果不明时先核对后置条件。
func (w *Service) runTask(ctx context.Context, who authz.Context, c pending) (any, error) {
	var p taskAuthority
	if err := json.Unmarshal(c.payload, &p); err != nil {
		return nil, err
	}
	reason := p.Reason
	if c.typ == "tasks.cancel" {
		var x taskCancel
		if err := json.Unmarshal(c.payload, &x); err != nil {
			return nil, err
		}
		p.TaskID, reason = x.TaskID, x.Reason
	}
	view, err := w.d.Tasks.Snapshot(ctx, p.TaskID)
	if err != nil {
		return nil, err
	}
	if taskSatisfied(view, c.typ, p.AuthorityOperationID) {
		return map[string]any{"task_id": p.TaskID, "already": true}, nil
	}
	ref := tasks.TaskRef{ProjectID: view.Task.ProjectID, TaskID: p.TaskID, ExpectedRevision: view.Task.Revision}
	key := fmt.Sprintf("wf-%s-%d", c.op, view.Task.Revision)
	var out tasks.Result
	switch c.typ {
	case "tasks.complete":
		out, err = w.d.Tasks.Complete(ctx, who, key, tasks.CompleteRequest{TaskRef: ref, AuthorityOperationID: p.AuthorityOperationID})
	case "tasks.rework":
		out, err = w.d.Tasks.Rework(ctx, who, key, tasks.ReworkRequest{TaskRef: ref, AuthorityOperationID: p.AuthorityOperationID, Reason: reason, Checkouts: p.Checkouts})
	default:
		out, err = w.d.Tasks.Cancel(ctx, who, key, tasks.CancelRequest{TaskRef: ref, Reason: reason})
	}
	if err != nil {
		if again, e := w.d.Tasks.Snapshot(ctx, p.TaskID); e == nil && taskSatisfied(again, c.typ, p.AuthorityOperationID) {
			return map[string]any{"task_id": p.TaskID, "already": true}, nil
		}
		return nil, err
	}
	return out, nil
}

func taskSatisfied(v tasks.View, typ string, authority ids.ID) bool {
	switch typ {
	case "tasks.complete":
		return v.Meta.Completion != nil && v.Meta.Completion.AuthorityOperationID == authority
	case "tasks.rework":
		return slices.ContainsFunc(v.Meta.History, func(h tasks.RoundOutcome) bool { return h.AuthorityOperationID == authority })
	default:
		return v.Task.State == "cancelled" || v.Meta.CancelRequested
	}
}

// runPublication 只执行台账已为该批准持久化的发布请求；手动发布模式下没有
// 请求，步骤等待负责人发布。已指向目标版本时视为完成，不重复发布。
func (w *Service) runPublication(ctx context.Context, who authz.Context, p publishPayload) (any, error) {
	control, err := w.d.Ledger.AssetControl(ctx, p.AssetID)
	if err != nil {
		return nil, err
	}
	if control.PublicationState == "published" && control.PublishedVersionID == p.VersionID {
		return map[string]any{"published": true}, nil
	}
	reviews, err := w.reviewService()
	if err != nil {
		return nil, err
	}
	reqs, err := reviews.PublicationRequests(ctx, who, p.AssetID)
	if err != nil {
		return nil, err
	}
	for _, r := range reqs {
		if r.ReviewID != p.ReviewID || r.VersionID != p.VersionID {
			continue
		}
		if r.Status == "succeeded" {
			return map[string]any{"published": true}, nil
		}
		if r.Status != "pending" && r.Status != "failed" {
			return nil, errcode.New(errcode.NotPublishable, "").WithDetails(errcode.Detail{Reason: "publication_request_" + r.Status})
		}
		pub, err := reviews.RunPublication(ctx, who, r.ID)
		if err != nil {
			if c, e := w.d.Ledger.AssetControl(ctx, p.AssetID); e == nil && c.PublicationState == "published" && c.PublishedVersionID == p.VersionID {
				return map[string]any{"published": true}, nil
			}
			return nil, err
		}
		return pub, nil
	}
	return map[string]any{"mode": "manual"}, nil
}

// transientCode 表示暂时不可用，按退避重试；其他领域拒绝使步骤阻塞等待处置。
func transientCode(err error) bool {
	switch errcode.CodeOf(err) {
	case errcode.MaintenanceMode, errcode.ResourceBusy, errcode.StorageUnavailable, errcode.RateLimited, errcode.Internal, errcode.AuthRequired, errcode.TokenExpired:
		return true
	case "":
		return true
	}
	return false
}

func (w *Service) finish(ctx context.Context, who authz.Context, c pending, receipt any, callErr error) error {
	lctx, h, err := w.d.Gate.Acquire(ctx, commands.Request{})
	if err != nil {
		return err
	}
	defer h.Release()
	var raw []byte
	if callErr == nil {
		if raw, err = canonjson.CanonicalizeValue(receipt); err != nil || len(raw) > 64<<10 {
			callErr = errcode.New(errcode.SchemaInvalid, "invalid or oversized downstream receipt")
		}
	}
	return inTx(lctx, w.d.DB, func(tx *sql.Tx) error {
		r, err := loadFlow(lctx, tx, c.flow)
		if err != nil {
			return err
		}
		var notes []note
		switch {
		case callErr == nil:
			res, err := tx.ExecContext(lctx, `UPDATE workflow_commands SET status='succeeded',receipt=?,failure_code='' WHERE operation_id=? AND status='pending'`, string(raw), c.op)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil // 已被取消或并发完成
			}
			r.settle(c.op)
			if err := w.applyReceipt(lctx, tx, &r, c, raw); err != nil {
				return err
			}
		case transientCode(callErr):
			_, err := tx.ExecContext(lctx, `UPDATE workflow_commands SET failure_code=? WHERE operation_id=? AND status='pending'`, string(errcode.CodeOf(callErr)), c.op)
			return err
		default:
			code := errcode.CodeOf(callErr)
			res, err := tx.ExecContext(lctx, `UPDATE workflow_commands SET status='blocked',failure_code=? WHERE operation_id=? AND status='pending'`, string(code), c.op)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil
			}
			r.settle(c.op)
			if s := r.step(c.step); s != nil && open(s.run.State) && r.flow.State != "cancelled" {
				notes = w.block(s, &r, "command_"+string(code))
			}
		}
		if err := w.persist(lctx, tx, &r); err != nil {
			return err
		}
		var evs []event.Envelope
		for _, n := range notes {
			e, err := w.flowEvent(who.PrincipalID, who.SessionID, c.op, n.typ, r.flow, n.payload)
			if err != nil {
				return err
			}
			evs = append(evs, e)
		}
		return w.store.AppendEvents(lctx, tx, c.op, evs)
	})
}

// applyReceipt 把目标回执中的对象引用绑定到步骤；状态推进仍以领域事件为准。
func (w *Service) applyReceipt(ctx context.Context, tx *sql.Tx, r *flowRow, c pending, raw []byte) error {
	s := r.step(c.step)
	if s == nil || !open(s.run.State) || r.flow.State == "cancelled" {
		return nil
	}
	start := func() error {
		if s.run.State == "ready" {
			return transition(s, "running")
		}
		return nil
	}
	switch c.typ {
	case "tasks.create":
		var res tasks.Result
		if err := json.Unmarshal(raw, &res); err != nil || !res.TaskID.Valid() {
			return errcode.New(errcode.SchemaInvalid, "invalid task receipt")
		}
		if !slices.Contains(s.run.TaskIDs, res.TaskID) {
			s.run.TaskIDs = append(s.run.TaskIDs, res.TaskID)
		}
		if s.meta.Ordinal == 0 {
			r.meta.ProduceTaskID = res.TaskID
		}
		if err := bind(ctx, tx, "task", res.TaskID, r.flow.ID, s.run.ID); err != nil {
			return err
		}
		return start()
	case "jobs.start":
		var ref JobRef
		if err := json.Unmarshal(raw, &ref); err != nil || !ref.JobID.Valid() {
			return errcode.New(errcode.SchemaInvalid, "invalid job receipt")
		}
		if !slices.Contains(s.run.JobIDs, ref.JobID) {
			s.run.JobIDs = append(s.run.JobIDs, ref.JobID)
		}
		if err := bind(ctx, tx, "job", ref.JobID, r.flow.ID, s.run.ID); err != nil {
			return err
		}
		return start()
	case "ledger.submit_review":
		var t ledger.ReviewTarget
		if err := json.Unmarshal(raw, &t); err != nil || !t.ID.Valid() {
			return errcode.New(errcode.SchemaInvalid, "invalid review receipt")
		}
		s.meta.TargetID, r.meta.ReviewTargetID = t.ID, t.ID
		return start()
	case "ledger.run_publication":
		if err := start(); err != nil {
			return err
		}
		if s.run.State == "running" {
			return transition(s, "waiting")
		}
	}
	return nil
}
