package ledger

import (
	"context"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func (s *Service) VersionFileLocation(ctx context.Context, asset, version ids.ID) (commit.FileLocation, error) {
	var out commit.FileLocation
	if _, err := s.Version(ctx, asset, version); err != nil {
		return out, err
	}
	v, err := s.VersionControl(ctx, version)
	if err != nil {
		return out, err
	}
	if v.PendingOperationID != "" {
		entry, err := readJSON[TrashEntry](ctx, s.db, `SELECT record FROM ledger_trash_entries WHERE asset_id=? AND json_extract(record,'$.pending_operation_id')=? AND EXISTS (SELECT 1 FROM json_each(record,'$.version_ids') WHERE value=?)`, asset, v.PendingOperationID, version)
		if err != nil {
			return out, err
		}
		var count int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_lifecycle_ops WHERE operation_id=? AND trash_id=? AND state='accepted'`, v.PendingOperationID, entry.ID).Scan(&count); err != nil {
			return out, err
		}
		if count != 1 {
			return out, errcode.New(errcode.OperationNeedsReconciliation, "pending lifecycle has no accepted plan")
		}
		out.PendingOperationID = v.PendingOperationID
		out.TrashID = entry.ID
		return out, nil
	}
	if v.Lifecycle == "purged" {
		out.Purged = true
		return out, nil
	}
	if v.Lifecycle != "trashed" {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT trash_id FROM ledger_trash_entries WHERE asset_id=? AND state='trashed' AND EXISTS (SELECT 1 FROM json_each(record,'$.version_ids') WHERE value=?)`, asset, version)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if err = rows.Scan(&out.TrashID); err != nil {
			return out, err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if count != 1 || !out.TrashID.Valid() {
		return out, errcode.New(errcode.OperationNeedsReconciliation, "trash location is not unique")
	}
	return out, nil
}

// Risk reads permit stable retained trash for restore authorization and history.
// They do not grant content delivery, evidence append, execution or publication.
func (s *Service) CheckRiskRead(ctx context.Context, asset, version ids.ID) error {
	a, err := s.AssetControl(ctx, asset)
	if err != nil {
		return err
	}
	if a.PendingOperationID != "" {
		return errcode.New(errcode.NotFound, "")
	}
	location, err := s.VersionFileLocation(ctx, asset, version)
	if err != nil {
		return err
	}
	if location.Purged {
		return errcode.New(errcode.AssetPurged, "")
	}
	if location.PendingOperationID != "" {
		return errcode.New(errcode.NotFound, "")
	}
	return nil
}
