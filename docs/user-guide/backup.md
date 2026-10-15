# 备份与恢复

目标：建立经过验证的完整保护点，并在独立空目录恢复。日常备份由交互式系统管理员在网页操作；部署与整馆恢复由设备管理员在服务主机处理。普通成员与 Agent 没有整馆备份权限。所有目录、镜像和版本必须先核对。

## 先确认版本

**管理页与每日计划是本次开发新增能力，不能当成现有部署已具备。已发布 rc.4 没有 `backup-online`。** 只有确认镜像实际包含该命令，并完成获准的升级后，才使用下面的在线流程。仍运行 rc.4 时使用本文后面的离线流程；不能把在线命令发给旧镜像。

备份包含四个权威库、清单、原件和约定可变文件，`index.db` 可重建。**旧主密钥必须在备份目录之外独立安全保管。** 数据副本不含主密钥，二者缺一不能完成恢复。

## 1. 部署时一次性配置备份目录

选择不与数据、密钥根重叠的外部目录，先确认目标盘确实挂载。服务账户应持有该目录，模式为 `0700` 或经过核验的等效私有 ACL。创建服务账户持有、0600、单一硬链接的普通文件 `.lantai-backup-root`，内容是事先选定的稳定标识加换行，例如 `library-backup-volume-v1`；它用于发现误挂载，不是密码或硬件身份证明。缺盘时不要在空挂载点重新创建标记。

在原私有环境文件增加 `LANTAI_BACKUP_DIR` 和 `LANTAI_BACKUP_ROOT_ID`。加载候选包中的 `compose.backup.yaml`；GUI 只有一个 Compose 文件时，把它一次性合入原 core，保留原有所有数据、密钥、网络、端口与安全设置：

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

首次加挂载在获准的部署/升级窗口重建 core。核心仍以 `65532:65532`、只读根运行，`/tmp` tmpfs 明确使用 `mode=1777`，其内部控制目录为 `0700`、socket 为 `0600`。此后正常备份不再改 Compose，不建独立维护服务，也不手工停启 core。标记缺失、不符、不可写或空间不足会拒绝启用备份。

## 2. 在网页立即备份或设置每日计划

以人类系统管理员账户登录，明确选择管理权限，再打开“备份管理”。先查看备份目录是否可用，再点击“立即备份”；核对服务器生成的整馆暂停说明，输入当前验证码确认。页面持续显示阶段、上次成功、历史及错误，只有“已完成”表示复制与验证成功。

每日计划填写 IANA 时区（例如 `Asia/Shanghai`）和当地时间，完成同样的敏感操作确认后保存。页面显示下一次运行时间。计划存入原数据卷，重启后保留，不需要容器命令、维护容器或再次编辑 Compose。

每天最多自动执行一次：遇到手动备份仍在运行会跳过并记录；重启错过的时间不补跑；夏令时回拨只执行第一次，跳过的本地时间会记录为未执行。状态页列出原因。目录缺失只会停用备份并显示原因，不会让正常业务因可选备份配置失败而无法启动。

备份会暂停全部业务读写；管理员可以刷新状态或取消。取消中仍在维护，必须等实际工作退出。失败、取消或重启中断的原任务，修复原因后点击“续传”并重新确认。过期会话和新登录不会绕过维护限制。

## 可选：已有本机终端

在原 Compose 项目和相同文件集合内执行；单文件 GUI 合并后使用原项目提供的 core 命令终端：

```sh
docker compose -f deploy/compose.yaml -f deploy/compose.backup.yaml exec -T core \
  lantai backup-online -home /var/lib/lantai
```

如果原项目另有 web 覆盖文件，继续保留它。原生 Linux/macOS 部署由 core 相同系统用户执行 `lantai backup-online -home /path/to/data`。

core 会先拒绝新业务读写、排空已有请求与后台写入，然后保持互斥到副本校验结束。任务回执为 `complete` 才是成功；CLI 断开不会取消任务。目标目录使用回执中的在线任务 ID，可独立执行 `lantai backup-verify -backup /path/to/backups/ONLINE_JOB_ID`。已完成副本应复制到另一介质，并在最终介质再次核验。

## 3. 本机查询、取消和失败恢复

以下参数附加到同一个 `backup-online` 命令，在原 core 中执行；在线任务 ID 与底层 backup_id 不同，不能混用。

```sh
lantai backup-online -home /var/lib/lantai -status ONLINE_JOB_ID
lantai backup-online -home /var/lib/lantai -cancel ONLINE_JOB_ID
lantai backup-online -home /var/lib/lantai -resume ONLINE_JOB_ID
```

并发备份会拒绝，不能排队绕过锁。取消等待实际工作退出，`cancelling` 不代表写入已经恢复。失败或取消保留原记录和部分副本；先修复挂载、权限或容量，再显式续传。重启会把旧执行态标记为 `interrupted`，不会自动继续或假定成功。

续传必须匹配原任务、实例、密钥 ID、代际、捕获时间、数据库矩阵、水位及完整文件清单。另一份即使完整且可验证的备份也不能替换目标；找回原副本后可重试。不要删除源 pin、伪造 COMPLETE、拼接目录或绕过实例锁。彻底放弃旧 pin 仍用受控停机后的离线取消，这不是正常在线备份的必需步骤。

## 旧 rc.4 的离线流程

rc.4 必须先暂停业务并优雅停止原 core，在服务主机用同版本程序、原服务用户和原卷挂载执行：

```sh
lantai backup -home /path/to/data -destination /path/to/private/backups/snapshot-001
lantai backup-verify -backup /path/to/private/backups/snapshot-001
```

目标是新空目录。保持服务停止直到备份与验证结束；`INSTANCE_LOCKED` 时查持锁者，不删锁文件。离线 `-status`、`-resume BACKUP_ID`、`-cancel BACKUP_ID` 都仍要求实例锁。这个旧流程不能在活动 core 旁执行，也不能用它推导 rc.4 已经支持在线备份。详细边界见[共同备份与恢复协议](../contracts/backup-restore.md)。

## 4. 整馆恢复到新空目录

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

整馆恢复与原卷重启不同：控制日志目录 `backups/` 不属于备份成品，空目录恢复不会复活原任务历史或自动计划。恢复完成并建立新管理员后，重新配置备份根并明确启用所需计划，避免旧自动授权随恢复复活。
