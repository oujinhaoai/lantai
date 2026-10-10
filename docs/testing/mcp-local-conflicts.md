# MCP 本地冲突错误分类补验

实际 Codex 在旧 main `e1cf5206b7b4611d5a7779438246643798b86eaf` 的 RUN-20261010-08 中发现：`resource_push` 改变输入却复用恢复状态、`resource_pull` 指向已有不同内容文件，两者安全拒绝后均返回 `INTERNAL/poll_operation`，没有可轮询的 operation ID。该历史记录保持原样。

客户端复用现有错误登记：请求摘要不同返回 `IDEMPOTENCY_CONFLICT`，非法状态格式、服务器绑定或缺少恢复键返回 `SCHEMA_INVALID`，已有不同内容目标返回 `PATH_CONFLICT`。均 `retryable=false`、`recovery_action=fix_request`；不制造操作 ID，不改未知错误兜底，不生成新幂等键，不覆盖目标，不调整服务端取消语义或超时预算。客户端恢复边界见[契约说明](../contracts/client.md)。

新测试使用真实 Go MCP SDK 的内存协议传输连接适配器与 HTTP 服务。修复前两个冲突均复现原错误；修复后检查已登记机器字段、无 operation ID、原状态逐字节保留且上传前无 REST 请求、原目标逐字节保留且 manifest/read grant 各一次而文件传输零次，不创建下载临时文件。另核对非法 schema/origin/create-key/commit-key 四子场景。定向 `internal/client`、`internal/mcpserver`、`internal/cli` 全包 race 通过。

新代码及测试属修复会话自测。协调会话独立代码复核、默认完整检查、最终 PR/main 十项 CI及实际 Codex 最终源码补验须各自绑定源码；内存协议与实际 Codex 的取消通知证据分开。工具报告超时不证明取消通知已送达或事务结束，恢复应保留原输入/state/key并按已有 operation 对账。旧 RUN08 观察到的未发送取消通知不能改写为成功。部署、设备故障、RPO、长稳、人工门禁均不由本项软件通过解除。

## 完整检查与源码绑定

继承Windows夹具修复93c9520后的最小分类候选，默认race/30m完整scripts/check.sh退出0，耗时1462.863秒；1184冻结文件前后无漂移，持共享排他锁串行。生产修复两文件与协调会话独立代码审阅的摘要相同；审阅后仅增HTTP方法断言并精确修正文案。完整结果、修复前原始失败和协议通过日志见[evidence/mcp-local-conflicts/result.json](evidence/mcp-local-conflicts/result.json)。记录仅绑定受测源码；最终候选/main及其CI和实际Codex补验另行确认。
