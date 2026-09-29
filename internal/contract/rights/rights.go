// Package rights 定义用途与来源限制的权威查询接口，由 provenance（T03.2）实现，
// storage（T02，下载与内容复用）、catalog 与 query（T04）调用。
//
// 判定读取当前已提交的许可证据与 rights_epoch，不依赖检索索引；索引滞后或
// 证据无法完成核验时返回 RIGHTS_PENDING，永不默认放行。未知来源不自动获得
// 最宽许可，同哈希多来源保留歧义。
package rights

import (
	"context"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Decision 是一次用途判定。
type Decision struct {
	Allowed bool
	// Code 在拒绝时为 USE_RESTRICTED（当前证据禁止该用途）或 RIGHTS_PENDING
	// （尚未完成核验，按 RetryAfter 再查）。
	Code errcode.Code
	// RightsEpoch 是判定依据的限制修订，调用方可记入授权（例如 ReadGrant）。
	RightsEpoch int64
	RetryAfter  time.Duration
}

// Err 把拒绝转换为结构化错误；允许时返回 nil。
func (d Decision) Err() error {
	if d.Allowed {
		return nil
	}
	switch d.Code {
	case errcode.RightsPending:
		return errcode.New(errcode.RightsPending, "").WithRetryAfter(d.RetryAfter)
	case errcode.UseRestricted:
		return errcode.New(errcode.UseRestricted, "")
	}
	// 未登记或意外的拒绝一律按限制处理，不放行。
	return errcode.New(errcode.UseRestricted, "")
}

// Evaluator 判定调用者能否把已提交版本用于 purpose。调用方另行完成读取授权；
// 本接口只回答许可与来源限制。
type Evaluator interface {
	EvaluateUse(ctx context.Context, who authz.Context, ref ids.PermanentRef, purpose authz.Purpose) (Decision, error)
}

// RiskReader is an internal diagnostic authorization port for lifecycle/risk
// commands. It checks project and inherited personal/source access while allowing
// examination of a disabled version or retained stable trash. It never grants
// ordinary byte reads or production use, and rejects pending/purged resources.
type RiskReader interface {
	EvaluateRiskAccess(context.Context, authz.Context, ids.PermanentRef) (Decision, error)
}
