# 管理员整馆备份 API

这是本次开发新增的公共管理接口；不追认已发布 rc.4 或仅有 `backup-online` 的旧版本已经提供该能力。权威字段与响应见 [OpenAPI](openapi.yaml)，本页解释授权、原子性与重试。部署和使用见[备份管理](../docs/online-backup.md)。

## 授权与维护窗口

所有路由必须是交互式人类会话、当前系统管理员角色和 `admin` scope 的交集。普通成员、Agent、委托会话均不获权。浏览器仍需同源 Cookie；写操作校验精确 Origin 与 CSRF。私有 Unix socket 没有代理到公共端口。

`GET /api/v1/admin/backups` 返回可用性、当前任务、上次成功、计划与最近 20 次计划事件；`GET /history` 返回完整任务历史；`GET /{id}` 返回单任务。`GET /schedule` 返回当前计划及修订号。缺少备份盘仍能查询配置错误，返回的错误简码不包含主机路径或密钥。

维护期只额外放行上述精确 GET、`GET /api/v1/whoami` 和 `POST /api/v1/admin/backups/{id}/cancel`。这些路由仍进行实时会话、代际、撤权和角色核查；不续期、不创建身份写事务。新增的维护控制会话路由只在严格限定的控制面工作，见下节。取消请求体为 `{}`，不要求创建新的 HumanGrant。开始、续传、计划写入、登录、挑战签发及其他业务 API 都不能借此穿过维护屏障。

## 维护期间重新认证

普通管理员会话在复制过程中到期时，网页提供“备份维护身份验证”。管理员重新输入自己的名称、口令和动态码，经 `POST /api/v1/admin/backups/control-session` 取得独立的短期控制身份。普通 `/sessions/login` 继续遵守业务屏障；过期会话不会被续期或当作身份依据。

此入口只在任务已持久受理且新业务准入关闭时受理，覆盖 `draining`、`backing_up`、`cancelling`。控制认证成功不代表已经排空，复制仍须等待完整一致性屏障，取消仍须等待实际工作退出。它使用既有 Argon2 口令校验，重新核对当前活跃人类、系统管理员角色、口令版本、因子、认证与恢复代际；错误口令/动态码按主体、来源及全局上限限速。成功消费的动态码与普通登录、挑战共用持久防重放高水位，跨维护结束、重启和新任务也不能重用。

响应只通过 `Set-Cookie` 设置 `lantai_backup_control`，具备 Secure、HttpOnly、SameSite=Strict、路径 `/api/v1/admin/backups`；JSON 不返回 token。响应包括 `principal`、`expires_at`、独立 `csrf_token`、`scope: {job_id, attempt}` 和只含 `backup.read`/`backup.cancel` 的 `capabilities`。有效期固定十分钟，不自动续期；当前任务离开维护、attempt 改变、进程重启、主体/因子/代际失效都会使其失效。

`GET /control-session` 供刷新后核验和重新获取独立 CSRF；`DELETE /control-session` 要求精确 Origin 与该身份的 CSRF，退出时清 Cookie。上述路径均以 `/api/v1/admin/backups` 为前缀。这个 Cookie 仅可读取备份概览、历史、计划、当前任务，以及取消所绑定的当前任务；不能读取普通业务、换普通会话、创建 HumanGrant、开始/续传或修改计划。普通 Cookie 与它可以共存，备份读取/取消显式选择受限身份，不能把受限身份转换成普通 `authz.Context`。

重新认证只读五个业务数据库。身份模块把限速预占和 TOTP 高水位以私有原子写保存在 `backups/control-auth/state.json`，成功持久化后才签发内存令牌。文件带实例/密钥绑定和完整性保护，不含口令、动态码、种子或原始令牌；目录链、所有者、链接和替换均检查。该身份安全账本由普通新鲜认证共同遵守，损坏或写入失败时拒绝新证明并返回 `STORAGE_UNAVAILABLE`（`authentication_safety_ledger_unavailable`），不得删除它绕过防重放。已有有效会话仍经实时权限检查；它与可选备份任务日志的故障降级分开处理。

普通业务的恢复仍须等待复制与验证工作实际退出；结束流程先关闭新控制身份准入并排空正在完成的发证段，再释放一致性保护。控制身份失效后，网页返回普通登录，不自动获得业务权限。

## 人类确认与持久幂等

开始、续传和保存计划先按现有 `/api/v1/identity/challenges` → `/{challenge_id}/verify` 流程获取 HumanGrant。客户端完整显示服务器的 `summary` 和 `targets` 后才请求验证码。挑战中的 `action` 与 `command.kind` 对应如下：

| action | command |
| --- | --- |
| `identity.backup_start` | `{"kind":"start","drain_seconds":120,"timeout_seconds":7200}` |
| `identity.backup_resume` | `{"kind":"resume","job_id":"原任务ID","drain_seconds":120,"timeout_seconds":7200}` |
| `identity.backup_schedule` | `{"kind":"schedule","schedule":{"enabled":true,"timezone":"Asia/Shanghai","time":"02:00","revision":0,"drain_seconds":120,"timeout_seconds":7200}}` |

验证后向 `POST /api/v1/admin/backups`、`POST /{id}/resume` 或 `PUT /schedule` 提交 `grant_id` 与相同参数，以及 `Idempotency-Key`。计划字段在 PUT 的顶层。`revision` 是读取到的计划修订号，过期返回冲突，须刷新并重新确认。通用 `/identity/commands` 不执行备份批准，防止只有批准回执却被误认为已经发起备份。

身份模块先完成并释放批准事务，HTTP 适配器再以批准 operation ID 调用同一 core 协调器。协调器把请求摘要与任务写在同一次原子持久记录内；同键同内容返回原任务，同键换内容拒绝。断线或不确定结果用相同批准、内容和键重试，不生成第二次意图；维护期间可先通过历史确认任务。任务完成前响应为 202，不能当成成功备份。

`drain_seconds` 默认 120（1–3600），`timeout_seconds` 默认 7200（1–86400），排空时间不能超过全任务时间；显式 0 使用默认值。并发只保留一个活动任务，冲突不排队。所有入口共用业务排空、实例维护锁、原捕获身份验证和最终恢复流程。

## 计划与失败

计划采用 IANA 时区；镜像内嵌 tzdata。每天同一时区/本地日期最多一个自动任务，调度器以持久计划运行，不保存或借用管理员令牌。`authorized_by` 与 `approval_operation` 由服务端保存；每次接受自动任务前，在现有安全锁下重新核查批准者仍是活跃的人类系统管理员。批准者会话自然到期不影响计划，停用该主体或撤掉系统管理员角色会使计划停用并记录 `authorization_revoked`；新的管理员须重新确认后启用。任务记录 `schedule_revision` 以对应实际计划。每 10 秒轮询，90 秒内正常到期执行；忙时记录 `skipped_busy`，目录不可用记 `unavailable`。重启后已过期或超过 90 秒的运行槽记 `missed`，随后计算未来时刻，不排队补跑。夏令时回拨选第一个真实时刻；不存在的当地时刻记 `dst_missing`。计划改动不删除已用日槽。

`cancelling` 仍持有保护；只有工作实际退出后才恢复业务。重启把执行态变为 `interrupted`，不会自动续传。历史错误、源 pin 和不完整目标都保留；管理员修复后对原任务显式续传。完成必须通过原有的复制、完整性验证和源端确认，不能替换另一任务的完整副本。控制日志损坏只停用备份、保留证据；业务启动不会因此失败。

日志存在原数据根的 `backups/online` 与 `backups/management.json`，权限为私有目录/文件。普通 v1 数据快照不包含主密钥；本分支的新 `.lantai` 完整包包含恢复材料，按敏感文件保护。任务/计划历史不会自动删除；控制日志设置有界容量，达到上限安全拒绝新记录，须经运维审查后处理，不能直接删改防重记录。

整馆恢复与原卷重启不同：控制日志目录 `backups/` 不属于备份成品，空目录恢复不会复活原任务历史或自动计划。恢复完成并建立新管理员后，重新配置备份根并明确启用所需计划，避免旧自动授权随恢复复活。

活动备份期间，目录可用性显示最近一次准入检查的结果与 `checked_at`，`check_deferred=true` 表示延后重新探测。状态查询和取消不再次访问外部盘，避免坏挂载阻塞管理通道；任务结束后刷新会重新检查。

## 本分支新增完整包下载

完成任务的 `package` 字段包含整包标识、格式、SHA256、大小和 `sensitive: true`。它表示自包含敏感恢复包；旧历史中没有该字段的任务不具有同一含义。

先以 `identity.backup_export` 创建并验证 HumanGrant，命令为 `kind: download`、`job_id` 和精确 `package_sha256`，再调用 `POST /api/v1/admin/backups/{id}/download-request`。返回15分钟下载能力，同时给浏览器设置HttpOnly/Secure/SameSite=Strict Cookie。API客户端通过 `X-Lantai-Backup-Download` 头提交返回的短期token。`GET /api/v1/admin/backups/{id}/package` 仍要求同一有效管理员会话；支持Range、If-Range和稳定ETag，拒绝控制身份、跨会话能力和摘要不匹配。token不放入URL或浏览器持久存储。

批准请求和下载均重新校验最终包，不将历史 `complete` 当作当前可下载保证。普通文件、私有访问、服务属主、单链接、打开后文件身份及包内容/完成记录绑定不满足时拒绝导出。仍返回 `STORAGE_UNAVAILABLE`，但使用 `details.reason=backup_package_*` 区分包本身的错误；具体原因及处置见[完整包安全与状态](../docs/recovery-package.md#容器与状态)。错误不包含宿主路径或恢复材料；`backup_package_hardlinks` 只表示计数异常，不自动断定底层文件系统或第二链接位置。
