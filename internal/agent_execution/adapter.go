package agent_execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
)

// Adapter binds the public execution protocol to the authenticated caller;
// credentials never appear in the protocol documents or persisted run records.
type Adapter struct {
	Service *Service
	Who     authz.Context
}

var _ ae.Adapter = Adapter{}

func (a Adapter) Describe(context.Context) (ae.Capabilities, error) { return Capabilities(), nil }
func (a Adapter) Start(ctx context.Context, in ae.StartRequest) (ae.Admission, error) {
	if in.Action != execution.ActStart {
		return ae.Admission{}, bad("start action required")
	}
	r, e := a.Service.Start(ctx, a.Who, string(in.OperationID), in)
	return r.Current, e
}
func (a Adapter) Resume(ctx context.Context, in ae.StartRequest) (ae.Admission, error) {
	if in.Action != execution.ActResume {
		return ae.Admission{}, bad("resume action required")
	}
	r, e := a.Service.Start(ctx, a.Who, string(in.OperationID), in)
	return r.Current, e
}
func (a Adapter) admission(ctx context.Context, column, value string) (ae.Admission, error) {
	// column is selected only by the two compiled methods below.
	var raw string
	var ad ae.Admission
	e := a.Service.d.DB.QueryRowContext(ctx, `SELECT admission FROM agent_execution_admissions WHERE `+column+`=?`, value).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return ad, errcode.New(errcode.NotFound, "")
	}
	if e != nil {
		return ad, e
	}
	if e = json.Unmarshal([]byte(raw), &ad); e != nil {
		return ad, e
	}
	if _, e = a.Service.Read(ctx, a.Who, ad.TaskRunID); e != nil {
		return ae.Admission{}, e
	}
	return ad, nil
}
func (a Adapter) Lookup(ctx context.Context, in ae.LookupRequest) (ae.Admission, error) {
	if e := tc.ValidateShape("lantai.agent-request/v1", in); e != nil {
		return ae.Admission{}, e
	}
	if in.Action != execution.ActLookup {
		return ae.Admission{}, bad("lookup action required")
	}
	return a.admission(ctx, "execution_key", in.ExecutionKey)
}
func (a Adapter) query(ctx context.Context, in ae.QueryRequest, action execution.ExecutionAction) (ae.Admission, Record, error) {
	if e := tc.ValidateShape("lantai.agent-request/v1", in); e != nil {
		return ae.Admission{}, Record{}, e
	}
	if in.Action != action {
		return ae.Admission{}, Record{}, bad("query action mismatch")
	}
	ad, e := a.admission(ctx, "execution_id", string(in.ExecutionID))
	if e != nil {
		return ad, Record{}, e
	}
	r, e := a.Service.Read(ctx, a.Who, ad.TaskRunID)
	return ad, r, e
}
func (a Adapter) Status(ctx context.Context, in ae.QueryRequest) (ae.Status, error) {
	ad, r, e := a.query(ctx, in, execution.ActStatus)
	if e != nil {
		return ae.Status{}, e
	}
	if ad.ExecutionID != r.Current.ExecutionID {
		return ae.Status{}, errcode.New(errcode.LeaseStale, "execution was superseded; query the current run")
	}
	return ae.Status{Protocol: execution.AgentExecution, Action: execution.ActStatus, ExecutionID: ad.ExecutionID, Revision: r.Run.Revision, State: r.Run.State, ObservedAt: clock.Format(a.Service.d.Clock.Now()), BudgetUsage: r.Usage, Termination: r.Run.Termination}, nil
}
func (a Adapter) Result(ctx context.Context, in ae.QueryRequest) (ae.Result, error) {
	ad, r, e := a.query(ctx, in, execution.ActResult)
	if e != nil {
		return ae.Result{}, e
	}
	if r.Result == nil || r.Result.ExecutionID != ad.ExecutionID {
		return ae.Result{}, errcode.New(errcode.ResultNotReady, "")
	}
	return *r.Result, nil
}
func (a Adapter) Cancel(ctx context.Context, in ae.CancelRequest) (ae.CancelResponse, error) {
	if e := ae.CheckCancel(in, Capabilities()); e != nil {
		return ae.CancelResponse{}, e
	}
	ad, e := a.admission(ctx, "execution_id", string(in.ExecutionID))
	if e != nil {
		return ae.CancelResponse{}, e
	}
	r, e := a.Service.Read(ctx, a.Who, ad.TaskRunID)
	if e != nil {
		return ae.CancelResponse{}, e
	}
	if r.Current.ExecutionID != ad.ExecutionID || in.Fence.AttemptID != ad.AttemptID || in.Fence.TaskID != r.Run.TaskID {
		return ae.CancelResponse{}, errcode.New(errcode.LeaseStale, "")
	}
	r, e = a.Service.cancel(ctx, a.Who, string(in.OperationID), CancelRequest{Mutation: Mutation{RunID: r.Run.ID, ExpectedRevision: r.Run.Revision, Fence: in.Fence}, Reason: in.Reason, Mode: in.RequestedCancelMode}, in)
	if e != nil {
		return ae.CancelResponse{}, e
	}
	return ae.CancelResponse{Protocol: execution.AgentExecution, Action: execution.ActCancel, ExecutionID: ids.ID(in.ExecutionID), State: r.Run.State, CancelMode: execution.CancelRevokeOnly, Termination: r.Run.Termination}, nil
}
