# 公共契约

状态：**M1 第一版，已实现并有自动化测试**（T00.1、T00.2）。本篇解释各模块共用的标识、摘要、幂等、错误、事件、操作阶段、跨模块接口、所有权与版本规则。字段定义以 [`schemas/`](../../schemas/) 中的 JSON Schema 为唯一权威，本篇不重复字段表；错误码与所有权表由生成器输出：[错误码表](error-codes.md)、[所有权](ownership.md)。执行与扩展宿主的公共约定见 [execution.md](execution.md)；实例生命周期、迁移与维护屏障见 [instance.md](instance.md)（T08.1）；身份、会话、授权与人类授权见 [identity.md](identity.md)（T01），动作与策略登记见 [identity-actions.md](identity-actions.md)；路径规则、清单、引用与说明修订见 [catalog.md](catalog.md)，内容库、上传、授权下载、安装与传输准入见 [storage.md](storage.md)（T02）；持久版本提交见 [ledger.md](ledger.md)，来源证据与用途判定见 [provenance.md](provenance.md)（T03），事件收录/消费/审计见 [events.md](events.md)，投影与重同步见 [query.md](query.md)（T04）。任务与流程协议见 [tasks.md](tasks.md)（T05），执行数据协议见 [execution.md](execution.md)（T06）；REST 与薄 CLI 见 [http.md](http.md)、[client.md](client.md)（T07）；共同备份/恢复见 [backup-restore.md](backup-restore.md)（T08），M1 内置扩展契约见 [extensions.md](extensions.md)（T09）；M2 扩展包治理与一次性宿主见 [extension-governance.md](extension-governance.md)，到期清除与 GC 调度见 [lifecycle-scheduler.md](lifecycle-scheduler.md)。

契约通过测试只说明规则被编码并可重复校验，不代表依赖它们的业务模块已经实现；各模块最终接线须换成真实实现并通过对应验收。

## 标识

- 所有对象 ID 是服务端生成的 ULID，只接受 26 位**大写** Crockford Base32 规范形式（`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`）。小写、`I/L/O/U` 等变体一律拒绝，保证一个对象只有一种拼写，可以直接作唯一键。ID 的时间成分只用于诊断，业务先后以 `revision` 为准。实现：[`internal/contract/ids`](../../internal/contract/ids/ids.go)。
- 子操作 ID 由父 `operation_id` 与稳定步骤键确定性派生：保留父 ID 的时间成分，后 80 位取 `SHA-256("lantai.child-operation/v1" 0x00 父ID 0x00 步骤键)`。同一父操作与步骤键永远得到同一子 ID，重试不会生成新子操作；步骤键由调用方稳定给出，例如 `item:<target_id>`。
- 永久引用 `lantai://<instance_id>/assets/<asset_id>/versions/<version_id>`。本馆入口可以省略 `instance_id`，持久保存（uses、审定、发布、任务输入、检查、迁移映射）前必须补齐。版本是否属于资产由所属模块判定，不符返回 `REF_MISMATCH`。
- 路径别名按代次记录（[`alias-generation`](../../schemas/common/v1/alias-generation.schema.json)），同一路径的代次单调递增、历史不可覆盖；当前占名由台账的 [`namespace-claim`](../../schemas/common/v1/namespace-claim.schema.json) 控制，孤立的别名历史文件不能抢占现名。

## 时间、修订与摘要

- 时间统一为 UTC 毫秒：JSON 中写作 `2026-09-26T10:14:34.123Z`（固定 3 位小数、以 `Z` 结尾），SQLite 中存 Unix 毫秒整数。领域代码只通过 [`clock.Clock`](../../internal/contract/clock/clock.go) 取时间，测试用可控时钟。
- `revision` 从 1 开始递增；条件写入的 `expected_revision` 为 0 表示对象必须尚不存在。
- 摘要有两种写法：字段名为 `sha256` 的值是 64 位小写十六进制；以 `_digest` 结尾或名为 `request_hash` 的字段写作 `sha256:<64 位小写十六进制>`。两者都不接受大写或其他算法。

## 规范化 JSON 与 YAML 解释

- 需要求摘要的 JSON 按 [RFC 8785（JCS）](https://www.rfc-editor.org/rfc/rfc8785) 规范化，输入先按 I-JSON 严格解析：必须是合法 UTF-8，拒绝重复键、孤立代理项、未转义控制字符和超过 128 层的嵌套。数字按 IEEE-754 双精度解释并按 ECMAScript 规则输出；不带小数点和指数的整数字面量必须在 ±(2^53−1) 以内，避免不同整数被舍入到同一个值。规范化不做 Unicode 规范化，路径等需要 NFC 的字段由领域命令先处理。规范化 Go 值（`CanonicalizeValue`）前先拒绝含非法 UTF-8 的字符串，因为 `encoding/json` 会把它们静默替换成 U+FFFD，使不同输入得到相同摘要。实现：[`canonjson`](../../internal/contract/canonjson/canonjson.go)。
- YAML 文档（`asset.yaml`、`manifest.yaml`、`extension.yaml` 等）按其 JSON 数据模型解释后再做校验与摘要：只允许单个文档；映射键必须是字符串且不重复；拒绝锚点、别名、合并键和自定义标签；数字规则与上面的 JSON 规则相同（未加引号且符合 JSON 数字语法的标量一律按数字处理，超出范围即拒绝，不会因为解析器把它当成浮点数或字符串而绕过）；嵌套深度只计映射与序列，与 JSON 一致；时间戳按原文作为字符串。实现：[`yamljson`](../../internal/contract/yamljson/yamljson.go)。

- schema 适配器在精确数值校验前限制原始数字表示：`json.Number` 最长 1024 字节、指数绝对值不超过 1024，原生浮点值必须有限；超限返回校验诊断，不进入依赖库的大数展开。JSON、YAML 和直接 `Validate` 均适用。保留原始数值文本进行整数与唯一性判断，不将极小小数误判为零。

## 请求摘要 `request_hash`

`request_hash = "sha256:" + hex(SHA-256(JCS(文档)))`，文档固定包含 `contract: "lantai.request-hash/v1"`、`command_type`、`project_id`（实例级命令省略）、`targets`（路径中的目标 ID，按命令定义的顺序）、`expected_revisions`、`input_versions` 与 `body`。

- 覆盖：规范化后的命令体、路径目标、预期修订与输入版本；用途、目标集合等语义字段都在其中。
- 不覆盖：bearer token、请求编号、trace ID 等传输细节；actor 已在幂等键作用域中，不重复参与。
- `body` 是所属命令完成领域规范化之后的表示；同一语义请求的重试必须得到相同 `body`。缺省字段与显式 `null` 视为不同请求。规则变化必须换新的契约版本。

实现：[`commands.RequestHash`](../../internal/commands/requesthash.go)。

## 幂等、回执与操作

写请求必须带 `Idempotency-Key`，作用域为 `(actor, project, command_type, key)`；实例级命令的 project 记为空串，避免 SQL 唯一约束对 NULL 失效。服务端为首次到达的请求分配 `operation_id`。

| 同一作用域再次到达 | 结果 | 副作用 |
|---|---|---|
| 没有回执 | 执行命令 | 一次 |
| 同摘要、已终结、在响应缓存期内 | 返回保存的结果；返回前按**当前**读取权限复核 | 不重做 |
| 同摘要、仍在进行 | 202，指向同一 operation | 不启动第二份 |
| 不同摘要 | 409 `IDEMPOTENCY_CONFLICT` | 无 |
| 同摘要、超过响应缓存期（24 小时） | 410 `IDEMPOTENCY_RESULT_EXPIRED`，附操作与对象引用 | 不当新请求执行 |

- **回执在业务所属库**：`command_receipts`、`operations`、`outbox` 三张基础设施表存在于 main、ledger、runtime 各自的库中，由命令所属模块在本库事务内通过 [`commands.Store`](../../internal/commands/store.go) 写入；没有全局业务回执库。Store 只写这三张表，只推进 `owner_module` 为本模块的记录。
- **短命令**（只改一个库）：`Store.Execute` 在一个事务中完成“查回执 → 判定 → 业务变更 → outbox → 终态回执”。handler 返回错误时整体回滚、不写回执（无效请求不占键）；返回 `failed` 结果表示已接受的业务失败，写终态回执，同键重放同一失败，修改请求须换新键。失败回执必须带登记的错误码，HTTP 状态取该错误码的登记值；成功回执只能是 2xx。数据库须由 `platform/sqlite.Open` 打开，写事务以 `BEGIN IMMEDIATE` 开始。
- **多阶段命令**（文件与数据库）：`Store.Accept` 持久接受操作并写进行中回执；各阶段用 `Advance` 推进；所属模块在业务提交事务内调用 `Complete` 同时终结操作、写 outbox、完成回执；放弃用 `Cancel`（原因必须是登记错误码）。
- 结果过期（410）的错误只带 `operation_id`，不附结果引用；调用方按当前读取权限过滤回执中的引用后再决定是否返回。
- 24 小时只限制完整响应缓存；回执与业务唯一映射至少保留到相关对象或墓碑的保留期，具体清理策略由运维模块定义。

### 操作阶段

`receiving → prepared → installed → committed` 为主线；`blocked`（前提变化，保留字节待处置）、`quarantined`（证据不足或内容不符，隔离）、`failed`、`cancelled` 为分支。允许的转移见 [`stage.go`](../../internal/commands/stage.go)；`committed`、`failed`、`cancelled` 之后不再有持久转移。`projected` 不写入数据库：对外视图在 `committed` 且该操作的事件已全部收录到 events.db 时显示为 `projected`，检索索引的落后量由查询水位另行报告。

`GET /api/v1/operations/{operation_id}` 返回 [`lantai.operation/v1`](../../schemas/common/v1/operation.schema.json)：阶段、结果引用（按当前读取权限过滤）、`retryable`、`next_action` 与精简原因，不返回令牌、签名 URL 或堆栈。进行中的操作以 202 返回 `operation_id`、阶段与状态地址。

## 错误模型

所有协议（REST、CLI JSON、MCP、adapter）使用同一错误信封 [`lantai.error/v1`](../../schemas/common/v1/error.schema.json)：

- `code`：登记在 [`error-codes.json`](../../schemas/common/v1/error-codes.json) 中的稳定错误码，一经发布不改名；
- `message`、`hint`：人读说明与修复提示，不得包含凭据、签名 URL、无权来源的路径或哈希；
- `retryable`：不做其他动作、以同一幂等键原样重试是否可能成功；
- `recovery_action`：`none`、`retry`、`reauthenticate`、`refresh_state`、`poll_operation`、`reconcile`、`human_action`、`fix_request`、`upload_content`、`resync`，客户端遇到未知值按 `none` 处理；
- 可选 `retry_after_ms`、`request_id`、`operation_id`、`refs`（调用者有权看到的引用）与 `details`（`reason` 为小写下划线原因，另可带 `pointer`、`message`、`ref`、`data`）。

每个错误码的 HTTP 状态、`retryable` 与 `recovery_action` 在注册表中固定，构造错误时不能改写；注册表同时列出旧草案中已被替代、服务端不得再返回的错误码。完整表见[错误码表](error-codes.md)。无权读取的对象统一返回 404 `NOT_FOUND`，不泄露存在性；内容哈希未授权与未上传统一返回 `BLOB_GRANT_REQUIRED`。

## 事件信封与 outbox

- 事件使用 [`lantai.event-envelope/v1`](../../schemas/common/v1/event-envelope.schema.json)：`event_id`、`event_type`（小写点分，至少两段，第一段登记在[所有权](ownership.md#事件类型前缀)中）、`schema_version`（payload 主版本）、聚合类型/ID/修订、actor、session、project、operation、correlation、causation、发生时间与 payload。项目范围事件必须带 `project_id`；实例级事件省略它，只对有实例级读取权的调用者可见。
- 业务模块把信封与业务变更在同一事务写入本库 outbox，存储为规范化 JSON。relay 用 [`commands.ReadUndelivered`](../../internal/commands/outbox.go) 按源库写入顺序读取，先在 events.db 按 `event_id` 唯一收录，再 `MarkDelivered`；两步之间崩溃只会重复收录尝试，不产生第二个逻辑事件。
- `global_seq` 由 events.db 收录时分配，只表示收录顺序；跨来源的业务顺序按 `aggregate_revision` 与因果 ID 判断。

## 跨模块接口与桩

为让资源存储（T02）与台账（T03）等模块各自开发而不互相等待整卡，公共接口与内存桩如下。桩按契约语义实现，同时提供可对任意实现运行的**契约测试套件**；真实实现接入时必须通过同一套件，桩不能替代阶段集成。

| 接口 | 实现方 | 桩 / 契约套件 |
|---|---|---|
| [`authz`](../../internal/contract/authz/authz.go)：可信调用者上下文、`Authorizer`、`SessionVerifier`、`EpochSource`；传输档位由主体类别推导，不信客户端自报，委托上下文一律为批量档 | identity（T01，已实现：[`identity.Service`](../../internal/identity/identity.go)；`EpochSource` 由 [`operations.Instance`](../../internal/operations/instance.go) 提供） | `authztest.Static`：授予、撤权、会话、整馆恢复；`RunAuthorizerContract`（桩与 identity 都通过） |
| [`install`](../../internal/contract/install/install.go)：安装请求（含 catalog 渲染的清单文件）与 [`lantai.install-proof/v1`](../../schemas/common/v1/install-proof.schema.json) 证明、`Installer`（按 operation 幂等、只证明 installed） | storage（T02，已实现：[`storage.Service`](../../internal/storage/install.go)） | `installtest.Memory`、`RunInstallerContract`（桩与 storage 都通过） |
| [`commit`](../../internal/contract/commit/commit.go)：`Ledger`（Prepare → Commit、Cancel，最终接受边界复验当前授权与证明）、`Reader`（只读已提交版本与资产登记，按号与最新版本，不依赖索引）、`Namespace`（带代次的占名）、`Metadata`（说明修订的保留与生效，文件由 `RevisionVerifier` 复核）、`Projects`（项目登记） | ledger（T03，已实现：[`ledger.Service`](../../internal/ledger/ledger.go)） | `committest.Memory`、`committest.Revisions`、`RunLedgerContract` |
| [`rights`](../../internal/contract/rights/rights.go)：`Evaluator`（按当前证据判定版本能否用于某用途；无法完成核验返回 `RIGHTS_PENDING`，不默认放行） | provenance（T03.2，已实现：[`provenance.Service`](../../internal/provenance/provenance.go)） | `rightstest.Static` |
| [`pin`](../../internal/contract/pin/pin.go)：[`lantai.pin/v1`](../../schemas/common/v1/pin.schema.json) 保留记录与 `Held`（任一来源出错即视为仍被保留） | storage / ledger / operations | `pintest.Memory` |

T02 接入后契约有三处变化：安装请求带清单文件、证明记录其 `manifest_sha256`，路径长度按码点计（与 schema 一致）；台账契约补齐上表中的读取、占名、说明修订、项目登记与取消；新增用途限制查询。取舍见 [ADR 0007](../adr/0007-storage-layout-and-catalog-ledger-split.md)。

授权契约套件覆盖：授予与撤权对之后的判定立即生效、收窄的会话不能扩大、验证返回可信上下文、结束或到期的会话与整馆恢复之前的会话一律失效。台账与安装契约套件覆盖：Prepare/Commit 按键与 operation 幂等、同键异摘要冲突、撤权或旧会话提交被拒且版本不可见（操作 `blocked`，字节保留）、整馆恢复前接受的操作即使换新会话也须先对账（`OPERATION_NEEDS_RECONCILIATION`）、未经安装器签发的证明被拒、证明不符或内容损坏时隔离、基线落后、同资产进行中提交返回 `RESOURCE_BUSY`、取消已终结的操作不释放别人的占用、占名冲突、取消后版本号不回收、`REF_MISMATCH`，以及安装端的幂等、冲突、缺内容、大小不符、路径越界与非法 UTF-8、只认本安装器签发的证明与隔离。

## 所有权与锁顺序

- 每张业务表只有一个模块写入，表名以模块名加下划线开头；模块只能在登记允许的库里建表；跨模块、跨库不联表、不建外键、不共用 SQL 事务。登记与检查函数在 [`internal/contract/ownership`](../../internal/contract/ownership/ownership.go)，迁移工具应对每个迁移创建的表调用 `CheckTable`；表格见[所有权](ownership.md)。
- 写入口：所有写入（前台命令与后台任务）经 [`commands.Gate`](../../internal/commands/gate.go) 取锁；实例未开放写入时立即返回 `MAINTENANCE_MODE`，维护先关闭写入再取屏障独占锁，见 [instance.md](instance.md#状态就绪与维护屏障)。
- 单核心进程内的统一取锁顺序：**实例维护屏障 → security_guard → project/namespace → asset → task/attempt → blob**，同层按键排序。业务提交取屏障共享与 security_guard 读；维护与备份取屏障独占；撤权、角色/策略变更、限制激活、锁定、审定撤销与敏感授权撤销取 security_guard 写。一次取锁给出完整集合；已持有锁时只能用 `Acquire` 返回的 ctx 继续取顺序更靠后的键，反向嵌套返回错误（顺序检查沿 ctx 传递，换用其他 ctx 会绕过检查）。等待中的写者会阻止后到的读者，撤权与维护不会被持续的提交饿死。`security_guard` 与领域锁内不做上传或远程网络调用；写上传暂存时仅持维护屏障共享锁，维护等待这些在途写入排空。实现：[`commands.Coordinator`](../../internal/commands/locks.go)。
- 这些锁只在一个核心进程内有效；将来多个进程写同一数据根时必须先换成可验证的共享串行化机制。

## Schema 版本与兼容

- 契约标识为 `lantai.<name>/v<major>`；schema `$id` 统一以 `https://github.com/oujinhaoai/lantai/raw/main/schemas/` 开头，校验时只从仓库内嵌文件解析，不访问网络。登记见 [`schemas/index.json`](../../schemas/index.json)。
- 同一主版本内只做兼容变更：增加可选字段、放宽输入约束、在声明为可扩展的枚举中追加值（客户端按未知值的约定处理）。增加必填字段、删除或改名字段、改变含义、收紧已有取值都要换新主版本；HTTP 破坏性变更开 `/api/v2`，旧版保留一个里程碑。
- 未知字段：客户端发给服务端的命令与请求一律拒绝（`additionalProperties: false`，返回 `SCHEMA_INVALID` 与逐项 details）；服务端响应可在同一主版本内增加字段，客户端必须忽略未知字段；持久记录与证据只追加，读取方保留未知字段不改写。扩展元数据只能放进 schema 明确允许的扩展位置，不能绕过权限、路径、哈希等安全字段的校验。
- 可空：只有“缺省”与“null”含义确实不同的字段才允许 `null`；其余用可选字段表示“未提供”。
- 每个文档 schema 都要有正反例：[`schemas/examples/`](../../schemas/examples/) 中每个样例声明所用契约、是否有效，无效样例还声明预期失败的位置与关键字；测试逐一核对，错误信封样例还必须与错误码注册表一致。
- schema 校验只检查结构，不替代授权、幂等与领域规则；生成代码同理。

## 已知限制

- OpenAPI 生成的传输类型把时间映射为 `time.Time`，其默认 JSON 编码会省略末尾零毫秒，不满足契约的时间格式；服务端输出须使用契约包的编码（见 [`internal/apiv1`](../../internal/apiv1/apiv1_test.go) 的测试）。
- 平台相关结论（SQLite 行为、文件语义）目前只在开发机 macOS/arm64 上实测，其他平台结果以 CI 与各平台验收记录为准，见[开发与验证](../development.md)。

- [M2 T01/T02 协作适配](collaboration-foundation.md)：动作绑定人审批次、里程碑、回收/GC 文件接口和固定版本上下文。

- [审定、发布与协作读取接口](review-collaboration.md)：固定目标、证据接受、人审与发布、讨论、权限事件和收件箱的内部接口及集成边界。

- [M2 手动执行、检查作业与远程协作](manual-execution.md)
- [M2 扩展包治理、一次性宿主与本机命令](extension-governance.md)：静态导入、审定引用、HumanGrant 启停、受限探测、准入/排空/撤权、熔断、`lantai ext` 与 MCP 投影。
- [M2 到期清除与可恢复 GC 调度](lifecycle-scheduler.md)：默认关闭的 T08 调度器、作业登记与重试。
