# 运行中互斥备份验证

实现基线为已发布 rc.4 提交 `d005b2ddc9911547365d7a5925cb59a8511efdbb`；本页描述其后的开发增量，不把新能力追溯到 rc.4。运行方法见[在线备份](../online-backup.md)和[本机接口](../../api/backup-control.md)。

## 源码核查和范围

- 旧 `backup` 命令通过 `OpenOffline → operations.Open` 获取数据根锁，保留该行为。在线命令只连接运行中的 core，不打开第二套数据库。
- 原 `Instance.Backup` 已具备共同点、四库 `VACUUM INTO`、pin、文件摘要与中断恢复。新增协调器通过真实的维护上下文复用这些协议，整个备份/验证/水位确认期间持有外层屏障。
- HTTP/传输请求的完整生命周期和所有已登记后台任务轮次都参与同一个互斥入口计数；关闭入口后等其退出，再取得领域屏障。后台任务在拿应用 mutex 前加入计数，避免与维护中的同步过程形成反向等待。
- 本机 socket 和 CLI 复用同一 `BackupControl`；没有新增角色、机器令牌、HumanGrant、公开网络接口或浏览器按钮。

## 定向验证

```sh
go test -race ./internal/application ./internal/operations ./cmd/lantai \
  -run '^TestIndependentReview|^TestIndependentFixReview|^TestOnlineBackup|^TestBackup' -count=1 -v
```

| 场景 | 实际验证 |
| --- | --- |
| 并发读取/写入 | 保持在途读取，验证新 GET/POST/PUT/DELETE 都被维护入口拒绝；在途退出前不开始备份 |
| 后台写入和完整轮次 | Gate 中的在途后台写入排空；持应用 mutex 的整个后台轮次排空；维护期间不接受新轮次 |
| 全程排他 | 在共同点、CAS 复制和 COMPLETE 边界注入失败，外层维护仍存活，退出后恢复 |
| 重复触发 | 活跃任务期间再次启动返回冲突，取消尚未退出也不允许第二份备份 |
| 取消/超时/异常 | 取消等待实际退出；故意忽略取消的复制保持锁；排空/执行期限、I/O 错误、panic 后安全清理 |
| 停止竞争 | 等待复制退出超时，实例锁仍不可被第二个实例取得；停止中的控制器不重新开放入口 |
| 真实副本 | 实际四库/文件快照、独立 VerifyBackup、禁止复制 secrets |
| 目录与恢复 | 未挂载/标记缺失、数据/密钥目录重叠、符号链接、替换目录 inode 均拒绝；目标故障保留不完整记录并可续传 |
| 原子日志恢复 | 正式日志存在/不存在、临时 JSON 截断/完整均隔离保留且不提升；未知文件、目录、符号链接、共享权限和畸形正式日志拒绝 |
| 捕获重置恢复 | 重置日志、rename、空目录、manifest 重建、清除重置记录等边界，含控制记录尚无 backup_id；异源/异目标/异 key/冻结点替换等负向拒绝 |
| 完成副本替换 | 同实例另一备份/跨实例完整副本，控制 ID 已知或缺失、源状态 copying/complete 均拒绝；保留原任务 ID、源日志和替换档案；找回原副本后续传成功 |
| 冻结点绑定 | 对 key/epoch/捕获时间/完成时间/矩阵/水位/文件/CAS 清单逐项替换；缺失或损坏 COMPLETE 不能绕过绑定；合法同点完成恢复及滞后源日志采纳仍通过 |
| 重启 | 模拟持久执行态恢复；另以真实子进程强杀验证 COMPLETE 已发布但结果未记录时，启动标 interrupted，续传重验同一 backup_id |
| 实际 CLI/传输 | 真实本机 TCP 服务与私有 socket，CLI 完成备份；socket 0600；缺少本机实例头拒绝；TCP 面不暴露备份入口；旧 CLI 不能绕过活动实例锁 |

定向套件使用临时合成实例、测试管理员和测试密钥。真实子进程强杀场景覆盖进程崩溃，不等同于断电、fsync 故障或 NAS 热拔盘。

## 验证边界

仓库完整检查命令为 `scripts/check.sh`；包含格式、依赖、vet、staticcheck、生成物、Python SDK、发布工具、全包 race、Go 构建及 Web 构建。最终测试结果与精确提交由随任务交付的日志确认，不以本页代替 CI。

Linux containerd/nerdctl 的独立容器补验已实际覆盖 bind mount、UID 65532、只读根、tmpfs/socket、重启与独立空目录/新命名卷恢复。UID 65533 的 CLI 调用被拒绝；由于数据根本身也拒绝该用户读取，不将它单独解释为绕过数据根后的 socket 直连穿透验证。Docker Engine/Compose、NAS UID/GID、真实磁盘满/热拔盘/掉电和原卷升级/回退仍需在批准的目标环境执行。Windows 只保证离线命令保持可构建，未提供在线 socket 控制。未发布、未部署、未更改已有 NAS，也未对其签署验收完成。

## 真实 Linux 容器补充验证

可在已经批准、独立于原业务卷的 Linux 容器环境中验证部署约束：固定提交本地构建的 scratch 镜像、UID/GID 65532、只读根、`mode=1777` 的 `/tmp` tmpfs 与私有控制目录/socket，部署时加载一次备份 bind，随后仅用原 core 内的 CLI 触发。至少覆盖并发 409、维护期业务 503、已有请求排空、目标故障后恢复、进程重启后显式续传，以及停止源 core 后恢复到另一独立空目录/卷、重置合成管理员、对账完成和实际就绪。

记录真实引擎与内核、镜像摘要、挂载来源、文件系统类型和 UID。containerd/nerdctl 在 Linux 上运行的结果只能标记为该引擎的容器验证，不能称作 Docker Engine/Compose 或 NAS 验收。所有账号、密钥和数据都必须为临时合成材料，旧密钥单独只读提供；恢复完成不能使源、恢复实例同时投入服务。没有可用引擎时如实报告阻塞，不用 macOS 进程测试替代。
