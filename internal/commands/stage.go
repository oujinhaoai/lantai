package commands

// Stage 是持久操作的阶段，与 schemas/common/v1/operation.schema.json 一致。
type Stage string

const (
	StageReceiving   Stage = "receiving"
	StagePrepared    Stage = "prepared"
	StageInstalled   Stage = "installed"
	StageCommitted   Stage = "committed"
	StageProjected   Stage = "projected" // 只出现在对外视图：committed 且事件已全部收录
	StageBlocked     Stage = "blocked"
	StageQuarantined Stage = "quarantined"
	StageFailed      Stage = "failed"
	StageCancelled   Stage = "cancelled"
)

// transitions 列出持久阶段之间允许的转移。committed、failed、cancelled 之后
// 不再有持久转移；projected 由 outbox 投递情况推导，不写入 operations。
var transitions = map[Stage][]Stage{
	StageReceiving:   {StagePrepared, StageCancelled, StageFailed},
	StagePrepared:    {StageInstalled, StageCommitted, StageBlocked, StageQuarantined, StageCancelled, StageFailed},
	StageInstalled:   {StageCommitted, StageBlocked, StageQuarantined, StageCancelled, StageFailed},
	StageBlocked:     {StagePrepared, StageInstalled, StageCommitted, StageQuarantined, StageCancelled, StageFailed},
	StageQuarantined: {StageCancelled, StageFailed},
}

// Valid 报告 s 是否为可持久化的阶段（projected 不可持久化）。
func (s Stage) Valid() bool {
	switch s {
	case StageReceiving, StagePrepared, StageInstalled, StageCommitted,
		StageBlocked, StageQuarantined, StageFailed, StageCancelled:
		return true
	}
	return false
}

// Final 报告持久阶段是否已结束（不再有持久转移）。
func (s Stage) Final() bool {
	return s == StageCommitted || s == StageFailed || s == StageCancelled
}

// CanTransition 报告 from → to 是否允许。
func CanTransition(from, to Stage) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}
