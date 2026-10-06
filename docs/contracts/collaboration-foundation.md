# M2 T01/T02 协作适配契约

本篇描述已经实现的领域接口。它们由核心组装调用，T07 已开放任务/人审/生命周期/上下文的 REST 与 CLI 入口；MCP 仅投影允许的 Agent 操作，见[远程协作](manual-execution.md)。T03 的真实审定/生命周期与 T05 的任务查询（`tasks.Service.MilestoneTasks`）已接线，T09 的启用管理（[扩展包治理](extension-governance.md)）与 T08 的调度（[生命周期调度](lifecycle-scheduler.md)，默认关闭）已接线；模块验证使用真实身份、文件和 SQLite，加上明确的领域接口测试替身，不代表 M2 整体闭环完成。

## 人审动作与批次

`identity.CreateDomainChallenge` 接受同一动作、同一项目的 1–100 个唯一精确目标，复用已有 Challenge → TOTP → HumanGrant。必需的 `HumanTargets` 只读适配器在展示挑战前从所属模块核对目标存在、实际项目/范围及历史修订归属（当前修订仍在最终提交复验），缺接口或跨项目伪报目标即拒绝；重新挑战也复查目标。宿主展示服务端固定的完整请求；不能用插件提示文本代替，也不能把 `Authorize` 返回的人审要求当成已授权。

`HumanAction` 固定动作、项目、资源 ID/修订和完整请求 JSON；审定、撤销审定、解除限制另绑定 manifest 摘要。插件启停固定登记对象修订、插件 ID、包摘要、server/node/cli target、作用范围、配置 revision/摘要与策略 revision；不接受网页 target。具体角色矩阵见自动生成的[动作登记](identity-actions.md)。`ledger.revoke_review` 的“原审定者或 owner”、自审限制、输入版本和租约/fence 等业务条件仍由 T03 最终检查。

主库 `identity_human_batches` 保存不可变请求；验证 TOTP 的同一事务写入 `identity_human_grant_items`，逐项固定子 operation、动作、目标摘要、请求摘要和预期修订。子 operation 由父 operation 与固定序号派生。`RechallengeDomain` 从已保存请求重新挑战，不能添加或替换目标，重新登录后可在当前人的新会话重验证同一批次。

`AcceptHumanItem` 在维护屏障、security guard 和调用方声明的资源锁内核对实际命令与批准项，再调用可信的核心 `HumanDomain` 适配器。该适配器只有按 operation 读取回执和执行命令两个方法；不能接收数据库句柄，不能调用第三方同步钩子，也不能重新取得已持有的锁。它必须在自己的库里原子保存业务效果、回执和 outbox，并复验目标状态、权限相关业务条件与 fence。权限收紧型命令应声明 `Security: ModeExclusive`；普通命令至少持共享 guard。

已成功项按当前会话和权限返回原回执，过期 grant 不导致第二次效果；回执响应缓存到期返回 `IDEMPOTENCY_RESULT_EXPIRED`，不重新执行。未完成项必须有仍有效的 grant，部分失败可逐项重试。身份模块不保存另一份业务“完成状态”。Agent、节点、worker、runner、service、委托人和受限恢复会话不能领取这种批准。

## 项目里程碑

`PutMilestone` 创建时使用空 ID、`expected=0`，服务端生成稳定 ID；更新必须提供 ID 与当前 revision。幂等回执和 `project.milestone_changed` outbox 与 `main.identity_milestones` 在同一事务提交。元数据包含名称、目标日期、负责人和去重的任务 ID 集合；只有当前项目 owner 可修改，指定负责人也必须是活跃项目 owner，修改该字段不会授予角色。

`ListMilestones` 按项目读取元数据；`MilestoneProgress` 经 `TaskProgressReader.MilestoneTasks` 获取 T05 的任务事实，进度为 `state=done` 数量 / 全部所列任务，空集合返回 0/0。返回任务必须恰好覆盖请求集合；缺失、重复、额外或跨项目任务均报错，不用不完整数据冒充进度。该进程内只读适配器继承安全锁，不重新取锁，不跨库联表。上下文内容与审定分别仍属 T02/T03。

## 回收站字节动作

`storage.ApplyFileIntent` 只读取 `FileIntentSource.AcceptedFileIntent` 给出的 T03 持久意图。该意图包含 operation、trash ID、trash/restore/purge 、确切版本安装证明及每条追加记录的版本/record ID、SHA-256、大小；不接受任意主机路径。T03 必须先持久化业务意图及读写禁令，持久保存完整清除清单，并在文件动作期间防止追加证据或其他版本写入。storage 在取得资产与 Blob 锁后再次读取意图，拒绝变更后的计划。

版本目录移入 `trash/<trash_id>/versions/<version_id>`，追加记录移入同条目的 `records/<version_id>`；恢复回原稳定 ID/版本号目录。名称占用和别名恢复归 T03，物理目录不以别名命名。移动前核验清单、逐件 SHA-256、文件集合与祖先路径，拒绝符号链接、未知文件及两边同时存在的目录；改名后刷新双方父目录。中断重试核验已落位目标。

`runtime.storage_file_actions` 只记录文件阶段。清除先保存完整且已验证（包括追加证据清单）的 ready 计划，随后仅删除该回收条目的版本/记录目录，部分删除重试继续同一计划；不删除共享 CAS。全部字节动作及刷盘完成才标记 done。`PendingFileActions` 向 T08 提供未完成计划，恢复仍须重新通过 T03 意图接口。它不是 T03 的业务回执或墓碑。项目/资产说明和历史控制记录保留用于追溯，不因清除某版本而删除。

## GC 与保留

`CollectBlob` 是 T08 的内部文件接口，由默认关闭的[生命周期调度器](lifecycle-scheduler.md)或本机 `lantai lifecycle` 调用，没有远程清除命令。必须提供 T03/T08 的 `GCSource`、真实提交 pin 和备份 pin 源；上传 pin 自动加入。缺任何接口即拒绝。

候选必须有稳定删除 operation 和首次候选时间，等待至少 24 小时。取得与上传、安装、复用相同的哈希锁后，先拒绝尚有 ready 文件操作的实例，再重新查询候选、全部权威 manifest 根以及 upload/commit/backup pin。`GCSource.Candidate` 必须在索引损坏/重建、未完成文件操作、未决证据引用或尚未完成权威对账时拒绝；台账根必须包括所有非清除版本、回收站和相关未完成操作。备份 pin 创建须通过同一哈希协调或维护屏障，完整备份复制结束前不得释放。

`ManifestReferences` 校验权威证明绑定的清单文件和文件条目，并检查 projects/trash/quarantine/install 暂存中是否存在未列出的清单或安装标记。发现未知根、损坏文件或查询错误均停止物理回收；`index.db` 的零引用、硬链接数量和墓碑中的哈希不能单独决定删除。删除意图持久化到 `storage_gc_deletions`，删除与目录刷盘后标 done；`PendingGC` 暴露待对账项。新验证上传在相同哈希锁内取消旧 deleting 意图，防止崩溃后的删除重试误删重新入库的字节。已完成/取消的旧 operation 不再删除同哈希的新对象。

## 版本化上下文与决议

上下文和决议复用普通 `doc` 资产：Markdown 作为冻结清单中的文件，`metadata.project_document` 使用 [`lantai.project-document/v1`](../../schemas/catalog/v1/project-document.schema.json)。推荐逻辑别名为 `context/...` 或 `context/decisions/...`；物理文件沿用稳定资产/版本目录，不再另存一份可变内容。

元数据包含 context/decision 类型、标题、global/project/asset_type 适用范围、Markdown 文件路径及被替代版本的永久引用。`CommitProjectDocument` 复用上传、预留、安装与提交；它只产生草稿版本，不写生效指针。`GetContextDocument` 固定本馆资产/版本，按当前权限及用途读取并核验 Markdown 字节和摘要，单篇最多 1 MiB；历史版本仍可追溯。生效后的全局适用文档仍要求调用者拥有来源项目读取权限。

T03 的 `ContextReviews` 提供一致的当前生效集合和不可变审定回执。回执绑定永久引用、manifest 摘要、review ID、批准者、生效时间和 revision；未接入时 `EffectiveContext` 明确拒绝，不以最新版本或文档自报 approved 代替。未审定新版不会改变生效集合；替代和撤销的裁决在 T03。`EffectiveContext` 组合所给快照，核对项目/类型范围、重复项和回执绑定，最多 100 篇/4 MiB，返回实际文档、确切版本和组合摘要；T05/T07 应把这个快照的引用和摘要记录到任务输入，不能执行时再解析浮动版本。在锁外读取的快照须在调用方持锁的最终接受边界用 `EffectiveContextUnchanged` 复核：调用方已持有 security_guard 时审定端口继承该锁而不重复申请，集合变化时调用方须重读或拒绝。

## 迁移与验证

新增 `main/0007.identity.collaboration.sql`、`runtime/0005.storage.lifecycle.sql`，既有迁移不改写。旧实例仍通过完整备份后的本机 `migrate` 升级；本轮不迁移真实实例。schema 和动作派生文档由 `scripts/generate.sh` 生成。

定向验证：

```sh
go test ./internal/identity ./internal/storage ./internal/catalog ./internal/contract/schema \
  -run 'TestHuman|TestMilestone|TestLifecycle|TestGC|TestContext|TestExamples'
scripts/check.sh
```

测试涵盖批次换目标、插件配置绑定、过期和换会话重验证、当前权限撤销、部分回执恢复、跨项目进度拒绝、移动后刷盘失败恢复、共享 Blob、GC 等待/全部 pin/损坏与遗漏清单/旧删除取消，以及上下文未审定与历史版本。T03/T05/T09 与 T08 调度的真实接线测试见各自契约；目标平台故障演练和 M2 独立 TEST/GATE 仍需对应任务提供证据。
