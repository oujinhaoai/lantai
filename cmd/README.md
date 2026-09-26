# cmd · 程序入口

归属：[T00](../docs/tasks/T00-foundation.md)、[T07](../docs/tasks/T07-api-clients.md)、[T08](../docs/tasks/T08-operations-deploy.md)。

开发时首先建立统一的 `lantai` 入口，承载服务端、CLI、MCP 和节点的子命令。这里只处理参数、依赖组装、信号和进程生命周期；领域规则和实现放在 `internal/`。

本轮不创建可执行源码，也不提供尚未验证的启动命令。新增独立二进制必须有实际部署需求。
