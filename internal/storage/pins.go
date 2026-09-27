package storage

import (
	"context"
	"database/sql"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/pin"
)

var _ pin.Source = (*Service)(nil)

// PinsFor 实现 pin.Source：返回覆盖该哈希的 upload pin（含已释放的，由调用方
// 按 ActiveAt 判断）。upload pin 随上传会话到期或关闭释放；提交 pin 归台账、
// 备份 pin 归运维，GC 必须同时查询全部来源。
func (s *Service) PinsFor(ctx context.Context, sha256 string) ([]pin.Pin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.pin_id, p.owner_id, p.created_at, p.expires_at, p.released_at
		FROM storage_pins p JOIN storage_pin_blobs b ON b.pin_id = p.pin_id WHERE b.sha256 = ? ORDER BY p.pin_id`, sha256)
	if err != nil {
		return nil, err
	}
	var out []pin.Pin
	for rows.Next() {
		var p pin.Pin
		var owner ids.ID
		var created, expires int64
		var released sql.NullInt64
		if err := rows.Scan(&p.PinID, &owner, &created, &expires, &released); err != nil {
			rows.Close()
			return nil, err
		}
		p.Kind, p.OwnerModule, p.Owner = pin.KindUpload, Module, pin.Owner{Kind: "upload_session", ID: owner}
		p.CreatedAt, p.ExpiresAt = clock.FromMillis(created), clock.FromMillis(expires)
		if released.Valid {
			p.ReleasedAt = clock.FromMillis(released.Int64)
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		blobs, err := s.pinBlobs(ctx, out[i].PinID)
		if err != nil {
			return nil, err
		}
		out[i].Blobs = blobs
	}
	return out, nil
}

func (s *Service) pinBlobs(ctx context.Context, id ids.ID) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha256 FROM storage_pin_blobs WHERE pin_id = ? ORDER BY sha256`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
