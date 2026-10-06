# 实例生命周期、迁移与维护屏障

状态：**M1 实例、备份恢复与本机运维已实现并有自动化测试**。实现：[`internal/operations`](../../internal/operations/)、[`internal/application`](../../internal/application/)、[`internal/commands/gate.go`](../../internal/commands/gate.go)、[`internal/platform/fsutil`](../../internal/platform/fsutil/)、[`internal/platform/sqlite/migrations`](../../internal/platform/sqlite/migrations/)。共同备份、空目录恢复和升级命令见[备份恢复](backup-restore.md)，单 HTTPS 网关模板与健康/指标见[部署](../deployment.md)。

## 数据根布局

数据根（`LANTAI_HOME`）必须位于服务端本机磁盘。当前由本模块建立的文件：

| 路径 | 写入者 | 内容 |
|---|---|---|
| `instance.json` | operations | 实例标记 [`lantai.instance/v1`](../../schemas/operations/v1/instance.schema.json)：实例 ID、名称、状态、数据格式版本、`recovery_epoch`、进行中的迁移和恢复门闩 |
| `lantai.lock` | operations | 单实例写锁；文件内容只是最近一次取锁的进程号与时间，用于诊断 |
| `config.yaml` | 运维人员 | 可选配置 [`lantai.config/v1`](../../schemas/operations/v1/config.schema.json)，不放密钥；未知字段拒绝 |
| `db/{main,ledger,runtime,events,index}.db` | 各所属模块 | 五库，目录权限 0700 |
| `secrets/master.key` | identity | 主密钥（默认位置，可用 `secrets.dir` 移到单独受控的位置）；权限必须只允许本用户访问 |
| `audit/` | events | 后台收录事件的审计导出与清单；未确认备份前不自动裁剪 |
| `backups/records/` | operations | 备份清单、复制状态与永久 backup pin |
| `logs/restore-<run_id>*` | operations | 已绑定的本机对账、原始证据与完成回执 |
| `logs/offline-recovery.log` | identity（本机离线恢复） | 离线恢复的本机审计行 |

## 单实例锁

打开或初始化实例时先以非阻塞方式取得 `lantai.lock` 上的独占锁（Unix `flock`，Windows `LockFileEx`）。锁由操作系统维护：进程退出或崩溃即释放，不留陈旧锁；同一进程的第二个句柄同样冲突，所以同一数据根不会有两个写实例。取不到锁返回原因 `instance_locked`。锁只在本机有效，多个核心进程写同一数据根不在支持范围内。

## 实例标记与恢复诊断

| 情况 | 行为 |
|---|---|
| 没有标记、没有库文件 | 未初始化：`Open` 返回 `not_initialized`，`lantai init` 可以初始化 |
| 没有标记、却有库文件 | `unmarked_data`：拒绝初始化与启动，需从备份恢复标记 |
| 标记为 `initializing` | 初始化未完成：只能由 `lantai init` 继续（沿用同一实例 ID） |
| 标记为 `active`、某个权威库缺失 | `database_missing`：进入恢复诊断，**不创建空库、不重新开放初始化** |
| `index.db` 缺失 | 新建空索引库并记 `index_recreated`，索引由查询模块重建 |
| 某库绑定到其他实例 | `instance_mismatch`（每个库的 `instance_binding` 表记录所属实例） |
| 库中有本构建不认识或内容被改动的迁移、未登记的表 | `schema_incompatible` |
| 标记的数据格式版本高于本构建 | `format_newer`，不支持降级 |
| 数据根或库目录在网络文件系统上 | `network_filesystem`，拒绝（Linux 按 `statfs` 类型、macOS 按 `MNT_LOCAL`、Windows 按驱动器类型判断；`db/` 是另一个挂载点或符号链接时单独核对；无法判断时记 `filesystem_unverified`） |
| 数据根或库目录在 FUSE 上 | `fuse_filesystem`，默认拒绝初始化与启动（Linux 按 `statfs` 魔数、macOS 按类型名识别）；配置 `storage.allow_fuse: true` 显式接受风险时放行，并记 `filesystem_unverified` 提示 |
| 较新构建开始的迁移尚未完成 | `format_newer`：旧构建不替它完成，由开始迁移的构建（或更新的构建）续做 |

以上是“恢复诊断”类原因：`Open` 返回 `*StartupError`，不打开实例。`lantai doctor` 只读给出同样的原因，实例运行中也可调用。

## 兼容矩阵与迁移

- 每个库有自己的迁移序列，版本从 1 连续递增，文件位于 `internal/platform/sqlite/migrations/sql/<库>/<版本>.<所有者>.<名称>.sql`；共享组件的基础设施表（命令回执、operations、outbox）直接引用 `commands.SchemaSQL`。已发布的迁移不修改，变更追加新迁移。
- `schema_migrations` 记录每个已应用迁移的内容摘要（CRLF 先统一为 LF）。启动时核对：已应用的迁移必须连续、摘要与本构建一致；否则拒绝。
- 每个迁移在所属库内单独成事务，与其版本记录一起提交；新建的每张表都必须按[所有权](ownership.md)登记在该迁移的所有者名下，否则迁移失败回滚。跨库没有共同事务。
- 本构建的兼容矩阵 = 数据格式版本 `DataFormatVersion` + 每库最高迁移版本。全部库与标记满足矩阵才能开放写入。
- 有待执行迁移时 `Open` 成功但实例为 `blocked`（`migration_required`），`Start` 拒绝。迁移只由 `lantai migrate`（`Instance.Migrate`）在维护屏障下显式执行：先把本次迁移写入标记（`migration`），再依库执行；中途失败标记保留，实例保持 `migration_incomplete`，重跑只补未完成的版本，不重复已提交的迁移，也不伪造成功。新迁移强制要求已经完整核验的共同备份并绑定其 ID/摘要；中断继续沿用原绑定。恢复中的旧 schema 升级绑定其原始备份，详见[备份恢复](backup-restore.md)。

## 状态、就绪与维护屏障

| 状态 | 含义 |
|---|---|
| `initializing` | `Create` 已建库，身份初始化尚未完成 |
| `blocked` | 已打开，但有阻止开放写入的原因（待迁移、磁盘余量不足、启动步骤失败） |
| `starting` | 已打开并通过检查，等待 `Start` |
| `recovering` | 正在执行启动步骤（恢复分派、身份核对等），写入关闭 |
| `ready` | 开放写入 |
| `maintenance` | 维护屏障独占（迁移、备份） |
| `stopping` / `closed` | 优雅退出中 / 已释放资源与锁 |

`Readiness()` 区分 `live`（进程与实例存活）与 `ready`（可安全接收写入），不就绪时给出机器可读原因；它是健康/就绪接口（T08.4）的数据来源。

**维护屏障**（`commands.Gate`）：所有写入——前台命令与后台任务——都经 `Gate.Acquire` 进入写路径。实例未开放写入时立即返回 `MAINTENANCE_MODE`（503，`details.reason` 为 `starting`、`recovering`、`maintenance`、`migrating`、`stopping` 等），而不是排队到维护结束后继续；开放时先取屏障共享锁，再按统一顺序取其余锁，取锁后再核对一次状态与写入口代次——等锁期间经历过维护或停止的写入即使维护已结束也拒绝，写入不会跨过维护窗口。维护（`Instance.Maintain` / `Gate.Maintain`）先关闭写入，再取屏障独占锁等待在途写入结束；结束后没有其他阻止原因才重新开放。领域读取不经写屏障。持有同一协调器活跃独占维护上下文的所属模块可重入写入口，仍遵守其余锁顺序；伪造、异实例或已释放的维护上下文拒绝。

**后台任务**用 `Instance.Go` 登记：就绪后启动，`Close` 时取消并等待；其写入同样经 `Gate`。

**启动顺序**：取锁 → 读配置与标记 → 文件系统检查 → 权威库存在性 → 打开五库 → 核对绑定、迁移与兼容矩阵 → 磁盘余量（数据根与库目录取较小者）→（`Start`）持屏障独占锁依次执行启动步骤 → 开放写入 → 启动后台任务。任何启动步骤失败，实例保持 `blocked`（`recovery_failed`），不边恢复边接受写入；启动期间收到 `Close` 时，`Close` 等启动步骤结束，之后不再开放写入。

**优雅退出**：关闭写入 → 取消并等待后台任务 → 取屏障独占锁等待在途写入 → 关闭五库 → 释放数据根锁。等待超过调用方期限而在途写入仍未结束时，**不关库、不释放数据根锁**（否则另一个实例可能在旧写入提交前取得写权），返回错误并保持 `stopping`；写入结束后可再次 `Close`，或退出进程由操作系统释放锁。

## 本机命令

| 命令 | 作用 |
|---|---|
| `lantai init -home <dir> -admin <name>` | 建立实例（写 `initializing` 标记、建五库并迁移、绑定），生成主密钥，交互式登记首个管理员（口令两次、验证器种子当场展示、以当前动态码确认、恢复码只展示一次），最后把标记改为 `active`。已初始化或存在无标记的库时拒绝；中断后重跑会继续同一次初始化 |
| `lantai migrate -home <dir> [-backup <complete-backup-dir>]` | 维护屏障下应用待执行的迁移；无事可做时报告 no-op |
| `lantai doctor -home <dir> [-json]` | 只读诊断，不取锁、不修改任何文件；退出码 0 表示可以启动 |
| `lantai recover-admin -home <dir> -admin <name>` | 单管理员的本机离线恢复，见[身份与授权](identity.md#恢复) |

这些命令只能在服务端本机、服务停止时运行（它们需要数据根锁）。`serve` 使用同一锁运行服务；网关与运维端点见[部署](../deployment.md)。

## 已知限制

- 断电与文件系统级故障演练、三平台实际文件语义属 TEST-M1-06/12，尚未执行；原子替换在 Windows 上不刷新目录。
- 网络文件系统检测只识别已知类型；FUSE 默认拒绝，其他无法判断的类型只记提示，部署前须在实际位置用 SQLite 探针的并发模式验证（见[部署的数据位置](../deployment.md#数据位置)）。Windows 上不识别 FUSE 类文件系统。
- 恢复点之后的撤权、删除与未知副作用需要有证据的本机对账；无法证明时不清除恢复门闩。M1 不执行物理 GC。
