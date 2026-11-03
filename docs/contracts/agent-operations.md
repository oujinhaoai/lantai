# Agent 操作目录与覆盖检查

`GET /api/v1/capabilities`、`lantai capabilities --limit 20` 和 MCP
`capability_list` 返回同一构建的 `lantai.agent-operations/v1` 分页目录。
目录只包含公开操作元数据；可公开读取，不返回项目、身份、凭据或业务数据。
每页最多 20 条，用最后的 `next_cursor` 继续，直到 `complete: true`。
`catalog_sha256` 标识准确目录；不透明游标绑定该摘要。未知游标明确拒绝；
升级导致目录变化后返回 `410 CURSOR_EXPIRED`，必须重新遍历并替换旧目录，
不能将升级前后的页面混合后宣称完整。

权威映射在 `api/agent-operations.json`。每个 HTTP operation_id 对应准确方法与
路径，列出实际 CLI、MCP、skill 入口。`cli_mcp` 仅说明两种适配存在，不表示业务
测试已通过；`cli_only`、`api_only`、`web_only`、`unimplemented` 保留真实缺口。
`human_only`、`local_only`、`not_applicable` 必须说明例外理由。没有交付的 skill
保持空数组。实例启用的领域能力继续以 `/meta` 为准；目录不能替代实时授权判断。

新增路由或客户端入口时同步登记；检查会核对 OpenAPI、实际 HTTP 路由和注册的
CLI/MCP 名称，并拒绝漏登记、重复、虚构入口或没有理由的例外。通用任意路径
HTTP 工具不能作为业务操作的完成证据。

## 完整操作与有界响应

发现时分别处理项目、当前资产著录、历史冻结版本及当前有效 rights。固定输入
和完整文件清单后执行；保存原 state、idempotency key、operation、revision 和
fence。响应未知时先按原操作对账；409/412 重新读取并显式修订，不能自动覆盖。
`description_pending` 表示文件版本已经提交，不换键重建版本。
最终验证包含当前权限与用途、准确版本、实际字节大小和 SHA256。

MCP 的**完整序列化结果**上限为 64 KiB，请求上限为 256 KiB。列表必须通过
稳定游标和显式 complete/truncated 表达完整性，空页有游标时仍继续。大型准确
对象使用有界文件清单页或显式配置工作区内的私有文件回执，回执含固定引用、
实际字节数与摘要；分段期间权限仍需复验。尚未有对应专用接口的操作在目录中
保留缺口，不能靠截断内容宣称完整。短时下载描述符只供同一获准会话使用并须
私有保存，不写入公开日志、提示词或 skill。

人类因素、人审、rights 放宽、安装认领、原生人类见证和现有备份/设备
HumanOnly 规则保持。普通 Agent 收到拒绝时交接准确动作、目标和所需授权，
不能取得人的凭据、转换用途或复用历史批准。
