package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/event"
)

const EvPostCommitCheckEnrolled = "version.check_enrolled"

// PostCommitCheckEnrollment proves the enrollment was produced in the same
// ledger transaction as this exact committed version, before relay.
func (s *Service) PostCommitCheckEnrollment(ctx context.Context, v commit.Committed) (event.Envelope, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT envelope FROM outbox WHERE operation_id=? AND event_type=? AND aggregate_id=? ORDER BY seq LIMIT 1`, v.OperationID, EvPostCommitCheckEnrolled, v.VersionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return event.Envelope{}, false, nil
	}
	if err != nil {
		return event.Envelope{}, false, err
	}
	var e event.Envelope
	if err := canonjson.Validate([]byte(raw)); err != nil {
		return e, false, err
	}
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return e, false, err
	}
	return e, true, nil
}
