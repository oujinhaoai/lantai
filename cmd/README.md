# cmd · 程序入口

归属：[T00](../docs/tasks/T00-foundation.md)、[T07](../docs/tasks/T07-api-clients.md)、[T08](../docs/tasks/T08-operations-deploy.md)。

开发时首先建立统一的 `lantai` 入口，承载服务端、CLI、MCP 和节点的子命令。这里只处理参数、依赖组装、信号和进程生命周期；领域规则和实现放在 `internal/`。

当前 [`lantai`](lantai/) 只包含已实现的 `version`（版本、契约与协议支持状态）与 `schema`（列出契约、校验 JSON/YAML 文档）子命令；服务端、业务 CLI、MCP 与节点子命令随对应任务加入，不预留空命令。用法见[开发与验证](../docs/development.md)。新增独立二进制必须有实际部署需求。
