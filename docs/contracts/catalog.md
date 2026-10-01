# 目录：路径规则、清单、引用与说明修订

实现：[`internal/catalog`](../../internal/catalog/)（子包 `pathrule` 跨平台路径规则、`manifest` 清单与类型）。应用组装接真实台账、身份与来源限制模块，REST/CLI 通过 T07 调用同一领域服务；模块单测保留 [`committest`](../../internal/contract/commit/committest/) 桩，真实 SQLite 与跨模块恢复另有集成测试。取舍见 [ADR 0007](../adr/0007-storage-layout-and-catalog-ledger-split.md)。

catalog 没有数据库表：说明与别名历史是数据根下的文件（内容真源）；哪个修订生效、路径由谁占用、版本是否提交由台账裁决（[`commit`](../../internal/contract/commit/commit.go) 接口）；字节由 [storage](storage.md) 管理。catalog 的文件写入与业务写入一样经实例写入口，服从维护屏障。

## 路径规则

版本内文件路径与资产路径别名（slug）入藏时统一规范化（[`pathrule`](../../internal/catalog/pathrule/pathrule.go)），数据目录能在三个系统之间搬移：

- 用 `/` 分隔，存成 Unicode NFC；拒绝绝对路径、结尾 `/`、空段、`.` 与 `..` 段、反斜杠、控制字符与双向文字控制符（防扩展名伪装）。
- 拒绝 Windows 建不出来的字符 `<>:"|?*`、保留设备名（`CON`、`NUL`、`COM1`、`LPT¹` 等，带扩展名也算）、以点或空格结尾的段。
- 路径最长 200 个 Unicode 码点（与 schema 的 `relative_path` 一致），单段最长 255 字节。
- 同一集合里规范化后只差大小写或 Unicode 写法的两个路径，以及一个路径同时作文件和目录，返回 `PATH_CONFLICT`，不静默改名。判定用折叠键（NFC → 完全大小写折叠 → NFC），比各文件系统的实际规则更保守。
- slug 另外不允许 `@`（引用语法用它分隔版本）；项目 key 为小写字母开头的 `[a-z0-9-]`，最长 63 个字符。
- 台账的占名按同一折叠键比较：只差大小写的两个名字是同一个名字。

## 版本清单

[`lantai.manifest/v1`](../../schemas/catalog/v1/manifest.schema.json) 是版本目录中的 `manifest.yaml`：

- `content` 是冻结的版本内容：类型、`type_schema`（类型定义库 `lantai.asset-types/v1`）、可选的 `base_version_id` 与版本说明、文件（相对路径、角色、哈希、大小，按路径字节序）、固定的 `uses` 永久引用（带实例 ID 与关系，`declared` 保留书写的引用作快照）、许可声明 `rights`（用途、SPDX 许可表达式、`redistribute_raw`、`noai`、IPTC 数字来源类型、敏感级别）与类型元数据。
- **`manifest_digest` = content 按 RFC 8785 规范化后的 SHA-256**，不含台账分配的版本 ID 与版本号，因此能在台账保留版本之前冻结并交给 `Prepare`。文件其余字段是台账分配的身份与提交者，写入文件供恢复核对；文件本身不能补造 committed 事实。读取时核对摘要与身份和台账一致。
- 渲染为确定性 YAML：同一内容总是得到相同字节，读回后的规范化 JSON 与原值相同。
- 规范化（`manifest.Normalize`）：路径按上节规则处理；角色取 `primary`、`source`、`interchange`、`texture`、`recipe`、`record`、`preview`、`doc`；许可表达式只核对语法，不推导法律兼容性；元数据按类型定义校验，**未登记的顶层字段移入 `extra`、不拒收**，与 `extra` 中的同名不同值冲突时拒绝；许可、用途、敏感级别等安全字段出现在元数据中一律拒绝（`reserved_metadata_field`），必须在 `rights` 中声明。
- 资产类型：`image`、`video`、`audio`、`doc`、`config`、`plugin`、`model`、`motion`、`scene`、`production`；类型建立后不变，追加版本换类型返回 `SCHEMA_INVALID`（`asset_type_immutable`）。

`content.producer` 可选，固定扩展 ID、版本、包摘要、贡献 ID 与来源。新接受的内置来源 `builtin_release` 还必须固定核心发布摘要 `core_release_digest`；旧 v1 文件缺少该字段仍按原字节与摘要读取，不能自动补值或用于新接受。来源身份进入请求摘要与冻结清单摘要。`Deps.Producers` / 启动时 `SetProducers` 注入 T09 的可信静态登记，最终 `VerifyAcceptance` 核对冻结的身份与当前登记，不执行第三方同步钩子；缺少登记时拒绝带 producer 的提交。

## 入藏：CommitVersion

一次入藏把上传、台账保留、文件安装与台账提交串起来。内容先通过上传会话上传（或以来源授权复用），会话的 `operation_id` 就是本次提交的操作：

1. 核对上传会话属于调用者、项目处于 active、调用者有 `catalog.create_asset`（新建）或 `ledger.commit_version`（追加）权限。
2. 按请求的声明形式（包括 upload ID 与是否省略 `rights`）计算请求摘要，以 `LookupPrepared` 查找原回执；命中时沿用原始身份与冻结内容，不重新解析浮动引用或当前默认许可。首次请求解析声明的 `uses`：可读引用或永久引用都解析为固定版本（需要读取权限），并按关系与用途检查来源限制（`reference` 关系或非生产用途按参考用途判定）；新建资产必须声明 `rights`，追加版本不给时沿用资产说明的默认许可与敏感级别。
3. 规范化并冻结清单，冻结内容按操作与摘要暂存到 `staging/frozen/`；首版说明补丁在保留之前校验。
4. 台账 `Prepare`：保留资产与版本身份、版本号与新资产的占名（代次）。
5. storage `Install`，失败时：清单与已上传内容不符（`HASH_MISMATCH` 等）同一请求重试也不会成功，放弃操作以释放保留；缺少授权（`BLOB_GRANT_REQUIRED`）、空间不足、文件占用等保留 prepared，补救后以**同一幂等键**重试。
6. 台账 `Commit`：在统一维护屏障与 `security_guard` 共享锁内调用本地只读 `AcceptanceVerifier`，复验当前项目状态、调用者权限、冻结内容中的每个输入版本及其用途限制、BlobGrant 与生产者静态登记；保持同一锁直到提交，版本此时对外可见。这是核心模块间接口，不是插件或网络钩子。
7. 提交后（各自幂等，失败不改变已提交的结果）：关闭上传会话、删除冻结内容、新建资产时写别名历史与第一个说明修订（由提交操作派生的子操作；失败时结果标 `description_pending`，重试补完）。

同一幂等键重放返回同一版本，不会产生第二个版本；已提交的操作直接返回原结果。若崩溃发生在 prepared 之后，而其间浮动引用（如 `@latest`）已解析到新版本，重试时还原冻结时的内容，不采用重新解析的结果。`CancelVersion` 由本人放弃尚未提交的入藏：台账释放保留（新资产的占名不消耗代次），上传会话关闭。

## 引用与别名代次

| 形式 | 解析 |
|---|---|
| 永久引用（`asset_id` + `version_id`，或 `lantai://<instance>/assets/<asset>/versions/<version>`） | 精确寻址；版本不属于该资产时与不存在一样返回 `NOT_FOUND`，不泄露版本是否存在于别的资产中；别的馆 `SCHEMA_INVALID`（`federation_unsupported`，M8） |
| `<项目>/<路径>@vNNN` | 路径从未被复用（当前代次为 1）时直接解析；复用过则 `REF_AMBIGUOUS`（`path_reused`），须带代次或改用永久引用 |
| `<项目>/<路径>@vNNN` + `alias_generation` | 当前代次查台账占名；更早的代次查别名历史文件，并核对该资产在台账中已提交 |
| `@latest` / `@approved` / `@published` | 只解析当前代次的 active 占名，返回具体永久引用；不接受代次参数。`@latest` 为号码最大的已提交版本；`@approved`、`@published` 由 M2 的审定与发布提供，M1 分别返回 `NOT_FOUND` 与 `NOT_PUBLISHED` |

- 调用者须能读取项目，否则无论对象是否存在一律 `NOT_FOUND`。
- 名称释放后同名新资产取得新代次，旧永久引用始终指向原资产，不会被接管。M1 没有释放名称的业务命令（宽限删除与管理员释放属于 M2），测试用台账桩的构造器建立名称复用历史。
- 别名历史每一代一个不可变的 [`lantai.alias-generation/v1`](../../schemas/common/v1/alias-generation.schema.json) 文件，提交后写出；孤立的历史文件不能抢占现名。历史文件缺失时解析旧代次返回 `OPERATION_NEEDS_RECONCILIATION`（`alias_history_missing`），`RepairAliasHistory` 按台账补齐。

## 说明修订

项目说明 [`lantai.project/v1`](../../schemas/catalog/v1/project.schema.json) 与资产说明 [`lantai.asset/v1`](../../schemas/catalog/v1/asset.schema.json) 按 `expected_revision` 条件写：

1. 按将要生效的修订号渲染新内容（确定性 YAML），台账 `PrepareMetadata` 核对预期修订（过期 `PRECONDITION_FAILED`，目标有进行中的修订 `RESOURCE_BUSY`）并保留新修订号。
2. 以排他方式写出不可变的修订文件 `.history/<项目|资产>.rNNNNNN.<operation_id>.yaml`：被取消的尝试各有自己的文件，互不覆盖。
3. 台账 `CommitMetadata` 在最终接受边界复验授权与修订文件（catalog 实现 `commit.RevisionVerifier`：文件位置属于该操作、内容摘要一致）后切换当前修订。
4. 先按目标串行化，再检查当前修订并替换可读快照 `project.yaml` / `asset.yaml`（只供阅读与对账，写入失败不影响已生效的修订；不用旧修订覆盖新快照）。

同一幂等键重放返回原结果，即使之后已有新修订；读取时核对当前修订文件的摘要，被绕过服务端改动时返回 `HASH_MISMATCH`。

- 资产说明：标题、摘要、标签、主题（集合语义，去重排序）与 `extra` 可按 `catalog.patch_metadata`（负责人、整理者）修改；制作者修改自己建立的资产按 `catalog.patch_own_metadata`。**敏感级别与新版本默认许可只在建立时由首版的许可声明确定**，普通著录接口修改它们返回 `FIELD_REQUIRES_SPECIAL_COMMAND`（专门命令属 M2 的 RightsAssertion）。还没有修订时由登记事实推导（修订 0）。
- 项目：`CreateProject` 登记项目（只有系统管理员本人，`catalog.create_project`）并写第一个说明修订；`PatchProject` 按 `catalog.patch_project`（负责人，管理员也可）修改。

## 读取

`GetProject`、`GetAsset`（登记、当前说明、占名与最新版本）、`GetVersion`（已提交版本与冻结清单）都不依赖检索索引；无权读取时 `NOT_FOUND`。

## 恢复与文件核对

- `CheckFiles(ctx, metadata)` 使用 ledger 提供的全部已提交修订历史，核对项目/资产不可变修订的摘要、schema 与身份，检查当前快照、冻结文件及别名历史。未接受修订文件只作为残留列出，不切换生效指针。证据文件由 storage/provenance 各自核对。
- `RecoverVersion` 只从原 operation 的冻结内容恢复安装与提交，`RecoverMetadata` 只接受已经写出且摘要相符的原修订；两者要求仍有效的维护上下文，最终授权与原 recovery epoch 仍由台账复验。缺失修订不能从当前快照猜测重建。
- `RepairSnapshots` 在维护屏障下只用当前已提交且已核验的历史修订补快照；`RepairAliasHistory` 按已提交资产补缺失别名历史。损坏的权威历史或冲突的别名记录不被覆盖。
- application 恢复分派先通过 identity 验证原会话与主体/代次，再调用这些入口；失效会话只报告待对账，保持原 operation 与 pin。深度 fsck 发现已提交权威证据损坏时拒绝启动/备份。

## 限制

- 资产移动、名称释放、回收站与发布别名属 M2；`@published`、`@approved` 在 M1 没有提供者。
- 目标 NAS 上的规模、三平台文件语义与故障矩阵属 TEST-M1-03/06/07。
