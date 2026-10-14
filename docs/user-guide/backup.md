# 备份与恢复

目标：建立经过验证的完整保护点，知道丢失实例后如何恢复。由**设备管理员在服务所在主机**操作；普通浏览器用户没有备份按钮。下列操作会写备份记录或新恢复目录，只在自己已确认的路径执行。

## 开发版本新增的运行中互斥备份

**以下新增能力不属于已发布 rc.4。** 包含 `backup-online` 的源码构建可在部署时一次性挂载备份目录，并设置 `LANTAI_BACKUP_ROOT` 与匹配 `.lantai-backup-root` 标记的 `LANTAI_BACKUP_ROOT_ID`。启用后，以 core 的操作系统用户执行 `lantai backup-online -home /path/to/data`；Compose 中通过 `exec -T core` 执行同一命令，不需每次修改配置或手工停启服务。任务会排空在途读写和后台写入，备份与校验期间暂停全部业务。

`-status ONLINE_JOB_ID` 查询，`-cancel ONLINE_JOB_ID` 请求停止并默认等到工作退出，`-resume ONLINE_JOB_ID` 显式续传。取消不会提前释放锁；CLI 断开也不自动取消任务。失败/取消保留记录与 pin；重启后未完成任务标为 interrupted，须显式续传。仅 Linux/macOS 本机私有 socket 提供此能力，没有普通网页入口。外部盘实际挂载来源和密钥独立保存仍由设备管理员确认。

下文保留 **rc.4 离线流程**，供仍使用发布版或需要离线维护时使用；正常的新版本在线备份无需采用其中的停机和一次性容器步骤。

## 当前能力与限制

| 能力 | rc.4 沿用的 CLI 状态 |
| --- | --- |
| 离线 CLI backup、verify、status、resume、cancel | 已实现；必须先停 core 并取得数据根锁 |
| 库内共同维护屏障备份 | 有内部实现，没有面向用户的在线网页/HTTP 操作入口 |
| 正在运行的备份主动取消、外部盘身份/挂载自动预检 | 仍在设计，不能当成已提供 |
| 备份主密钥、完整卷身份保护 | 备份不包含旧主密钥；全链路卷身份保护仍在设计 |

备份包含四个权威库、清单、原件和约定可变文件，`index.db` 可重建。**数据备份和旧主密钥必须分别保存**；只存其中一个不构成可用恢复点。

## 1. 准备与停机

1. 选择与数据根互不包含的私有备份目录，检查剩余空间、写权限。使用外部盘时人工核对它当前确实挂载、设备身份正确；仅看到一个目录不够。此版本不会自动替你完成这项预检。
2. 记录原 core 实际挂载、镜像和实例 ID。暂停客户端写入，优雅停止 gateway，再停止 core，保留容器与卷。
3. 确认 core 已正常退出且无 OOM/强杀。备份期间保持它停止；`INSTANCE_LOCKED` 时查谁持锁，不删锁文件强行绕过。
4. 单独复制旧主密钥目录，保留权限和完整内容，私下保管。容器布局通常为 `/var/lib/lantai-secrets`，以实际配置为准；不要打印密钥内容。

## 2. 原生部署的备份命令

在服务主机，用同版本 Lantai 二进制和原服务所用的系统用户执行。把占位路径替换成已确认位置；`-destination` 必须是空目录或尚不存在的新目录，父目录应已存在且可写。

```sh
lantai backup -home /path/to/data -destination /path/to/backups/snapshot-001
lantai backup-verify -backup /path/to/backups/snapshot-001
```

成功标志：backup 回执 `state=complete`、目录有 `COMPLETE`，且独立 backup-verify 成功。只看到文件夹或 `copying` 不算完成。保留 backup ID、核验结果、版本和密钥备份的私有存放位置；不要把密钥塞进公开记录。

## 3. NAS 没有 Compose CLI 时

有 Docker 管理终端即可运行一次性容器，不需要 `docker compose`。若只有 GUI，需其支持一次性容器、继承原挂载和退出码核对；不支持时让设备管理员提供等效受控入口，不编造“网页备份”步骤。

先在 NAS 文件管理器创建独立私有备份父目录，并确认 UID/GID `65532:65532` 可写；密钥目标为本次尚不存在的新私有目录。core 已停止且仍存在。逐条执行示例，变量全部换成实际值：

```sh
CORE='填入已停止的原 core 容器 ID'
IMAGE='填入核实后的原镜像 ID'
BACKUP_PARENT='/path/to/private/backups'
KEY_BACKUP='/path/to/private/key-snapshot-001'
ARCHIVE='snapshot-001'
```

核对 core 与路径后，在已有管理员终端执行：

```sh
docker inspect --format 'running={{.State.Running}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}}' "$CORE"
test -d "$BACKUP_PARENT"
test ! -e "$BACKUP_PARENT/$ARCHIVE"
test ! -e "$KEY_BACKUP"
df -h "$BACKUP_PARENT"
```

要求 core 为 `running=false exit=0 oom=false`，每条检查成功，并已人工确认目标盘。随后单独复制密钥，再备份：

```sh
umask 077
mkdir -m 700 "$KEY_BACKUP"
docker cp "$CORE:/var/lib/lantai-secrets/." "$KEY_BACKUP"
test -s "$KEY_BACKUP/master.key"
docker run --name lantai-backup-snapshot-001 --pull never --network none --read-only --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m --volumes-from "$CORE" --mount "type=bind,src=$BACKUP_PARENT,dst=/backup" "$IMAGE" backup -home /var/lib/lantai -destination "/backup/$ARCHIVE"
docker run --name lantai-verify-snapshot-001 --pull never --network none --read-only --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,nodev,size=64m --mount "type=bind,src=$BACKUP_PARENT,dst=/backup,readonly" "$IMAGE" backup-verify -backup "/backup/$ARCHIVE"
```

该例只适用于标准容器挂载，路径不能含逗号；实际密钥或 home 路径不同则由管理员调整。每个 `docker run` 都必须结束且退出码 0，并核对 complete/核验回执。辅助容器名必须未被使用；保留辅助容器供检查，不删除卷。`--volumes-from` 继承原挂载，不凭猜测新建数据卷。

完成后确认辅助容器均已结束、数据锁已释放，再按原方式启动 core，确认就绪后启动 gateway。失败时停止升级，保留旧服务、备份 ID 和部分文件；不要为成功而跳过核验。

## 4. 备份中断、续跑与取消

仍保持 core 停止，在原生环境执行以下命令；容器环境沿用上面的同镜像、原挂载和原备份路径运行相同子命令。

```sh
lantai backup -home /path/to/data -status
lantai backup -home /path/to/data -resume BACKUP_ID
lantai backup -home /path/to/data -cancel BACKUP_ID
```

按需要选择一条，不是依次全跑。resume 会恢复同一冻结点，取消后的记录不可继续。cancel 需等原备份进程结束并释放锁，不能用来主动打断另一个进程。不要手动删除源 backup pin 或把残留目录改名成完整备份。

## 5. 整馆恢复到新空目录

前提：原服务器持续停止、完整备份核验通过、独立旧主密钥可用、目标目录为空，且恢复程序版本兼容。准备备份之后的最新撤权、删除和未决操作记录；恢复不是简单复制文件后启动。

在服务主机的恢复环境执行，所有路径均指恢复环境：

```sh
lantai restore -home /path/to/empty-recovery -backup /path/to/backups/snapshot-001 -key-dir /path/to/old-keys
lantai recover-admin -home /path/to/empty-recovery -admin ada
```

如知道更高的当前恢复代际，restore 还需给 `-minimum-epoch`；不能恢复到更旧代际。恢复保留实例 ID 并提高 recovery_epoch；用旧密钥验证后生成新主密钥。**全部旧密码、TOTP、恢复码、机器长期凭据、会话和人类授权失效**，管理员须交互设置新密码/TOTP，其他身份后续重新签发。主体、角色与历史仍保留。

此时服务保持关闭，`restore_incomplete` 是预期状态。按[恢复协议](../contracts/backup-restore.md#完成恢复的本机对账)准备本次 run 绑定的 `review.json` 与真实对账证据 `review.txt`，核对较新撤权、删除和外部副作用；不能照抄示例 ID 或没有核对就填 true。

```sh
lantai schema validate lantai.recovery-review/v1 review.json
lantai restore-complete -home /path/to/empty-recovery -review review.json -evidence review.txt
lantai doctor -home /path/to/empty-recovery -json
```

成功标志：恢复门闩解除，doctor 正常；再按部署方式启动，检查 readiness、新管理员登录和已知版本下载哈希。旧服务器不可同时上线。密钥缺失、备份损坏或对账资料不足时保留维护状态，不能宣称恢复完成。
