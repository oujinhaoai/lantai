# 本机备份控制接口

本接口属于含 `backup-online` 的开发版本，**不属于已发布的 v0.1.0-rc.4**。入口所有者为 `application.BackupControl`，复制与恢复协议仍由 `operations` 拥有。它不是公开 REST API，不增加业务身份、角色、HumanGrant 或网络端口。

## 权限和路由

仅 Linux/macOS 启用。core 配置 `LANTAI_BACKUP_ROOT` 和 `LANTAI_BACKUP_ROOT_ID` 后，在固定的 `/tmp/lantai-control-<uid>/<instance_id>.sock` 监听。父目录权限必须为 `0700`，socket 为 `0600`；服务端及 CLI 均核对目录/socket 的本机用户所有权和权限；操作系统服务账户及主机管理员是运维授权边界。容器内用 `docker compose exec -T core lantai backup-online ...`，无需读出或签发凭据。拥有服务账户、数据根或 Docker 管理权限的人本来就能读取实例；此接口不能交给普通业务用户。

HTTP Host 必须为 `lantai-backup`，`X-Lantai-Instance` 必须匹配实例 ID，带任何 Origin 的请求拒绝。它们是防误路由/浏览器访问的补充，不能替代 socket 权限。API、传输、loopback 诊断 TCP 监听均不注册这些路由。Windows 保留离线命令，尚无在线控制传输。

| 方法与路径 | 请求 | 结果 |
| --- | --- | --- |
| `POST /backup` | `{}` 或下述可选字段 | `202` 和持久任务 ID |
| `GET /backup/{id}` | 无 | `200` 当前状态；不存在 `404` |
| `POST /backup/{id}/cancel` | 无 | `202` 当前状态；正在执行则请求取消 |

创建请求仅允许 `resume`（已有在线任务 ID）、`drain_seconds`（默认 120，1–3600）、`timeout_seconds`（默认 7200，1–86400，须不小于排空期限）。严格拒绝未知字段、额外 JSON、目标路径和密钥。并发触发返回 `409`，不排队。请求最大 1024 字节。

响应字段：`id`、`backup_id`（形成备份后）、`state`、`error`（稳定简码）、`capture_started`、`created_at`、`updated_at`。在线任务 ID 用于查询/续传，`backup_id` 属于原有备份协议，两者不能混用。状态为 `draining → backing_up → complete`，或 `failed/cancelled/interrupted`。`cancelling` 仅表示已请求停止，仍持有互斥保护。`error` 当前包括 `timeout`、`cancelled`、`backup_failed`、`journal_write_failed`、`process_interrupted`。

## 互斥、取消与恢复

1. 持久记录任务，再关闭 API 和传输业务请求入口。入口计数涵盖整个请求，包括纯读取、下载、上传及请求内同步等待的 Job/子进程；新业务读写均返回 `503 MAINTENANCE_MODE`；从排空阶段起 `/readyz` 即不就绪，原因包含 `backup_maintenance`。
2. 在排空期限内等待已有业务请求与整轮后台任务退出（包括取得应用锁前后的阶段），再通过 `Instance.Maintain` 关闭领域 Gate，排空前台及后台已取得屏障的写入。后台后续写入被拒绝；不强杀已有业务任务，也不产生新业务重试。
3. 持有同一实例的维护能力，执行深度检查、outbox/审计同步、四库 `VACUUM INTO`、可变文件和固定 CAS 复制、全量校验、`COMPLETE`、pin 释放及水位确认。备份全程与业务互斥；不在 `copying` 时提前恢复。
4. 全部工作实际退出后才释放维护和业务入口。正常异常、取消、超时及 panic 路径执行同一清理。停止中的控制器不重新开放入口；实例原有停止/阻止状态不被覆盖。

HTTP 创建请求/CLI 断开不取消 core 任务。取消接口、总期限及 core 退出会取消工作上下文；文件系统阻塞调用不保证立即结束，在其返回前仍保持维护。排空失败可安全恢复原业务，因为尚未复制。超时/取消不是强制解锁。

控制记录保存于源数据根 `backups/online/<id>.json`，原有 pin/manifest 保存在原位置。启动仅将符合原子写协议名称且为私有普通文件的残留移至 `backups/online-interrupted/`；不采用临时内容、不覆盖既有隔离证据，正式日志的 ID/字段/重复键/文件类型继续严格验证。启动时，旧执行态记录转为 `interrupted`；正常实例启动检查先完成，没有工作在重启后自动继续。显式 `resume` 重新获得互斥保护；存在 `copying/complete` 的目标只续传/重验同一点，捕获未完成沿用既有重置日志。续传由 operations 核验绑定的源记录：合法 `RetiredDestination` 重置日志允许原目录/manifest 暂缺，并先核验保留目录的 backup/key/instance/state，再完成同一次重置。没有源记录、已发布冻结点丢失、异实例/异目标或不一致捕获仍拒绝，不能静默创建新点。取消/失败保留部分文件和 pin，可显式续传；若要放弃并释放 pin，服务停止后使用原有 `backup -cancel <backup_id>`。控制日志写入失败只在内存报告失败，磁盘旧执行态在下次启动转为 `interrupted`，不可据此宣称成功。

## 目标与密钥

root 必须预先存在、无符号链接别名、与数据根和密钥根互不包含。`.lantai-backup-root` 是预先配置的普通文件，内容须匹配配置的卷身份；core 不创建根目录/身份标记。每次触发检查身份、目录 inode、可写和 fsync，维护中保守估算所需容量（捕获范围字节的两倍加数据根配置的 `min_free_bytes`），复制阶段及完成前再次检查目录身份和空间余量。目标为 root 下自动生成的任务 ID 子目录。`create_host_path:false` 加身份标记避免常见的缺盘落回系统目录，但无法替代设备管理员确认真实挂载来源；错误地复制身份标记仍可能指向错误设备。

备份内容继续排除密钥、socket、锁、索引库及其他备份。core 只加载既有主密钥的 ID，不生成或导出密钥；旧密钥仍须独立安全保存。容量预估是保守门槛，不是磁盘预留；其他进程抢占空间、热拔盘和 I/O 错误仍可能导致失败。

续传成功还要求目标与源记录属于同一冻结点，不能用另一份完整且可独立验证的档案替代。核对 backup_id、instance_id、recovery_epoch、key_id、created_at、数据库矩阵、水位及文件/CAS 清单；源状态已 complete 时核对 completed_at。源状态 copying 只允许同一点完成状态的推进。控制任务一旦从源记录确定 backup_id，工作结果不能改写该绑定；拒绝替换时保留原 ID 和源记录。
