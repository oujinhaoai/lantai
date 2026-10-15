# 整馆备份管理与互斥备份

本页适用于新增整馆备份管理的开发版本。**已发布 rc.4 不具备这里的在线备份；已有 rc.5 在线命令也不等于具备本页新增的管理页面与每日计划。** 部署/升级仍需单独协调。部署时一次性挂载外部目录，此后管理员通过网页或同一 core 内的命令触发；备份与验证期间所有业务读写暂停。

## 部署时配置一次

1. 在实际备份文件系统上创建私有目录，确认外部盘/共享目录当前已挂载，目录不在数据或密钥根内部。服务账户须能创建文件及 fsync；容器默认 UID/GID 为 `65532:65532`。只配置该目录权限，不递归改已有磁盘权限。
2. 为这块备份位置选择一个稳定的非秘密标识，例如 `library-backup-volume-v1`，将它加换行写入目录下 `.lantai-backup-root` 普通文件（服务账户持有、0600、单一硬链接）。标记是防错配置，不是密码或设备身份证明，不能在缺盘后的空挂载点自动重建。
3. 在现有部署环境中设置 `LANTAI_BACKUP_DIR` 为上述宿主机目录，`LANTAI_BACKUP_ROOT_ID` 为该标识，部署时同时加载 `deploy/compose.yaml` 和 `deploy/compose.backup.yaml`（如已有 web overlay，继续保留）。bind 使用 `create_host_path:false`，不会自动创建缺失的宿主机目录。

```sh
# 使用经审查且含该功能的镜像，在部署/升级阶段执行一次。
docker compose -f deploy/compose.yaml -f deploy/compose.backup.yaml up -d
```

core 启动时加载备份管理状态。备份配置缺失、目录失联、身份不符、不可写或空间不足时，正常业务仍可启动，管理页明确显示不可用并拒绝备份。修复原挂载/权限/空间后刷新重试；不会在缺盘挂载点自动创建标记。损坏的控制日志也会停用备份，不会重置或静默忽略。

GUI 只接受一个 Compose 文件时，将 `compose.backup.yaml` 的两项 environment 和一个 bind **一次性合入原 core**，保留原数据/密钥卷、网络、UID 和只读根；不要为每次备份创建或切换维护服务：

```yaml
services:
  core:
    environment:
      LANTAI_BACKUP_ROOT: /var/lib/lantai-backup
      LANTAI_BACKUP_ROOT_ID: ${LANTAI_BACKUP_ROOT_ID:?set the provisioned backup volume identity}
    volumes:
      - type: bind
        source: ${LANTAI_BACKUP_DIR:?set an existing private directory on the backup filesystem}
        target: /var/lib/lantai-backup
        bind:
          create_host_path: false
```

只在原私有环境文件增加 `LANTAI_BACKUP_DIR` 和 `LANTAI_BACKUP_ROOT_ID`；示例片段用于增量合并，不能替换整个原配置。首次加入挂载需要在经批准的升级/部署窗口重建 core；之后正常备份使用管理页，不再改 YAML，也不手工停启 core。只读根容器还必须给 `/tmp` 提供可写 tmpfs；模板明确设置 `mode=1777`，让 UID 65532 在容器重启后也能建立私有 `0700` 控制目录和 `0600` socket。目录应由服务账户持有且为 `0700`（或经过核验的等效私有 ACL）；本版不会代替管理员纠正共享可写目录。它与数据根、密钥根互不包含，密钥不能存放其中。

原生部署在 core 的服务环境设置 `LANTAI_BACKUP_ROOT=<备份目录绝对规范路径>` 和 `LANTAI_BACKUP_ROOT_ID=<标记内容>`。管理 HTTP API 不依赖本机 socket；Linux/macOS 另支持私有本机控制 socket。Windows 的管理备份须以相应平台验证为准。

## 管理员日常操作

以自己的交互式人类管理员身份登录，并在登录时明确选择管理权限。会话须同时具备系统管理员角色与 `admin` scope；普通浏览会话、普通成员和 Agent 无权读取整馆备份状态或触发整馆备份。打开“备份管理”后：

1. 查看目录可用性、当前阶段、上次成功、历史任务与失败原因。
2. 点击“立即备份”，核对服务器生成的整馆停读写说明与目标，再输入当前 TOTP 完成敏感操作确认。
3. `draining` 等待已有请求和后台写入退出，`backing_up` 进行复制与验证，`complete` 才表示成功。没有可依据的百分比时不显示伪造进度。
4. 取消按钮只发出停止请求；`cancelling` 仍处于维护状态，直到实际工作退出。该按钮继续校验当前管理员、会话、Origin 和 CSRF，但不需要在冻结期间重新写入人类确认记录。
5. 对 `failed`、`cancelled`、`interrupted` 任务，先修复原因，再用“续传”及新的敏感操作确认恢复原任务。记录保留原捕获身份，不能用另一份副本替换。

备份期间普通业务请求返回维护响应；已登录管理员仍能刷新备份页、查看状态和取消。新登录、签发/续期会话、创建/验证人类挑战及修改计划不会绕过维护屏障。长任务前应确认管理员会话有效：默认会话 12 小时，默认备份全任务上限 2 小时。会话过期或被撤销后不保留旧权限；过期后仍须等待任务完成或使用已有的本机控制方式。本版不支持在维护期间签发新登录会话，因为身份写入也在一致性保护范围内。

### 每日自动计划

管理页可启用/停用每日计划、选择 IANA 时区（例如 `Asia/Shanghai`）、当地时间、排空及全任务超时。保存也需要敏感操作确认，修订号用于拒绝覆盖他人刚修改的计划；页面显示下一次运行的实际时间。计划和任务记录保存在实例数据根内，重启后保留；调度器不保存管理员令牌，不用个人会话续期，也不依赖 Docker 命令。每次自动接受前复核批准者仍是活跃的人类系统管理员；其会话到期不影响计划，主体停用或系统管理员角色撤销会停用计划，须由有效管理员重新确认启用。

- 每 10 秒检查到期计划；正常运行时允许 90 秒调度延迟。
- 同一时区的同一本地日期最多自动运行一次，手动与计划共享一个协调器，不排队并发备份。到期正忙记录 `skipped_busy`。
- 重启后已过期的待运行槽，或进程暂停超过 90 秒，记录 `missed`，随后计算未来时间，不堆积补跑；长时间离线只记录最后待运行槽。
- 夏令时回拨的重复时间取第一次真实时刻；春季跳过的不存在时间记 `dst_missing`，当天不补跑。
- 目录不可用记录 `unavailable`。手动运行仍可在目录恢复后发起；历史成功不会掩盖当前目录故障。
- 开始/续传使用持久幂等回执。请求结果不确定时，用同一操作重试并刷新历史，不另造键重复提交。计划改动不清除当天防重记录。

管理元数据与业务库分离；状态、取消仅访问控制日志和当前身份，只读身份验证不产生审计/续期写入。控制日志以私有原子写方式落盘，执行中断后变为 `interrupted`，始终先取实例锁再恢复。详见[管理 API](../api/backup-management.md)。

## 可选：本机命令

网页是正常日常入口。已有运维终端也可以使用相同 core 协调服务：

```sh
# 使用相同 Compose 文件集合/项目名；不修改 YAML，不停 core。
docker compose -f deploy/compose.yaml -f deploy/compose.backup.yaml exec -T core \
  lantai backup-online -home /var/lib/lantai

# 原生部署：以 core 相同操作系统用户运行
lantai backup-online -home /path/to/data
```

命令先输出持久任务 ID，默认等待并输出终态。`complete` 才表示副本校验、完成标记和水位确认都成功；备份路径为配置 root 下的在线任务 ID。它不会输出密钥。`-wait=false` 可在任务被接收后退出，任务仍由 core 执行。

```sh
lantai backup-online -home /path/to/data -status ONLINE_JOB_ID
lantai backup-online -home /path/to/data -cancel ONLINE_JOB_ID
lantai backup-online -home /path/to/data -resume ONLINE_JOB_ID
# 示例：排空最多 5 分钟，整个任务最多 4 小时
lantai backup-online -home /path/to/data -drain-seconds 300 -timeout-seconds 14400
```

容器中查询、取消、续传也使用相同 `exec -T core lantai ...`。并发第二次备份直接冲突，不会悄悄排队。复制和验证可能很久，期间新读取、下载、上传和其他业务请求均得到维护响应；现有长请求先自然排空。取消必须等实际工作停止，`cancelling` 不是已经恢复。CLI 断连不会取消任务。

## 失败与重启

检查任务终态和错误简码。目标不可写、身份不符、空间不足、超时、复制/完整性失败都不会报告成功。修复原因后，使用相同在线任务 ID 续传；不要删除源 pin、伪造 `COMPLETE`、改名拼接不同捕获点。取消也保留源 pin 和不完整文件，供续传/诊断。要彻底放弃旧备份并释放 pin，仍需另行停机运行离线 `backup -cancel <backup_id>`，这不是正常备份的必需步骤。

core 重启后，旧执行中任务变为 `interrupted`。控制日志原子写入留下的、名称/类型/权限均符合协议的临时文件，会移至数据根 `backups/online-interrupted/` 保留；即使里面是完整 JSON，也不会提升为正式任务。正式日志仍严格验证，未知或不安全条目不会被忽略。实例照常完成启动检查；不会自动继续备份，也不会从旧内存标记恢复“完成”。显式续传重新排空并取得维护锁。已有完整副本会重验后确认，已发布 `copying` 的副本继续同一个冻结点。续传以源实例的绑定记录为准：重置日志已经记录旧目录改名时，即使新目录或 manifest 尚未重建，也会先核验被保留的捕获身份，再继续同一任务。已发布的冻结点丢失、无源记录或身份/目标不一致仍拒绝，不能静默改成新捕获；若确认旧目标不可恢复，应另建新任务并保留旧记录。

续传不会仅凭目标档案能通过独立校验就确认成功。它还必须匹配源记录的 backup_id、实例、恢复 epoch、密钥 ID、捕获时间、数据库矩阵、水位和全部文件/CAS 清单；已经完成的源记录同时核对完成时间。目标被替换、混入另一任务或另一实例副本时拒绝，保留源记录与目标证据，任务 backup_id 不改绑。找回原副本后可再次显式续传。

可独立验证成品：

```sh
lantai backup-verify -backup /path/to/backups/ONLINE_JOB_ID
```

正常业务不会读取备份目录；保管方可将已经完成的目录复制到异地，并在最终介质上再校验。旧主密钥必须独立安全保存，不能放进备份目录。恢复仍按[共同备份与空目录恢复](contracts/backup-restore.md)执行。

本机测试不代表 NAS 验收。挂载来源、UID/GID、NAS 的 fsync/断电行为、磁盘满/热拔盘及真实数据恢复须在批准的目标环境演练；本任务没有操作 NAS 或当前部署。

整馆恢复与原卷重启不同：控制日志目录 `backups/` 不属于备份成品，空目录恢复不会复活原任务历史或自动计划。恢复完成并建立新管理员后，重新配置备份根并明确启用所需计划，避免旧自动授权随恢复复活。

活动备份期间，目录可用性显示最近一次准入检查的结果与 `checked_at`，`check_deferred=true` 表示延后重新探测。状态查询和取消不再次访问外部盘，避免坏挂载阻塞管理通道；任务结束后刷新会重新检查。
