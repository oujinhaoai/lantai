# CLI 与 MCP 下载链接

这些入口复用公共 REST 的读取授权与完整恢复包导出授权。它们只返回有期限的下载描述符，不内联文件，不创建公开分享或永久下载权限，不启动备份或执行恢复。

## 普通文件

```sh
lantai download-link --server HTTPS_ORIGIN --session-file PRIVATE_SESSION.json \
  --asset ASSET_ID --version VERSION_ID --path content.txt --purpose archive_review
```

也可用 `--ref lantai://...` 代替 `--asset` 和 `--version`。路径是精确版本内的文件路径。用途使用现有 read-grant 枚举；服务端仍检查项目、版本生命周期、权利和用途。Agent 可在已有权限内调用。

stdio MCP 工具 `resource_download_link` 的参数为 `asset_id`、`version_id`、`path`、`purpose`。无需启用本机工作区即可获取链接；文件传输继续由下载客户端执行。已有 `resource_pull` / CLI `pull` 可直接在工作区下载并验证整个精确版本。

## 整馆备份

`lantai backup` 是原有的服务端本机离线命令。新增远程命令另用复数 `backups`：

```sh
lantai backups overview --server HTTPS_ORIGIN --session-file PRIVATE_SESSION.json
lantai backups history --server HTTPS_ORIGIN --session-file PRIVATE_SESSION.json
lantai backups show --id JOB_ID --server HTTPS_ORIGIN --session-file PRIVATE_SESSION.json
```

先从任务读取 `package.sha256`。只有带 `package` 的完整任务具有自包含恢复包；任务 `complete` 表示当时验证通过，实际导出还会检查当前文件。

本人管理员使用同一正常人类会话，完成现有 `identity challenge` / `identity verify`。挑战输入为：

```json
{
  "action": "identity.backup_export",
  "command": {
    "kind": "download",
    "job_id": "JOB_ID",
    "package_sha256": "EXACT_64_HEX_SHA256"
  }
}
```

审阅服务器摘要与目标后，通过私有动态码文件验证。将返回的 `grant_id` 与同一包摘要放入批准输入：

```json
{"grant_id":"GRANT_ID","package_sha256":"EXACT_64_HEX_SHA256"}
```

```sh
lantai backups download-link --id JOB_ID --input PRIVATE_APPROVAL.json \
  --idempotency-key SAVED_KEY --server HTTPS_ORIGIN --session-file PRIVATE_SESSION.json
```

stdio MCP 的 `backup_read` 不带参数时读取概览，带 `job_id` 时读取精确任务；`backup_download_link` 接收：

```json
{
  "job_id": "JOB_ID",
  "approval": {"grant_id":"GRANT_ID","package_sha256":"EXACT_64_HEX_SHA256"},
  "idempotency_key": "SAVED_KEY"
}
```

MCP 仅接收已签发的精确包批准，不提供身份挑战、动态码验证、备份启动、计划修改或恢复工具。首次兑换必须使用批准绑定的原会话。对于已经成功执行的批准，既有身份幂等协议允许同一人以相同内容及幂等键重放，并重新核验当前管理员权限；即使原批准已到期，也可为当前有效的正常人类会话重新签发短期下载能力。这是原审批意图的重放，不是新的动态码验证。GET 始终绑定本次能力签发所用的会话，原链接不能转交另一个会话；人类批准也不能转交 Agent。Agent、委托会话、非管理员及维护控制身份继续拒绝。普通 Agent 如需业务文件，使用普通文件链接。

## 描述符与下载

两种入口统一返回 `url`（完整网关 URL）、`method`、`headers`、`expires_at`、`sha256`、`size`、`path`、`range`、`session_required`、`sensitive`。普通文件 URL 含限时签名；恢复包的秘密只在 `headers.X-Lantai-Backup-Download` 中。描述符不含原会话 token。

用相同会话执行 GET：携带 `Authorization: Bearer <原会话>` 和描述符中的所有请求头；只信任配置的 HTTPS origin，不跟随重定向。恢复包链接最长 15 分钟，普通文件按 read-grant 期限；实际可用期还受会话到期、撤权、进程重启和文件状态影响。支持 Range；同一有效批准可重复下载。URL 单独复制给另一用户或会话不能取件。

如使用 curl，把 `url` 和所需 `header` 放入权限为 0600 的私有配置，以免凭据出现在进程参数中：

```sh
curl --fail --config PRIVATE_DOWNLOAD.conf --output downloaded.lantai
shasum -a 256 downloaded.lantai
```

配置包含 curl 的 `url = "服务器返回的完整URL"`、`header = "Authorization: Bearer 原会话"`，完整包另加 `header = "X-Lantai-Backup-Download: 描述符返回的短期令牌"`。保持 TLS 校验；私有 CA 用受信任的 `cacert` 文件配置，不关闭证书验证。下载后将字节数及 SHA-256 与描述符核对；HTTP 200 本身不证明客户端已正确落盘。

描述符、curl 配置和完整恢复包都按私密材料处理，不加入仓库、聊天报告或日志。若保存命令输出，先使用私有目录和 `umask 077`。下载过期时重新获取普通读取授权，或重新确认备份导出；不要自动重新登录、换幂等键发起备份或改成匿名链接。
