package commands

import (
	"context"
	"database/sql"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"time"
)

// OutboxSource 仅访问共享 outbox 基础设施；由库的组装者交给 relay，不暴露业务表。
type OutboxSource struct {
	Label string
	DB    *sql.DB
	Gate  *Gate
}

func (s OutboxSource) Name() string { return s.Label }
func (s OutboxSource) Pull(ctx context.Context, limit int) ([]OutboxRecord, error) {
	return ReadUndelivered(ctx, s.DB, 0, limit)
}
func (s OutboxSource) Ack(ctx context.Context, eventIDs []ids.ID, at time.Time) error {
	if s.Gate == nil {
		return maintenanceErr(ReasonStarting)
	}
	ctx, h, err := s.Gate.Acquire(ctx, Request{})
	if err != nil {
		return err
	}
	defer h.Release()
	_, err = MarkDelivered(ctx, s.DB, eventIDs, at)
	return err
}
