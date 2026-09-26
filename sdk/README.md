# sdk · 外部客户端与插件 SDK

归属：[T07 接口与客户端](../docs/tasks/T07-api-clients.md)。

M2 提供最小 Python SDK；其他语言在有实际调用方时增加。SDK 复用 `api/` 和 `schemas/` 契约，维护生成方式、版本与兼容性测试。手写扩展与生成文件明确分开。

Go 核心及 CLI 使用的内部 HTTP client 放在 `internal/`。SDK 只访问授权接口，不要求调用者共享服务端文件系统或五库。目前没有可安装 SDK 包。

[T09](../docs/tasks/T09-extension-platform.md) 维护插件协议辅助库：清单、请求/结果校验、宿主能力与可撤销注册，以及处理器/CLI/web broker 的薄封装。SDK 不注入原始数据库、长期凭据或人审能力，不提供另一套业务状态机；各语言有实际实现者再增加。
