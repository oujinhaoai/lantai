# internal · Go 核心与内部适配器

领域模块目前只有[职责与任务](../docs/architecture.md)，具体子包随实现创建。已建立的公共部分（T00）：

- `contract/`：公共契约的 Go 实现——`ids`、`clock`、`digest`、`canonjson`、`yamljson`、`schema`（加载 `schemas/` 并校验）、`errcode`（由注册表生成）、`event`、`execution`（执行与扩展宿主的公共判定）、`ownership`（表/文件/事件所有权登记），以及跨模块接口 `authz`、`install`、`commit`、`pin` 与各自的 `…test` 桩和契约套件。
- `commands/`：命令上下文、请求摘要、操作阶段、幂等判定、业务所属库内的回执/operations/outbox 组件与统一锁顺序协调器；不拥有任何业务表。
- `platform/sqlite/`：SQLite 驱动的连接基线与能力探测；五库连接编排与迁移归 T08。
- `apiv1/`：由 `api/` 契约生成的 Go 传输类型，不手改。

规则见[公共契约](../docs/contracts/README.md)。

- 领域：identity/policy、catalog/storage、ledger/provenance、events/query、tasks/workflow、agent_execution/jobs、extensions。
- 应用与适配：命令协调、HTTP、CLI/MCP 内部适配、节点与插件宿主、文件系统与 SQLite。
- 运维：实例启动、迁移、维护屏障、恢复与诊断。

每个领域模块拥有自己的仓储和表；跨模块仅经明确接口调用。共享机制不成为共同写业务数据的工具箱。数据库迁移计划放在 `internal/platform/sqlite/migrations/`，按五库和表所有者组织，运行顺序由 T08 协调。

包内单元测试与源码相邻；跨模块测试在 `tests/`。只暴露当前调用方需要的接口，暂不建立通用 `pkg/`。

[T09 扩展平台](../docs/tasks/T09-extension-platform.md) 拥有扩展登记与宿主：M1 只做内置静态登记及来源字段，M2 保存 main 启用配置和 runtime 激活/宿主故障状态，引用 T06 invocation/attempt，运行一次性处理器并实施包治理；包审定仍引用 ledger，TaskRun/JobAttempt 仍属 T06。通用 DI、跨插件依赖和常驻服务容器按真实需求建设，不能从 registry 名称推导为 M1 前置。可信内置模块采用静态组装，扩展不能替换授权和提交核心；实现规格见[扩展设计](../docs/extensions.md)。
