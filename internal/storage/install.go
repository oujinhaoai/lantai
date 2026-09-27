package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

var _ install.Installer = (*Service)(nil)

// 版本目录中的文件名。
const (
	ManifestFile  = "manifest.yaml"
	FilesDir      = "files"
	installMarker = ".install.json"
	quarantineTag = ".quarantine.json"
)

// markerContract 是版本目录中安装标记的格式标识；标记记录目录属于哪个操作，
// 用于崩溃后核对，不是提交证明。
const markerContract = "lantai.install-marker/v1"

type marker struct {
	Contract      string        `json:"contract"`
	OperationID   ids.ID        `json:"operation_id"`
	RequestDigest digest.Digest `json:"request_digest"`
	InstalledAt   string        `json:"installed_at"`
}

type quarantineMarker struct {
	OperationID   ids.ID       `json:"operation_id"`
	Reason        errcode.Code `json:"reason"`
	QuarantinedAt string       `json:"quarantined_at"`
}

type installRow struct {
	OperationID      ids.ID
	State            string
	ProjectID        ids.ID
	AssetID          ids.ID
	VersionID        ids.ID
	VersionNumber    int64
	ManifestDigest   digest.Digest
	ManifestSHA256   string
	RequestDigest    digest.Digest
	InstallRef       string
	LinkMode         string
	InstalledAt      time.Time
	QuarantineReason errcode.Code
}

func (s *Service) installRow(ctx context.Context, q commands.DBTX, op ids.ID) (*installRow, error) {
	var r installRow
	var project, asset, version, manifest, manifestSHA, reqDigest, ref, mode, reason sql.NullString
	var number, installed sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT operation_id, state, project_id, asset_id, version_id, version_number, manifest_digest,
		manifest_sha256, request_digest, install_ref, link_mode, installed_at, quarantine_reason
		FROM storage_installs WHERE operation_id = ?`, op).
		Scan(&r.OperationID, &r.State, &project, &asset, &version, &number, &manifest, &manifestSHA, &reqDigest, &ref, &mode,
			&installed, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.ProjectID, r.AssetID, r.VersionID = ids.ID(project.String), ids.ID(asset.String), ids.ID(version.String)
	r.VersionNumber, r.ManifestDigest, r.ManifestSHA256 = number.Int64, digest.Digest(manifest.String), manifestSHA.String
	r.RequestDigest, r.InstallRef, r.LinkMode = digest.Digest(reqDigest.String), ref.String, mode.String
	if installed.Valid {
		r.InstalledAt = clock.FromMillis(installed.Int64)
	}
	r.QuarantineReason = errcode.Code(reason.String)
	return &r, nil
}

func (s *Service) filesOf(ctx context.Context, q commands.DBTX, version ids.ID) ([]install.File, error) {
	rows, err := q.QueryContext(ctx, `SELECT path, sha256, size FROM storage_version_files WHERE version_id = ? ORDER BY path`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []install.File
	for rows.Next() {
		var f install.File
		if err := rows.Scan(&f.Path, &f.SHA256, &f.Size); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Service) proofOf(ctx context.Context, r *installRow) (install.Proof, error) {
	files, err := s.filesOf(ctx, s.db, r.VersionID)
	if err != nil {
		return install.Proof{}, err
	}
	return install.Proof{
		OperationID: r.OperationID, ProjectID: r.ProjectID, AssetID: r.AssetID, VersionID: r.VersionID,
		VersionNumber: r.VersionNumber, ManifestDigest: r.ManifestDigest, Files: files, ManifestSHA256: r.ManifestSHA256,
		InstallRef: r.InstallRef, InstalledAt: r.InstalledAt,
	}, nil
}

// requestDigest 是安装请求的规范摘要，用于判定同一 operation 的重入是否为
// 同一请求（含清单文件内容）。
func requestDigest(req install.Request, manifestSHA string) (digest.Digest, error) {
	files := req.Files
	if files == nil {
		files = []install.File{}
	}
	raw, err := canonjson.CanonicalizeValue(map[string]any{
		"contract": "lantai.install-request/v1", "operation_id": req.OperationID, "project_id": req.ProjectID,
		"asset_id": req.AssetID, "version_id": req.VersionID, "version_number": req.VersionNumber,
		"manifest_digest": req.ManifestDigest, "manifest_sha256": manifestSHA, "files": files,
	})
	if err != nil {
		return "", err
	}
	return digest.Of(raw), nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (s *Service) validateInstall(req install.Request) error {
	if err := req.Validate(); err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "invalid install request", err)
	}
	paths := make([]string, len(req.Files))
	for i, f := range req.Files {
		if err := pathrule.Check(f.Path); err != nil {
			return err
		}
		paths[i] = f.Path
	}
	if err := pathrule.CheckSet(paths); err != nil {
		return err
	}
	switch {
	case len(req.Manifest) == 0:
		return invalid("the manifest file is required")
	case len(req.Manifest) > s.cfg.MaxManifestBytes:
		return reasonErr(errcode.QuotaExceeded, "manifest_too_large", "the manifest file exceeds the configured limit",
			map[string]any{"limit": s.cfg.MaxManifestBytes})
	}
	return nil
}

// Install 实现 install.Installer：在私有安装区组装版本目录（文件为内容库原件的
// 硬链接或经核验的副本，外加清单文件与安装标记），刷盘后整体改名到版本目录，
// 再在 runtime.db 一个事务中记录安装、版本文件清单并消费本操作的复用授权。
//
// 按 operation 幂等：同一请求重入返回同一证明，崩溃后重入会接管已落位且标记
// 相符的目录，不会产生第二个版本目录；同一 operation 的不同请求返回
// IDEMPOTENCY_CONFLICT，已隔离的操作不能再安装。版本目录出现不等于提交：
// 读取一律先经台账确认 committed。
func (s *Service) Install(ctx context.Context, req install.Request) (install.Proof, error) {
	if err := s.validateInstall(req); err != nil {
		return install.Proof{}, err
	}
	manifestSHA := sha256Hex(req.Manifest)
	rd, err := requestDigest(req, manifestSHA)
	if err != nil {
		return install.Proof{}, err
	}
	unlock := s.lockKey("install/" + string(req.OperationID))
	defer unlock()
	if p, done, err := s.existingInstall(ctx, req.OperationID, rd); done || err != nil {
		return p, err
	}
	final := s.layout.VersionDir(req.ProjectID, req.AssetID, req.VersionNumber)
	unlockVersion := s.lockKey("version/" + s.layout.ref(final))
	defer unlockVersion()
	var owner ids.ID
	err = s.db.QueryRowContext(ctx, `SELECT operation_id FROM storage_installs
		WHERE version_id = ? OR (asset_id = ? AND version_number = ?) LIMIT 1`,
		req.VersionID, req.AssetID, req.VersionNumber).Scan(&owner)
	if err == nil {
		return install.Proof{}, reasonErr(errcode.OperationNeedsReconciliation, "version_already_installed",
			"this version is already bound to another install operation", nil)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return install.Proof{}, err
	}
	if err := s.checkGrants(ctx, s.db, req, s.now()); err != nil {
		return install.Proof{}, err
	}
	for _, f := range req.Files {
		size, exists, err := s.blobSize(f.SHA256)
		if err != nil {
			return install.Proof{}, err
		}
		if !exists || size != f.Size {
			return install.Proof{}, errcode.New(errcode.OperationNeedsReconciliation, "granted content is missing from the content store").
				WithDetails(errcode.Detail{Reason: "blob_missing", Data: map[string]any{"sha256": f.SHA256}})
		}
	}
	// 私有安装区也属于实例写入，组装到记录全程受维护屏障保护；不取
	// security_guard，复制与哈希期间撤权仍能完成，台账接受时复验当前授权。
	lctx, release, err := s.write(ctx, commands.Request{})
	if err != nil {
		return install.Proof{}, err
	}
	defer release()
	installedAt, mode, adopted, err := s.adopt(final, req, rd)
	if err != nil {
		return install.Proof{}, err
	}
	if !adopted {
		// 私有组装、最终落位与安装记录都在同一次维护屏障内。
		installedAt = s.now()
		if mode, err = s.assemble(req, rd, installedAt); err != nil {
			return install.Proof{}, err
		}
	}
	if !adopted {
		if err := s.place(req.OperationID, final); err != nil {
			return install.Proof{}, err
		}
	} else if err := s.syncPlacement(req.OperationID, final); err != nil {
		// 上次改名后可能尚未完成刷盘；只有补齐持久化后才可以记录安装。
		return install.Proof{}, err
	}
	return s.recordInstall(lctx, req, rd, manifestSHA, final, mode, installedAt)
}

// existingInstall 按 operation 处理重入：已安装且请求相同返回原证明。
func (s *Service) existingInstall(ctx context.Context, op ids.ID, rd digest.Digest) (install.Proof, bool, error) {
	row, err := s.installRow(ctx, s.db, op)
	switch {
	case err != nil:
		return install.Proof{}, true, err
	case row == nil:
		return install.Proof{}, false, nil
	case row.State == "quarantined":
		return install.Proof{}, true, errcode.New(errcode.OperationNeedsReconciliation, "the operation was quarantined and cannot install again").
			WithOperation(string(op))
	case row.RequestDigest != rd:
		return install.Proof{}, true, errcode.New(errcode.IdempotencyConflict, "the operation already installed a different request").
			WithOperation(string(op))
	}
	p, err := s.proofOf(ctx, row)
	return p, true, err
}

// checkGrants 要求每个文件都有本操作的有效复用授权；授权记录的大小与清单
// 不符时返回 HASH_MISMATCH。没有授权时不区分内容是否存在。
func (s *Service) checkGrants(ctx context.Context, q commands.DBTX, req install.Request, now time.Time) error {
	for _, f := range req.Files {
		size, ok, err := s.grantedSize(ctx, q, req.ProjectID, req.OperationID, f.SHA256, now)
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.BlobGrantRequired, "").WithDetails(errcode.Detail{Reason: "grant_missing", Data: map[string]any{"path": f.Path}})
		}
		if size != f.Size {
			return reasonErr(errcode.HashMismatch, "size_mismatch", "the manifest size does not match the uploaded content",
				map[string]any{"path": f.Path})
		}
	}
	return nil
}

// adopt 接管崩溃前已经落位的版本目录：标记属于同一操作与同一请求时，浅层核验
// 后沿用；目录属于别的操作或没有标记时拒绝，不覆盖、不补记。
func (s *Service) adopt(final string, req install.Request, rd digest.Digest) (time.Time, fileop.Mode, bool, error) {
	raw, err := os.ReadFile(filepath.Join(final, installMarker))
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Stat(final); errors.Is(statErr, fs.ErrNotExist) {
			return time.Time{}, "", false, nil
		}
		return time.Time{}, "", false, errcode.New(errcode.OperationNeedsReconciliation, "the version directory exists without an install marker").
			WithDetails(errcode.Detail{Reason: "version_dir_occupied"})
	}
	if err != nil {
		return time.Time{}, "", false, fileop.Wrap("reading an install marker", err)
	}
	var m marker
	if err := json.Unmarshal(raw, &m); err != nil || m.Contract != markerContract || m.OperationID != req.OperationID || m.RequestDigest != rd {
		return time.Time{}, "", false, errcode.New(errcode.OperationNeedsReconciliation, "the version directory belongs to another operation").
			WithDetails(errcode.Detail{Reason: "version_dir_occupied"})
	}
	at, err := clock.Parse(m.InstalledAt)
	if err != nil {
		return time.Time{}, "", false, errcode.New(errcode.OperationNeedsReconciliation, "the install marker is corrupt")
	}
	mode, err := s.checkTree(final, req.Files, sha256Hex(req.Manifest))
	if err != nil {
		return time.Time{}, "", false, err
	}
	return at, mode, true, nil
}

// assemble 在私有安装区组装版本目录：files/ 下为原件的硬链接，文件系统不支持
// 硬链接或跨卷时，先核对可用空间再复制并重新核验；写入清单文件与安装标记后
// 刷新目录。落位由 place 在维护屏障内完成。
func (s *Service) assemble(req install.Request, rd digest.Digest, installedAt time.Time) (fileop.Mode, error) {
	stage := s.layout.installStaging(req.OperationID)
	// 上次崩溃留下的私有安装区没有被记录，也从未对外可见，直接重建。
	if err := os.RemoveAll(stage); err != nil {
		// Windows 上删除只读硬链接会先清掉整份文件的只读属性：失败时恢复。
		for _, f := range req.Files {
			s.sealBlob(f.SHA256)
		}
		return "", fileop.Wrap("clearing the install area", err)
	}
	if err := s.checkSpace(int64(len(req.Manifest))); err != nil {
		return "", err
	}
	if err := s.fs.MkdirAll(filepath.Join(stage, FilesDir)); err != nil {
		return "", err
	}
	var modes []fileop.Mode
	dirs := map[string]bool{filepath.Join(stage, FilesDir): true, stage: true}
	var remaining int64
	for _, f := range req.Files {
		remaining += f.Size
	}
	spaceChecked := false
	for _, f := range req.Files {
		dst := filepath.Join(stage, FilesDir, filepath.FromSlash(f.Path))
		if err := s.fs.MkdirAll(filepath.Dir(dst)); err != nil {
			return "", err
		}
		dirs[filepath.Dir(dst)] = true
		mode := fileop.ModeHardlink
		err := s.fs.Link(s.layout.BlobPath(f.SHA256), dst)
		if errors.Is(err, fileop.ErrLinkUnsupported) {
			if !spaceChecked {
				// 需要复制：先确认剩余内容放得下，不让复制进行到一半才失败。
				if err := s.checkSpace(remaining); err != nil {
					return "", err
				}
				spaceChecked = true
			}
			mode, err = fileop.ModeCopy, s.fs.CopyVerified(s.layout.BlobPath(f.SHA256), dst, f.SHA256, f.Size)
		}
		if err != nil {
			return "", err
		}
		modes = append(modes, mode)
		remaining -= f.Size
	}
	if _, err := s.fs.CreateOrMatch(filepath.Join(stage, ManifestFile), req.Manifest, 0o444); err != nil {
		return "", err
	}
	mk, err := json.Marshal(marker{Contract: markerContract, OperationID: req.OperationID, RequestDigest: rd, InstalledAt: clock.Format(installedAt)})
	if err != nil {
		return "", err
	}
	if _, err := s.fs.CreateOrMatch(filepath.Join(stage, installMarker), mk, 0o444); err != nil {
		return "", err
	}
	for d := range dirs {
		if err := s.fs.SyncDir(d); err != nil {
			return "", err
		}
	}
	return linkMode(modes), nil
}

// place 把组装好的私有安装区整体改名到版本目录并刷新上级目录。调用方持有
// 维护屏障。
func (s *Service) place(op ids.ID, final string) error {
	if err := s.fs.MkdirAll(filepath.Dir(final)); err != nil {
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		return errcode.New(errcode.OperationNeedsReconciliation, "the version directory appeared while installing").
			WithDetails(errcode.Detail{Reason: "version_dir_occupied"})
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fileop.Wrap("checking the version directory", err)
	}
	if err := s.fs.Rename(s.layout.installStaging(op), final); err != nil {
		return err
	}
	return s.syncPlacement(op, final)
}

func (s *Service) syncPlacement(op ids.ID, final string) error {
	if err := s.fs.SyncDir(filepath.Dir(final)); err != nil {
		return err
	}
	return s.fs.SyncDir(filepath.Dir(s.layout.installStaging(op)))
}

func linkMode(modes []fileop.Mode) fileop.Mode {
	switch {
	case len(modes) == 0, !slices.Contains(modes, fileop.ModeCopy):
		return fileop.ModeHardlink
	case !slices.Contains(modes, fileop.ModeHardlink):
		return fileop.ModeCopy
	}
	return "mixed"
}

// recordInstall 在 runtime.db 一个事务中记录安装与版本文件清单并消费授权。
// 调用方持有维护屏障（lctx）。
func (s *Service) recordInstall(lctx context.Context, req install.Request, rd digest.Digest, manifestSHA, final string,
	mode fileop.Mode, installedAt time.Time) (install.Proof, error) {
	ref := s.layout.ref(final)
	err := s.inTx(lctx, func(tx *sql.Tx) error {
		// 授权可能在组装期间被撤销（例如会话被取消）：在记录事务内复核。
		if err := s.checkGrants(lctx, tx, req, s.now()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(lctx, `INSERT INTO storage_installs (operation_id, state, project_id, asset_id, version_id,
			version_number, manifest_digest, manifest_sha256, request_digest, install_ref, link_mode, installed_at)
			VALUES (?, 'installed', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			req.OperationID, req.ProjectID, req.AssetID, req.VersionID, req.VersionNumber, req.ManifestDigest, manifestSHA,
			rd, ref, string(mode), clock.Millis(installedAt)); err != nil {
			return err
		}
		for _, f := range req.Files {
			if _, err := tx.ExecContext(lctx, `INSERT INTO storage_version_files (version_id, path, sha256, size) VALUES (?, ?, ?, ?)`,
				req.VersionID, f.Path, f.SHA256, f.Size); err != nil {
				return err
			}
			if _, err := tx.ExecContext(lctx, `UPDATE storage_blob_grants SET consumed_at = ? WHERE project_id = ? AND operation_id = ?
				AND sha256 = ? AND consumed_at IS NULL AND revoked_at IS NULL`, clock.Millis(s.now()), req.ProjectID, req.OperationID, f.SHA256); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return install.Proof{}, err
	}
	// Windows 删除只读硬链接（例如清理上次失败留下的安装区）时会清掉整份文件的
	// 只读属性；安装成功后重新设置。
	for _, f := range req.Files {
		s.sealBlob(f.SHA256)
	}
	row, err := s.installRow(lctx, s.db, req.OperationID)
	if err != nil {
		return install.Proof{}, err
	}
	return s.proofOf(lctx, row)
}

// checkTree 浅层核验版本目录：清单文件的 SHA-256、每个文件存在且大小相符。
// 返回文件的落位方式（与内容库原件是否为同一文件）。
func (s *Service) checkTree(dir string, files []install.File, manifestSHA string) (fileop.Mode, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errcode.New(errcode.HashMismatch, "the manifest file is missing")
		}
		return "", fileop.Wrap("reading the manifest file", err)
	}
	if sha256Hex(raw) != manifestSHA {
		return "", errcode.New(errcode.HashMismatch, "the manifest file no longer matches its hash")
	}
	var modes []fileop.Mode
	for _, f := range files {
		st, err := os.Stat(filepath.Join(dir, FilesDir, filepath.FromSlash(f.Path)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", errcode.New(errcode.HashMismatch, "an installed file is missing").
					WithDetails(errcode.Detail{Reason: "file_missing", Data: map[string]any{"path": f.Path}})
			}
			return "", fileop.Wrap("checking an installed file", err)
		}
		if st.Size() != f.Size || !st.Mode().IsRegular() {
			return "", errcode.New(errcode.HashMismatch, "an installed file no longer matches its size").
				WithDetails(errcode.Detail{Reason: "file_size", Data: map[string]any{"path": f.Path}})
		}
		mode := fileop.ModeCopy
		if blob, err := os.Stat(s.layout.BlobPath(f.SHA256)); err == nil && os.SameFile(st, blob) {
			mode = fileop.ModeHardlink
		}
		modes = append(modes, mode)
	}
	return linkMode(modes), nil
}

// Verify 实现 install.Installer：证明必须与本安装器为该操作签发的完全一致，
// 版本目录的清单文件哈希、文件存在与大小相符（浅层核验，适合在最终接受前
// 调用；逐字节复算用 VerifyDeep）。
func (s *Service) Verify(ctx context.Context, p install.Proof) error {
	row, err := s.installRow(ctx, s.db, p.OperationID)
	switch {
	case err != nil:
		return err
	case row == nil:
		return errcode.New(errcode.OperationNeedsReconciliation, "no install recorded for operation")
	case row.State != "installed":
		return errcode.New(errcode.OperationNeedsReconciliation, "the operation was quarantined")
	}
	issued, err := s.proofOf(ctx, row)
	if err != nil {
		return err
	}
	a, err := issued.Digest()
	if err != nil {
		return err
	}
	b, err := p.Digest()
	if err != nil || a != b {
		return errcode.New(errcode.OperationNeedsReconciliation, "proof was not issued for this install")
	}
	_, err = s.checkTree(s.layout.VersionDir(row.ProjectID, row.AssetID, row.VersionNumber), issued.Files, row.ManifestSHA256)
	return err
}

// VerifyDeep 逐字节复算已安装版本的全部文件与清单文件；用于恢复对账与 fsck，
// 不在最终接受边界的锁内调用。
func (s *Service) VerifyDeep(ctx context.Context, op ids.ID) error {
	row, err := s.installRow(ctx, s.db, op)
	if err != nil {
		return err
	}
	if row == nil || row.State != "installed" {
		return errcode.New(errcode.OperationNeedsReconciliation, "no install recorded for operation")
	}
	files, err := s.filesOf(ctx, s.db, row.VersionID)
	if err != nil {
		return err
	}
	dir := s.layout.VersionDir(row.ProjectID, row.AssetID, row.VersionNumber)
	if _, err := s.checkTree(dir, files, row.ManifestSHA256); err != nil {
		return err
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		got, n, err := fileop.HashFile(filepath.Join(dir, FilesDir, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		if got != f.SHA256 || n != f.Size {
			return errcode.New(errcode.HashMismatch, "an installed file no longer matches its hash").
				WithDetails(errcode.Detail{Reason: "file_digest", Data: map[string]any{"path": f.Path}})
		}
	}
	return nil
}

// Quarantine 实现 install.Installer：把不能提交的安装内容（版本目录或私有
// 安装区）整体移入隔离区并记录原因，保留字节供诊断，不删除。之后该操作不能
// 再安装，已签发的证明不再通过核验。重复调用无副作用。
func (s *Service) Quarantine(ctx context.Context, op ids.ID, reason errcode.Code) error {
	if !op.Valid() {
		return invalid("operation id is not valid")
	}
	if _, ok := errcode.Lookup(reason); !ok {
		return invalid("quarantine reason must be a registered error code")
	}
	unlock := s.lockKey("install/" + string(op))
	defer unlock()
	row, err := s.installRow(ctx, s.db, op)
	if err != nil {
		return err
	}
	if row != nil && row.State == "quarantined" {
		return nil
	}
	// 移动与记录都在维护屏障内：维护与备份期间目录不会凭空消失或出现。
	lctx, release, err := s.write(ctx, commands.Request{})
	if err != nil {
		return err
	}
	defer release()
	qdir := s.layout.quarantineDir(op)
	var src string
	if row != nil {
		src = s.layout.VersionDir(row.ProjectID, row.AssetID, row.VersionNumber)
	} else {
		// 也覆盖整体落位后、安装记录前崩溃的窗口；此时只剩版本目录，暂存区
		// 已消失。按标记找本操作的孤立目录，不凭目录存在补记安装成功。
		orphans, err := s.ScanOrphans(lctx)
		if err != nil {
			return err
		}
		for _, o := range orphans {
			if o.OperationID != op {
				continue
			}
			if src != "" {
				return reasonErr(errcode.OperationNeedsReconciliation, "multiple_install_locations",
					"the operation has more than one install location; preserve them for reconciliation", nil)
			}
			// Ref 是小写逻辑引用；ULID 目录名是大写，不能用 Ref 反推
			// 大小写敏感文件系统上的路径。
			src = o.directory
		}
	}
	now := s.now()
	if src != "" {
		if _, err := os.Lstat(src); err == nil {
			if _, err := os.Lstat(qdir); errors.Is(err, fs.ErrNotExist) {
				if err := s.fs.MkdirAll(filepath.Dir(qdir)); err != nil {
					return err
				}
				if err := s.fs.Rename(src, qdir); err != nil {
					return err
				}
				if err := s.fs.SyncDir(filepath.Dir(src)); err != nil {
					return err
				}
			} else if err != nil {
				return fileop.Wrap("checking the quarantine directory", err)
			} else {
				return reasonErr(errcode.OperationNeedsReconciliation, "quarantine_dir_occupied",
					"the quarantine directory is occupied while install content is still in place", nil)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fileop.Wrap("checking the install directory", err)
		}
	}
	if _, err := os.Lstat(qdir); err == nil {
		// 改名/标记之后、记账之前也可能失败。沿用已经保存的原因与时间，
		// 避免重试把磁盘证据与数据库状态写成互相矛盾的两份记录。
		tagPath := filepath.Join(qdir, quarantineTag)
		tag, err := os.ReadFile(tagPath)
		if err == nil {
			var qm quarantineMarker
			decodeErr := json.Unmarshal(tag, &qm)
			at, timeErr := clock.Parse(qm.QuarantinedAt)
			_, known := errcode.Lookup(qm.Reason)
			if decodeErr != nil || qm.OperationID != op || timeErr != nil || !known {
				return reasonErr(errcode.OperationNeedsReconciliation, "corrupt_quarantine_marker",
					"the quarantine marker does not identify this operation consistently", nil)
			}
			reason, now = qm.Reason, at
		} else if errors.Is(err, fs.ErrNotExist) {
			tag, _ = json.Marshal(quarantineMarker{OperationID: op, Reason: reason, QuarantinedAt: clock.Format(now)})
		} else {
			return fileop.Wrap("reading a quarantine marker", err)
		}
		if _, err := s.fs.CreateOrMatch(tagPath, tag, 0o444); err != nil {
			return err
		}
		if err := s.fs.SyncDir(qdir); err != nil {
			return err
		}
		if err := s.fs.SyncDir(filepath.Dir(qdir)); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fileop.Wrap("checking the quarantine directory", err)
	} else if row != nil {
		return reasonErr(errcode.OperationNeedsReconciliation, "install_missing",
			"the recorded install content is missing from both its version and quarantine directories", nil)
	}
	return s.inTx(lctx, func(tx *sql.Tx) error {
		qref := s.layout.ref(qdir)
		if row != nil {
			_, err := tx.ExecContext(lctx, `UPDATE storage_installs SET state = 'quarantined', quarantine_reason = ?, quarantine_ref = ?,
				quarantined_at = ? WHERE operation_id = ? AND state = 'installed'`, string(reason), qref, clock.Millis(now), op)
			return err
		}
		_, err := tx.ExecContext(lctx, `INSERT OR IGNORE INTO storage_installs (operation_id, state, quarantine_reason, quarantine_ref,
			quarantined_at) VALUES (?, 'quarantined', ?, ?, ?)`, op, string(reason), qref, clock.Millis(now))
		return err
	})
}

// InstallState 是一个操作的安装状态，供恢复对账使用。
type InstallState struct {
	OperationID      ids.ID       `json:"operation_id"`
	State            string       `json:"state"`
	VersionID        ids.ID       `json:"version_id,omitempty"`
	InstallRef       string       `json:"install_ref,omitempty"`
	QuarantineReason errcode.Code `json:"quarantine_reason,omitempty"`
}

// InstallStatus 返回操作的安装状态；从未安装时 State 为空。
func (s *Service) InstallStatus(ctx context.Context, op ids.ID) (InstallState, error) {
	row, err := s.installRow(ctx, s.db, op)
	if err != nil || row == nil {
		return InstallState{OperationID: op}, err
	}
	return InstallState{OperationID: op, State: row.State, VersionID: row.VersionID, InstallRef: row.InstallRef,
		QuarantineReason: row.QuarantineReason}, nil
}

// Orphan 是没有匹配安装记录的版本目录或私有安装区。
type Orphan struct {
	Ref         string `json:"ref"`
	OperationID ids.ID `json:"operation_id,omitempty"`
	Reason      string `json:"reason"`
	directory   string // 扫描所得原始路径，只用于模块内隔离，不对外输出。
}

// ScanOrphans 只读列出孤立的版本目录（没有安装标记、标记的操作没有安装记录
// 或记录不符）与残留的私有安装区。它不修改任何数据：是否隔离由台账按操作
// 证据决定，不能凭目录存在补记成功。
func (s *Service) ScanOrphans(ctx context.Context) ([]Orphan, error) {
	var out []Orphan
	projects, err := os.ReadDir(filepath.Join(s.layout.Home, "projects"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fileop.Wrap("listing projects", err)
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		assets, err := os.ReadDir(filepath.Join(s.layout.Home, "projects", p.Name(), "assets"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fileop.Wrap("listing assets", err)
		}
		for _, a := range assets {
			if !a.IsDir() {
				continue
			}
			versions, err := os.ReadDir(filepath.Join(s.layout.Home, "projects", p.Name(), "assets", a.Name(), "versions"))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fileop.Wrap("listing versions", err)
			}
			for _, v := range versions {
				dir := filepath.Join(s.layout.Home, "projects", p.Name(), "assets", a.Name(), "versions", v.Name())
				o, err := s.orphanCheck(ctx, dir)
				if err != nil {
					return nil, err
				}
				if o != nil {
					out = append(out, *o)
				}
			}
		}
	}
	stages, err := os.ReadDir(filepath.Join(s.layout.Home, "staging", "install"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fileop.Wrap("listing install staging", err)
	}
	for _, st := range stages {
		dir := filepath.Join(s.layout.Home, "staging", "install", st.Name())
		out = append(out, Orphan{Ref: s.layout.ref(dir), OperationID: ids.ID(st.Name()),
			Reason: "unplaced_install_staging", directory: dir})
	}
	return out, nil
}

func (s *Service) orphanCheck(ctx context.Context, dir string) (*Orphan, error) {
	ref := s.layout.ref(dir)
	raw, err := os.ReadFile(filepath.Join(dir, installMarker))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fileop.Wrap("reading an install marker", err)
		}
		return &Orphan{Ref: ref, Reason: "no_install_marker", directory: dir}, nil
	}
	var m marker
	if json.Unmarshal(raw, &m) != nil || m.Contract != markerContract || !m.OperationID.Valid() || !m.RequestDigest.Valid() {
		return &Orphan{Ref: ref, Reason: "corrupt_install_marker", directory: dir}, nil
	}
	row, err := s.installRow(ctx, s.db, m.OperationID)
	if err != nil {
		return nil, err
	}
	switch {
	case row == nil:
		return &Orphan{Ref: ref, OperationID: m.OperationID, Reason: "no_install_record", directory: dir}, nil
	case row.State != "installed" || row.InstallRef != ref || row.RequestDigest != m.RequestDigest:
		return &Orphan{Ref: ref, OperationID: m.OperationID, Reason: "install_record_mismatch", directory: dir}, nil
	}
	return nil, nil
}

// ReadManifest 返回已提交版本的清单文件内容：安装记录必须与台账一致，文件的
// SHA-256 必须与安装时记录的相同。本方法不做授权，调用方（catalog）负责。
func (s *Service) ReadManifest(ctx context.Context, v commit.Committed) ([]byte, error) {
	if err := s.checkInstalledFor(ctx, v); err != nil {
		return nil, err
	}
	row, err := s.installRow(ctx, s.db, v.OperationID)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(s.layout.VersionDir(row.ProjectID, row.AssetID, row.VersionNumber), ManifestFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errcode.New(errcode.OperationNeedsReconciliation, "the manifest file of a committed version is missing")
		}
		return nil, fileop.Wrap("reading the manifest file", err)
	}
	if sha256Hex(raw) != row.ManifestSHA256 {
		return nil, errcode.New(errcode.HashMismatch, "the manifest file no longer matches its hash")
	}
	return raw, nil
}
