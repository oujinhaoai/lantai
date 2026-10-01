// Package commit 定义台账（ledger，T03）对外的业务提交与权威读取接口，由
// ledger 实现，catalog/storage（T02）、transport（T07）与 events/query（T04）
// 调用。台账是版本提交、占名、说明修订生效与项目登记的唯一负责人；catalog
// 与 storage 只负责内容与文件，不直接写台账的控制表。
//
// 版本流程：Prepare（台账持久化 prepared、保留版本 ID/号与占名）→ storage
// Install（产出 installed 证明）→ Commit（在最终接受边界复验当前授权、基线与
// 证明后一次事务写入版本登记、回执与 outbox）。committed 才是对外可见点；
// Reader 只返回已提交版本，并且不依赖检索索引。
//
// 说明修订（project.yaml、asset.yaml）同理：PrepareMetadata 保留新修订号并
// 核对预期修订 → storage 写出不可变的修订文件 → CommitMetadata 复验后切换
// 当前修订指针。文件决定“内容是什么”，台账决定“哪一个修订生效”。
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

// ActionCommitVersion 是提交版本所需的授权动作；已提交操作的重放由同一
// 操作者以同一动作复核，撤权后不能取回结果。
const ActionCommitVersion authz.Action = "ledger.commit_version"

// PrepareRequest 是创建资产首版或追加版本的请求，清单已冻结、内容已上传。
type PrepareRequest struct {
	Who       authz.Context
	ProjectID ids.ID
	// AssetID 为空表示新建资产，此时 Slug 必填并占名。
	AssetID ids.ID
	// Slug 是 catalog 规范化后的资产路径别名（pathrule.NormalizeSlug）。
	// 占名按 pathrule.Key 比较：只差大小写或 Unicode 写法的两个名字是同一个名字。
	Slug string
	// BaseVersionID 是追加版本时声明的基线；资产尚无版本时为空。
	BaseVersionID  ids.ID
	ManifestDigest digest.Digest
	Files          []install.File
}

// Prepared 是持久化的 prepared 操作；版本 ID 与版本号已保留，不会重新分配。
type Prepared struct {
	OperationID ids.ID
	// ActorID 与 SessionID 固定为原始受理命令的身份；同主体换会话续办不改写清单归属。
	ActorID        ids.ID
	SessionID      ids.ID
	ProjectID      ids.ID
	AssetID        ids.ID
	VersionID      ids.ID
	VersionNumber  int64
	ManifestDigest digest.Digest
	Files          []install.File
	// AliasGeneration 在新建资产时为本次占名将取得的别名代次（同一路径从 1
	// 单调递增、不复用）；追加版本时为 0。
	AliasGeneration int64
}

// InstallRequest 返回交给 storage 的安装请求；调用方另行填入 catalog 渲染的
// 版本清单文件内容（install.Request.Manifest）。
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
	// CommittedBy 是提交该版本的主体。
	CommittedBy ids.ID
	// AliasGeneration 在该版本同时建立资产时为取得的别名代次，否则为 0。
	AliasGeneration int64
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
// 新建资产的占名在 prepared 时保留，committed 时生效为 active 并取得代次；
// 未提交就取消的占名释放，且不消耗代次。
//
// Commit 以 operation_id 幂等：已提交时同一证明返回原结果。最终接受边界复验
// 当前授权；授权已收紧时操作进入 blocked 并返回拒绝，已安装字节保留；证明
// 与冻结清单不符或内容损坏时隔离并返回 OPERATION_NEEDS_RECONCILIATION 或
// HASH_MISMATCH。永远不能凭孤立的安装目录补记提交。
//
// Cancel 由发起操作的主体放弃尚未提交的操作：释放资产的进行中占用与新资产
// 的占名（不消耗代次），已保留的版本号不回收；已提交或已终结的操作返回
// INVALID_STATE_TRANSITION，别人的操作返回 NOT_FOUND。
type Ledger interface {
	// LookupPrepared 按原命令的幂等作用域和摘要只读查找保留（含已提交操作）。
	// 不存在返回 NOT_FOUND，同键异摘要返回 IDEMPOTENCY_CONFLICT；不重新解析浮动输入。
	// 调用方须先核对当前读取/操作权限，不信任外部自报的 ActorID。
	LookupPrepared(ctx context.Context, cmd commands.Context) (Prepared, error)
	Prepare(ctx context.Context, cmd commands.Context, req PrepareRequest) (Prepared, error)
	Commit(ctx context.Context, operationID ids.ID, who authz.Context, proof install.Proof) (Committed, error)
	Cancel(ctx context.Context, operationID ids.ID, who authz.Context) error
	Operation(ctx context.Context, operationID ids.ID) (*commands.View, error)
}

// AcceptanceVerifier 是核心 catalog/storage 的只读最终复核，不是外部插件钩子。
// 台账须在统一 security_guard 读锁内、提交所属库事务之前调用它，并保持该锁至
// 提交完成；它核对冻结的输入版本、当前来源读取/用途权限和本操作 BlobGrant。
// 不能用安装时的授权或已消费标记代替最终复验。已提交操作的回执重放不再调用。
// 不得在接口内做网络调用、再次取维护/安全锁或写入其他模块的数据。
type AcceptanceVerifier interface {
	VerifyAcceptance(ctx context.Context, who authz.Context, prepared Prepared) error
}

// Asset 是已提交资产的登记：资产在首个版本 committed 时建立。
type Asset struct {
	AssetID   ids.ID
	ProjectID ids.ID
	// Slug 与 Generation 是建立资产时取得的路径别名与代次。
	Slug        string
	Generation  int64
	CreatedBy   ids.ID
	CreatedAt   time.Time
	OperationID ids.ID
}

// Reader 提供不依赖检索索引的权威读取与枚举。未提交或不存在的对象返回
// NOT_FOUND；版本不属于给定资产时同样返回 NOT_FOUND，不泄露该版本是否存在
// 于别的资产中。调用方另行做读取授权。
type Reader interface {
	Version(ctx context.Context, assetID, versionID ids.ID) (Committed, error)
	// Versions 按 version_id 升序枚举已提交版本，after 为空从头开始。
	Versions(ctx context.Context, after ids.ID, limit int) ([]Committed, error)
	// AssetVersions 按版本号升序枚举一个资产的已提交版本，afterNumber 为 0
	// 从头开始；未知资产返回空列表。读取同一权威登记，只是按资产定位，供
	// 投影刷新单个资产时不必枚举全库。
	AssetVersions(ctx context.Context, assetID ids.ID, afterNumber int64, limit int) ([]Committed, error)
	// VersionByNumber 返回资产第 number 号已提交版本；号码被保留但未提交时为 NOT_FOUND。
	VersionByNumber(ctx context.Context, assetID ids.ID, number int64) (Committed, error)
	// LatestVersion 返回资产号码最大的已提交版本。
	LatestVersion(ctx context.Context, assetID ids.ID) (Committed, error)
	// Asset 返回已提交资产的登记。
	Asset(ctx context.Context, assetID ids.ID) (Asset, error)
}

// ClaimState 是占名状态，与 lantai.namespace-claim/v1 的 claim_state 一致。
type ClaimState string

const (
	ClaimActive   ClaimState = "active"
	ClaimReserved ClaimState = "reserved"
	ClaimReleased ClaimState = "released"
)

// Claim 是一个路径当前的占名记录（lantai.namespace-claim/v1）。
type Claim struct {
	ProjectID ids.ID
	// Slug 是取得当前（或最近一次）代次时的规范路径。
	Slug  string
	State ClaimState
	// Generation 是当前代次；released 时为最后一代。同一路径的代次从 1 起
	// 单调递增，从不复用，因此 Generation > 1 表示该路径曾被复用。
	Generation int64
	// AssetID 是持有该代次的资产（reserved 时为 prepared 操作保留的资产）。
	AssetID     ids.ID
	OperationID ids.ID
	Revision    int64
}

// Namespace 由 ledger 实现，提供路径占名的权威查询。旧代次到资产的映射
// 由 catalog 的别名历史文件保存；台账只回答当前（或最近一次）占名。
type Namespace interface {
	// Claim 按 pathrule.Key(slug) 查找；从未被占用返回 NOT_FOUND。
	Claim(ctx context.Context, projectID ids.ID, slug string) (Claim, error)
}

// 说明修订命令与授权。
const (
	// CommandCommitMetadata 是说明修订的命令类型，回执与 outbox 在 ledger.db。
	CommandCommitMetadata = "ledger.commit_metadata"
	// CommandRegisterProject 是登记项目的命令类型。
	CommandRegisterProject = "ledger.register_project"
	// ActionCreateProject 是登记项目所需的授权动作（实例级）。
	ActionCreateProject authz.Action = "catalog.create_project"
)

// TargetKind 是说明修订的对象类别。
type TargetKind string

const (
	TargetProject TargetKind = "project"
	TargetAsset   TargetKind = "asset"
)

// MetadataTarget 是被修订的说明：项目（ID 等于 ProjectID）或资产。
type MetadataTarget struct {
	Kind      TargetKind
	ProjectID ids.ID
	ID        ids.ID
}

// MetadataRequest 是一次说明修订的请求。
type MetadataRequest struct {
	Who    authz.Context
	Target MetadataTarget
	// ExpectedRevision 是调用方读到的当前修订；0 表示目标还没有任何已提交修订。
	ExpectedRevision int64
	// ContentDigest 是新修订规范内容的摘要。
	ContentDigest digest.Digest
	// Action 是最终接受时复验的授权动作，由 catalog 按修改者与字段选定
	// （例如 catalog.patch_metadata 或 catalog.patch_own_metadata）。
	Action authz.Action
}

// PreparedMetadata 是已保留的修订：Revision 为将要生效的新修订号。
type PreparedMetadata struct {
	OperationID   ids.ID
	Target        MetadataTarget
	Revision      int64
	ContentDigest digest.Digest
}

// RevisionProof 是 storage 写出不可变修订文件后的证明。
type RevisionProof struct {
	OperationID   ids.ID
	Target        MetadataTarget
	Revision      int64
	ContentDigest digest.Digest
	// FileRef 是 storage 内部的文件位置引用，不是主机路径。
	FileRef   string
	WrittenAt time.Time
}

// RevisionVerifier 由 storage 实现：复核修订文件仍在且内容与摘要一致。
type RevisionVerifier interface {
	VerifyRevision(ctx context.Context, p RevisionProof) error
}

// CommittedMetadata 是已生效的说明修订。
type CommittedMetadata struct {
	OperationID   ids.ID
	Target        MetadataTarget
	Revision      int64
	ContentDigest digest.Digest
	CommittedAt   time.Time
	CommittedBy   ids.ID
}

// Metadata 由 ledger 实现，是说明修订生效的唯一负责人。
//
// PrepareMetadata 以幂等键判定（同键同摘要返回原保留，异摘要
// IDEMPOTENCY_CONFLICT）；预期修订不是当前修订时 PRECONDITION_FAILED；
// 目标已有进行中的修订时 RESOURCE_BUSY；目标不存在 NOT_FOUND。
// CommitMetadata 以 operation_id 幂等，在最终接受边界复验 req.Action 的当前
// 授权、恢复代次与修订文件；授权收紧时进入 blocked。
type Metadata interface {
	PrepareMetadata(ctx context.Context, cmd commands.Context, req MetadataRequest) (PreparedMetadata, error)
	CommitMetadata(ctx context.Context, operationID ids.ID, who authz.Context, proof RevisionProof) (CommittedMetadata, error)
	// CurrentMetadata 返回当前生效的修订；还没有任何已提交修订时 NOT_FOUND。
	CurrentMetadata(ctx context.Context, target MetadataTarget) (CommittedMetadata, error)
}

// ProjectState 是项目生命周期。
type ProjectState string

const (
	ProjectActive   ProjectState = "active"
	ProjectArchived ProjectState = "archived"
)

// Project 是项目登记：稳定 ID、不可变 key 与生命周期；说明在 project.yaml。
type Project struct {
	ProjectID   ids.ID
	Key         string
	State       ProjectState
	CreatedAt   time.Time
	CreatedBy   ids.ID
	OperationID ids.ID
}

// ProjectRequest 是登记项目的请求；Key 已按 pathrule.CheckProjectKey 校验。
type ProjectRequest struct {
	Who authz.Context
	Key string
}

// Projects 由 ledger 实现。RegisterProject 是短命令：同键同摘要返回原项目，
// 异摘要 IDEMPOTENCY_CONFLICT；key 已被占用 PATH_CONFLICT；在最终接受边界
// 按 ActionCreateProject 授权。项目 ID 由台账分配，永不复用。
type Projects interface {
	RegisterProject(ctx context.Context, cmd commands.Context, req ProjectRequest) (Project, error)
	Project(ctx context.Context, projectID ids.ID) (Project, error)
	ProjectByKey(ctx context.Context, key string) (Project, error)
}
