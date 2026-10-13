# 在 Codex 使用 Lantai MCP

目标：让 Codex 通过本机 Lantai 程序访问授权资源。前提：这台电脑已装 `lantai`，能访问实例 HTTPS 地址；管理员已登记 Agent、分配项目角色，并取得有效的 `agent-session.json`。先按[账户与权限](accounts.md)及[日常操作](daily.md)用 CLI 验证，不在聊天中提供任何令牌。

## 1. 在运行 Codex 的电脑准备文件

1. 记录 `lantai` 可执行文件的绝对路径。会话文件也使用本机绝对路径；它属于该电脑，不是 NAS 挂载目录。
2. 创建专用工作目录，例如 macOS/Linux 的 `/path/to/lantai-work`；Windows 可用 `C:/Users/Example/lantai-work`。所有示例路径均需替换。
3. 需要上传时先建立 `input/` 和 `state/`；下载时先建立空 `downloads/`。这些必须在 workspace 内实际存在。
4. 用同一个 session 文件运行 whoami，确认是预期 Agent。只给最小项目范围和 scope；完成标题等著录还需 `organize`。

成功标志：CLI 能查询目标项目和读取一个已知版本。`TOKEN_EXPIRED` 时由本人用原长期凭据重新换会话；不要让 Codex 索取管理员动态码。

## 2. 添加 stdio 服务

在 **Codex 的 MCP 配置**添加服务，传输类型选本机 **STDIO**。程序填 `lantai` 的绝对路径，参数分别填 `mcp`、`--server`、实例 HTTPS origin、`--session-file`、会话文件绝对路径、`--workspace`、工作目录绝对路径。

也可以用编辑器打开用户的 `~/.codex/config.toml`；保留已有配置，增加一个尚未使用的名称。示例：

```toml
[mcp_servers.lantai]
command = "/path/to/bin/lantai"
args = ["mcp", "--server", "https://library.example:32444", "--session-file", "/path/to/private/agent-session.json", "--workspace", "/path/to/lantai-work"]
```

私有 CA 场景在 args 末尾加 `"--ca-file", "/path/to/ca.pem"`，文件只含经可信核对的 CA 公钥证书，不能放签发私钥。没有 CA 参数时使用系统信任，不跳过 TLS。

配置只放路径，不放令牌或密码。保存后在客户端重启该 MCP 服务或重开会话。配置方法依据 [OpenAI 官方 MCP 文档](https://learn.chatgpt.com/docs/extend/mcp?surface=cli)，核对日期 2026-10-13；菜单会随客户端版本调整。此处没有替你修改 Codex 配置。

## 3. 先读后写验证

1. 在 Codex 的 MCP 服务列表确认 `lantai` 已连接；CLI 的 `/mcp` 可以查看当前连接。
2. 请求“使用 Lantai whoami 确认身份，然后列出可读项目，不进行写入”。
3. 选择一个实际授权的项目，让它搜索并通过 `resource_read` 读取精确版本。
4. 确认后再试 `resource_pull`，directory 用 workspace 内的相对目录 `downloads`，purpose 明确填 `archive_review` 等固定枚举。
5. 需要入藏时，让 `resource_push` 使用本机已存在的相对目录与 state 父目录；保存原输入、state 和回执。先用合成文件验证，不直接拿唯一原件试验。

成功标志：身份、项目、版本一致；文件只出现在授权 workspace 内，下载通过校验。工具结果只包含摘要和引用，不把原件字节或签名 URL 放入上下文。未配置 workspace 时不注册文件搬运工具。

## 故障处理

| 现象 | 排查与下一步 |
| --- | --- |
| 服务不能启动 | 核对程序路径、执行权限、session 文件权限和 origin，先用相同参数运行 CLI whoami |
| 已连接但没有 push/pull | 核对 `--workspace`；它必须存在且可访问，然后重启服务 |
| 本地 `SCHEMA_INVALID`，没有 operation ID | 建立 workspace 内缺失的目录，核对相对路径及 state 父目录；修好原输入再试，不轮询不存在的 operation |
| `PATH_CONFLICT` | 下载目录有不同内容；保留原内容，选择新的空目录 |
| `FORBIDDEN` 或 `NOT_FOUND` | 核对角色、会话 scope、项目范围、资源权利，不靠更换模型解决 |
| 超时或取消 | 不代表服务端未提交；保存原 state/幂等键，已有 operation ID 时先查询，再恢复原操作 |

Lantai 提供的是本机 stdio 适配器，通过 HTTPS REST 访问实例；不要把 NAS 地址填成远程 MCP URL，也不要在 NAS 开新 MCP 公网端口。MCP 不提供身份管理、终审、清除或任意 shell。它不会自动替你更新会话、签发权限或完成生产部署。
