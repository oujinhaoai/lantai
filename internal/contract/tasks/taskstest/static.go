// Package taskstest 提供固定事实与响应的 T05 契约桩。它不生成 ID、不推进状态、
// 不发放租约，不可接入生产入口；授权由消费者测试单独注入并断言。
package taskstest

import (
	"context"
	"encoding/json"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/tasks"
)

type Outcome struct {
	Receipt   tasks.Receipt
	ErrorCode errcode.Code
}
type Fixture struct {
	Task          tasks.Task
	Seat          tasks.Seat
	Attempt       *tasks.Attempt
	Flow          tasks.Flow
	Step          tasks.StepRun
	Now           time.Time
	RecoveryEpoch int64
	Completion    tasks.CompletionEvidence
	// Outcomes 是通过实时条件检查后要返回的预设结果；Committed 是既有持久回执。
	// 同 operation 的 Committed 重放不重新写入，只校验原请求摘要。
	Outcomes  map[ids.ID]Outcome
	Committed map[ids.ID]tasks.Receipt
}
type Static struct{ f Fixture }

var _ tasks.Reader = (*Static)(nil)
var _ tasks.Commands = (*Static)(nil)

func New(f Fixture) *Static { return &Static{f: clone(f)} }
func clone[T any](v T) T {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	var c T
	if e = json.Unmarshal(b, &c); e != nil {
		panic(e)
	}
	return c
}
func (s *Static) ReadTask(ctx context.Context, id ids.ID) (tasks.Task, error) {
	if e := ctx.Err(); e != nil {
		return tasks.Task{}, e
	}
	if id != s.f.Task.ID {
		return tasks.Task{}, errcode.New(errcode.NotFound, "")
	}
	return clone(s.f.Task), nil
}
func (s *Static) ReadSeat(ctx context.Context, id ids.ID) (tasks.Seat, error) {
	if e := ctx.Err(); e != nil {
		return tasks.Seat{}, e
	}
	if id != s.f.Seat.ID {
		return tasks.Seat{}, errcode.New(errcode.NotFound, "")
	}
	return clone(s.f.Seat), nil
}
func (s *Static) ReadAttempt(ctx context.Context, id ids.ID) (tasks.Attempt, error) {
	if e := ctx.Err(); e != nil {
		return tasks.Attempt{}, e
	}
	if s.f.Attempt == nil || id != s.f.Attempt.ID {
		return tasks.Attempt{}, errcode.New(errcode.NotFound, "")
	}
	return clone(*s.f.Attempt), nil
}
func (s *Static) ReadFlow(ctx context.Context, id ids.ID) (tasks.Flow, error) {
	if e := ctx.Err(); e != nil {
		return tasks.Flow{}, e
	}
	if id != s.f.Flow.ID {
		return tasks.Flow{}, errcode.New(errcode.NotFound, "")
	}
	return clone(s.f.Flow), nil
}
func (s *Static) ReadStepRun(ctx context.Context, id ids.ID) (tasks.StepRun, error) {
	if e := ctx.Err(); e != nil {
		return tasks.StepRun{}, e
	}
	if id != s.f.Step.ID {
		return tasks.StepRun{}, errcode.New(errcode.NotFound, "")
	}
	return clone(s.f.Step), nil
}
func (s *Static) Execute(ctx context.Context, c tasks.Command) (tasks.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return tasks.Receipt{}, err
	}
	if err := c.Validate(); err != nil {
		return tasks.Receipt{}, err
	}
	if r, ok := s.f.Committed[c.OperationID]; ok {
		if err := execution.CheckOperation(r.RequestHash, c.RequestHash); err != nil {
			return tasks.Receipt{}, err
		}
		return clone(r), nil
	}
	if err := execution.CheckRecoveryEpoch(execution.SubjectAttempt, c.RecoveryEpoch, s.f.RecoveryEpoch); err != nil {
		return tasks.Receipt{}, err
	}
	var err error
	switch c.Type {
	case "tasks.complete":
		if c.TargetID != s.f.Task.ID || c.ProjectID != s.f.Task.ProjectID {
			return tasks.Receipt{}, errcode.New(errcode.NotFound, "")
		}
		err = tasks.CheckRevision(c.ExpectedRevision, s.f.Task.Revision)
		if err == nil && c.AuthorityOperationID != s.f.Completion.AuthorityOperationID {
			err = errcode.New(errcode.InvalidStateTransition, "authority operation does not match")
		}
		if err == nil {
			err = tasks.CheckTaskCompletion(s.f.Task, s.f.Completion)
		}
	case "tasks.claim":
		if c.TargetID != s.f.Seat.ID || c.ProjectID != s.f.Task.ProjectID {
			return tasks.Receipt{}, errcode.New(errcode.NotFound, "")
		}
		err = tasks.CheckClaim(s.f.Task, s.f.Seat, s.f.Attempt, c.ExpectedRevision, c.RecoveryEpoch)
	case "tasks.requeue":
		if c.TargetID != s.f.Seat.ID || c.ProjectID != s.f.Task.ProjectID {
			return tasks.Receipt{}, errcode.New(errcode.NotFound, "")
		}
		err = tasks.CheckRevision(c.ExpectedRevision, s.f.Seat.Revision)
		if err == nil && (s.f.Attempt == nil || s.f.Seat.State != "reconciling") {
			err = errcode.New(errcode.InvalidStateTransition, "")
		}
		if err == nil {
			err = tasks.SafeToReclaim(s.f.Attempt.Reconciliation)
		}
	case "workflow.advance":
		if c.TargetID != s.f.Step.ID || c.ProjectID != s.f.Flow.ProjectID {
			return tasks.Receipt{}, errcode.New(errcode.NotFound, "")
		}
		err = tasks.CheckRevision(c.ExpectedRevision, s.f.Step.Revision)
	default:
		if s.f.Attempt == nil {
			return tasks.Receipt{}, errcode.New(errcode.NotFound, "")
		}
		// 管理员无 fence 的取消只经过修订/epoch 检查；授权是独立依赖。
		if c.Type == "tasks.cancel" && c.Fence == nil {
			if c.TargetID != s.f.Attempt.ID || c.ProjectID != s.f.Task.ProjectID {
				return tasks.Receipt{}, errcode.New(errcode.NotFound, "")
			}
			err = tasks.CheckRevision(c.ExpectedRevision, s.f.Attempt.Revision)
		} else {
			err = tasks.AcceptCommand(c, s.f.Task, s.f.Seat, *s.f.Attempt, s.f.Now, s.f.RecoveryEpoch)
		}
	}
	if err != nil {
		return tasks.Receipt{}, err
	}
	out, ok := s.f.Outcomes[c.OperationID]
	if !ok {
		return tasks.Receipt{}, errcode.New(errcode.NotFound, "no scripted outcome")
	}
	if out.ErrorCode != "" {
		return tasks.Receipt{}, errcode.New(out.ErrorCode, "")
	}
	if out.Receipt.OperationID != c.OperationID || out.Receipt.TargetID != c.TargetID {
		return tasks.Receipt{}, errcode.New(errcode.Internal, "fixture receipt does not match command")
	}
	if err := execution.CheckOperation(out.Receipt.RequestHash, c.RequestHash); err != nil {
		return tasks.Receipt{}, err
	}
	if err := tasks.ValidateShape("lantai.task-receipt/v1", out.Receipt); err != nil {
		return tasks.Receipt{}, err
	}
	return clone(out.Receipt), nil
}
