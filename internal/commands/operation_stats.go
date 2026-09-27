package commands

import (
	"context"
	"errors"
)

// OperationCounts 是当前库的操作数量；只暴露已知阶段，不暴露对象或操作 ID。
type OperationCounts struct {
	Pending int64
	ByStage map[Stage]int64
}

// OperationStats 不将 committed/projected、failed/cancelled 误计为待处理。
// 未知持久阶段意味着契约不兼容，不能把缺失统计当作零。
func OperationStats(ctx context.Context, q DBTX) (OperationCounts, error) {
	rows, err := q.QueryContext(ctx, `SELECT stage,count(*) FROM operations GROUP BY stage`)
	if err != nil {
		return OperationCounts{}, err
	}
	defer rows.Close()
	v := OperationCounts{ByStage: make(map[Stage]int64)}
	for rows.Next() {
		var stage Stage
		var n int64
		if err := rows.Scan(&stage, &n); err != nil {
			return OperationCounts{}, err
		}
		switch stage {
		case StageReceiving, StagePrepared, StageInstalled, StageBlocked, StageQuarantined:
			v.Pending += n
		case StageCommitted, StageProjected, StageFailed, StageCancelled:
		default:
			return OperationCounts{}, errors.New("commands: unknown operation stage in statistics")
		}
		v.ByStage[stage] = n
	}
	return v, rows.Err()
}
