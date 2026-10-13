# 常见问题与版本兼容

先保存错误码、发生步骤、时间和可公开的 request/operation ID，再按下表处理。不要提供密码、TOTP 密钥、恢复码、会话文件、私钥或完整私有配置；不要贴包含它们的截图。

## 先分清故障在哪一层

| 看到什么 | 在哪里检查 | 下一步 |
| --- | --- | --- |
| 浏览器 `/` 或 `/help/` 是 404 | 当前发行版与访问路径 | rc.2/rc.3 原包无业务首页；rc.4 应核对 web 静态包、只读挂载和 Caddy 路由，不能只换 core 或删卷重装 |
| `/setup/` 是 404 | 浏览器 `/api/v1/meta` | meta 200 表示通常已初始化；meta 未恢复则查原服务状态，保留凭据 |
| `Enrollment expired` / `Verification failed` | 首次设置第二屏 | 按[重新登记](setup.md)更换本次未完成的 TOTP 与恢复码草稿 |
| `TOTP_REPLAY` | 自己的验证器 | 登录或其他授权刚用过这一步动态码；等待下一时间步，不关闭 TOTP |
| `TOKEN_EXPIRED` | CLI/MCP 会话文件 | 人重新登录，Agent 用有效长期凭据重新 exchange；不重新初始化实例 |
| `TOKEN_REVOKED` | 管理员的授权记录 | 先确认停用、撤权、恢复或父会话失效，不自动重新签发绕过撤权 |
| `FORBIDDEN` / `NOT_FOUND`、项目为空 | 身份、项目角色、session scope/projects | 请管理员核对同一实例的实际权限；NOT_FOUND 也可能隐藏无权资源 |
| `HUMAN_PROOF_REQUIRED` / `HUMAN_GRANT_MISMATCH` | 人本人的 challenge 和命令文件 | 核对动作、目标、修订、同一会话与有效期，再按授权流程处理 |
| `IDEMPOTENCY_CONFLICT` | 原输入、state 与幂等键 | 保留并恢复原绑定输入，查原 operation；不自动换键制造重复提交 |
| `description_pending=true` | push/commit 回执 | 文件已提交，检查著录错误和 organize 权限，别重复创建资产 |
| `PATH_CONFLICT` | 本机下载目标目录 | 保留现有不同内容，改用空目录 |
| MCP 本地 `SCHEMA_INVALID`，无 operation ID | workspace、目录、state 父目录 | 建好目录或修正输入后重试，不查询不存在的远程操作 |
| `INSTANCE_LOCKED` | 服务主机的 core/备份/维护进程 | 让原进程正常释放锁，不删除锁文件绕过 |
| `restore_incomplete` | 恢复环境的 review 和证据 | 完成管理员重置和对账，再 restore-complete；不强制开放服务 |
| 网关 502 / core 反复退出 | NAS 原项目状态、挂载、脱敏日志 | 确认 core 就绪、网络与挂载正确；检查保留原卷，不重建空卷 |
| TLS 或域名错误 | 证书、系统时间、DNS、客户端 CA | 用证书覆盖的域名；私有 CA 用 --ca-file，不跳过校验 |
| SQLite 损坏 / FUSE 拒绝 | 实际数据盘和备份 | 停止写入，保留现场，转受支持存储与核验恢复，不放宽检查硬启动 |

## 核对客户端和服务端版本

在自己的电脑运行：

```sh
lantai version
lantai meta --server https://library.example:32444
```

version 反映当前这个本机二进制；meta 提供远端实例、契约和启用能力。源码构建可能显示 `(devel)`，此时需管理员从容器 image ID、OCI revision 和发行清单确认，不能拿本机版本冒充服务端版本。

| 组合或能力 | 兼容判断 |
| --- | --- |
| rc.3 服务 + rc.3 CLI/MCP（9bf4b56） | 历史发行基线；没有本手册的 Web/help 静态交付 |
| 旧客户端配较新服务 | 不假定全部可用；对照 meta 契约、发行说明，优先使用匹配发行客户端 |
| rc.2 → rc.3 镜像更换 | 仅是历史版本对；不能替代 rc.2→rc.4 直接升级证明 |
| 更高 schema / 数据格式回退 | 旧程序会拒绝；依据该版说明和兼容备份恢复，不能强行打开 |
| MCP 本地路径修复与 pull purpose | 使用 rc.3 客户端；purpose 是固定枚举，目录需预先存在 |
| 业务网页与帮助页 | rc.4 统一包包含；以对应 Release/manifest 和实际挂载核对，不代表原 rc.2/rc.3 自动获得 |

平台与文件系统要分别判断：有二进制或交叉编译通过，不代表对应 NAS、文件系统、容器 GUI 已验收。具体平台证据见[开发与验证](../development.md#平台验证状态)，真实目标机仍需安装、权限、数据盘及恢复演练。

## 向维护者报告问题

提供以下内容即可开始定位：客户端/服务器 revision（能确认的部分）、操作入口、错误码、request/operation ID、是否重试、是否有已提交版本回执、最后一次核验备份时间，以及不含秘密的最小复现步骤。无法确认的项目直接写“未验证”。

截图使用合成项目和账号，检查地址栏、终端命令、文件路径及侧栏。若疑似凭据泄漏，先通过现有管理员途径撤销相应凭据，公开报告仅描述撤销结果。
