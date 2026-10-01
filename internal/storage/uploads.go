package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// FileSpec 是申报要上传（或检查）的一份内容。
type FileSpec struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// UploadState 是上传会话状态。
type UploadState string

const (
	UploadOpen      UploadState = "open"
	UploadCompleted UploadState = "completed"
	UploadExpired   UploadState = "expired"
	UploadCancelled UploadState = "cancelled"
)

// FileState 是会话中一份内容的状态。
type FileState string

const (
	FilePending  FileState = "pending"
	FileVerified FileState = "verified"
	FileFailed   FileState = "failed"
)

// UploadFile 是会话中的一份内容及其分片进度。
type UploadFile struct {
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	PartSize  int64     `json:"part_size"`
	PartCount int       `json:"part_count"`
	State     FileState `json:"state"`
	// Received 是已写入并核验的分片号（升序），续传时只需补其余分片。
	Received []int `json:"received_parts"`
}

// Upload 是上传会话：一次提交操作的 receiving 阶段。OperationID 是之后提交
// 版本时使用的操作；会话签发的复用授权只对该操作有效。
type Upload struct {
	UploadID      ids.ID       `json:"upload_id"`
	OperationID   ids.ID       `json:"operation_id"`
	ProjectID     ids.ID       `json:"project_id"`
	PrincipalID   ids.ID       `json:"principal_id"`
	State         UploadState  `json:"state"`
	Files         []UploadFile `json:"files"`
	TotalBytes    int64        `json:"total_bytes"`
	CreatedAt     time.Time    `json:"created_at"`
	IdleExpiresAt time.Time    `json:"idle_expires_at"`
	ExpiresAt     time.Time    `json:"expires_at"`
	CloseReason   string       `json:"close_reason,omitempty"`
	// PartsURL 是写入分片的相对地址前缀：PUT {PartsURL}{sha256}/parts/{n}。
	PartsURL string `json:"parts_url"`
}

// ExpiresAt 与空闲到期中较早者。
func (u Upload) deadline() time.Time {
	if u.IdleExpiresAt.Before(u.ExpiresAt) {
		return u.IdleExpiresAt
	}
	return u.ExpiresAt
}

// CreateUploadRequest 是创建上传会话的请求。
type CreateUploadRequest struct {
	Who            authz.Context
	IdempotencyKey string
	ProjectID      ids.ID
	// Files 是要上传的内容（按哈希去重）；可为空，用于只复用已授权来源的提交。
	Files []FileSpec
}

func (s *Service) partLayout(size int64) (partSize int64, count int) {
	if size <= s.cfg.SinglePartMax {
		return max(size, 1), 1
	}
	n := (size-1)/s.cfg.PartSize + 1
	return s.cfg.PartSize, int(n)
}

// normalizeFiles 校验、去重并按哈希排序；超出配置的限制以 QUOTA_EXCEEDED 拒绝。
func (s *Service) normalizeFiles(in []FileSpec) ([]FileSpec, int64, error) {
	seen := map[string]int64{}
	var out []FileSpec
	var total int64
	for i, f := range in {
		if !digest.ValidHex(f.SHA256) {
			return nil, 0, invalid("files[%d].sha256 must be 64 lowercase hex characters", i)
		}
		if f.Size < 0 {
			return nil, 0, invalid("files[%d].size must not be negative", i)
		}
		if f.Size > s.cfg.MaxFileBytes {
			return nil, 0, reasonErr(errcode.QuotaExceeded, "file_too_large", "a file exceeds the configured size limit",
				map[string]any{"limit": s.cfg.MaxFileBytes, "requested": f.Size, "index": i})
		}
		if prev, ok := seen[f.SHA256]; ok {
			if prev != f.Size {
				return nil, 0, invalid("files[%d] repeats a hash with a different size", i)
			}
			continue
		}
		seen[f.SHA256] = f.Size
		out = append(out, f)
		if f.Size > s.cfg.MaxUploadBytes-total {
			return nil, 0, reasonErr(errcode.QuotaExceeded, "upload_too_large", "the upload exceeds the configured total size limit",
				map[string]any{"limit": s.cfg.MaxUploadBytes})
		}
		total += f.Size
	}
	if len(out) > s.cfg.MaxUploadFiles {
		return nil, 0, reasonErr(errcode.QuotaExceeded, "too_many_files", "the upload has more files than the configured limit",
			map[string]any{"limit": s.cfg.MaxUploadFiles, "requested": len(out)})
	}
	if total > s.cfg.MaxUploadBytes {
		return nil, 0, reasonErr(errcode.QuotaExceeded, "upload_too_large", "the upload exceeds the configured total size limit",
			map[string]any{"limit": s.cfg.MaxUploadBytes, "requested": total})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SHA256 < out[j].SHA256 })
	return out, total, nil
}

// checkSpace 在接受写入前核对可用空间，不足时返回 STORAGE_FULL，不让写入
// 进行到一半才失败。
func (s *Service) checkSpace(need int64) error {
	free, err := fileop.FreeBytes(s.layout.Home)
	if err != nil {
		return nil // 无法判断时不阻止；实际写入失败仍会归类为 STORAGE_FULL
	}
	if need < 0 {
		need = 0
	}
	if free < s.cfg.MinFreeBytes || free-s.cfg.MinFreeBytes < uint64(need) {
		return reasonErr(errcode.StorageFull, "insufficient_space", "not enough free space for this transfer",
			map[string]any{"required_bytes": need})
	}
	return nil
}

// CreateUpload 创建上传会话。需要项目的 storage.upload 权限；幂等键作用域为
// (主体, 项目, storage.create_upload)，同键同请求返回原会话，异请求
// IDEMPOTENCY_CONFLICT。会话分配的 operation_id 用于之后提交版本。
func (s *Service) CreateUpload(ctx context.Context, req CreateUploadRequest) (Upload, error) {
	if err := req.Who.Validate(); err != nil {
		return Upload{}, err
	}
	if !req.ProjectID.Valid() {
		return Upload{}, invalid("project_id is required")
	}
	files, total, err := s.normalizeFiles(req.Files)
	if err != nil {
		return Upload{}, err
	}
	if _, err := s.authorize(ctx, req.Who, ActionUpload, authz.Resource{ProjectID: req.ProjectID, Kind: "project", ID: req.ProjectID}); err != nil {
		return Upload{}, err
	}
	body, err := json.Marshal(struct {
		Files []FileSpec `json:"files"`
	}{nonNilFiles(files)})
	if err != nil {
		return Upload{}, err
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: CommandCreateUpload, ProjectID: req.ProjectID, Body: body})
	if err != nil {
		return Upload{}, err
	}
	receiptOp, err := s.newID()
	if err != nil {
		return Upload{}, err
	}
	uploadID, err := s.newID()
	if err != nil {
		return Upload{}, err
	}
	commitOp, err := ids.DeriveChild(receiptOp, "step:commit")
	if err != nil {
		return Upload{}, err
	}
	cmd := commands.Context{
		OperationID: receiptOp, IdempotencyKey: req.IdempotencyKey, RequestHash: hash, CommandType: CommandCreateUpload,
		ActorID: req.Who.PrincipalID, SessionID: req.Who.SessionID, ProjectID: req.ProjectID, RecoveryEpoch: req.Who.RecoveryEpoch,
	}
	if err := cmd.Validate(); err != nil {
		return Upload{}, invalid("invalid request: %v", err)
	}
	lctx, release, err := s.write(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return Upload{}, err
	}
	defer release()
	if _, err := s.authorize(lctx, req.Who, ActionUpload, authz.Resource{ProjectID: req.ProjectID, Kind: "project", ID: req.ProjectID}); err != nil {
		return Upload{}, err
	}
	resp, err := s.store.Execute(lctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		if err := s.checkSpace(total); err != nil {
			return commands.Result{}, err
		}
		now := s.now()
		abs := now.Add(s.cfg.UploadMaxTTL)
		idle := min64Time(now.Add(s.cfg.UploadIdleTTL), abs)
		if _, err := tx.ExecContext(ctx, `INSERT INTO storage_uploads (upload_id, operation_id, principal_id, session_id,
			project_id, state, file_count, total_bytes, created_at, last_activity, idle_expires_at, expires_at)
			VALUES (?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?)`,
			uploadID, commitOp, req.Who.PrincipalID, req.Who.SessionID, req.ProjectID, len(files), total,
			clock.Millis(now), clock.Millis(now), clock.Millis(idle), clock.Millis(abs)); err != nil {
			return commands.Result{}, err
		}
		for _, f := range files {
			partSize, count := s.partLayout(f.Size)
			if _, err := tx.ExecContext(ctx, `INSERT INTO storage_upload_files (upload_id, sha256, size, part_size, part_count, state)
				VALUES (?, ?, ?, ?, ?, 'pending')`, uploadID, f.SHA256, f.Size, partSize, count); err != nil {
				return commands.Result{}, err
			}
		}
		pinID, err := s.newID()
		if err != nil {
			return commands.Result{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO storage_pins (pin_id, owner_kind, owner_id, created_at, expires_at)
			VALUES (?, 'upload_session', ?, ?, ?)`, pinID, uploadID, clock.Millis(now), clock.Millis(abs)); err != nil {
			return commands.Result{}, err
		}
		ev, err := s.event(EvUploadCreated, "upload", uploadID, 1, req.Who, req.ProjectID, receiptOp,
			map[string]any{"upload_id": uploadID, "operation_id": commitOp, "file_count": len(files), "total_bytes": total})
		if err != nil {
			return commands.Result{}, err
		}
		return commands.Result{
			Status: commands.ReceiptSucceeded, ResponseCode: 201,
			Summary:    map[string]any{"upload_id": uploadID, "operation_id": commitOp},
			ResultRefs: []commands.ResultRef{{Kind: "upload", ID: uploadID}},
			Events:     []event.Envelope{ev},
		}, nil
	})
	if err != nil {
		return Upload{}, err
	}
	if resp.Outcome == commands.OutcomeReplay {
		if len(resp.Receipt.ResultRefs) != 1 || resp.Receipt.ResultRefs[0].Kind != "upload" {
			return Upload{}, errcode.New(errcode.Internal, "")
		}
		uploadID = resp.Receipt.ResultRefs[0].ID
	}
	return s.GetUpload(ctx, req.Who, uploadID)
}

func nonNilFiles(f []FileSpec) []FileSpec {
	if f == nil {
		return []FileSpec{}
	}
	return f
}

func min64Time(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

type uploadRow struct {
	Upload
	SessionID ids.ID
}

func (s *Service) loadUpload(ctx context.Context, q commands.DBTX, id ids.ID) (*uploadRow, error) {
	var u uploadRow
	var created, idle, abs int64
	var reason sql.NullString
	err := q.QueryRowContext(ctx, `SELECT upload_id, operation_id, principal_id, session_id, project_id, state, total_bytes,
		created_at, idle_expires_at, expires_at, close_reason FROM storage_uploads WHERE upload_id = ?`, id).
		Scan(&u.UploadID, &u.OperationID, &u.PrincipalID, &u.SessionID, &u.ProjectID, &u.State, &u.TotalBytes,
			&created, &idle, &abs, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, u.IdleExpiresAt, u.ExpiresAt = clock.FromMillis(created), clock.FromMillis(idle), clock.FromMillis(abs)
	u.CloseReason = reason.String
	u.PartsURL = PartsURL(u.UploadID)
	rows, err := q.QueryContext(ctx, `SELECT sha256, size, part_size, part_count, state FROM storage_upload_files
		WHERE upload_id = ? ORDER BY sha256`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f UploadFile
		if err := rows.Scan(&f.SHA256, &f.Size, &f.PartSize, &f.PartCount, &f.State); err != nil {
			return nil, err
		}
		u.Files = append(u.Files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	parts, err := q.QueryContext(ctx, `SELECT sha256, part_number FROM storage_upload_parts WHERE upload_id = ?
		ORDER BY sha256, part_number`, id)
	if err != nil {
		return nil, err
	}
	defer parts.Close()
	byHash := map[string][]int{}
	for parts.Next() {
		var h string
		var n int
		if err := parts.Scan(&h, &n); err != nil {
			return nil, err
		}
		byHash[h] = append(byHash[h], n)
	}
	if err := parts.Err(); err != nil {
		return nil, err
	}
	for i := range u.Files {
		u.Files[i].Received = byHash[u.Files[i].SHA256]
	}
	if u.Files == nil {
		u.Files = []UploadFile{}
	}
	return &u, nil
}

// ownUpload 读取调用者本人的上传会话；别人的会话一律 NOT_FOUND，不泄露存在性。
func (s *Service) ownUpload(ctx context.Context, who authz.Context, id ids.ID) (*uploadRow, error) {
	if !id.Valid() {
		return nil, errcode.New(errcode.NotFound, "")
	}
	u, err := s.loadUpload(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if u.PrincipalID != who.PrincipalID {
		return nil, errcode.New(errcode.NotFound, "")
	}
	return u, nil
}

// openUpload 在 ownUpload 的基础上要求会话仍开放、未到期，并复核当前上传权限。
func (s *Service) openUpload(ctx context.Context, who authz.Context, id ids.ID) (*uploadRow, error) {
	u, err := s.ownUpload(ctx, who, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.authorize(ctx, who, ActionUpload, authz.Resource{ProjectID: u.ProjectID, Kind: "upload", ID: u.UploadID}); err != nil {
		return nil, err
	}
	switch {
	case u.State != UploadOpen:
		return nil, reasonErr(errcode.InvalidStateTransition, "upload_closed", "the upload session is closed",
			map[string]any{"state": u.State})
	case !s.now().Before(u.deadline()):
		return nil, reasonErr(errcode.InvalidStateTransition, "upload_expired", "the upload session has expired; create a new one", nil)
	}
	return u, nil
}

// GetUpload 返回调用者本人的上传会话与分片进度，供续传使用。
func (s *Service) GetUpload(ctx context.Context, who authz.Context, id ids.ID) (Upload, error) {
	u, err := s.ownUpload(ctx, who, id)
	if err != nil {
		return Upload{}, err
	}
	if _, err := s.authorize(ctx, who, ActionUpload, authz.Resource{ProjectID: u.ProjectID, Kind: "upload", ID: u.UploadID}); err != nil {
		return Upload{}, err
	}
	return u.Upload, nil
}

// UploadForOperation 返回操作所属的上传会话（供 catalog 组装提交）；只返回
// 调用者本人的会话。
func (s *Service) UploadForOperation(ctx context.Context, who authz.Context, op ids.ID) (Upload, error) {
	var id ids.ID
	err := s.db.QueryRowContext(ctx, `SELECT upload_id FROM storage_uploads WHERE operation_id = ?`, op).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Upload{}, errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return Upload{}, err
	}
	return s.GetUpload(ctx, who, id)
}

func fileOf(u *uploadRow, sha string) (UploadFile, bool) {
	for _, f := range u.Files {
		if f.SHA256 == sha {
			return f, true
		}
	}
	return UploadFile{}, false
}

// PartRequest 是写入一个分片的请求。
type PartRequest struct {
	Who        authz.Context
	UploadID   ids.ID
	SHA256     string
	PartNumber int
	// PartSHA256 是本分片字节的 SHA-256；Size 是本分片的长度。
	PartSHA256 string
	Size       int64
	Body       io.Reader
}

// PartResult 是分片写入结果。
type PartResult struct {
	PartNumber int  `json:"part_number"`
	Duplicate  bool `json:"duplicate"`
	Received   int  `json:"received_parts"`
	PartCount  int  `json:"part_count"`
}

// PutPart 写入一个分片：字节流式写入暂存区并计算 SHA-256，刷盘后才记录。
// 已记录的分片再次到达时，摘要相同视为重复（不再写入），摘要不同一律
// HASH_MISMATCH（原分片保持不变）。长度不符或实际字节与摘要不符同样拒绝，
// 不完整或未核验的字节不会被记录。每次请求都复核当前上传权限。
func (s *Service) PutPart(ctx context.Context, req PartRequest) (PartResult, error) {
	switch {
	case !digest.ValidHex(req.SHA256) || !digest.ValidHex(req.PartSHA256):
		return PartResult{}, invalid("sha256 and part sha256 must be 64 lowercase hex characters")
	case req.PartNumber < 1:
		return PartResult{}, invalid("part numbers start at 1")
	case req.Size < 0:
		return PartResult{}, invalid("part size must not be negative")
	case req.Body == nil:
		return PartResult{}, invalid("part body is required")
	}
	u, err := s.openUpload(ctx, req.Who, req.UploadID)
	if err != nil {
		return PartResult{}, err
	}
	f, ok := fileOf(u, req.SHA256)
	if !ok {
		return PartResult{}, errcode.New(errcode.NotFound, "the upload does not declare this content")
	}
	if req.PartNumber > f.PartCount {
		return PartResult{}, reasonErr(errcode.SchemaInvalid, "part_number", "part number is beyond the last part",
			map[string]any{"part_count": f.PartCount})
	}
	offset := int64(req.PartNumber-1) * f.PartSize
	want := min(f.PartSize, f.Size-offset)
	if req.Size != want {
		return PartResult{}, reasonErr(errcode.SchemaInvalid, "part_size", "part length does not match the upload layout",
			map[string]any{"expected": want})
	}
	res := PartResult{PartNumber: req.PartNumber, PartCount: f.PartCount}
	unlock := s.lockKey("part/" + string(req.UploadID) + "/" + req.SHA256 + "/" + fmt.Sprint(req.PartNumber))
	defer unlock()

	recorded, err := s.recordedPart(ctx, req.UploadID, req.SHA256, req.PartNumber)
	if err != nil {
		return PartResult{}, err
	}
	if recorded != "" {
		if recorded != req.PartSHA256 {
			return PartResult{}, reasonErr(errcode.HashMismatch, "part_conflict",
				"this part was already received with different content", map[string]any{"part_number": req.PartNumber})
		}
		res.Duplicate = true
		res.Received, err = s.receivedCount(ctx, req.UploadID, req.SHA256)
		return res, err
	}
	if f.State == FileVerified {
		// 整件已核验入库：迟到的分片不再写入。
		res.Duplicate = true
		res.Received = f.PartCount
		return res, nil
	}
	if err := s.checkSpace(want); err != nil {
		return PartResult{}, err
	}
	// 暂存字节同样是实例写入：维护需要等待在途写入排空。此阶段不持
	// security_guard，网络接收期间仍允许撤权；接受分片前再取得安全锁复验。
	_, finishReceive, err := s.write(ctx, commands.Request{})
	if err != nil {
		return PartResult{}, err
	}
	err = s.writePart(req.UploadID, req.SHA256, offset, want, req.PartSHA256, req.Body)
	finishReceive()
	if err != nil {
		return PartResult{}, err
	}
	lctx, release, err := s.write(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return PartResult{}, err
	}
	defer release()
	if _, err := s.authorize(lctx, req.Who, ActionUpload, authz.Resource{ProjectID: u.ProjectID, Kind: "upload", ID: u.UploadID}); err != nil {
		return PartResult{}, err
	}
	now := s.now()
	err = s.inTx(lctx, func(tx *sql.Tx) error {
		if err := s.stillOpen(lctx, tx, req.UploadID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(lctx, `INSERT INTO storage_upload_parts (upload_id, sha256, part_number, start_offset, size,
			part_sha256, received_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, req.UploadID, req.SHA256, req.PartNumber, offset, want,
			req.PartSHA256, clock.Millis(now)); err != nil {
			return err
		}
		return s.touch(lctx, tx, req.UploadID, now)
	})
	if err != nil && !sqlite.IsUniqueViolation(err) {
		return PartResult{}, err
	}
	res.Received, err = s.receivedCount(ctx, req.UploadID, req.SHA256)
	return res, err
}

// touch 推进会话的空闲到期（不超过绝对到期）。
func (s *Service) touch(ctx context.Context, tx *sql.Tx, id ids.ID, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE storage_uploads SET last_activity = ?, idle_expires_at = MIN(?, expires_at)
		WHERE upload_id = ? AND state = 'open'`, clock.Millis(now), clock.Millis(now.Add(s.cfg.UploadIdleTTL)), id)
	return err
}

// stillOpen 在写事务内复核会话仍开放且未到期：与关闭、到期清理串行化，
// 已关闭的会话不会再记录分片或签发授权。
func (s *Service) stillOpen(ctx context.Context, tx *sql.Tx, id ids.ID, now time.Time) error {
	var state UploadState
	var deadline int64
	err := tx.QueryRowContext(ctx, `SELECT state, MIN(idle_expires_at, expires_at) FROM storage_uploads WHERE upload_id = ?`, id).
		Scan(&state, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return err
	}
	switch {
	case state != UploadOpen:
		return reasonErr(errcode.InvalidStateTransition, "upload_closed", "the upload session is closed", map[string]any{"state": state})
	case clock.Millis(now) >= deadline:
		return reasonErr(errcode.InvalidStateTransition, "upload_expired", "the upload session has expired; create a new one", nil)
	}
	return nil
}

// bumpRevision 推进上传会话的聚合修订并返回新值（事件的 aggregate_revision）。
func (s *Service) bumpRevision(ctx context.Context, tx *sql.Tx, id ids.ID) (int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx, `UPDATE storage_uploads SET revision = revision + 1 WHERE upload_id = ? RETURNING revision`, id).Scan(&rev)
	return rev, err
}

func (s *Service) recordedPart(ctx context.Context, upload ids.ID, sha string, part int) (string, error) {
	var h string
	err := s.db.QueryRowContext(ctx, `SELECT part_sha256 FROM storage_upload_parts WHERE upload_id = ? AND sha256 = ? AND part_number = ?`,
		upload, sha, part).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return h, err
}

func (s *Service) receivedCount(ctx context.Context, upload ids.ID, sha string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_upload_parts WHERE upload_id = ? AND sha256 = ?`, upload, sha).Scan(&n)
	return n, err
}

// writePart 把分片字节写到暂存文件的对应位置，同时计算摘要；读取恰好 size
// 字节并刷盘，摘要或长度不符时返回 HASH_MISMATCH（该区域未被记录，之后的
// 正确重试会覆盖它）。内存占用与分片大小无关。
func (s *Service) writePart(upload ids.ID, sha string, offset, size int64, partSHA string, body io.Reader) error {
	path := s.layout.uploadData(upload, sha)
	if err := s.fs.MkdirAll(s.layout.uploadDir(upload)); err != nil {
		return err
	}
	if s.fs.Faults != nil && s.fs.Faults.Write != nil {
		if err := s.fs.Faults.Write(path); err != nil {
			return fileop.Wrap("writing upload data", err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fileop.Wrap("opening upload data", err)
	}
	defer f.Close()
	h := sha256.New()
	w := io.NewOffsetWriter(f, offset)
	// 只写本分片的范围；多出的字节不写盘，读 1 字节判断是否超长。
	n, err := io.CopyN(io.MultiWriter(w, h), body, size)
	if err != nil && !errors.Is(err, io.EOF) {
		if code := fileop.Classify(err); code != "" {
			return fileop.Wrap("writing upload data", err)
		}
		return errcode.Wrap(errcode.HashMismatch, "the part body could not be read completely", err).
			WithDetails(errcode.Detail{Reason: "part_truncated"})
	}
	var extra [1]byte
	if n == size {
		if m, _ := io.ReadFull(body, extra[:]); m > 0 {
			n++
		}
	}
	if n != size {
		return reasonErr(errcode.HashMismatch, "part_length", "the part body length does not match the declared size",
			map[string]any{"expected": size, "received_at_least": n})
	}
	if hex.EncodeToString(h.Sum(nil)) != partSHA {
		return reasonErr(errcode.HashMismatch, "part_digest", "the part body does not match its declared hash", nil)
	}
	if s.fs.Faults != nil && s.fs.Faults.Sync != nil {
		if err := s.fs.Faults.Sync(path); err != nil {
			return fileop.Wrap("syncing upload data", err)
		}
	}
	if err := f.Sync(); err != nil {
		return fileop.Wrap("syncing upload data", err)
	}
	// 首个分片会创建文件；记录分片前必须使目录项也持久，防止重启后只剩回执。
	return s.fs.SyncDir(s.layout.uploadDir(upload))
}

// BlobGrant 是内容复用授权：允许 OperationID 在 ProjectID 中使用这份字节。
type BlobGrant struct {
	GrantID     ids.ID            `json:"grant_id"`
	ProjectID   ids.ID            `json:"project_id"`
	OperationID ids.ID            `json:"operation_id"`
	SHA256      string            `json:"sha256"`
	Size        int64             `json:"size"`
	Basis       string            `json:"basis"`
	Purpose     authz.Purpose     `json:"purpose"`
	Source      *ids.PermanentRef `json:"source,omitempty"`
	SourcePath  string            `json:"source_path,omitempty"`
	IssuedAt    time.Time         `json:"issued_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
}

// 复用授权的依据。
const (
	BasisUploaded         = "uploaded"
	BasisAuthorizedSource = "authorized_source"
)

// CompleteFile 在全部分片到齐后核验整件：流式重算 SHA-256 与大小，一致才放入
// 内容库，并签发绑定本会话操作的 uploaded 复用授权与上传 pin。整件不符时把
// 该内容标为失败并清掉分片，调用方须以正确的哈希重新上传。重复调用返回同一
// 授权。
func (s *Service) CompleteFile(ctx context.Context, who authz.Context, uploadID ids.ID, sha string) (BlobGrant, error) {
	unlock := s.lockKey("complete/" + string(uploadID) + "/" + sha)
	defer unlock()
	u, err := s.openUpload(ctx, who, uploadID)
	if err != nil {
		return BlobGrant{}, err
	}
	f, ok := fileOf(u, sha)
	if !ok {
		return BlobGrant{}, errcode.New(errcode.NotFound, "the upload does not declare this content")
	}
	if f.State == FileVerified {
		return s.uploadedGrant(ctx, u.OperationID, sha)
	}
	if f.State == FileFailed {
		return BlobGrant{}, reasonErr(errcode.HashMismatch, "file_failed", "this content failed verification; upload it again in a new session", nil)
	}
	if missing := missingParts(f); len(missing) > 0 {
		if len(missing) > 100 {
			missing = missing[:100]
		}
		return BlobGrant{}, reasonErr(errcode.InvalidStateTransition, "parts_missing", "not all parts have been received",
			map[string]any{"missing_parts": missing})
	}
	staging := s.layout.uploadData(uploadID, sha)
	got, n, err := fileop.HashFile(staging)
	if errors.Is(err, fs.ErrNotExist) {
		// 并发的完成请求可能已经把内容入库并删掉了暂存：以记录为准。
		if cur, lerr := s.loadUpload(ctx, s.db, uploadID); lerr == nil {
			if cf, ok := fileOf(cur, sha); ok && cf.State == FileVerified {
				return s.uploadedGrant(ctx, u.OperationID, sha)
			}
		}
		got, n, err = "", -1, nil
	}
	if err != nil {
		return BlobGrant{}, err
	}
	if got != sha || n != f.Size {
		if err := s.failFile(ctx, who, u, sha); err != nil {
			return BlobGrant{}, err
		}
		return BlobGrant{}, reasonErr(errcode.HashMismatch, "file_digest", "the uploaded content does not match its declared hash or size", nil)
	}
	// 硬链接回退可能复制整件，不持有 security_guard；只有最终授权提交
	// 才与撤权串行化。期间关闭或撤权只可能留下未授权、待 GC 的 CAS 原件。
	if err := func() error {
		_, release, err := s.write(ctx, blobLock(sha))
		if err != nil {
			return err
		}
		defer release()
		_, err = s.placeBlob(staging, sha, f.Size)
		return err
	}(); err != nil {
		return BlobGrant{}, err
	}
	lctx, release, err := s.write(ctx, commands.Request{Security: commands.ModeShared, Blobs: []string{sha}})
	if err != nil {
		return BlobGrant{}, err
	}
	defer release()
	if _, err := s.openUpload(lctx, who, uploadID); err != nil {
		return BlobGrant{}, err
	}
	now := s.now()
	grantID, err := s.newID()
	if err != nil {
		return BlobGrant{}, err
	}
	err = s.inTx(lctx, func(tx *sql.Tx) error {
		if err := s.stillOpen(lctx, tx, uploadID, now); err != nil {
			return err
		}
		res, err := tx.ExecContext(lctx, `UPDATE storage_upload_files SET state = 'verified', verified_at = ?
			WHERE upload_id = ? AND sha256 = ? AND state = 'pending'`, clock.Millis(now), uploadID, sha)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // 并发完成：保留先完成者的授权
		}
		if _, err := tx.ExecContext(lctx, `INSERT INTO storage_blob_grants (grant_id, principal_id, session_id, project_id,
			operation_id, sha256, size, basis, purpose, issued_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'uploaded', 'ingest', ?, ?)`,
			grantID, who.PrincipalID, who.SessionID, u.ProjectID, u.OperationID, sha, f.Size, clock.Millis(now), clock.Millis(u.ExpiresAt)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(lctx, `INSERT OR IGNORE INTO storage_pin_blobs (pin_id, sha256)
			SELECT pin_id, ? FROM storage_pins WHERE owner_kind = 'upload_session' AND owner_id = ?`, sha, uploadID); err != nil {
			return err
		}
		if err := s.touch(lctx, tx, uploadID, now); err != nil {
			return err
		}
		rev, err := s.bumpRevision(lctx, tx, uploadID)
		if err != nil {
			return err
		}
		ev, err := s.event(EvUploadFileVerified, "upload", uploadID, rev, who, u.ProjectID, u.OperationID,
			map[string]any{"upload_id": uploadID, "sha256": sha, "size": f.Size})
		if err != nil {
			return err
		}
		return s.store.AppendEvents(lctx, tx, u.OperationID, []event.Envelope{ev})
	})
	if err != nil {
		return BlobGrant{}, err
	}
	// 暂存链接在授权提交之后删除；失败只留下可清理的暂存文件。
	os.Remove(staging)
	s.sealBlob(sha)
	return s.uploadedGrant(ctx, u.OperationID, sha)
}

func missingParts(f UploadFile) []int {
	var missing []int
	for i := 1; i <= f.PartCount; i++ {
		if !slices.Contains(f.Received, i) {
			missing = append(missing, i)
		}
	}
	return missing
}

// failFile 把整件核验失败的内容标为 failed 并清掉分片与暂存数据。
func (s *Service) failFile(ctx context.Context, who authz.Context, u *uploadRow, sha string) error {
	lctx, release, err := s.write(ctx, commands.Request{})
	if err != nil {
		return err
	}
	defer release()
	err = s.inTx(lctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(lctx, `UPDATE storage_upload_files SET state = 'failed' WHERE upload_id = ? AND sha256 = ? AND state = 'pending'`,
			u.UploadID, sha)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // 已被并发请求核验或标记，保留原状
		}
		_, err = tx.ExecContext(lctx, `DELETE FROM storage_upload_parts WHERE upload_id = ? AND sha256 = ?`, u.UploadID, sha)
		return err
	})
	if err != nil {
		return err
	}
	os.Remove(s.layout.uploadData(u.UploadID, sha))
	return nil
}

// BlobStatus 是 CheckBlobs 对一份内容的回答。
type BlobStatus struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Status 为 granted（本操作已有有效复用授权）或 upload_required。不论内容
	// 是否已在内容库中，没有本操作授权一律 upload_required，不泄露存在性。
	Status string `json:"status"`
}

// CheckBlobs 在上传会话与授权范围内查询哪些内容还需要上传。
func (s *Service) CheckBlobs(ctx context.Context, who authz.Context, uploadID ids.ID, files []FileSpec) ([]BlobStatus, error) {
	u, err := s.openUpload(ctx, who, uploadID)
	if err != nil {
		return nil, err
	}
	out := make([]BlobStatus, 0, len(files))
	now := s.now()
	for i, f := range files {
		if !digest.ValidHex(f.SHA256) || f.Size < 0 {
			return nil, invalid("files[%d] is not a valid content reference", i)
		}
		st := BlobStatus{SHA256: f.SHA256, Size: f.Size, Status: "upload_required"}
		ok, err := s.hasGrant(ctx, s.db, u.ProjectID, u.OperationID, f.SHA256, f.Size, now)
		if err != nil {
			return nil, err
		}
		if ok {
			st.Status = "granted"
		}
		out = append(out, st)
	}
	return out, nil
}

// CancelUpload 由本人放弃上传会话：未消费的复用授权撤销，暂存数据删除，
// upload pin 释放。已被 prepared 操作消费的授权不受影响。
func (s *Service) CancelUpload(ctx context.Context, who authz.Context, uploadID ids.ID) error {
	u, err := s.ownUpload(ctx, who, uploadID)
	if err != nil {
		return err
	}
	_, err = s.close(ctx, who, u, UploadCancelled, "cancelled")
	return err
}

// CompleteUpload 在版本提交成功后关闭会话（由 catalog 调用）：内容已由台账
// 的提交记录与不可变清单维持，upload pin 释放，未用到的授权撤销。
func (s *Service) CompleteUpload(ctx context.Context, who authz.Context, uploadID ids.ID) error {
	u, err := s.ownUpload(ctx, who, uploadID)
	if err != nil {
		return err
	}
	if u.State != UploadOpen {
		return nil
	}
	_, err = s.close(ctx, who, u, UploadCompleted, "committed")
	return err
}

func (s *Service) close(ctx context.Context, who authz.Context, u *uploadRow, state UploadState, reason string) (bool, error) {
	if u.State != UploadOpen {
		return false, nil
	}
	lctx, release, err := s.write(ctx, commands.Request{})
	if err != nil {
		return false, err
	}
	defer release()
	now := s.now()
	closed := false
	err = s.inTx(lctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(lctx, `UPDATE storage_uploads SET state = ?, closed_at = ?, close_reason = ?
			WHERE upload_id = ? AND state = 'open' AND (? <> 'expired' OR MIN(idle_expires_at, expires_at) <= ?)`,
			state, clock.Millis(now), reason, u.UploadID, state, clock.Millis(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		closed = true
		if _, err := tx.ExecContext(lctx, `UPDATE storage_pins SET released_at = ? WHERE owner_kind = 'upload_session'
			AND owner_id = ? AND released_at IS NULL`, clock.Millis(now), u.UploadID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(lctx, `UPDATE storage_blob_grants SET revoked_at = ?, revoke_reason = ?
			WHERE operation_id = ? AND consumed_at IS NULL AND revoked_at IS NULL`, clock.Millis(now), "upload_"+string(state), u.OperationID); err != nil {
			return err
		}
		rev, err := s.bumpRevision(lctx, tx, u.UploadID)
		if err != nil {
			return err
		}
		ev, err := s.event(EvUploadClosed, "upload", u.UploadID, rev, who, u.ProjectID, u.OperationID,
			map[string]any{"upload_id": u.UploadID, "state": state, "reason": reason})
		if err != nil {
			return err
		}
		return s.store.AppendEvents(lctx, tx, u.OperationID, []event.Envelope{ev})
	})
	if err != nil {
		return false, err
	}
	if closed {
		os.RemoveAll(s.layout.uploadDir(u.UploadID))
	}
	return closed, nil
}

// SweepReport 是一次到期清理的结果。
type SweepReport struct {
	Expired        int `json:"expired"`
	StagingRemoved int `json:"staging_removed"`
}

// SweepExpiredUploads 关闭已到期（空闲或绝对到期）的上传会话：删除暂存数据、
// 撤销未消费的复用授权并释放 upload pin；同时删除已关闭或未知会话残留的
// 暂存目录。内容库中的原件不在这里删除——已被 prepared 操作消费的内容由
// 台账的提交 pin 保留，其余原件由 GC（M2）按全部 pin 与引用核对后回收。
// 暂存删除失败时已完成的关闭保留，其余目录继续处理，最后返回汇总错误；
// 残留目录在下一轮再次删除。
func (s *Service) SweepExpiredUploads(ctx context.Context) (SweepReport, error) {
	var rep SweepReport
	now := s.now()
	rows, err := s.db.QueryContext(ctx, `SELECT upload_id FROM storage_uploads WHERE state = 'open'
		AND MIN(idle_expires_at, expires_at) <= ? ORDER BY upload_id`, clock.Millis(now))
	if err != nil {
		return rep, err
	}
	var expired []ids.ID
	for rows.Next() {
		var id ids.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return rep, err
		}
		expired = append(expired, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	for _, id := range expired {
		u, err := s.loadUpload(ctx, s.db, id)
		if err != nil {
			return rep, err
		}
		system := authz.Context{PrincipalID: u.PrincipalID, SessionID: u.SessionID}
		closed, err := s.close(ctx, system, u, UploadExpired, "expired")
		if err != nil {
			return rep, err
		}
		if closed {
			rep.Expired++
		}
	}
	entries, err := os.ReadDir(s.layoutUploadsRoot())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return rep, fileop.Wrap("listing upload staging", err)
	}
	var failed []error
	for _, e := range entries {
		id := ids.ID(e.Name())
		var state string
		err := s.db.QueryRowContext(ctx, `SELECT state FROM storage_uploads WHERE upload_id = ?`, id).Scan(&state)
		if err == nil && state == string(UploadOpen) {
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return rep, errors.Join(append(failed, err)...)
		}
		_, release, err := s.write(ctx, commands.Request{})
		if err != nil {
			return rep, errors.Join(append(failed, err)...)
		}
		err = os.RemoveAll(s.layout.uploadDir(id))
		release()
		if err != nil {
			failed = append(failed, fileop.Wrap("removing upload staging", err))
			continue
		}
		rep.StagingRemoved++
	}
	return rep, errors.Join(failed...)
}

func (s *Service) layoutUploadsRoot() string {
	return s.layout.uploadDir("")
}
