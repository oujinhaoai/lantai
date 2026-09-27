# internal · Go 核心与内部适配器

领域模块目前只有[职责与任务](../docs/architecture.md)，具体子包随实现创建。已建立的公共部分（T00）：

- `contract/`：公共契约的 Go 实现——`ids`、`clock`、`digest`、`canonjson`、`yamljson`、`schema`（加载 `schemas/` 并校验）、`errcode`（由注册表生成）、`event`、`execution`（执行与扩展宿主的公共判定）、`ownership`（表/文件/事件所有权登记），以及跨模块接口 `authz`、`install`、`commit`、`pin`、`rights` 与各自的 `…test` 桩和契约套件。
- `commands/`：命令上下文、请求摘要、操作阶段、幂等判定、业务所属库内的回执/operations/outbox 组件、统一锁顺序协调器与维护屏障写入口（`Gate`）；不拥有任何业务表。
- `platform/sqlite/`：SQLite 驱动的连接基线与能力探测；`platform/sqlite/migrations/` 按库与表所有者登记五库迁移（T08 编排执行）。
- `platform/fsutil/`：数据根单实例锁、原子替换写入、磁盘余量与文件系统类别探测（T08.1）。
- `operations/`：实例生命周期（T08.1）——配置、实例标记、五库打开与按库迁移、兼容矩阵、维护屏障、启动恢复与就绪门禁、优雅退出、只读诊断。规则见[实例生命周期](../docs/contracts/instance.md)。
- `identity/`：身份与安全（T01 的 M1 部分）——主体与角色、本机初始化、口令与 TOTP、会话与委托、实时授权与最终接受协调、Challenge/HumanGrant 与凭据/策略管理命令、受限因子恢复；子包 `masterkey`、`totp`、`password`、`httpauth`（浏览器与 CLI 凭据的服务端防护）。规则见[身份与授权](../docs/contracts/identity.md)。
- `catalog/`：资源目录（T02 的 M1 部分）——项目与资产说明的条件修订、路径别名代次与引用解析、清单冻结与入藏组装；子包 `pathrule`（跨平台路径与名称规则）、`manifest`（版本清单、类型登记、确定性 YAML）。规则见[目录](../docs/contracts/catalog.md)。
- `storage/`：存储（T02 的 M1 部分）——内容库、上传会话与分片续传、内容复用授权、读取授权与逐请求核验的下载、版本文件安装与隔离、证据追加、upload pin 与到期清理、传输面 HTTP 处理器；子包 `fileop`（文件适配器与错误归类）、`transfer`（交互/批量传输准入）。规则见[存储](../docs/contracts/storage.md)。
- `apiv1/`：由 `api/` 契约生成的 Go 传输类型，不手改。

规则见[公共契约](../docs/contracts/README.md)。

- 领域：identity/policy、catalog/storage、ledger/provenance、events/query、tasks/workflow、agent_execution/jobs、extensions。
- 应用与适配：命令协调、HTTP、CLI/MCP 内部适配、节点与插件宿主、文件系统与 SQLite。
- 运维：实例启动、迁移、维护屏障、恢复与诊断。

每个领域模块拥有自己的仓储和表；跨模块仅经明确接口调用。共享机制不成为共同写业务数据的工具箱。数据库迁移计划放在 `internal/platform/sqlite/migrations/`，按五库和表所有者组织，运行顺序由 T08 协调。

包内单元测试与源码相邻；跨模块测试在 `tests/`。只暴露当前调用方需要的接口，暂不建立通用 `pkg/`。

[T09 扩展平台](../docs/tasks/T09-extension-platform.md) 拥有扩展登记与宿主：M1 只做内置静态登记及来源字段，M2 保存 main 启用配置和 runtime 激活/宿主故障状态，引用 T06 invocation/attempt，运行一次性处理器并实施包治理；包审定仍引用 ledger，TaskRun/JobAttempt 仍属 T06。通用 DI、跨插件依赖和常驻服务容器按真实需求建设，不能从 registry 名称推导为 M1 前置。可信内置模块采用静态组装，扩展不能替换授权和提交核心；实现规格见[扩展设计](../docs/extensions.md)。
