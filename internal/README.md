# internal · Go 核心与内部适配器

所有服务模块目前只有[职责与任务](../docs/architecture.md)，具体子包随实现创建。

- 领域：identity/policy、catalog/storage、ledger/provenance、events/query、tasks/workflow、agent_execution/jobs、extensions。
- 应用与适配：命令协调、HTTP、CLI/MCP 内部适配、节点与插件宿主、文件系统与 SQLite。
- 运维：实例启动、迁移、维护屏障、恢复与诊断。

每个领域模块拥有自己的仓储和表；跨模块仅经明确接口调用。共享机制不成为共同写业务数据的工具箱。数据库迁移计划放在 `internal/platform/sqlite/migrations/`，按五库和表所有者组织，运行顺序由 T08 协调。

包内单元测试与源码相邻；跨模块测试在 `tests/`。只暴露当前调用方需要的接口，暂不建立通用 `pkg/`。

[T09 扩展平台](../docs/tasks/T09-extension-platform.md) 拥有插件 registry/manager/broker/host：main 保存登记、启用与配置，runtime 保存实例和健康；包审定仍引用 ledger，TaskRun/JobAttempt 仍属 T06。可信内置模块采用静态组装，运行时插件不能替换授权和提交核心。
