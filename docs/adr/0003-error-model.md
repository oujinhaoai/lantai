# 0003 错误模型与错误码注册表

状态：已采纳（2026-09-27）。错误码全表见[错误码表](../contracts/error-codes.md)。

## 背景

早期接口草案与后来的领域、提交协议对部分错误码的名称和 HTTP 状态不一致。错误码一经发布就不能改名，开工前需要统一来源。

## 决定

1. `schemas/common/v1/error-codes.json` 是错误码的唯一来源；Go 常量与文档表由生成器输出。每个错误码固定 HTTP 状态、`retryable` 与 `recovery_action`，构造错误时不能改写。
2. `retryable` 的含义是“不做其他动作、以同一幂等键原样重试可能成功”；`recovery_action` 给出下一步。生成器校验两者一致：建议 `retry` 必须可重试，可重试的错误只能建议 `retry` 或 `poll_operation`。
3. 与领域、提交协议冲突时以后两者为准：`UNAUTHENTICATED` 由 `AUTH_REQUIRED`/`TOKEN_EXPIRED`/`TOKEN_REVOKED` 替代；`FORBIDDEN_ROLE`、`POLICY_VIOLATION` 由 `FORBIDDEN` 等更具体的错误码替代；`LEASE_EXPIRED` 并入 `LEASE_STALE`（原因写在 details）；`ASSET_LOCKED` 使用 409；`FLOW_REQUIRED` 使用 422。
4. `MISSING_BLOBS` 由 `BLOB_GRANT_REQUIRED` 替代：区分“未上传”与“无授权”会泄露内容是否已存在。
5. T00 新增跨模块需要的错误码：`EXTENSION_ACTIVATION_STALE`、`INVALID_STATE_TRANSITION`、`MAINTENANCE_MODE`、`STORAGE_UNAVAILABLE`、`STORAGE_FULL`；保留接口草案中的 `TASK_ALREADY_CLAIMED`、`ASSET_IN_TRASH`。
6. 非结构化的内部错误对外一律为 `INTERNAL`，消息不带内部细节，原因只进日志。

## 后果

- 服务端不得返回被替代的错误码；注册表保留它们供客户端对照。
- 各模块新增错误码必须先改注册表并补样例，再使用。
