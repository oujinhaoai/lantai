package ledger

import (
	"context"
	"encoding/json"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func readJSON[T any](ctx context.Context, q commands.DBTX, query string, args ...any) (T, error) {
	var out T
	var raw string
	if err := q.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
		return out, missing(err)
	}
	err := json.Unmarshal([]byte(raw), &out)
	return out, err
}
func (s *Service) Project(ctx context.Context, id ids.ID) (commit.Project, error) {
	return readJSON[commit.Project](ctx, s.db, `SELECT record FROM ledger_projects WHERE project_id = ?`, id)
}
func (s *Service) ProjectByKey(ctx context.Context, key string) (commit.Project, error) {
	return readJSON[commit.Project](ctx, s.db, `SELECT record FROM ledger_projects WHERE project_key = ?`, key)
}

// Projects 是受信任的权威枚举；面向用户的授权和分页由 catalog 完成。
func (s *Service) Projects(ctx context.Context, after ids.ID, limit int) ([]commit.Project, error) {
	if limit < 1 || limit > 1000 || (after != "" && !after.Valid()) {
		return nil, invalid("invalid project enumeration")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM ledger_projects WHERE project_id > ? ORDER BY project_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []commit.Project{}
	for rows.Next() {
		var raw string
		var p commit.Project
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// VersionByID 是内部精确定位，不执行调用者授权。
func (s *Service) VersionByID(ctx context.Context, id ids.ID) (commit.Committed, error) {
	return readJSON[commit.Committed](ctx, s.db, `SELECT record FROM ledger_versions WHERE version_id = ?`, id)
}
func (s *Service) Asset(ctx context.Context, id ids.ID) (commit.Asset, error) {
	return readJSON[commit.Asset](ctx, s.db, `SELECT record FROM ledger_assets WHERE asset_id = ? AND record IS NOT NULL`, id)
}
func (s *Service) Version(ctx context.Context, asset, id ids.ID) (commit.Committed, error) {
	v, err := readJSON[commit.Committed](ctx, s.db, `SELECT record FROM ledger_versions WHERE version_id = ?`, id)
	if err == nil && v.AssetID != asset {
		return commit.Committed{}, errcode.New(errcode.RefMismatch, "")
	}
	return v, err
}
func (s *Service) VersionByNumber(ctx context.Context, asset ids.ID, number int64) (commit.Committed, error) {
	return readJSON[commit.Committed](ctx, s.db, `SELECT record FROM ledger_versions WHERE asset_id = ? AND version_number = ?`, asset, number)
}
func (s *Service) LatestVersion(ctx context.Context, asset ids.ID) (commit.Committed, error) {
	return readJSON[commit.Committed](ctx, s.db, `SELECT record FROM ledger_versions WHERE asset_id = ? ORDER BY version_number DESC LIMIT 1`, asset)
}
func (s *Service) Versions(ctx context.Context, after ids.ID, limit int) ([]commit.Committed, error) {
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		return nil, invalid("version enumeration limit must not exceed 10000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM ledger_versions WHERE version_id > ? ORDER BY version_id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []commit.Committed{}
	for rows.Next() {
		var raw string
		var v commit.Committed
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Service) Claim(ctx context.Context, project ids.ID, slug string) (commit.Claim, error) {
	return readJSON[commit.Claim](ctx, s.db, `SELECT record FROM ledger_namespace_claims WHERE project_id = ? AND slug_key = ?`, project, pathrule.Key(slug))
}
func (s *Service) CurrentMetadata(ctx context.Context, t commit.MetadataTarget) (commit.CommittedMetadata, error) {
	return readJSON[commit.CommittedMetadata](ctx, s.db, `SELECT record FROM ledger_metadata_targets WHERE kind = ? AND target_id = ? AND project_id = ? AND record IS NOT NULL`, t.Kind, t.ID, t.ProjectID)
}

type versionOp struct {
	Command  commands.Context
	Prepared commit.Prepared
	Base     ids.ID
}

func (s *Service) versionOp(ctx context.Context, q commands.DBTX, id ids.ID) (versionOp, error) {
	var o versionOp
	var cmd, p string
	err := q.QueryRowContext(ctx, `SELECT command,prepared,base_version_id FROM ledger_prepared WHERE operation_id = ?`, id).Scan(&cmd, &p, &o.Base)
	if err != nil {
		return o, missing(err)
	}
	if err = json.Unmarshal([]byte(cmd), &o.Command); err != nil {
		return o, err
	}
	err = json.Unmarshal([]byte(p), &o.Prepared)
	return o, err
}
func (s *Service) LookupPrepared(ctx context.Context, cmd commands.Context) (commit.Prepared, error) {
	if err := cmd.Validate(); err != nil {
		return commit.Prepared{}, err
	}
	if cmd.CommandType != commit.CommandType {
		return commit.Prepared{}, invalid("unexpected command type")
	}
	r, err := s.lookup(ctx, s.db, cmd)
	if err != nil {
		return commit.Prepared{}, err
	}
	if r == nil {
		return commit.Prepared{}, errcode.New(errcode.NotFound, "")
	}
	o, err := s.versionOp(ctx, s.db, r.OperationID)
	return o.Prepared, err
}

// PreparedOperation supplies the frozen reservation for explicit recovery.
// It is an internal read hook; the caller must authorize operation visibility.
func (s *Service) PreparedOperation(ctx context.Context, id ids.ID) (commit.Prepared, error) {
	o, err := s.versionOp(ctx, s.db, id)
	return o.Prepared, err
}
