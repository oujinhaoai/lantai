# 远程 CLI 与可恢复文件客户端

`internal/cli.Run(ctx, args, stdout, stderr) int` 接收含命令名的参数，由 `cmd/lantai` 组装。`internal/client` 只调用公共 REST 与服务端返回的传输 URL；不导入领域服务，不读取服务端的数据目录，MCP 复用相同 HTTP client，任务等入口直接调用已组装的领域服务。M2 新增命令见[手动执行与远程协作](manual-execution.md)。公共接口以 [OpenAPI](../../api/openapi.yaml) 为准。

## 连接与凭据

所有命令使用 `--server https://gateway.example` 或 `LANTAI_SERVER`。网关必须是没有用户名、路径、查询参数的 HTTPS origin；`--allow-http` 仅显式允许 loopback HTTP 开发入口。客户端不提供跳过 TLS 校验开关。

普通 JSON 与文件传输各有独立的连接池：JSON 总超时 30 秒、每 origin 最多 16 连接；传输总超时 24 小时、最多 4 连接；拨号与 TLS 握手 10 秒、响应头 30 秒。调用方可通过 `client.Config` 调整合理的正数上限，传输连接数最多 64。取消 context 会中止请求，下载保留已同步的临时文件。当前 CLI 顺序处理文件和分片，以有界内存流式传输；不接受本机自报身份优先级。

两个连接池均不采用 `HTTP_PROXY` / `HTTPS_PROXY`，包括私网与内网域名，避免局域网传输意外绕行。当前没有显式代理配置。任何 HTTP 重定向均不跟随，凭据不会因重定向跨 origin。文件 URL 必须来自当前服务器授权响应、属于相同 origin 且位于 `/xfer/`；相对 URL 按同一网关解析。上传只替换服务端 `part_url_template` 的 `{part_number}`，不推导内部地址。

会话凭据读取顺序为 `LANTAI_SESSION_TOKEN`、`--session-file`（或 `LANTAI_SESSION`）。不接受命令行明文 token、密码或验证码参数。会话文件绑定 origin，拒绝跨服务器复用；文件不允许符号链接。Unix 要求无 group/other 权限；Windows 使用当前用户专有 ACL 创建文件，并核对 owner 与访问条目，不能检查时拒绝读取。更安全的跨平台写法是 `client.WritePrivate`。凭据和上传恢复文件经临时文件同步后原子替换；不要将这些文件加入版本控制。

```sh
# credentials.json 是私有 JSON：{"name":"H-example","password":"...","code":"..."}
lantai login --server https://gateway.example --credentials-file credentials.json --session-file session.json
lantai whoami --server https://gateway.example --session-file session.json
lantai meta --server https://gateway.example

# token.txt 是私有原始长期凭据文件；可用 LANTAI_TOKEN 代替此文件。
lantai session exchange --server https://gateway.example --token-file token.txt --session-file session.json
lantai session end --server https://gateway.example --session-file session.json
```

`login` 和 `session exchange` 可用 `--input session-options.json` 显式给出 `scopes`、`projects`、`ttl_seconds`、`purpose`、`model`。服务端决定最终会话范围。token 保存到明确指定的私有文件，stdout 不返回 token 或 CSRF token。`session end` 撤销后清空指定文件中的 token。

## 命令与稳定输出

参数位于命令/子命令之后；公共参数可放在其余参数前后。所有成功输出均为一行 JSON；`--json` 显式声明这一默认行为。服务端结构化错误写 stderr，保留 `code`、`retryable`、`recovery_action`、`request_id`、`operation_id` 等；不打印网络错误中的 URL。输出删除 token、口令、一次性秘密和临时传输 URL，错误文本中的签名 URL 与请求凭据也会脱敏。调用者应按机器字段处理错误，不解析消息文字。

| 命令 | 参数与行为 |
| --- | --- |
| `meta` | 无需凭据，读取公共实例能力 |
| `whoami` | 读取当前身份与会话 |
| `project list` | `--limit`、`--cursor`、`--view brief\|full` |
| `project show` | `--name PROJECT_KEY` |
| `project create` | `--input project.json --idempotency-key KEY` |
| `types` | 可选 `--name TYPE` |
| `upload` | `--input push.yaml [--directory DIR] [--state FILE]`，上传并核验文件但不提交版本 |
| `upload status/cancel` | `--upload ID`，查询或取消服务端上传会话 |
| `upload check` | `--upload ID --input files.json`，显式调用授权范围内内容检查 |
| `push` | 与 upload 相同参数，上传后提交版本 |
| `commit` | `--upload ID --input commit.json --idempotency-key KEY` |
| `show` | `--asset ID [--version ID] --view brief\|full`，也支持 `--ref lantai://...` |
| `pull` | `--asset ID --version ID --directory DIR [--purpose archive_review]`，也支持永久 `--ref` |
| `metadata get` | `--asset ID` 返回当前 full 资产描述 |
| `metadata set` | `--asset ID --input patch.json --if-match '"REVISION"' --idempotency-key KEY` |
| `search` | `--query TEXT --project ID --type TYPE --limit N --cursor CURSOR --view brief\|full`，按需给出过滤条件 |
| `operation` | `--id ID`，对账稳定操作，不自动推进状态 |
| `plugin import/list/enablements/probe/commands` | 扩展包静态导入（`--input {asset_id,version_id} --idempotency-key KEY`）、登记与启用诊断、按 `--id` 重跑已授权探测、列出对本人生效的本机命令；启停经 `human prepare` |
| `ext install/list/remove/run` | 显式本机扩展：`--registry FILE`（或 `LANTAI_EXTENSIONS`）为私有注册表；`install --package DIR` 只登记服务器当前启用的精确包；`run <plugin-id> <command> --output DIR [--input-file F] [--arg A]` 复验摘要与启用后运行一次，不查 PATH/CWD；见[扩展包治理](extension-governance.md) |

`--ref` 在访问资源前核对 `/meta` 的 `instance_id`，不能把外馆永久引用当成本馆 ID。`show` 的默认视图是 brief；`pull` 始终读取 full 精确版本。分页由服务端授权过滤，CLI 不以本地缓存推断可见性。metadata 冲突返回原错误，不静默刷新修订或覆盖。

| 退出码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 服务端领域拒绝，包括无权限 |
| 2 | 参数、请求格式或 schema 错误 |
| 3 | 本机文件、网络、响应协议或客户端内部错误 |
| 4 | 冲突/前置条件失败（HTTP 409/412） |
| 5 | 认证失败（HTTP 401） |
| 6 | 服务端可重试错误或配额/限流；是否重试仍须读取 `retryable` |
| 130 | context 取消 |

## 上传与提交恢复

`push.yaml` 是本机命令输入，`project_id` 用于创建上传，其余字段传给 commit。它不是服务器 `manifest.yaml` 的替代格式。`content.files` 的 `path` 相对于 `--directory`，默认相对于输入文件目录。省略摘要时 CLI 流式计算 `sha256` 和 `size`；如果给出了摘要，真实文件必须与申报摘要和大小一致。

```yaml
project_id: PROJECT_ID
slug: sample
content:
  asset_type: doc
  files:
    - path: sample.txt
      role: source
  rights:
    usage: production
    license: CC0-1.0
    redistribute_raw: true
    noai: false
    sensitivity: normal
describe:
  title: Sample
```

```sh
lantai push --server https://gateway.example --session-file session.json --input push.yaml --state push.state.json
# 网络中断后运行完全相同的命令，保留 state；不要换 key 或另建上传。
lantai operation --server https://gateway.example --session-file session.json --id OPERATION_ID
lantai pull --server https://gateway.example --session-file session.json --asset ASSET_ID --version VERSION_ID --directory working-copy
```

`resource_push` 使用同一 `PushInput`：`input.content.rights`、`metadata`、`producer`、`describe` 和 `task` 是 JSON 对象，`uses` 是数组，不能把原始 JSON 转成字节数组。工具 schema 从嵌入的公共提交契约投影，保留字段、枚举和未知字段校验；只有本机计算的文件摘要/大小及服务端既定的可选字段作输入适配。`rights.noai` 与 `redistribute_raw` 省略时为 false；整个 `rights` 省略或为 null 时，新资产由领域拒绝，已有资产按当前规则继承。输入结构校验发生在创建本机恢复状态和远程上传之前；授权、依赖、生产者身份、提交和继承规则仍由服务端复验。

状态文件在首次网络写入前持久化两个独立的 create/commit 幂等键，绑定 origin、完整输入及文件摘要；服务端返回后保存 upload/operation ID。每次恢复重新核对本机文件与请求绑定，GET 上传状态并仅补缺少的分片。每个分片先按服务端大小流式散列，再以 `Lantai-Part-Sha256` 发送；complete 由服务端核对整个文件。不同输入不能悄悄复用原状态。状态旁的 `.lock` 用操作系统文件锁防止同一恢复文件的并发写入；崩溃自动释放锁，保留 lock 文件是正常行为。

发送提交前先保存 `committing`。响应丢失后读取原 operation，再以同一 commit key 重放，取回权威回执；不会生成第二个版本或自动换键。禁用 Go HTTP 客户端基于 `Idempotency-Key` 的隐式请求体重放，恢复流程由显式对账控制。若服务端返回需要恢复/人工处理的错误，保留状态并返回该错误。单独使用 `commit` 时调用者负责持久保存其输入、key 与 upload ID。

状态文件不保存会话或授权 URL。它包含业务请求摘要、对象 ID、键和已收到的版本回执，应按私有工作数据保存。取消上传是显式 `upload cancel`；context 取消只停止本机请求并保留恢复机会，不擅自销毁服务端内容。

## 精确拉取与中断恢复

pull 先取得 exact manifest，为每个文件申请当前会话的 read grant，核对路径、摘要、大小一致后才使用服务端 URL。新运行重新授权，不持久化短时 URL。已有相同内容文件会重新计算摘要后跳过；内容不同则拒绝覆盖。

下载写入同目录 `NAME.lantai-part`，绑定信息写入 `NAME.lantai-download.json`。稳定的 `NAME.lantai-lock` 跨进程文件锁覆盖读取、续传、校验与发布全程；同目标并发下载立即返回可见的本机错误，锁文件保留以避免锁定不同 inode。Range 恢复必须得到正确的 206 和精确 `Content-Range`；服务端忽略 Range 时不追加字节。完整大小和 SHA-256 通过后，以硬链接原子发布且不覆盖已有目标，再移除临时文件。摘要失败会把部分数据归零，避免后续继续使用损坏内容。目标文件系统必须支持同目录硬链接；不支持时保留已验证的部分文件并返回错误。

上传源和下载目标均使用 Go `os.Root` 限制在指定的工作目录内，符号链接不能逃出该目录。拒绝绝对路径、`..`、Windows 特殊路径、保留设备名、非 NFC 路径、大小写/目录冲突和恢复文件保留后缀。上传与下载均使用固定 256 KiB 缓冲区，不把素材整体读入内存。JSON 请求/响应与本机命令输入上限 8 MiB。

## 人员设置与管理命令

CLI 不自行确认 HumanGrant。管理员显式完成三步：

1. `identity challenge --input command.json`，输入含 `action`、`command` 和可选 `operation_id`，审阅服务端 challenge。
2. `identity verify --id CHALLENGE_ID --credentials-file code.json`，私有 JSON 含 `code`。
3. `identity execute --input granted-command.json --idempotency-key KEY`，输入含相同 action/command 与 `grant_id`。登记主体或签发凭据还必须指定新的 `--secret-file`。

`identity principal --id ID` 与 `identity members --project ID` 提供只读核对。受支持 action 由服务端白名单限制。

新成员使用 `session setup --credentials-file setup.json --session-file session.json`（私有 JSON 含 name/code），然后 `identity enroll --secret-file enrollment.json`、`identity password --credentials-file password.json`、`identity confirm --credentials-file code.json --secret-file recovery.json`。含 TOTP seed、配置 URI、恢复码或一次性凭据的完整响应只保存至指定私有文件，不输出到终端。已有秘密输出文件不被覆盖。发生响应丢失时应对账，不自动重新签发一次性秘密。

## 验证边界

包测试覆盖同源凭据保护、拒绝重定向、URL/凭据脱敏、私有文件、并发状态锁、连接池隔离、取消、分片丢响应、同键提交对账、Range 中断续传、摘要失败、目标路径约束、稳定退出码和条件更新。真实服务闭环由服务集成测试负责；这些开发测试不替代独立 TEST 卡要求的 1 GiB 压测、100 次接口 p95 或 Linux/macOS/Windows 实机行为证据。交叉编译只证明编译兼容。
