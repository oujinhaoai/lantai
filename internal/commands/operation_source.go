package commands

import (
	"context"
	"database/sql"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// OperationSource 只读所属库的共享回执/operation/outbox 基础设施表。
// 返回值仅供受信任的应用组装；对外前必须按当前主体和资源权限过滤。
type OperationSource struct{ DB *sql.DB }

func (s OperationSource) Lookup(ctx context.Context, id ids.ID) (*Receipt, *View, error) {
	st, err := NewStore("commands", clock.System{})
	if err != nil {
		return nil, nil, err
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	r, err := st.ReceiptByOperation(ctx, tx, id)
	if err != nil {
		return nil, nil, err
	}
	v, err := st.View(ctx, tx, id)
	return r, v, err
}
