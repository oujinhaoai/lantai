# 台账提交与恢复（M1）

实现位于 [`internal/ledger`](../../internal/ledger/ledger.go)，接口继续使用 [`internal/contract/commit`](../../internal/contract/commit/commit.go)。台账只访问 `ledger.db`，复用 `commands.Store` 的 operation、receipt 与 outbox；文件操作由 storage/catalog 完成。M1 不提供审定、发布、名称释放、删除与墓碑命令。

## 组装与权威边界

`ledger.New(ledger.Deps{DB, Gate, Authority, Clock, IDs})` 使用已经过实例迁移的数据库。`Authority` 组合身份授权与实例恢复代次；同一实例的 identity、ledger、catalog、storage 必须共用 `Gate`。先创建台账，再创建 storage/catalog，最后通过 `SetInstaller`、`SetRevisionVerifier`、`SetAcceptanceVerifier` 接线。缺失最终验收或安装复核时提交拒绝，不能把缺失依赖当作许可通过。

`ledger_projects`、`ledger_assets`、`ledger_versions`、`ledger_version_states`、`ledger_namespace_claims` 与修订相关表均由 ledger 独占写入。版本、资产与项目登记可由 `Reader` / `Projects` 精确读取；`Versions` 按版本 ID 分页枚举。未提交版本没有 `ledger_versions` 行，不因文件安装或索引出现而变成可见。上层必须另行核对读取权限。

## 版本提交

1. `Prepare` 在维护屏障与共享 security guard 内核对当前身份和项目状态。项目必须已登记且 active；资产追加须匹配最新已提交基线，没有其他在途操作。新资产按规范路径折叠键占名。同一事务保留资产/版本 ID、版本号、冻结请求与 prepared operation、进行中回执。
2. catalog 渲染冻结清单，storage 安装文件，返回 `install.Proof`。`MarkInstalled` 可以单独保存已复核的安装证明；正常 `Commit` 也先持久化 installed。号码一经预留不回收；重开数据库后按原幂等作用域读取原预留。
3. `Commit` 在同一个共享 security guard 内核对原主体、当前权限与 recovery epoch，复核安装证明，并调用核心 `AcceptanceVerifier` 检查冻结输入、来源用途与本操作 BlobGrant。该回调必须只读，不重新取前序锁，不执行外部同步钩子。
4. 最终 ledger 事务再次核对当前基线与占用，写已提交版本、初始 draft 状态、资产和 active 占名、终态回执及 `version.committed` outbox。事件中继失败不影响已经提交的版本。

撤权、过期恢复代次或输入限制变化使操作变为 blocked，字节保留；证明不符或安装损坏使操作进入 quarantined，并通过安装器隔离。逻辑隔离先持久化，物理隔离在释放安全锁后执行；崩溃后可再次调用恢复 hook 完成隔离。不能凭版本目录反推业务成功。

已提交 operation 的相同证明重放返回原版本，不执行安装或验收。重放仍校验当前读取权限，在维护期间也可进行。相同幂等键的不同请求摘要、不同证明重放、旧基线和占名冲突分别返回确定错误。同一 operation ID 不能绑定另一命令。`LookupPrepared` 只读取冻结身份，不重新解析 `@latest` 等浮动输入；它不返回完整命令响应缓存。

`Cancel` 只允许发起主体取消尚未结束的操作，释放当前操作持有的资产占用和未生效的名称。取消不回收版本号，新资产占名取消不消耗别名代次。已经隔离或终结的操作不能借取消释放别人的占用。

## 著录生效修订

`PrepareMetadata` 核对目标存在、目标类型对应的修改动作与预期修订：项目使用 `catalog.patch_project`，资产使用 `catalog.patch_metadata` 或拥有者限定的 `catalog.patch_own_metadata`。已有在途修订返回 `RESOURCE_BUSY`；旧修订返回 `PRECONDITION_FAILED`。修改内容在 catalog 保存为不可变文件，台账只保留内容摘要与待生效修订。

`CommitMetadata` 复验原主体、当前权限、恢复代次与修订文件，随后同事务切换当前指针、保存修订历史、完成回执并写 `ledger.metadata_committed`。已提交回执按当前 `catalog.read` 权限只读重放，维护期与保留读取权限的角色降级不要求再次修改。重放比较 operation、目标、修订、内容摘要与文件引用；重新构造证明时的 `WrittenAt` 不改变证明身份。普通著录不能改变许可快照，许可与追加证据规则由 provenance 提供。

## 事件与恢复 hooks

| 接口/事件 | 行为 |
|---|---|
| `ledger.project_registered` | 项目登记事务内发出，payload 含 project ID 与不可变 key |
| `version.committed` | 聚合为 version，revision 为 1；payload 固定项目、资产、版本 ID、版本号、清单摘要 |
| `ledger.metadata_committed` | 聚合为目标类型与 ID，revision 为著录修订；payload 包含 target kind、目标与项目 ID、内容摘要 |
| `OpenOperations` | 只读列出 ledger 尚未终结的 operation，供实例恢复分派 |
| `PreparedOperation` | 读取 operation 固定的提交身份与清单；调用方负责操作可见性授权 |
| `MarkInstalled` | 保存与冻结清单一致且经过安装器复核的证明，不产生已提交版本 |
| `QuarantineOrphan` | 按可信 operation ID 隔离待对账安装；已提交 operation 拒绝；没有台账记录也不会补记版本 |

事件消费按 event ID 去重，再读取权威状态；登记修订与著录修订不是一个计数器，不能把同目标的登记事件 revision=1 当作已经消费了第一份著录。outbox 搬运采用共享 `commands.OutboxSource`，与 events 收录不共用跨库事务。`Operation` 的 projected 仅说明源事件已被收录，不代表查询索引已经追平。

## 维护清单与 commit pin

`RecoveryInventory` 只读枚举未终结 operation 的原始命令与 prepared 版本/说明意图、全部已提交版本、全部已提交说明修订历史及 commit pins。原命令的主体、会话与 recovery epoch 不在恢复时改写；application 经 identity 重新核对后，才调用 catalog/ledger 既有最终接受路径。原授权不可用的操作保持待对账，不换键重做。共同备份核对在同一维护屏障下组合各模块接口，不跨所有者查询业务表。

`PinsFor` 从持久 prepared 文件清单与 operation 派生稳定的 child pin ID，不维护第二张可能过时的保留表。prepared、installed、blocked 持续保留且没有 TTL；已提交、取消、失败或明确隔离按操作终态时间释放。来源 intent 或 operation 缺失/不一致时失败关闭，不把读取失败当作无引用。已提交版本此后的长期引用仍由版本事实与不可变清单维持。

## 验证

运行 `go test -race ./internal/ledger ./internal/contract/commit/committest`。真实 SQLite fixture 运行公共提交契约套件，另测 prepared/installed/committed 重开、事务故障回滚、同键并发、operation ID 碰撞、维护期重放、security guard、未知/停用项目、缺少验收依赖与孤立安装隔离；恢复清单保留历史说明修订，commit pin 覆盖 blocked 重开、并发提交、显式终态释放和损坏意图失败关闭。包内授权、安装和修订文件使用契约桩；真实身份、文件、台账、事件与查询接线由 [`tests/integration`](../../tests/integration) 验证。模块测试通过不能替代实例恢复、平台故障与完整 M1 验收。
