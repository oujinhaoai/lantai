package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// ReadRequest 申请读取已提交版本中的一个文件。
type ReadRequest struct {
	Who       authz.Context
	AssetID   ids.ID
	VersionID ids.ID
	Path      string
	Purpose   authz.Purpose
}

// ReadGrant 是读取授权与传输描述。URL 是传输面的相对地址（由服务端给出，
// 客户端不拼接内部监听地址）；下载时仍须带本人的会话凭据，单凭 URL 不能取件。
type ReadGrant struct {
	GrantID   ids.ID        `json:"grant_id"`
	ProjectID ids.ID        `json:"project_id"`
	AssetID   ids.ID        `json:"asset_id"`
	VersionID ids.ID        `json:"version_id"`
	Path      string        `json:"path"`
	SHA256    string        `json:"sha256"`
	Size      int64         `json:"size"`
	Purpose   authz.Purpose `json:"purpose"`
	IssuedAt  time.Time     `json:"issued_at"`
	ExpiresAt time.Time     `json:"expires_at"`
	Method    string        `json:"method"`
	URL       string        `json:"url"`
	// Range 表示支持按字节范围续传。
	Range bool `json:"range"`
}

// ReadPathPrefix 是读取授权在传输面上的路径前缀。
const ReadPathPrefix = "/xfer/v1/reads/"

type readRow struct {
	ReadGrant
	PrincipalID ids.ID
	SessionID   ids.ID
	KeyID       string
	AuthEpoch   int64
	RightsEpoch int64
	RevokedAt   time.Time
}

// signature 对读取授权的全部身份与资源绑定字段计算 HMAC-SHA256。签名只是
// 纵深防御（防止猜测授权 ID）；授权以 runtime.db 中的记录为准，每次请求另行
// 复核当前权限。
func (s *Service) signature(r readRow) string {
	m := hmac.New(sha256.New, s.key)
	fields := []string{"lantai.read-grant/v1", string(r.GrantID), string(r.PrincipalID), string(r.SessionID),
		string(r.ProjectID), string(r.AssetID), string(r.VersionID), r.Path, r.SHA256, fmt.Sprint(r.Size), r.Method,
		string(r.Purpose), fmt.Sprint(clock.Millis(r.ExpiresAt)), r.KeyID, fmt.Sprint(r.AuthEpoch), fmt.Sprint(r.RightsEpoch)}
	for _, f := range fields {
		m.Write([]byte(f))
		m.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// IssueReadGrant 为已提交版本中的文件签发读取授权：按资产所在项目的当前读取
// 权限（无权时不泄露版本或文件是否存在）、台账已提交状态与用途限制判定，
// 绑定调用者本人会话，默认 15 分钟且不超过会话有效期。
func (s *Service) IssueReadGrant(ctx context.Context, req ReadRequest) (ReadGrant, error) {
	if err := req.Who.Validate(); err != nil {
		return ReadGrant{}, err
	}
	if !validPurpose(req.Purpose) {
		return ReadGrant{}, invalid("purpose %q is not a registered use", req.Purpose)
	}
	lctx, release, err := s.write(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return ReadGrant{}, err
	}
	defer release()
	ctx = lctx
	file, v, err := s.readableFile(ctx, req.Who, req.AssetID, req.VersionID, req.Path)
	if err != nil {
		return ReadGrant{}, err
	}
	ref := ids.PermanentRef{InstanceID: s.instance, AssetID: v.AssetID, VersionID: v.VersionID}
	d, err := s.rights.EvaluateUse(ctx, req.Who, ref, req.Purpose)
	if err != nil {
		return ReadGrant{}, err
	}
	if err := d.Err(); err != nil {
		return ReadGrant{}, err
	}
	now := s.now()
	expires := now.Add(s.cfg.ReadGrantTTL)
	if session := clock.Truncate(req.Who.ExpiresAt); session.Before(expires) {
		expires = session
	}
	if !expires.After(now) {
		return ReadGrant{}, errcode.New(errcode.TokenExpired, "")
	}
	grantID, err := s.newID()
	if err != nil {
		return ReadGrant{}, err
	}
	r := readRow{
		ReadGrant: ReadGrant{GrantID: grantID, ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID,
			Path: file.Path, SHA256: file.SHA256, Size: file.Size, Purpose: req.Purpose, IssuedAt: now, ExpiresAt: expires,
			Method: "GET", Range: true},
		PrincipalID: req.Who.PrincipalID, SessionID: req.Who.SessionID, KeyID: s.keyID,
		AuthEpoch: req.Who.AuthEpoch, RightsEpoch: d.RightsEpoch,
	}
	_, err = s.db.ExecContext(lctx, `INSERT INTO storage_read_grants (grant_id, principal_id, session_id, project_id, asset_id,
		version_id, path, sha256, size, method, purpose, issued_at, expires_at, signing_key_id, auth_epoch, rights_epoch)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'GET', ?, ?, ?, ?, ?, ?)`,
		r.GrantID, r.PrincipalID, r.SessionID, r.ProjectID, r.AssetID, r.VersionID, r.Path, r.SHA256, r.Size, r.Purpose,
		clock.Millis(r.IssuedAt), clock.Millis(r.ExpiresAt), r.KeyID, r.AuthEpoch, r.RightsEpoch)
	if err != nil {
		return ReadGrant{}, err
	}
	g := r.ReadGrant
	g.URL = ReadPathPrefix + string(g.GrantID) + "?sig=" + s.signature(r)
	return g, nil
}

func (s *Service) loadRead(ctx context.Context, id ids.ID) (*readRow, error) {
	var r readRow
	var issued, expires int64
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT grant_id, principal_id, session_id, project_id, asset_id, version_id, path, sha256,
		size, method, purpose, issued_at, expires_at, signing_key_id, auth_epoch, rights_epoch, revoked_at
		FROM storage_read_grants WHERE grant_id = ?`, id).
		Scan(&r.GrantID, &r.PrincipalID, &r.SessionID, &r.ProjectID, &r.AssetID, &r.VersionID, &r.Path, &r.SHA256, &r.Size,
			&r.Method, &r.Purpose, &issued, &expires, &r.KeyID, &r.AuthEpoch, &r.RightsEpoch, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return nil, err
	}
	r.IssuedAt, r.ExpiresAt, r.Range = clock.FromMillis(issued), clock.FromMillis(expires), true
	if revoked.Valid {
		r.RevokedAt = clock.FromMillis(revoked.Int64)
	}
	return &r, nil
}

// ReadHandle 是一次已通过全部检查的读取：调用方在锁外流式发送并关闭 File。
type ReadHandle struct {
	Grant ReadGrant
	File  *os.File
	Size  int64
}

// Close 关闭文件。
func (h *ReadHandle) Close() error { return h.File.Close() }

// OpenRead 是每个新的 GET/Range 请求的检查：签名、授权记录（未撤销、未到期）、
// 本人会话与主体一致，然后在最终读取检查（security_guard 读锁）内按当前权限、
// 台账已提交状态与用途限制复核并打开原件。转发来的 URL、别人的授权或只知道
// 哈希都不能取件；撤权返回后开始的请求一律拒绝，已在发送的响应可能完成。
func (s *Service) OpenRead(ctx context.Context, who authz.Context, grantID ids.ID, sig string) (*ReadHandle, error) {
	if !grantID.Valid() {
		return nil, errcode.New(errcode.NotFound, "")
	}
	r, err := s.loadRead(ctx, grantID)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(sig), []byte(s.signature(*r))) || r.SessionID != who.SessionID || r.PrincipalID != who.PrincipalID {
		// 签名不符、转发的地址或别人的授权：与不存在的授权一样处理。
		return nil, errcode.New(errcode.NotFound, "")
	}
	switch now := s.now(); {
	case !r.RevokedAt.IsZero():
		return nil, errcode.New(errcode.TokenRevoked, "the read grant was revoked")
	case !now.Before(r.ExpiresAt):
		return nil, errcode.New(errcode.TokenExpired, "the read grant has expired; request a new one")
	}
	var h *ReadHandle
	res := authz.Resource{ProjectID: r.ProjectID, Kind: "version", ID: r.VersionID}
	err = s.reads.BeginRead(ctx, who, ActionReadContent, res, func(ctx context.Context, _ authz.Decision) error {
		// 等待最终读取锁期间可能已撤销或到期，必须在与撤销互斥的边界重读。
		current, err := s.loadRead(ctx, grantID)
		if err != nil {
			return err
		}
		if !current.RevokedAt.IsZero() {
			return errcode.New(errcode.TokenRevoked, "the read grant was revoked")
		}
		if !s.now().Before(current.ExpiresAt) {
			return errcode.New(errcode.TokenExpired, "the read grant has expired; request a new one")
		}
		v, err := s.ledger.Version(ctx, r.AssetID, r.VersionID)
		if err != nil {
			return err
		}
		if err := s.checkInstalledFor(ctx, v); err != nil {
			return err
		}
		ref := ids.PermanentRef{InstanceID: s.instance, AssetID: r.AssetID, VersionID: r.VersionID}
		d, err := s.rights.EvaluateUse(ctx, who, ref, r.Purpose)
		if err != nil {
			return err
		}
		if err := d.Err(); err != nil {
			return err
		}
		f, err := os.Open(s.layout.BlobPath(r.SHA256))
		if errors.Is(err, fs.ErrNotExist) {
			return errcode.New(errcode.OperationNeedsReconciliation, "stored content is missing")
		}
		if err != nil {
			return fileop.Wrap("opening stored content", err)
		}
		st, err := f.Stat()
		if err != nil || st.Size() != r.Size {
			f.Close()
			return errcode.New(errcode.OperationNeedsReconciliation, "stored content does not match its recorded size")
		}
		h = &ReadHandle{Grant: r.ReadGrant, File: f, Size: r.Size}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}

// RevokeReadGrant 由持有者撤销自己的读取授权；之后的新请求立即拒绝。
func (s *Service) RevokeReadGrant(ctx context.Context, who authz.Context, grantID ids.ID) error {
	r, err := s.loadRead(ctx, grantID)
	if err != nil {
		return err
	}
	if r.PrincipalID != who.PrincipalID {
		return errcode.New(errcode.NotFound, "")
	}
	if _, err := s.authorize(ctx, who, ActionReadContent, authz.Resource{ProjectID: r.ProjectID, Kind: "version", ID: r.VersionID}); err != nil {
		return err
	}
	return s.revokeReads(ctx, `grant_id = ?`, grantID, "revoked_by_holder")
}

// RevokeSessionReads 撤销某个会话的全部读取授权（会话结束或被吊销时调用）。
// 即使不调用，会话失效后的请求也会在逐请求检查中被拒绝。
func (s *Service) RevokeSessionReads(ctx context.Context, sessionID ids.ID, reason string) error {
	return s.revokeReads(ctx, `session_id = ?`, sessionID, reason)
}

func (s *Service) revokeReads(ctx context.Context, where string, arg any, reason string) error {
	if !strings.HasPrefix(where, "grant_id") && !strings.HasPrefix(where, "session_id") {
		return errors.New("storage: unsupported revocation filter")
	}
	lctx, release, err := s.write(ctx, commands.Request{Security: commands.ModeExclusive})
	if err != nil {
		return err
	}
	defer release()
	_, err = s.db.ExecContext(lctx, `UPDATE storage_read_grants SET revoked_at = ?, revoke_reason = ? WHERE `+where+` AND revoked_at IS NULL`,
		clock.Millis(s.now()), reason, arg)
	return err
}
