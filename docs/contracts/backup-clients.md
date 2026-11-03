# 备份远程客户端与真人边界

远程 CLI、普通 stdio MCP 和服务端本机运维连接同一备份协调器，适用身份保持原定义。
所有备份业务动作继续 HumanOnly：本人真人系统管理员、当前 admin scope，启动、
续传、计划变更和精确包导出另要求原会话中对应的精确 HumanGrant。普通 Agent、
委托会话和仅 read scope 被服务端拒绝。CLI 入口存在不代表获得批准。

## 远程本人 CLI

沿用 `--server`、私有 `--session-file`、可选 `--ca-file`，不在命令参数中放密码或 TOTP。

```sh
lantai backups overview
lantai backups history --explicit-completion --limit 20
lantai backups show --id JOB_ID
lantai backups start --input START.json --idempotency-key SAVED_KEY
lantai backups cancel --id JOB_ID
lantai backups resume --id JOB_ID --input RESUME.json --idempotency-key SAVED_RESUME_KEY
lantai backups schedule-get
lantai backups schedule-set --input SCHEDULE.json --idempotency-key SAVED_SCHEDULE_KEY
```

执行前，由本人通过既有 `identity challenge`、`identity verify` 或正式本人界面完成批准。
密码、TOTP、grant 和真人会话不交给 Agent。冻结的 challenge 内容为：

| 动作 | action | command |
| --- | --- | --- |
| 开始 | `identity.backup_start` | `{kind:"start",drain_seconds,timeout_seconds}` |
| 续传 | `identity.backup_resume` | `{kind:"resume",job_id,drain_seconds,timeout_seconds}` |
| 计划 | `identity.backup_schedule` | `{kind:"schedule",schedule:{enabled,timezone,time,revision,drain_seconds,timeout_seconds}}` |

获得 grant 后，START/RESUME 请求是 `{grant_id,drain_seconds,timeout_seconds}`；
SCHEDULE 请求是 `{grant_id,enabled,timezone,time,revision,drain_seconds,timeout_seconds}`。
可省略的期限沿用核心默认值，不能在确认批准后改动目标、期限或计划字段。
`timezone` 为显式 IANA 名称，`time` 为该区本地 HH:MM；revision 从 schedule-get 读取。
冲突先重读并由本人确认新意图，不能重用旧批准覆盖新修订。

开始/续传/计划写入必须显式保存原 idempotency key。响应丢失先查看原 operation
及任务历史，再用相同请求/key 对账；不得通过新键产生替代任务。续传仍传原 job ID，
原 attempt、捕获点与同点重验由协调器决定；`cancelling` 仅表示请求停止，必须等待
实际退出后状态变为终态。复制期间业务暂停，不添加强制解锁或取消后提前开放入口。

## 历史和只读 MCP

REST `GET /api/v1/admin/backups/history` 默认保留旧 `{items}` 响应。
`completion=explicit&limit=1..20&cursor=LAST_JOB_ID` 返回 `items`、`complete`、
`truncated:false` 和必要的 `next_cursor`。CLI 对应 `--explicit-completion`、
`--limit` 和 `--cursor`。未知游标返回 CURSOR_EXPIRED，重新替换此前所有页。
排序沿用创建时间/任务 ID 倒序；这是实时历史视图，新出现的任务在新一轮枚举读取，
任务状态须在操作前后按原 ID 重读，列表不承诺冻结快照。

MCP `backup_read` 保留默认 overview 和 `job_id` 精确读取，新增 `kind:"schedule"`
与 `kind:"history"`。历史使用 `limit`（最多20）和 `after`，直到 complete；要求
服务端宣告 `bounded_backup_history`，旧服务端应先升级。每次 GET 均重新授权。
普通 MCP 不提供备份 start/resume/cancel/schedule 写工具，不签发人类证明。
原 `backup_download_link` 仍须本人精确包批准及原 session/key；拿到链接不等于
下载成功。实际字节验证继续使用[下载链接契约](download-links.md)。

## 维护身份和本机入口

浏览器的 backupControlCookie 只允许原绑定 job/attempt 的状态与取消，不能开始、
续传、写计划或导出。普通 CLI 不导入或转换该 Cookie，也不刷新它的授权。
服务端本机 `backup-online` 使用受保护 Unix socket 与本机服务账户权限，属于
运维边界，见[本机备份控制](../../api/backup-control.md)；不向远程 Agent 开放主机权限。
这些开发入口不部署实例、不创建生产凭据、不替换证书、网关或恢复目录。
