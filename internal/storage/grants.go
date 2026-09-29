package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
)

// validPurpose 报告 p 是否为登记的用途。
func validPurpose(p authz.Purpose) bool {
	switch p {
	case authz.PurposeArchiveReview, authz.PurposeReference, authz.PurposeProduction,
		authz.PurposeGenerativeInput, authz.PurposeRawExport:
		return true
	}
	return false
}

const grantColumns = `grant_id, project_id, operation_id, sha256, size, basis, purpose, source_project_id, source_asset_id,
	source_version_id, source_path, issued_at, expires_at`

func scanGrant(row interface{ Scan(...any) error }) (BlobGrant, error) {
	var g BlobGrant
	var srcProject, srcAsset, srcVersion, srcPath sql.NullString
	var issued, expires int64
	if err := row.Scan(&g.GrantID, &g.ProjectID, &g.OperationID, &g.SHA256, &g.Size, &g.Basis, &g.Purpose,
		&srcProject, &srcAsset, &srcVersion, &srcPath, &issued, &expires); err != nil {
		return g, err
	}
	g.IssuedAt, g.ExpiresAt = clock.FromMillis(issued), clock.FromMillis(expires)
	if srcVersion.Valid {
		g.Source = &ids.PermanentRef{AssetID: ids.ID(srcAsset.String), VersionID: ids.ID(srcVersion.String)}
		g.SourcePath = srcPath.String
	}
	return g, nil
}

func (s *Service) uploadedGrant(ctx context.Context, op ids.ID, sha string) (BlobGrant, error) {
	g, err := scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM storage_blob_grants
		WHERE operation_id = ? AND sha256 = ? AND basis = 'uploaded' AND revoked_at IS NULL`, op, sha))
	if errors.Is(err, sql.ErrNoRows) {
		return BlobGrant{}, errcode.New(errcode.BlobGrantRequired, "")
	}
	return g, err
}

// hasGrant 报告操作是否对这份内容（哈希与大小都相符）持有有效的复用授权：
// 未撤销，且已被本操作消费或尚未到期。
func (s *Service) hasGrant(ctx context.Context, q commands.DBTX, project, op ids.ID, sha string, size int64, now time.Time) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_blob_grants WHERE project_id = ? AND operation_id = ?
		AND sha256 = ? AND size = ? AND revoked_at IS NULL AND (consumed_at IS NOT NULL OR expires_at > ?)`,
		project, op, sha, size, clock.Millis(now)).Scan(&n)
	return n > 0, err
}

// grantedSize 返回操作对这份内容持有的有效授权记录的大小；没有授权时 ok 为 false。
func (s *Service) grantedSize(ctx context.Context, q commands.DBTX, project, op ids.ID, sha string, now time.Time) (int64, bool, error) {
	var size int64
	err := q.QueryRowContext(ctx, `SELECT size FROM storage_blob_grants WHERE project_id = ? AND operation_id = ? AND sha256 = ?
		AND revoked_at IS NULL AND (consumed_at IS NOT NULL OR expires_at > ?) LIMIT 1`,
		project, op, sha, clock.Millis(now)).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return size, err == nil, err
}

// VerifyGrantAcceptance 在台账最终接受的 security_guard 读锁内复验复用授权。
// 调用方负责锁与当前提交权限；本方法只读，不再取得写入口或嵌套安全锁。
// 已消费的授权仅免于到期，不免于撤权、主体绑定及来源用途的当前复验。
// 同一主体可换本人新会话继续上传和提交，不能借用另一主体的操作授权。
// expectedPurpose 必须由冻结版本清单中的实际用途推导；来源授权的用途须
// 与其一致，不能用仅供参考的授权完成生产用途的版本。
func (s *Service) VerifyGrantAcceptance(ctx context.Context, who authz.Context, req install.Request, expectedPurpose authz.Purpose) error {
	if !validPurpose(expectedPurpose) {
		return invalid("purpose %q is not a registered use", expectedPurpose)
	}
	if _, err := s.authorize(ctx, who, ActionUpload, authz.Resource{ProjectID: req.ProjectID, Kind: "project", ID: req.ProjectID}); err != nil {
		return err
	}
	for _, file := range req.Files {
		rows, err := s.db.QueryContext(ctx, `SELECT `+grantColumns+` FROM storage_blob_grants
			WHERE principal_id = ? AND project_id = ? AND operation_id = ? AND sha256 = ? AND size = ?
			AND revoked_at IS NULL AND (consumed_at IS NOT NULL OR (expires_at > ? AND EXISTS (
				SELECT 1 FROM storage_uploads u WHERE u.operation_id = storage_blob_grants.operation_id
				AND u.state = 'open' AND MIN(u.idle_expires_at, u.expires_at) > ?))) ORDER BY grant_id`,
			who.PrincipalID, req.ProjectID, req.OperationID, file.SHA256, file.Size, clock.Millis(s.now()), clock.Millis(s.now()))
		if err != nil {
			return err
		}
		var grants []BlobGrant
		for rows.Next() {
			g, err := scanGrant(rows)
			if err != nil {
				rows.Close()
				return err
			}
			grants = append(grants, g)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var denied error
		accepted := false
		for _, grant := range grants {
			if grant.Basis == BasisUploaded {
				accepted = true
				break
			}
			if grant.Source == nil || grant.Purpose != expectedPurpose {
				continue
			}
			source, v, err := s.readableFile(ctx, who, grant.Source.AssetID, grant.Source.VersionID, grant.SourcePath)
			if err != nil {
				denied = err
				continue
			}
			if source.SHA256 != file.SHA256 || source.Size != file.Size {
				denied = errcode.New(errcode.OperationNeedsReconciliation, "source content no longer matches its grant")
				continue
			}
			decision, err := s.rights.EvaluateUse(ctx, who, ids.PermanentRef{InstanceID: s.instance, AssetID: v.AssetID, VersionID: v.VersionID}, expectedPurpose)
			if err != nil {
				return err
			}
			if err := decision.Err(); err != nil {
				denied = err
				continue
			}
			accepted = true
			break
		}
		if !accepted {
			if denied != nil {
				return denied
			}
			return errcode.New(errcode.BlobGrantRequired, "").WithDetails(errcode.Detail{Reason: "grant_missing"})
		}
	}
	return nil
}

// SourceGrantRequest 请求复用一个已提交版本中的文件：调用者须对来源版本有
// 读取与用途授权，授权只对本上传会话的操作有效。
type SourceGrantRequest struct {
	Who      authz.Context
	UploadID ids.ID
	Source   ids.PermanentRef
	Path     string
	// Purpose 是新版本使用这份内容的用途，按来源的许可与限制判定。
	Purpose authz.Purpose
}

// GrantFromSource 签发 authorized_source 复用授权，使本操作不必重传未改动
// 的文件。来源须为已提交版本，调用者对来源项目有当前的读取权限，且限制允许
// 所声明的用途；同一来源、同一操作重复请求返回同一授权。相同字节来自不同
// 来源时各自独立判定，不合并许可。
func (s *Service) GrantFromSource(ctx context.Context, req SourceGrantRequest) (BlobGrant, error) {
	if !validPurpose(req.Purpose) {
		return BlobGrant{}, invalid("purpose %q is not a registered use", req.Purpose)
	}
	if req.Source.InstanceID != "" && req.Source.InstanceID != s.instance {
		return BlobGrant{}, errcode.New(errcode.NotFound, "")
	}
	lctx, release, err := s.write(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return BlobGrant{}, err
	}
	defer release()
	ctx = lctx
	u, err := s.openUpload(ctx, req.Who, req.UploadID)
	if err != nil {
		return BlobGrant{}, err
	}
	file, v, err := s.readableFile(ctx, req.Who, req.Source.AssetID, req.Source.VersionID, req.Path)
	if err != nil {
		return BlobGrant{}, err
	}
	ref := ids.PermanentRef{InstanceID: s.instance, AssetID: v.AssetID, VersionID: v.VersionID}
	d, err := s.rights.EvaluateUse(ctx, req.Who, ref, req.Purpose)
	if err != nil {
		return BlobGrant{}, err
	}
	if err := d.Err(); err != nil {
		return BlobGrant{}, err
	}
	now := s.now()
	grantID, err := s.newID()
	if err != nil {
		return BlobGrant{}, err
	}
	expires := min64Time(now.Add(s.cfg.SourceGrantTTL), u.ExpiresAt)
	err = s.inTx(lctx, func(tx *sql.Tx) error {
		if err := s.stillOpen(lctx, tx, u.UploadID, now); err != nil {
			return err
		}
		res, err := tx.ExecContext(lctx, `INSERT INTO storage_blob_grants (grant_id, principal_id, session_id, project_id,
			operation_id, sha256, size, basis, source_project_id, source_asset_id, source_version_id, source_path, purpose,
			issued_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'authorized_source', ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			grantID, req.Who.PrincipalID, req.Who.SessionID, u.ProjectID, u.OperationID, file.SHA256, file.Size,
			v.ProjectID, v.AssetID, v.VersionID, file.Path, req.Purpose, clock.Millis(now), clock.Millis(expires))
		if err != nil {
			return err
		}
		created, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if created > 0 {
			ev, err := s.event(EvBlobGrantIssued, "blob_grant", grantID, 1, req.Who, u.ProjectID, u.OperationID, map[string]any{
				"grant_id": grantID, "basis": BasisAuthorizedSource, "sha256": file.SHA256, "size": file.Size,
				"source_asset_id": v.AssetID, "source_version_id": v.VersionID, "purpose": req.Purpose,
			})
			if err != nil {
				return err
			}
			if err := s.store.AppendEvents(lctx, tx, u.OperationID, []event.Envelope{ev}); err != nil {
				return err
			}
		}
		// 重放也重新验证来源与用途；未消费的旧授权可在当前有效授权下续期。
		_, err = tx.ExecContext(lctx, `UPDATE storage_blob_grants SET expires_at = MAX(expires_at, ?) WHERE operation_id = ?
			AND sha256 = ? AND basis = 'authorized_source' AND source_version_id = ? AND source_path = ? AND purpose = ?
			AND revoked_at IS NULL AND consumed_at IS NULL`, clock.Millis(expires), u.OperationID, file.SHA256, v.VersionID, file.Path, req.Purpose)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(lctx, `INSERT OR IGNORE INTO storage_pin_blobs(pin_id,sha256)
			SELECT pin_id,? FROM storage_pins WHERE owner_kind='upload_session' AND owner_id=?`, file.SHA256, u.UploadID); err != nil {
			return err
		}
		return s.touch(lctx, tx, u.UploadID, s.now())
	})
	if err != nil {
		return BlobGrant{}, err
	}
	g, err := scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM storage_blob_grants WHERE operation_id = ?
		AND sha256 = ? AND basis = 'authorized_source' AND source_version_id = ? AND source_path = ? AND purpose = ? AND revoked_at IS NULL`,
		u.OperationID, file.SHA256, v.VersionID, file.Path, req.Purpose))
	if errors.Is(err, sql.ErrNoRows) {
		// 同一来源的授权已被撤销（例如会话关闭）：不重新签发。
		return BlobGrant{}, errcode.New(errcode.BlobGrantRequired, "")
	}
	if g.Source != nil {
		g.Source.InstanceID = s.instance
	}
	return g, err
}

// VersionFile 是已安装版本中的一个文件。
type VersionFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// readableFile 解析调用者当前有权读取的已提交版本中的文件：先按资产所在项目
// 授权（无权时不泄露版本或文件是否存在），再核对台账已提交该版本、安装记录
// 与台账的清单摘要一致。
func (s *Service) readableFile(ctx context.Context, who authz.Context, assetID, versionID ids.ID, path string) (VersionFile, commit.Committed, error) {
	if !assetID.Valid() || !versionID.Valid() {
		return VersionFile{}, commit.Committed{}, errcode.New(errcode.NotFound, "")
	}
	asset, err := s.ledger.Asset(ctx, assetID)
	if err != nil {
		return VersionFile{}, commit.Committed{}, err
	}
	if _, err := s.authorize(ctx, who, ActionReadContent, authz.Resource{ProjectID: asset.ProjectID, Kind: "asset", ID: assetID}); err != nil {
		return VersionFile{}, commit.Committed{}, err
	}
	v, err := s.ledger.Version(ctx, assetID, versionID)
	if err != nil {
		return VersionFile{}, commit.Committed{}, err
	}
	if controls, ok := s.ledger.(commit.Controls); ok {
		if err := controls.CheckVersionRead(ctx, assetID, versionID); err != nil {
			return VersionFile{}, commit.Committed{}, err
		}
	}
	norm, err := pathrule.Normalize(path)
	if err != nil {
		return VersionFile{}, commit.Committed{}, err
	}
	if err := s.checkInstalledFor(ctx, v); err != nil {
		return VersionFile{}, commit.Committed{}, err
	}
	var f VersionFile
	err = s.db.QueryRowContext(ctx, `SELECT path, sha256, size FROM storage_version_files WHERE version_id = ? AND path = ?`,
		versionID, norm).Scan(&f.Path, &f.SHA256, &f.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return VersionFile{}, commit.Committed{}, errcode.New(errcode.NotFound, "the version has no file at this path")
	}
	return f, v, err
}

// checkInstalledFor 核对已提交版本的安装记录与台账一致；不一致说明文件端
// 与台账需要对账，停止向下游交付。
func (s *Service) checkInstalledFor(ctx context.Context, v commit.Committed) error {
	var state string
	var manifest sql.NullString
	var op ids.ID
	err := s.db.QueryRowContext(ctx, `SELECT operation_id, state, manifest_digest FROM storage_installs WHERE version_id = ?`, v.VersionID).
		Scan(&op, &state, &manifest)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (state != "installed" || op != v.OperationID || manifest.String != string(v.ManifestDigest))) {
		return errcode.New(errcode.OperationNeedsReconciliation, "the committed version's files are not in a consistent installed state").
			WithDetails(errcode.Detail{Reason: "install_mismatch"})
	}
	return err
}

// VersionFiles 返回调用者有权读取的已提交版本的文件清单（按路径字节序）。
func (s *Service) VersionFiles(ctx context.Context, who authz.Context, assetID, versionID ids.ID) ([]VersionFile, error) {
	if !assetID.Valid() || !versionID.Valid() {
		return nil, errcode.New(errcode.NotFound, "")
	}
	ctx, release, err := s.write(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return nil, err
	}
	defer release()
	asset, err := s.ledger.Asset(ctx, assetID)
	if err != nil {
		return nil, err
	}
	if _, err := s.authorize(ctx, who, ActionReadContent, authz.Resource{ProjectID: asset.ProjectID, Kind: "asset", ID: assetID}); err != nil {
		return nil, err
	}
	v, err := s.ledger.Version(ctx, assetID, versionID)
	if err != nil {
		return nil, err
	}
	if controls, ok := s.ledger.(commit.Controls); ok {
		if err = controls.CheckVersionRead(ctx, assetID, versionID); err != nil {
			return nil, err
		}
	}
	decision, err := s.rights.EvaluateUse(ctx, who, v.Ref(s.instance), authz.PurposeArchiveReview)
	if err != nil {
		return nil, err
	}
	if err = decision.Err(); err != nil {
		return nil, err
	}
	if err := s.checkInstalledFor(ctx, v); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path, sha256, size FROM storage_version_files WHERE version_id = ? ORDER BY path`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VersionFile
	for rows.Next() {
		var f VersionFile
		if err := rows.Scan(&f.Path, &f.SHA256, &f.Size); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
