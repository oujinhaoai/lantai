# api · HTTP 契约

归属：[T00 工程与契约](../docs/tasks/T00-foundation.md)、[T07 接口与客户端](../docs/tasks/T07-api-clients.md)。

开发确认后在这里维护 OpenAPI 3.1、HTTP 请求/响应示例、代码生成配置和兼容性说明。JSON 接口、传输授权和错误模型使用同一份契约；生成器、请求校验与领域授权分别验收。

数据、事件和插件 schema 归 `schemas/`，避免相同对象手工维护两份。handler 实现及核心内部 client 归 `internal/`，公开 SDK 归 `sdk/`。目前尚无可调用 API。

扩展仅注册 `/api/v1/ext/<plugin-id>/` 内明确的方法与 schema，经核心统一认证、授权、配额与回执；不代理任意 URL，不开放运维入口。包治理归 [T09](../docs/tasks/T09-extension-platform.md)，通用 service 调用仍由所属领域保存副作用 intent/receipt，见[扩展设计](../docs/extensions.md)。
