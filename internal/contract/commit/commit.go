// Package commit 定义版本业务提交与权威读取的跨模块接口，由 ledger（T03）
// 实现，storage（T02）、transport（T07）与 events/query（T04）调用。
//
// 流程：Prepare（台账持久化 prepared、保留版本 ID/号与占名）→ storage
// Install（产出 installed 证明）→ Commit（在最终接受边界复验当前授权、
// 基线与证明后一次事务写入版本登记、回执与 outbox）。committed 才是对外
// 可见点；Reader 只返回已提交版本，并且不依赖检索索引。
package commit

import (
	"context"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
)

// CommandType 是版本提交命令类型。
const CommandType = "ledger.commit_version"

// ActionCommitVersion 是提交版本所需的授权动作。
const ActionCommitVersion authz.Action = "ledger.commit_version"

// ActionReadVersion 是读取已提交版本所需的授权动作。
const ActionReadVersion authz.Action = "ledger.read_version"

// PrepareRequest 是创建资产首版或追加版本的请求，清单已冻结、内容已上传。
type PrepareRequest struct {
	Who       authz.Context
	ProjectID ids.ID
	// AssetID 为空表示新建资产，此时 Slug 必填并占名。
	AssetID ids.ID
	Slug    string
	// BaseVersionID 是追加版本时声明的基线；资产尚无版本时为空。
	BaseVersionID  ids.ID
	ManifestDigest digest.Digest
	Files          []install.File
}

// Prepared 是持久化的 prepared 操作；版本 ID 与版本号已保留，不会重新分配。
type Prepared struct {
	OperationID    ids.ID
	ProjectID      ids.ID
	AssetID        ids.ID
	VersionID      ids.ID
	VersionNumber  int64
	ManifestDigest digest.Digest
	Files          []install.File
}

// InstallRequest 返回交给 storage 的安装请求。
func (p Prepared) InstallRequest() install.Request {
	return install.Request{
		OperationID: p.OperationID, ProjectID: p.ProjectID, AssetID: p.AssetID, VersionID: p.VersionID,
		VersionNumber: p.VersionNumber, ManifestDigest: p.ManifestDigest, Files: p.Files,
	}
}

// Committed 是已提交版本的登记。
type Committed struct {
	OperationID    ids.ID
	ProjectID      ids.ID
	AssetID        ids.ID
	VersionID      ids.ID
	VersionNumber  int64
	ManifestDigest digest.Digest
	ProofDigest    digest.Digest
	CommittedAt    time.Time
}

// Ref 返回永久引用。
func (c Committed) Ref(instance ids.ID) ids.PermanentRef {
	return ids.PermanentRef{InstanceID: instance, AssetID: c.AssetID, VersionID: c.VersionID}
}

// Ledger 由 ledger 模块实现，是版本业务提交的唯一负责人。
//
// Prepare 以 cmd 的幂等键判定：同键同摘要返回原保留，异摘要 IDEMPOTENCY_CONFLICT；
// 资产已有进行中的提交时返回 RESOURCE_BUSY（带 operation_id）；基线落后
// BASE_VERSION_CONFLICT；新建资产占名冲突 PATH_CONFLICT；无权 FORBIDDEN。
//
// Commit 以 operation_id 幂等：已提交时同一证明返回原结果。最终接受边界复验
// 当前授权；授权已收紧时操作进入 blocked 并返回拒绝，已安装字节保留；证明
// 与冻结清单不符或内容损坏时隔离并返回 OPERATION_NEEDS_RECONCILIATION 或
// HASH_MISMATCH。永远不能凭孤立的安装目录补记提交。
type Ledger interface {
	Prepare(ctx context.Context, cmd commands.Context, req PrepareRequest) (Prepared, error)
	Commit(ctx context.Context, operationID ids.ID, who authz.Context, proof install.Proof) (Committed, error)
	Operation(ctx context.Context, operationID ids.ID) (*commands.View, error)
}

// Reader 提供不依赖检索索引的权威读取与枚举。未提交或不存在的版本返回
// NOT_FOUND；版本不属于给定资产返回 REF_MISMATCH。调用方另行做读取授权。
type Reader interface {
	Version(ctx context.Context, assetID, versionID ids.ID) (Committed, error)
	// Versions 按 version_id 升序枚举已提交版本，after 为空从头开始。
	Versions(ctx context.Context, after ids.ID, limit int) ([]Committed, error)
}
