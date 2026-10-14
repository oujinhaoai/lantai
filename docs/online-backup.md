# 运行中触发互斥备份

本页适用于包含 `backup-online` 的开发版本，**已发布 rc.4 没有此命令**。部署/升级仍需单独协调。本实现不要求每次编辑 Compose 或手工停启 core；备份期间所有业务读写暂停。

## 部署时配置一次

1. 在实际备份文件系统上创建私有目录，确认外部盘/共享目录当前已挂载，目录不在数据或密钥根内部。服务账户须能创建文件及 fsync；容器默认 UID/GID 为 `65532:65532`。只配置该目录权限，不递归改已有磁盘权限。
2. 为这块备份位置选择一个稳定的非秘密标识，例如 `library-backup-volume-v1`，将它加换行写入目录下 `.lantai-backup-root` 普通文件。标记是防错配置，不是密码或设备身份证明，不能在缺盘后的空挂载点自动重建。
3. 在现有部署环境中设置 `LANTAI_BACKUP_DIR` 为上述宿主机目录，`LANTAI_BACKUP_ROOT_ID` 为该标识，部署时同时加载 `deploy/compose.yaml` 和 `deploy/compose.backup.yaml`（如已有 web overlay，继续保留）。bind 使用 `create_host_path:false`，不会自动创建缺失的宿主机目录。

```sh
# 使用经审查且含该功能的镜像，在部署/升级阶段执行一次。
docker compose -f deploy/compose.yaml -f deploy/compose.backup.yaml up -d
```

启用后，core 启动时也会核验备份根；根缺失、身份不符、不可写或低于空间余量会拒绝启动，应先修复挂载/权限/空间。运行中备份发生错误则在工作退出后恢复原业务状态。

原生部署在 core 的服务环境设置 `LANTAI_BACKUP_ROOT=<备份目录绝对规范路径>` 和 `LANTAI_BACKUP_ROOT_ID=<标记内容>`。Linux/macOS 支持本机控制 socket；Windows 本版继续使用离线流程。

## 每次备份

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
