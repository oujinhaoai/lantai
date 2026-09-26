// Package install 定义版本文件安装的跨模块契约：storage（T02）实现
// Installer 并产出 InstallProof，ledger（T03）在最终接受边界复核证明后
// 才提交版本。证明只表示 installed；台账 committed 才是对外可见点。
package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

// Contract 是安装证明的契约标识。
const Contract = "lantai.install-proof/v1"

// File 是冻结清单中的一个文件。
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Request 是一次安装请求，内容来自已冻结的清单。
type Request struct {
	OperationID    ids.ID
	ProjectID      ids.ID
	AssetID        ids.ID
	VersionID      ids.ID
	VersionNumber  int64
	ManifestDigest digest.Digest
	// Files 按 Path 字节序升序、不重复。
	Files []File
}

// relativePath 取自 schemas/common/v1/defs.schema.json#/$defs/relative_path。
var relativePath = schema.MustDefPattern("relative_path")

// ErrInvalidRequest 表示安装请求不合法。
var ErrInvalidRequest = errors.New("install: invalid request")

// Validate 检查请求结构：ID、摘要、路径形态、排序与唯一性。路径的 NFC、
// 大小写冲突与平台保留名由 catalog 在冻结清单时校验。
func (r Request) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
	}
	for name, id := range map[string]ids.ID{"operation_id": r.OperationID, "project_id": r.ProjectID, "asset_id": r.AssetID, "version_id": r.VersionID} {
		if !id.Valid() {
			return bad("%s", name)
		}
	}
	if r.VersionNumber < 1 {
		return bad("version_number %d", r.VersionNumber)
	}
	if !r.ManifestDigest.Valid() {
		return bad("manifest_digest")
	}
	for i, f := range r.Files {
		if len(f.Path) > 200 || !utf8.ValidString(f.Path) || !relativePath.MatchString(f.Path) {
			return bad("files[%d].path %q", i, f.Path)
		}
		if !digest.ValidHex(f.SHA256) {
			return bad("files[%d].sha256", i)
		}
		if f.Size < 0 {
			return bad("files[%d].size", i)
		}
		if i > 0 && r.Files[i-1].Path >= f.Path {
			return bad("files must be sorted by path and unique (at %q)", f.Path)
		}
	}
	return nil
}

// Proof 是 lantai.install-proof/v1。
type Proof struct {
	OperationID    ids.ID
	ProjectID      ids.ID
	AssetID        ids.ID
	VersionID      ids.ID
	VersionNumber  int64
	ManifestDigest digest.Digest
	Files          []File
	// InstallRef 是 storage 内部的安装位置引用，不是主机路径。
	InstallRef  string
	InstalledAt time.Time
}

type proofWire struct {
	Contract       string        `json:"contract"`
	OperationID    ids.ID        `json:"operation_id"`
	ProjectID      ids.ID        `json:"project_id"`
	AssetID        ids.ID        `json:"asset_id"`
	VersionID      ids.ID        `json:"version_id"`
	VersionNumber  int64         `json:"version_number"`
	ManifestDigest digest.Digest `json:"manifest_digest"`
	Files          []File        `json:"files"`
	InstallRef     string        `json:"install_ref"`
	InstalledAt    string        `json:"installed_at"`
}

// MarshalJSON 输出契约格式。
func (p Proof) MarshalJSON() ([]byte, error) {
	files := p.Files
	if files == nil {
		files = []File{}
	}
	return json.Marshal(proofWire{
		Contract: Contract, OperationID: p.OperationID, ProjectID: p.ProjectID, AssetID: p.AssetID,
		VersionID: p.VersionID, VersionNumber: p.VersionNumber, ManifestDigest: p.ManifestDigest,
		Files: files, InstallRef: p.InstallRef, InstalledAt: clock.Format(p.InstalledAt),
	})
}

// UnmarshalJSON 解析契约格式。
func (p *Proof) UnmarshalJSON(data []byte) error {
	var w proofWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.Contract != Contract {
		return fmt.Errorf("install: unexpected contract %q", w.Contract)
	}
	t, err := clock.Parse(w.InstalledAt)
	if err != nil {
		return err
	}
	*p = Proof{OperationID: w.OperationID, ProjectID: w.ProjectID, AssetID: w.AssetID, VersionID: w.VersionID,
		VersionNumber: w.VersionNumber, ManifestDigest: w.ManifestDigest, Files: w.Files,
		InstallRef: w.InstallRef, InstalledAt: t}
	return nil
}

// Canonical 按权威 schema 校验并返回规范化 JSON。
func (p Proof) Canonical() ([]byte, error) {
	if err := canonjson.CheckUTF8(p); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return nil, err
	}
	reg, err := schema.Default()
	if err != nil {
		return nil, err
	}
	if err := reg.Validate(Contract, doc); err != nil {
		return nil, err
	}
	return canonjson.Canonicalize(raw)
}

// Digest 返回证明的摘要，由台账保存；证明本身不含自身摘要。
func (p Proof) Digest() (digest.Digest, error) {
	c, err := p.Canonical()
	if err != nil {
		return "", err
	}
	return digest.Of(c), nil
}

// Matches 核对证明与请求逐项一致；不一致说明证明不属于这次提交，
// 台账不得据此提交，应隔离并对账。
func (p Proof) Matches(r Request) error {
	if p.OperationID != r.OperationID || p.ProjectID != r.ProjectID || p.AssetID != r.AssetID ||
		p.VersionID != r.VersionID || p.VersionNumber != r.VersionNumber {
		return errcode.New(errcode.OperationNeedsReconciliation, "install proof identifies a different operation or version")
	}
	if p.ManifestDigest != r.ManifestDigest || len(p.Files) != len(r.Files) {
		return errcode.New(errcode.OperationNeedsReconciliation, "install proof does not match the frozen manifest")
	}
	for i := range p.Files {
		if p.Files[i] != r.Files[i] {
			return errcode.New(errcode.OperationNeedsReconciliation, "install proof file list differs from the frozen manifest")
		}
	}
	return nil
}

// Installer 由 storage（T02）实现。
//
// Install 以 OperationID 幂等：同一请求重入返回同一证明，不产生第二个版本
// 目录；同一 OperationID 携带不同请求返回 IDEMPOTENCY_CONFLICT。内容缺失或
// 无本操作授权返回 BLOB_GRANT_REQUIRED（不区分是否已存在），哈希或大小不符
// 返回 HASH_MISMATCH，空间不足 STORAGE_FULL，介质或占用问题 STORAGE_UNAVAILABLE。
// 安装内容在台账提交前不得对外可读。
type Installer interface {
	Install(ctx context.Context, req Request) (Proof, error)
	// Verify 复核证明对应的安装内容仍完整；恢复与最终接受前调用。
	Verify(ctx context.Context, p Proof) error
	// Quarantine 把不能提交的安装内容移入隔离区，保留字节供诊断，不删除。
	Quarantine(ctx context.Context, operationID ids.ID, reason errcode.Code) error
}
