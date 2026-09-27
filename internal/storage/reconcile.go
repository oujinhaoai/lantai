package storage

import (
	"context"
	"database/sql"
	"errors"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type PinReport struct {
	Created       int `json:"created"`
	Corrected     int `json:"corrected"`
	BlobsAdded    int `json:"blobs_added"`
	UnknownOwners int `json:"unknown_owners"`
}

// ReconcilePins repairs only upload retention facts under an actual, live
// maintenance barrier. It never expires a session, revokes authorization, removes
// excess retention or deletes bytes. Unknown pin owners remain retained for
// explicit reconciliation. Repeated calls are idempotent.
func (s *Service) ReconcilePins(ctx context.Context) (PinReport, error) {
	var out PinReport
	if err := s.gate.RequireMaintenance(ctx); err != nil {
		return out, err
	}
	lctx, release, err := s.write(ctx, commands.Request{})
	if err != nil {
		return out, err
	}
	defer release()
	err = s.inTx(lctx, func(tx *sql.Tx) error {
		type upload struct {
			id               ids.ID
			created, expires int64
			closed           sql.NullInt64
		}
		rows, err := tx.QueryContext(lctx, `SELECT upload_id,created_at,expires_at,closed_at FROM storage_uploads ORDER BY upload_id`)
		if err != nil {
			return err
		}
		var all []upload
		for rows.Next() {
			var u upload
			if err = rows.Scan(&u.id, &u.created, &u.expires, &u.closed); err != nil {
				rows.Close()
				return err
			}
			all = append(all, u)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, u := range all {
			var id ids.ID
			var created, expires int64
			var released sql.NullInt64
			err = tx.QueryRowContext(lctx, `SELECT pin_id,created_at,expires_at,released_at FROM storage_pins WHERE owner_id=?`, u.id).Scan(&id, &created, &expires, &released)
			if errors.Is(err, sql.ErrNoRows) {
				id, err = ids.DeriveChild(u.id, "pin:upload")
				if err != nil {
					return err
				}
				if _, err = tx.ExecContext(lctx, `INSERT INTO storage_pins(pin_id,owner_kind,owner_id,created_at,expires_at,released_at) VALUES(?,'upload_session',?,?,?,?)`, id, u.id, u.created, u.expires, u.closed); err != nil {
					return err
				}
				out.Created++
			} else if err != nil {
				return err
			} else if created != u.created || expires != u.expires || released != u.closed {
				if _, err = tx.ExecContext(lctx, `UPDATE storage_pins SET created_at=?,expires_at=?,released_at=? WHERE pin_id=?`, u.created, u.expires, u.closed, id); err != nil {
					return err
				}
				out.Corrected++
			}
			// Include verified uploads and granted source reuse. These sources are
			// storage-owned facts; neither implies a committed ledger reference.
			res, err := tx.ExecContext(lctx, `INSERT OR IGNORE INTO storage_pin_blobs(pin_id,sha256)
			 SELECT ?,sha256 FROM storage_upload_files WHERE upload_id=? AND state='verified'
			 UNION SELECT ?,g.sha256 FROM storage_blob_grants g JOIN storage_uploads u ON u.operation_id=g.operation_id
			 WHERE u.upload_id=? AND g.revoked_at IS NULL`, id, u.id, id, u.id)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			out.BlobsAdded += int(n)
		}
		return tx.QueryRowContext(lctx, `SELECT COUNT(*) FROM storage_pins p WHERE NOT EXISTS(SELECT 1 FROM storage_uploads u WHERE u.upload_id=p.owner_id)`).Scan(&out.UnknownOwners)
	})
	if err != nil {
		return PinReport{}, err
	}
	return out, nil
}

// Stats are bounded SQL aggregates, not filesystem traversal or deep hashes.
// UploadStagingBytes is the durable received-part count for pending open files;
// orphan/private in-flight bytes are reported by Inventory, not this gauge.
type Stats struct {
	OpenUploads         int64 `json:"open_uploads"`
	UploadStagingBytes  int64 `json:"upload_staging_bytes"`
	ActiveUploadPins    int64 `json:"active_upload_pins"`
	UploadRetainedBytes int64 `json:"upload_retained_bytes"`
	GCSupported         bool  `json:"gc_supported"`
}

func (s *Service) Stats(ctx context.Context) (Stats, error) {
	var out Stats
	err := s.db.QueryRowContext(ctx, `SELECT
	 (SELECT COUNT(*) FROM storage_uploads WHERE state='open'),
	 (SELECT COALESCE(SUM(p.size),0) FROM storage_upload_parts p JOIN storage_upload_files f ON f.upload_id=p.upload_id AND f.sha256=p.sha256 JOIN storage_uploads u ON u.upload_id=p.upload_id WHERE u.state='open' AND f.state='pending'),
	 (SELECT COUNT(*) FROM storage_pins WHERE released_at IS NULL AND expires_at>?),
	 (SELECT COALESCE(SUM(size),0) FROM (SELECT g.sha256,MAX(g.size) size FROM storage_blob_grants g JOIN storage_uploads u ON u.operation_id=g.operation_id JOIN storage_pins p ON p.owner_id=u.upload_id WHERE p.released_at IS NULL AND p.expires_at>? GROUP BY g.sha256))`, clock.Millis(s.now()), clock.Millis(s.now())).Scan(&out.OpenUploads, &out.UploadStagingBytes, &out.ActiveUploadPins, &out.UploadRetainedBytes)
	return out, err
}
