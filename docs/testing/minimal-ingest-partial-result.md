# 最小权限入藏的部分成功

## 实际复现

2026-10-11，固定发布源 `676de7aa5777e6e71e10d68ef98331b87776b8b8`，macOS arm64 的真实 Codex CLI 通过 stdio MCP 访问 NAS 部署。模型为 `gpt-6.1-sol`，推理 `xhigh`，服务档位 `ultrafast`。这里只记录去除主机地址、身份/资源 ID 和本机私有路径的证据摘要；完整脱敏调用日志另存项目私有知识库。

身份为专用沙箱中的 contributor，会话仅含 `read,ingest,self`。准备好的 113 字节合成文本 SHA-256 为 `df5e033579ef6b9c56e0e4d77d2e7a2471aae7085f64f1155dc1ede9654acb8f`。

| 实际调用 | 观察 |
| --- | --- |
| `whoami` | 符合专用项目、Agent、角色与 scope；空 `system_roles` 依现有 `omitempty` 省略 |
| `resource_search` | 入藏前为空 |
| `resource_push` | 唯一版本已提交；含精确引用和 `committed_at`，同时 `description_pending=true` / `description_error=FORBIDDEN` |
| `resource_read` | 以提交回执中的精确 asset/version 读取成功 |
| `resource_pull` | 同一精确版本下载至预先验证为空的目录；113 字节与 SHA-256 均匹配原样本 |
| `resource_search` | 可见同一资产/版本，著录修订为 0、标题回退到 slug |
| 虚构不存在项目的只读搜索 | 返回空列表；不是明确拒绝，不据此宣称真实跨项目隔离通过 |

首轮执行器因省略的空角色字段停止；另一次非交互 CLI 在调用服务前被本机工具审批策略拦截。这两次都未上传。明确既有授权的逐工具进程配置后才实际提交一次；发现著录警告后禁用 push，继续读取和下载原版本。未重传、删除、增加角色/scope 或续期。

## 契约判断与呈现修复

`catalog.afterCommit` 的后续著录失败不撤销已提交版本；`describeNew` 使用 `catalog.patch_own_metadata`，身份矩阵要求 `organize`。现有 `description_pending` / `description_error` 正确表达两个结果，不能通过减少授权检查来消除警告。即使省略 `describe`，首次著录仍会尝试生成。

修复仅增强呈现：CLI stdout 和 MCP 结构化回执、成功状态、永久引用保持不变，另提供明确的待完成说明与保留原恢复状态的指引。它不新建协议状态、不改变错误码、scope、权限矩阵或服务端提交语义。

`TestIngestPartialResult` 在隔离真实核心中覆盖省略/提供标题两种输入：只授予 read/ingest/self 的 Agent 通过真实 stdio MCP 入藏，著录仍被拒绝；经真实 CLI 以同一输入/state 取回同一回执，上传和版本数保持 1；精确读取、下载仍成功。包测试另覆盖正常回执不警告、非提交错误不误报、未登记错误文本不回显。

```sh
go test ./internal/client ./internal/cli ./internal/mcpserver ./tests/integration -run 'TestCommitNotice|TestIngestPartialResult' -count=1
```

该回归与原 NAS 观测分别留存。修复未部署回 NAS，原实测不能冒充修复后产品实测；开发测试不等于完整 TEST-M2-11 或 GATE-M1/M2 验收。
