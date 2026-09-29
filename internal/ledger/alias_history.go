package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func saveAliasAllocation(ctx context.Context, tx *sql.Tx, a commit.Asset, reason string) error {
	var old string
	err := tx.QueryRowContext(ctx, `SELECT record FROM ledger_alias_allocations WHERE project_id=? AND slug_key=? AND generation=?`, a.ProjectID, pathrule.Key(a.Slug), a.Generation).Scan(&old)
	allocation := commit.AliasAllocation{Asset: a, Reason: reason}
	if err == nil {
		var prior commit.AliasAllocation
		if err = json.Unmarshal([]byte(old), &prior); err != nil {
			return err
		}
		if prior.Asset != a || prior.Reason != reason {
			return errcode.New(errcode.OperationNeedsReconciliation, "alias allocation differs")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ledger_alias_allocations(project_id,slug_key,generation,record) VALUES(?,?,?,?)`, a.ProjectID, pathrule.Key(a.Slug), a.Generation, encoded(allocation))
	return err
}
func (s *Service) AliasAllocation(ctx context.Context, project ids.ID, slug string, generation int64) (commit.AliasAllocation, error) {
	var out commit.AliasAllocation
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT sequence,record FROM ledger_alias_allocations WHERE project_id=? AND slug_key=? AND generation=?`, project, pathrule.Key(slug), generation).Scan(&out.Sequence, &raw)
	if err != nil {
		return out, missing(err)
	}
	seq := out.Sequence
	err = json.Unmarshal([]byte(raw), &out)
	out.Sequence = seq
	return out, err
}
func (s *Service) AliasAllocations(ctx context.Context, after int64, limit int) ([]commit.AliasAllocation, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, invalid("invalid alias allocation cursor")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,record FROM ledger_alias_allocations WHERE sequence>? ORDER BY sequence LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []commit.AliasAllocation{}
	for rows.Next() {
		var seq int64
		var raw string
		if err = rows.Scan(&seq, &raw); err != nil {
			return nil, err
		}
		var a commit.AliasAllocation
		if err = json.Unmarshal([]byte(raw), &a); err != nil {
			return nil, err
		}
		a.Sequence = seq
		out = append(out, a)
	}
	return out, rows.Err()
}
