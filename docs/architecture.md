# Lantai 第一版架构与一级目录

状态：**待确认的开发交接方案**。本轮确定目录职责与工作拆分，不创建可执行服务或声明技术验证通过。服务模块是进程内边界，不是独立部署单元。

## 运行与数据边界

- 一个 Go 核心进程，统一处理领域规则、文件提交与五库；网页独立构建、独立部署，节点在外部执行作业。
- 一个 HTTPS 网关入口；核心区分 JSON 接口面、文件传输面和仅限本机的运维面。长连接面在 M7 实现。开发合并监听可以配置，但不能改变授权语义。
- 传输层按服务端认定的用途区分交互与批量，分别设置并发、带宽及超时；CLI 的接口和传输连接池分开。
- 五库使用服务端本机文件系统。每张表归一个模块，跨模块/跨库不建外键、不联表、不共用 SQL 事务。
- 文件是内容、说明与证据的真源；权限、审定和运行态分别有自己的权威库。索引与快照不能覆盖权威事实。
- M1/M2 由单核心进程串行化最终授权与提交；多进程共同写入需要新的协调方案，不能复制进程内锁的假设。

<a id="top-level-layout"></a>

## 一级目录

只固定一级目录，具体 Go 子包在实施任务时按拥有的数据和接口创建。避免为未来能力提前建立空接口和包树。

| 目录 | 放什么 | 边界与首个使用阶段 |
|---|---|---|
| [`api/`](../api/README.md) | OpenAPI、HTTP 示例、生成配置 | M1；不放 handler 实现，不重复定义数据 schema |
| [`cmd/`](../cmd/README.md) | 可执行程序入口，计划统一使用 `lantai` | M1；只做组装、参数和生命周期，领域逻辑在 internal |
| [`deploy/`](../deploy/README.md) | 网关、容器、系统服务的通用模板 | M1 验证起；仅占位变量，无生产配置与地址 |
| [`docs/`](README.md) | 公开架构、ADR、任务、操作说明和验证摘要 | 本轮；内部研究和真实环境记录留私有位置 |
| [`internal/`](../internal/README.md) | 核心领域、应用命令、仓储和内部适配器 | M1；包名按模块职责定，不按数据库机械拆成服务 |
| [`plugins/`](../plugins/README.md) | 官方扩展源码、单一 extension.yaml 清单与测试；各端字段按阶段启用 | M1 内置 processor/validator 静态登记；M2 一次性检查插件和 CLI；M3 官方/声明式 UI，禁止直接写权威库 |
| [`schemas/`](../schemas/README.md) | manifest、事件、证据、插件和执行协议 schema | M1；协议定义一处维护，OpenAPI 可引用或经受控生成使用 |
| [`scripts/`](../scripts/README.md) | 可重复的开发、生成、构建和检查脚本 | T00 起；不放领域业务、不替代正式运维命令 |
| [`sdk/`](../sdk/README.md) | 外部客户端 SDK、插件 SDK 及其生成规则 | M2 最小客户端/插件 SDK；其余语言有使用方再加。核心内部 HTTP client 留 internal |
| [`tests/`](../tests/README.md) | 跨模块契约、故障注入、集成、端到端测试与合成 fixture | T00/M1；包内单元测试仍与源码相邻 |
| [`web/`](../web/README.md) | TypeScript/React 网页及其构建、测试配置 | M3；只走公开接口，不读取数据库或服务端目录 |

根文件为 `README.md`（中文默认）、`README.en.md`、`LICENSE` 和 `.gitignore`。`go.mod`/`go.sum`（固定 Go 工具链）、`scripts/tools/go.mod`（开发工具版本）与 CI 配置 `.github/workflows/` 归 T00，已建立，命令见[开发与验证](development.md)；`.github/` 只放 CI 配置，不计入一级目录；`NOTICE` 随实际引入的第三方代码建立，不预填归属声明。

数据目录由运行配置指定，位于源码仓库之外。部署模板与真实部署配置分别管理；`.gitignore` 只是减少误提交，不能代替内容审查。

## 服务模块与任务归属

| 任务 | 主要模块 | 拥有的数据或职责 | 首轮交付 |
|---|---|---|---|
| [T00 工程与契约](tasks/T00-foundation.md) | 组装、公共协议及工程工具 | schema/API 版本、依赖记录、接口边界、验证基础 | M1 起步，预留 Agent 协议；扩展只做清单/静态登记契约 |
| [T01 身份与安全](tasks/T01-identity-security.md) | `identity`、`policy` | main 身份/权限/授权与最小里程碑；会话状态归 runtime | 初始化、会话、授权与撤销 |
| [T02 资源与存储](tasks/T02-assets-storage.md) | `catalog`、`storage` | 原件、manifest、著录及上下文/决议修订；runtime 上传会话和内容授权 | 可靠上传、提交、精确版本读取 |
| [T03 台账与溯源](tasks/T03-ledger-provenance.md) | `ledger`、`provenance` | ledger 控制事实及讨论评论；不可变来源/检查证据文件 | M1 版本登记与来源，M2 审定发布与生命周期 |
| [T04 事件与查询](tasks/T04-events-query.md) | `events`、`query` | events、index；inbox 已读位置归 runtime | outbox 交付、重放、索引与重建 |
| [T05 任务与流程](tasks/T05-tasks-workflow.md) | `tasks`、`workflow` | runtime 中 Task/Seat/Attempt、租约、业务 Flow | M2 单席位与固定业务闭环 |
| [T06 执行与节点](tasks/T06-execution-nodes.md) | `agent_execution`、`jobs`、`node` | runtime 中执行映射、作业、节点观测与预算；检查点/结果证据文件 | M2 手动 Agent 和最小 worker，M3 自动化 |
| [T07 接口与客户端](tasks/T07-api-clients.md) | transport、CLI、MCP、内部 HTTP client | 传输适配；不另设业务真源 | M1 REST/CLI/传输，M2 MCP/最小 SDK |
| [T08 运维与部署](tasks/T08-operations-deploy.md) | `operations`、维护协调、诊断 | 配置、库迁移编排、恢复、GC、备份清单 | M1 可恢复实例及平台支持证据 |
| [T09 扩展平台](tasks/T09-extension-platform.md) | `extensions`、静态登记与分期宿主 | M1 内置登记与来源字段；M2 main 启用配置、runtime 激活/宿主故障，引用 T06 invocation/attempt；后续实例按需建立 | M1 清单/扩展点/静态登记；M2 一次性宿主/治理/CLI；M3 官方/声明式 UI |

`commands` 是共享命令上下文与执行协调代码，由 T00 定协议、T01 提供最终授权协调、T02/T03 实现提交流程、T08 接入恢复。它不拥有一套覆盖所有业务的回执库。业务模块在自己的事务中写结果、回执与 outbox；T04 提供交付及去重机制。

节点的确定性 JobAttempt 由 jobs 管理；Agent Task 的 Seat/Attempt/lease_fence 只归 tasks。agent_execution 使用这些租约，不再实现另一份领取机制。

扩展平台统一归 T09，插件实现规格以[服务端、客户端与网页扩展设计 v2.1](extensions.md)为唯一权威；T06.7 仅保留已转交的历史索引。包清单统一为 `extension.yaml` / `lantai.extension/v1`，processor 字段合入，CLI/Web 字段在 M1 只预留。包字节/不可变清单由 catalog/storage 保存，M2 包审定沿用 T03 台账，启用授权由 T01 提供。T09 拥有按阶段所需的登记/启用配置与激活/宿主故障状态，引用 T06 invocation/attempt；T06 拥有业务执行与结果接受规则，不建立第二个审批或业务运行真源。知识库只维护摘要、背景和任务进度，不能覆盖公开插件规格。

M1 只通过受控 Go 组装登记服务端/节点 processor、validator，记录产物/证据的插件 ID、版本和包摘要；不实现通用 DI、跨插件依赖图、多层配置或卸载容器。M2 一次性 processor 按文件协议 spawn/run/exit，启用前 capability probe 先获授权并受限执行；常驻 Describe/Negotiate/Configure/Health/Drain/Stop 只在真实服务需求出现后建设。进程外运行本身不构成安全沙箱。扩展走既定网关与授权接口，不覆盖核心路由、不自行绑定公共端口、不进入核心提交事务的同步钩子。

M3 只承诺官方/声明式网页贡献。第三方网页按需另行验证独立 origin、与主站不同的可注册域及目标浏览器 site 隔离、无会话凭据及有界 broker；内网特殊后缀须实测，精确 Origin/CSRF 检查仍必需。仅有同站点子域或 host-only cookie 不能替代完整隔离验证。

## 模块之间如何协作

1. T00 先定 ID、错误、命令回执、事件、授权检查与文件提交接口。协议变更由受影响模块一起审阅，版本与兼容性写入契约。
2. 各模块只修改自己拥有的表/文件，经应用接口协调其他模块。共享同一个 SQLite 文件也不代表可以联表或借用对方事务。
3. 创建版本以台账 committed 为可见点。文件安装成功尚不对外暴露；恢复按持久 operation 和清单核验，不能见目录就补一条成功记录。
4. 旧 attempt/fence、过期授权和被撤销权限在最终接受时拒绝。外部副作用不明时先对账，不能仅因租约超时就启动第二份执行。
5. 业务 Flow 负责验收和发布条件；Agent 动态计划仅在 Task 范围内执行。模型输出、执行成功和人审结论相互独立。
6. 自动审核建议、对话答复和 human grant 分开。敏感操作授权绑定具体 action、目标、revision 与 operation。
7. M2 的包审定、probe 成功和启用分别记录；一次性调用固定包/配置/输入版本，接受结果时同时核验任务 fence、扩展 activation 及当前授权。停用/撤权停止接单，排空跟踪在途进程；进程成功不代替业务验收，不为此强制常驻 Health/Drain 协议。

## 阶段门禁

M1：可靠入藏和精确读取、权限/撤权、事件/索引重建、空目录恢复、平台文件协议及流量隔离均通过；T09.1–T09.3 只验收统一清单、扩展点 ID、内置静态登记、来源字段及不支持能力拒绝。CLI/Web 不激活，通用 DI/依赖图/配置容器及进程宿主隔离均不列为 M1 前置，也不依赖业务 Flow 或已发布别名。

M2：制作 → 检查 → 独立质检 → 人退回 → 新版提交 → 人审 → 发布 → 下一 Agent 读取；再验证过期执行、取消、误删恢复及清除。T06 与 T09.4–T09.6 联合验证文件协议一次性检查、受限 probe、包治理、撤权/排空/熔断与显式 CLI；不要求常驻控制握手。手动 Agent 执行适配器仍需要幂等、检查点和取消状态测试。

M3：每个自动执行后端重新通过启动丢包、失联对账、预算与取消验收，再启用事件或定时触发。T09.7 提供官方/声明式网页贡献；T09.8 第三方网页、T09.9 常驻服务与 T09.3 的依赖生命周期后续部分按需立项，不属于 M3 完成条件。T09.10 随实际启用能力补契约/故障套件。DeerFlow 为可选后端，不是核心启动依赖。

网页、更多处理器及后续扩展列在[任务总表](tasks/README.md)，不混入首批 M1 承诺。跨平台支持以运行测试为准，交叉编译成功只证明构建成功。
