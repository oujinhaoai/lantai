# cmd · 程序入口

归属：[T00](../docs/tasks/T00-foundation.md)、[T07](../docs/tasks/T07-api-clients.md)、[T08](../docs/tasks/T08-operations-deploy.md)。

开发时首先建立统一的 `lantai` 入口，承载服务端、CLI、MCP 和节点的子命令。这里只处理参数、依赖组装、信号和进程生命周期；领域规则和实现放在 `internal/`。

当前 [`lantai`](lantai/) 包含 `version`、`schema`，本机实例命令 `init`、`migrate`、`doctor`、`recover-admin`，以及 `serve`。远程命令复用 `internal/cli`，通过 REST 提供身份/会话、项目/类型、upload/push/commit、show/pull、metadata/search/operation；没有直接读取服务端数据根的旁路。MCP、任务与节点命令尚未启用。用法见[开发与验证](../docs/development.md)及[薄 CLI](../docs/contracts/client.md)。新增独立二进制必须有实际部署需求。
