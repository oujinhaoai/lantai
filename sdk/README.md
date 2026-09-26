# sdk · 外部客户端与插件 SDK

归属：[T07 接口与客户端](../docs/tasks/T07-api-clients.md)。

M2 提供最小 Python SDK；其他语言在有实际调用方时增加。SDK 复用 `api/` 和 `schemas/` 契约，维护生成方式、版本与兼容性测试。手写扩展与生成文件明确分开。

Go 核心及 CLI 使用的内部 HTTP client 放在 `internal/`。SDK 只访问授权接口，不要求调用者共享服务端文件系统或五库。目前没有可安装 SDK 包。

[T09](../docs/tasks/T09-extension-platform.md) 按阶段维护插件协议辅助库：统一清单与请求/结果校验，M2 的一次性处理器文件协议和显式 CLI 薄封装；M3 官方/声明式 UI 及未来按需网页 broker 另行扩充。SDK 不注入原始数据库、长期凭据或人审能力，不提供另一套业务状态机或通用 DI 容器；各语言有实际实现者再增加。实现规格见[扩展设计](../docs/extensions.md)。
