# 提交故障矩阵执行

本驱动使用真实实例、身份、SQLite 台账、文件存储与合成输入，验证 [提交契约](../contracts/ledger.md)和[存储契约](../contracts/storage.md)的持久化边界。测试控制器及参数只编入 Go 测试二进制；产品入口、环境变量与公共协议不增加故障开关。执行状态、RUN、缺陷及独立评审仍在维护者知识库，不以驱动存在或开发机通过代表三平台门禁通过。

## 定点进程终止

```sh
go test ./tests/integration -run '^TestCommitCrashMatrix$' -count=1 -v
# 保存不含口令、令牌、主密钥或实例目录的矩阵 JSON / 子进程日志：
go test ./tests/integration -run '^TestCommitCrashMatrix$' -count=1 -v \
  -args -commit-crash-evidence=<本次新建的证据目录>
```

Windows 上，父进程在原有 45 秒握手等待期限内重试 `ERROR_SHARING_VIOLATION` / `ERROR_LOCK_VIOLATION`，以处理原子握手文件的短暂占用；权限拒绝、其他 I/O 错误及无效 JSON 仍立即失败。原生占用反例持有无读取共享的文件句柄，核对占用时等待、释放后可读以及权限错误不被当作等待：

```powershell
go test ./tests/integration -run '^TestCommitCrashHandshakeWindowsOccupiedRead$' -count=1 -v
```

每个点启动一个独立测试子进程，完成合成身份/项目/上传，在真实提交路径上暂停并以原子文件通知父进程。父进程用 `Process.Kill` 强制终止（Unix 为 SIGKILL，Windows 为 TerminateProcess），不执行关闭或 defer；等待退出后重新持有数据根锁。离线组装的实例先在启动维护上下文读取原 operation、版本可见性与读取授权结果，然后分派恢复，再经正常应用启动和原请求重放两次核验。

| 点 | 暂停位置 | 强杀后预期 |
|---|---|---|
| `before_frozen_write` | 创建冻结文件的写入前 | 无台账预留、无冻结文件，可重试原请求 |
| `frozen_temp_synced` | 冻结临时文件已写入并 Sync，尚未落位 | 无台账预留，临时文件不构成已接受版本 |
| `frozen_before_prepared` | 冻结内容已落位，Prepare 调用前 | 无台账预留，原请求可继续 |
| `prepared` | Prepare 事务已提交 | 原资产/版本身份与号码保留，不可读 |
| `install_partial` | 内容文件组装后、manifest 写入前 | prepared，私有安装区残留，不可读 |
| `assembled_before_rename` | manifest、标记与目录刷新后，整体改名前 | prepared，不可读 |
| `renamed_before_sync` | 目录已改名，目标父目录刷新前 | prepared，目录存在不构成安装或提交记录 |
| `placement_synced` | 两侧父目录刷新后，storage 安装事务前 | prepared，完整目录待原 operation 接管 |
| `storage_installed` | storage 安装事务完成，ledger Commit 调用前 | storage installed / ledger prepared，不可读 |
| `ledger_installed` | ledger 安装证明事务完成，最终接受复验前 | ledger installed，不可读 |
| `accepted_before_commit` | 最终接受复验成功，committed 事务前 | ledger installed，不可读 |
| `ledger_committed` | committed 事务后、catalog 补写前 | 原版本完整可读，重试不创建第二版本 |
| `response_lost` | catalog 补写完成，结果尚未交给父进程 | 原版本完整可读，重复原请求返回原结果 |

报告同时保留子进程终止状态、测试二进制 SHA-256、实际文件系统类别、冻结文件/安装文件摘要、原 operation 与预留身份、恢复报告、最终唯一版本与逐文件授权读取摘要。prepared 前尚未分配资产/版本 ID，不能要求恢复保留一个不存在的预留。事务前后边界测试不声称覆盖事务内部的全部指令，也不模拟断电、宿主崩溃或设备写缓存丢失。

## 文件错误、路径与孤立目录

```sh
go test ./tests/integration \
  -run '^TestCommit(FileFaultMatrix|RejectsUnsafeManifestBeforeWriting|OrphansNeverCreateAuthority)$' \
  -count=1 -v
go test ./internal/storage/fileop ./internal/storage ./internal/ledger \
  ./internal/catalog/pathrule ./internal/operations -count=1
```

`TestCommitFileFaultMatrix` 分别注入复制时 ENOSPC、改名 EBUSY、文件 Sync EIO、改名后目录 Sync EIO 和硬链接 EXDEV；核验未提交版本不可见，清除注入后恢复原预留并逐件深度核验。EXDEV 注入只证明经过摘要校验的复制回退，不代表实际跨卷。非法 manifest 经真实 catalog 提交入口拒绝；没有标记或标记引用未知 operation 的孤立目录只被报告，未知 operation 可显式隔离并保留字节，不补造版本。

## 原生文件系统语义

```sh
go test ./internal/storage/fileop -run '^TestNativeOpenHandleReplacement$' -count=1 -v
# 下列两项必须由执行者准备可丢弃的卷；默认跳过：
go test ./internal/storage/fileop -run '^TestNativeCrossVolumeCopyAndRenameRefusal$' \
  -count=1 -v -args -fileop-cross-volume=<另一个卷上的可丢弃目录>
go test ./internal/storage/fileop -run '^TestNativeFullVolumePreservesAtomicTarget$' \
  -count=1 -v -args -fileop-full-volume=<独立小容量可丢弃卷>
```

Unix 的打开句柄保留旧 inode，改名后新路径读取完整新内容；Windows 用原生非 delete-sharing 句柄验证占用拒绝，关闭后可继续。跨卷驱动要求实际硬链接被拒绝，验证复制的 SHA-256/大小及目录改名明确失败、源保留且目标未出现。真实磁盘满驱动仅接受剩余空间不超过 256 MiB 的显式卷，最多写入 512 MiB；只清理自己新建的子目录。此容量限制不能代替卷隔离，绝不可把主机盘、业务数据卷或共享实例作为参数。

macOS 可用单独小容量 APFS 磁盘映像；Windows 需单独虚拟卷；Linux 使用获准的独立小容量文件系统。卷的创建、挂载与卸载由平台执行计划管理，本驱动不修改系统挂载配置。记录真实卷身份和挂载选项，使用独立 TMPDIR（Windows 同时设置 TEMP/TMP）。Linux 实例目录须放已核实的本地命名卷独立子目录，不能放 FUSE 映射目录。

真实磁盘满用例覆盖 fileop 原子替换；并不宣称覆盖 SQLite 磁盘满或整实例提交在真实满卷上的全部行为。刷盘失败在错误注入层验证；Windows 目录 Sync 不提供目录刷新，三个平台的进程终止结果都不能据此推导断电保证。Windows 驱动可交叉编译准备，但须在 Windows 原生运行后才产生该平台证据。
