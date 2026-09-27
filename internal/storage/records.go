package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// RecordContract 是证据记录信封的契约标识。
const RecordContract = "lantai.evidence-record/v1"

// MaxRecordBytes 限制单条证据记录的大小；大的证据文件作为原件上传，记录里只放引用。
const MaxRecordBytes = 1 << 20

// Record 是一条追加式证据记录（lantai.evidence-record/v1）的信封。
type Record struct {
	RecordID       ids.ID          `json:"record_id"`
	ProjectID      ids.ID          `json:"project_id"`
	AssetID        ids.ID          `json:"asset_id"`
	VersionID      ids.ID          `json:"version_id"`
	ManifestDigest digest.Digest   `json:"manifest_digest"`
	Kind           string          `json:"kind"`
	PayloadSchema  string          `json:"payload_schema,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	Producer       *Producer       `json:"producer,omitempty"`
	AuthorID       ids.ID          `json:"author_id"`
	SessionID      ids.ID          `json:"session_id,omitempty"`
	OperationID    ids.ID          `json:"operation_id,omitempty"`
	Supersedes     ids.ID          `json:"supersedes,omitempty"`
	// CreatedAt 由写入方给出（UTC 毫秒文本），重试时必须相同，否则视为不同内容。
	CreatedAt string `json:"created_at"`
}

// Producer 是产出记录的扩展或内置处理器的身份（defs 中的 producer_ref）。
type Producer struct {
	ExtensionID      string        `json:"extension_id"`
	ExtensionVersion string        `json:"extension_version"`
	PackageDigest    digest.Digest `json:"package_digest"`
	Source           string        `json:"source"`
	ContributionID   string        `json:"contribution_id,omitempty"`
}

// RecordRef 是写入结果。
type RecordRef struct {
	RecordID ids.ID        `json:"record_id"`
	Digest   digest.Digest `json:"digest"`
	// Ref 是 storage 内部的文件位置引用，不是主机路径。
	Ref string `json:"ref"`
	// Created 为 false 表示同一内容此前已写入（幂等重放）。
	Created bool `json:"created"`
}

func (r Record) canonical() ([]byte, error) {
	if err := canonjson.CheckUTF8(r); err != nil {
		return nil, invalid("record contains invalid UTF-8: %v", err)
	}
	type wire struct {
		Contract string `json:"contract"`
		Record
	}
	raw, err := json.Marshal(wire{Contract: RecordContract, Record: r})
	if err != nil {
		return nil, err
	}
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return nil, invalid("record is not valid JSON: %v", err)
	}
	reg, err := schema.Default()
	if err != nil {
		return nil, err
	}
	if err := reg.Validate(RecordContract, doc); err != nil {
		return nil, errcode.Wrap(errcode.SchemaInvalid, "record does not satisfy "+RecordContract, err)
	}
	return canonjson.Canonicalize(raw)
}

// AppendRecord 是证据追加适配器：把记录以规范化 JSON 排他写入目标版本的记录
// 区。目标必须是台账中已提交的版本，且 manifest_digest 与台账一致（否则
// PRECONDITION_FAILED，防止把旧检查用于新版本）；supersedes 必须指向同一版本
// 已有的记录。同一 record_id 同内容幂等，异内容 IDEMPOTENCY_CONFLICT，已写入
// 的记录不被覆盖。
//
// 适配器只保证字节与目标绑定；谁可以追加、记录是否被接受为证据由调用方
// （provenance，T03）按其授权与验收规则决定。
func (s *Service) AppendRecord(ctx context.Context, r Record) (RecordRef, error) {
	raw, err := r.canonical()
	if err != nil {
		return RecordRef{}, err
	}
	if len(raw) > MaxRecordBytes {
		return RecordRef{}, reasonErr(errcode.QuotaExceeded, "record_too_large", "the record exceeds the size limit; upload large evidence as content",
			map[string]any{"limit": MaxRecordBytes})
	}
	v, err := s.ledger.Version(ctx, r.AssetID, r.VersionID)
	if err != nil {
		return RecordRef{}, err
	}
	if v.ProjectID != r.ProjectID {
		return RecordRef{}, errcode.New(errcode.RefMismatch, "the version does not belong to this project")
	}
	if v.ManifestDigest != r.ManifestDigest {
		return RecordRef{}, reasonErr(errcode.PreconditionFailed, "manifest_digest_mismatch",
			"the record targets a different manifest than the committed version", nil)
	}
	dir := s.layout.recordsDir(r.ProjectID, r.AssetID, r.VersionID)
	if r.Supersedes != "" {
		if _, err := os.Stat(filepath.Join(dir, string(r.Supersedes)+".json")); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return RecordRef{}, reasonErr(errcode.SchemaInvalid, "supersedes_unknown", "supersedes must name an existing record of the same version", nil)
			}
			return RecordRef{}, fileop.Wrap("checking a record", err)
		}
	}
	_, release, err := s.write(ctx, commands.Request{})
	if err != nil {
		return RecordRef{}, err
	}
	defer release()
	path := filepath.Join(dir, string(r.RecordID)+".json")
	created, err := s.fs.CreateOrMatch(path, raw, 0o444)
	if errors.Is(err, fileop.ErrContentDiffers) {
		return RecordRef{}, errcode.New(errcode.IdempotencyConflict, "a different record with this record_id already exists")
	}
	if err != nil {
		return RecordRef{}, err
	}
	return RecordRef{RecordID: r.RecordID, Digest: digest.Of(raw), Ref: s.layout.ref(path), Created: created}, nil
}

// ReadRecord 读取一条已写入的记录（规范化 JSON）。适配器不做授权，调用方负责。
func (s *Service) ReadRecord(ctx context.Context, project, asset, version, record ids.ID) ([]byte, error) {
	if !project.Valid() || !asset.Valid() || !version.Valid() || !record.Valid() {
		return nil, errcode.New(errcode.NotFound, "")
	}
	raw, err := os.ReadFile(filepath.Join(s.layout.recordsDir(project, asset, version), string(record)+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errcode.New(errcode.NotFound, "")
	}
	if err != nil {
		return nil, fileop.Wrap("reading a record", err)
	}
	return raw, ctx.Err()
}
