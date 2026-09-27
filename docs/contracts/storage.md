# 存储：内容库、上传、授权下载与安装

实现：[`internal/storage`](../../internal/storage/)（子包 `fileop` 文件适配器、`transfer` 传输准入）。应用组装接真实 ledger、provenance、identity；接口与传输面由 T07 复用同一服务，模块测试另保留契约桩。取舍见 [ADR 0007](../adr/0007-storage-layout-and-catalog-ledger-split.md)。

storage 只管字节与短期传输授权：版本是否提交由台账裁决，安装只产出 `installed` 证明。控制记录在 runtime.db 中本模块的表里（迁移 `runtime/0003.storage.transfers.sql`），字节在数据根下，只有服务进程写入；暂存接收、私有安装区组装、文件落位与持久记录均经实例写入口（维护屏障）。大文件传输与哈希不持有 `security_guard`；同分片、同操作的重试仍以局部互斥串行化，最终授权复核与短提交才取得 `security_guard`。

## 数据目录

数据根必须位于服务端本机磁盘上的同一文件系统（硬链接去重与同卷改名）。目录按稳定 ID 而不是可变路径命名，名称复用、改名与 Windows 路径长度都不影响版本目录：

```text
blobs/sha256/ab/cd/<sha256>                          内容库：只读原件，文件名即哈希
staging/uploads/<upload_id>/<sha256>.part             上传暂存（未核验，不可引用）
staging/install/<operation_id>/                       私有安装区
staging/frozen/<operation_id>/<digest>.json           catalog 冻结的清单内容
projects/<project_id>/project.yaml、.history/          项目说明（catalog）
projects/<project_id>/namespace/<键摘要>/gNNNNNN.json  别名历史（catalog）
projects/<project_id>/assets/<asset_id>/asset.yaml、.history/   资产说明（catalog）
projects/<project_id>/assets/<asset_id>/versions/vNNN/          不可变版本目录
    manifest.yaml、files/<相对路径>、.install.json
projects/<project_id>/assets/<asset_id>/records/<version_id>/<record_id>.json   证据
quarantine/<operation_id>/                            隔离区（保留字节）
```

版本目录出现不等于版本已提交：所有读取先经台账确认 `committed`，再核对安装记录与台账的清单摘要一致。

## 上传会话与分片续传

1. **创建会话**（`CreateUpload`，需要项目的 `storage.upload`）：申报要上传的内容（哈希与大小，按哈希去重），幂等键作用域为 `(主体, 项目, storage.create_upload)`。会话分配 `operation_id`——它就是之后提交版本所用的操作（上传是提交操作的 `receiving` 阶段），会话签发的复用授权只对这个操作有效。空会话也可以创建，用于只复用已授权来源的提交。
2. **限额**：单文件、单会话总字节与文件数可配置（`storage.Config`），超限在接受上传前返回 `QUOTA_EXCEEDED`，`details.reason` 为 `file_too_large`、`upload_too_large` 或 `too_many_files` 并给出限额；不静默截断。可用空间不足以容纳申报内容时返回 `STORAGE_FULL`。
3. **分片**：不超过 `SinglePartMax`（默认 100 MB）的文件整件一个分片，更大的按 `PartSize`（默认 64 MiB）分片。`PUT {parts_url}{sha256}/parts/{n}` 带 `Content-Length` 与 `Lantai-Part-Sha256`；每次请求都复核当前上传权限与会话状态。字节流式写到暂存文件的对应位置并同时计算摘要，**刷盘之后才记录**。长度与布局不符 `SCHEMA_INVALID`（`part_size`）；实际字节不足或超出 `HASH_MISMATCH`（`part_length`）；摘要不符 `HASH_MISMATCH`（`part_digest`），不记录。已记录的分片再次到达时，摘要相同视为重复、不再写入；**摘要不同一律拒绝（`part_conflict`），原分片保持不变**。
4. **续传**：分片记录持久化，重启不清空暂存；`GetUpload` 返回每个文件已收到的分片号，客户端只补缺的分片。
5. **完成文件**（`CompleteFile`）：全部分片到齐后流式重算整件 SHA-256 与大小，一致才放入内容库（已有同一哈希时去重，只核对大小，不覆盖），在一个事务中把文件标为已核验、签发 `uploaded` 复用授权、把原件加入会话的 upload pin。缺分片 `INVALID_STATE_TRANSITION`（`parts_missing`，列出缺的分片号）；整件不符 `HASH_MISMATCH`（`file_digest`），该内容标为失败、分片清除，须以正确哈希重新上传。并发的完成请求返回同一授权。
6. **内存**：上传、完成与下载都是流式的，内存占用与文件大小无关（见[验证记录](#验证与限制)）。

## 内容复用授权（BlobGrant）

- 只允许指定项目中的**指定操作**使用这份字节，不授予按哈希下载的权利。来源有两种：`uploaded`（本人完整上传并经服务端核验，禁止只报哈希换取授权）与 `authorized_source`（`GrantFromSource`：调用者对某个已提交版本的文件有当前的读取权限，且限制允许所声明的用途，用于新版本复用未改动的文件）。相同字节来自不同来源或用于不同用途时分别登记授权，不合并许可；续期仍需当前源版本的读取和用途授权。
- `CheckBlobs` 只在上传会话与本操作的授权范围内回答：有有效授权为 `granted`，否则一律 `upload_required`——**不论内容是否已在内容库中**，不泄露他人资产的存在性。
- 安装时每个文件都必须有本操作、本项目、大小相符的有效授权（未撤销，且已被本操作消费或尚未到期），否则 `BLOB_GRANT_REQUIRED`（不区分内容是否存在）；授权记录的大小与清单不符 `HASH_MISMATCH`。安装记录事务内复核授权并标记“已消费”；已消费只豁免到期，不豁免撤销、主体/操作绑定与当前源版本用途限制；台账最终接受时通过 `VerifyGrantAcceptance` 再次核对；来源授权的用途须与冻结清单推导的实际用途一致，不能用参考授权提交生产版本。已提交版本的引用由台账提交记录与不可变清单维持。

## 到期与 pin

- 会话空闲 24 小时、绝对 7 天到期（可配置），取较早者。`SweepExpiredUploads` 在事务内再次核对到期时间，避免扫描后已续期的会话被关闭：删除暂存、撤销**未消费**的复用授权、释放 upload pin，同时清理已关闭或未知会话残留的暂存目录。
- **到期不删除内容库中的原件**：已被 prepared 操作消费的内容由台账的提交 pin 保留，其余原件由 GC（M2，T02.6）按全部 pin 来源与引用核对后回收。storage 实现 `pin.Source`（`PinsFor`）供 GC 查询 upload pin。
- 版本提交后 catalog 调用 `CompleteUpload` 关闭会话；本人可 `CancelUpload` 放弃。

`ReconcilePins` 只在实际持有本实例维护屏障的上下文执行：依据 upload 的持久创建/关闭事实修复 pin，补齐已核验上传和来源复用的 Blob 集合；终态按原关闭时间释放，未知 owner 不删除，额外保留不缩减。失败同事务回滚。它不自动关闭上传、不调用 `SweepExpiredUploads`，也不删除暂存或 CAS。`PinsFor` 发现 owner 丢失或 pin 与 owner 不符时返回待对账错误，不能当作无保留；commit pin 由 ledger 派生，backup pin 由 operations 提供。

## 读取授权与下载

- `IssueReadGrant` 按资产所在项目的当前 `storage.read_content` 权限（无权时 `NOT_FOUND`，不泄露版本或文件是否存在）、台账已提交状态、安装记录一致性与用途限制判定，签发绑定**主体、本人会话**、项目、资产、版本、文件路径、哈希、方法与用途的读取授权；默认 15 分钟且不超过会话有效期。返回的 `url` 是传输面的相对地址（`/xfer/v1/reads/{grant_id}?sig=…`），客户端不自行拼接内部监听地址。
- 签名是主密钥派生子密钥（`storage/read-grant/v1`）对全部绑定字段的 HMAC-SHA256，只作纵深防御；授权以 runtime.db 中的记录为准。
- **每个新的 GET/Range 请求**（`OpenRead`）依次核对：签名、授权记录存在、请求者是授权绑定的同一主体与同一会话、未撤销（`TOKEN_REVOKED`）、未到期（`TOKEN_EXPIRED`）；然后在最终读取检查（`BeginRead`，`security_guard` 读锁）内按当前权限、台账已提交状态、安装记录与用途限制复核并打开原件；锁内只打开，传输在锁外。签名不符、转发给别人的地址或同一主体的另一个会话都按不存在处理（404）；只带 URL 没有会话凭据是 401。撤权、限制收紧（`USE_RESTRICTED`）或核验未完成（`RIGHTS_PENDING`）在下一个请求即生效，不等签名到期；已经开始发送的响应可能完成，不宣称能收回已交付的字节。
- 下载用标准库 `http.ServeContent`：支持 `Range`、`If-Range` 与 `HEAD`，`ETag` 为 `"sha256:<哈希>"`，`Cache-Control: private, no-store`，`Content-Disposition: attachment`。
- 本人可 `RevokeReadGrant`；会话结束或被吊销后，其授权的请求在会话核对时即被拒绝（`RevokeSessionReads` 另可立即标记撤销）。

## 版本文件安装（install.Installer）

1. 校验请求：路径必须已是规范形式（NFC、跨平台规则，见 [catalog](catalog.md#路径规则)），规范化后不能冲突（`PATH_CONFLICT`），必须带清单文件且不超过限额。
2. 按 operation 幂等：已安装且请求（含清单文件内容）相同返回原证明；请求不同 `IDEMPOTENCY_CONFLICT`；已隔离的操作不能再安装。
3. 核对每个文件的复用授权与内容库原件。
4. 在私有安装区组装：`files/` 下为原件的**硬链接**，文件系统不支持硬链接或跨卷时退回**复制并对副本重新计算 SHA-256**；写入 `manifest.yaml` 与安装标记 `.install.json`（记录所属操作与请求摘要）；刷新文件与目录后整体改名为 `versions/vNNN`。
5. 在 runtime.db 一个事务中记录安装、版本文件清单并消费授权，返回 `lantai.install-proof/v1` 证明（新增 `manifest_sha256`）。
6. 崩溃恢复：改名之后、记录之前退出时，重入会**接管**标记属于同一操作与同一请求的目录（浅层核验后沿用标记中的安装时间），不产生第二个目录；目录属于别的操作或没有标记时 `OPERATION_NEEDS_RECONCILIATION`，不覆盖、不补记。
7. 稳定的失败：空间不足 `STORAGE_FULL`，文件被占用、只读介质或权限问题 `STORAGE_UNAVAILABLE`。改名后刷盘或记账失败时保留带标记的完整目录，重入补刷旧、新父目录后才记录成功，也可按操作隔离；未被台账提交的目录始终不能正常下载。

`Verify` 要求证明与签发的完全一致，并浅层核验清单文件哈希、文件存在与大小（适合在最终接受前调用）；`VerifyDeep` 逐字节复算，用于恢复对账与 fsck。`Quarantine` 把版本目录或私有安装区整体移入 `quarantine/<operation_id>/` 并记录原因，保留字节、之后不能再安装。`ScanOrphans` 只读列出没有匹配安装记录的版本目录与残留安装区；是否隔离由台账按操作证据决定，不能凭目录存在补记成功。`ReadManifest` 返回已提交版本的清单文件（核对哈希），供 catalog 读取。

## 只读清单与 fsck

`Inventory(ctx, deep)` 枚举全 CAS（包括未引用原件）、安装证明、上传状态、暂存区、追加记录和孤立安装目录。深度模式流式核对原件、安装文件及已接收分片摘要；已核验上传与安装文件的持久引用缺少 CAS 时报告异常。未知/不可读文件、符号链接及非普通文件列为 findings，不作为正常原件接受；坏安装记录不会触发按非法哈希寻址。所有引用均为实例内相对位置。

`VerifyBlob`、`VerifyDeep`、`ReadManifest`、`ReadRecord` 拒绝不安全的文件位置；深哈希支持上下文取消，清单/记录读取有大小上限。只读核对不注册目录、不接受孤立证据、不释放 pin、不删文件。共同备份点由调用方持有维护屏障；application 将已提交事实损坏与未接受残留分开报告。

`Stats` 只聚合 storage 所有的表，返回开放上传数、已持久接收的 pending 分片逻辑字节、活跃 upload pin 数与保留字节。未知暂存与未确认的在途字节不冒充精确磁盘统计；完整残留见 `Inventory`。M1 的 `GCSupported` 为 false。

## 证据追加适配器

`AppendRecord` 把 [`lantai.evidence-record/v1`](../../schemas/storage/v1/evidence-record.schema.json) 记录以规范化 JSON 排他写入目标版本的记录区：目标必须是台账中已提交的版本，`manifest_digest` 必须与台账一致（否则 `PRECONDITION_FAILED`，防止把旧检查用于新版本）；`supersedes` 必须指向同一版本已有的记录（更正追加，不覆盖）；插件或内置处理器产出的记录带 `producer`（扩展 ID、版本、包摘要、贡献与来源；新接受的 `builtin_release` 另固定 `core_release_digest`，旧 v1 记录按原字节与摘要读取）。同一 `record_id` 同内容幂等、异内容 `IDEMPOTENCY_CONFLICT`。适配器只保证字节与目标绑定；谁能追加、记录是否被接受为证据由 provenance（T03）决定。

## 传输准入（T02.5）

[`transfer.Scheduler`](../../internal/storage/transfer/transfer.go) 按服务端验证过的会话定档（`authz.Context.TransferClass`：只有人本人的非委托会话是交互档），客户端自报的优先级一律忽略：

| 档 | 并发（默认） | 带宽 |
|---|---|---|
| 交互 | 保留 4，不与批量借用 | 不限 |
| 批量 | 全局 8、每个主体 4；先按主体排队再进全局池，同档 FIFO | 可配置总上限；有交互传输进行时降到另一上限（默认一半） |

两档并发池互不借用：交互请求永远不等批量，批量也不会因持续的交互流量永久饥饿；取消排队不泄露名额。限速按虚拟调度（允许一秒突发）。监听、超时与客户端连接池归 T07，容量与 p95 实测归 TEST-M1-10。

## 验证与限制

- 自动化测试：`go test ./internal/storage/... ./tests/integration/`。其中安装器通过与内存桩相同的契约套件，集成测试接真实身份模块验证撤权后的下一个 Range 请求被拒。大文件续传测试默认 32 MiB；`LANTAI_TEST_LARGE_MB=1024 go test -run TestLargeResumableTransfer -v ./tests/integration/` 运行 1 GiB 规模（上传中途断开、只补缺的分片、分两段 Range 下载后整件哈希一致，并记录堆占用增长）。
- 硬链接回退、空间不足、文件占用与改名失败通过故障注入验证；三个平台真实文件系统上的断电与占用演练属 TEST-M1-06，目标 NAS 上的吞吐与 p95 属 TEST-M1-04/10。
- Windows 上删除只读硬链接会清除整份文件（含内容库原件）的只读属性；安装成功后重新设置。只读只是防误改的附加措施，完整性由哈希保证。
- 台账、用途限制与恢复分派已有真实集成测试；M1 不启用物理 GC。恢复/备份接口不代替目标设备断电演练。
