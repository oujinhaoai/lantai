# api · HTTP 契约

归属：[T00 工程与契约](../docs/tasks/T00-foundation.md)、[T07 接口与客户端](../docs/tasks/T07-api-clients.md)。

这里维护 OpenAPI 3.1、代码生成配置和 HTTP 兼容性说明。JSON 接口、传输授权和错误模型使用同一份契约；生成器、传输校验与领域授权分别验证。

数据、事件和扩展 schema 归 `schemas/`；HTTP 请求与精简视图由 [`openapi.yaml`](openapi.yaml) 定义，清单、引用、错误等共享结构引用权威 schema。`scripts/gen/openapi` 生成自包含的 [`gen/openapi.bundle.json`](gen/openapi.bundle.json)，oapi-codegen 按 [`oapi-codegen.yaml`](oapi-codegen.yaml) 生成 `internal/apiv1` 传输类型；两者都是生成物，不手改。

M1 handler 在 `internal/transport/httpapi`，已接会话、最小身份管理、项目、类型、上传与版本提交、精确读取、授权下载、条件著录、检索和操作状态。API 与传输面使用同一认证策略；分面监听与合并模式复用相同 handler。分片上传和 GET/HEAD/Range 下载调用现有 `internal/storage` 流式实现。组装与监听生命周期在 `internal/application` 和 `cmd/lantai`；命令见[开发与验证](../docs/development.md)，行为和限制见 [HTTP 接口契约](../docs/contracts/http.md)。公开 SDK 仍归 `sdk/`，内部 client 归 `internal/client`。

M2 已接入任务/执行、审定、生命周期、讨论和权限过滤事件长轮询，见[实际接口](../docs/contracts/manual-execution.md)。动态扩展路由尚未启用。后续扩展仅注册 `/api/v1/ext/<plugin-id>/` 内明确的方法与 schema，经核心统一认证、授权、配额与回执；不代理任意 URL，不开放运维入口。包治理归 [T09](../docs/tasks/T09-extension-platform.md)，通用 service 调用仍由所属领域保存副作用 intent/receipt，见[扩展设计](../docs/extensions.md)。
