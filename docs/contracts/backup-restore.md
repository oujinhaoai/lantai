# 共同备份与空目录恢复

M1 实现由 `operations` 编排、各领域所有者核验；实现见 [`internal/operations`](../../internal/operations/)、[`application`](../../internal/application/)、[实例生命周期](instance.md)。公开文件协议为 [`lantai.backup/v1`](../../schemas/operations/v1/backup.schema.json) 和 [`lantai.recovery-review/v1`](../../schemas/operations/v1/recovery-review.schema.json)。本机命令需要停止服务并持有数据根独占锁；库内 API 也可在运行实例的共同维护屏障下备份。

## 冻结共同点

1. 关闭写入口、等待前台和后台在途写入结束。通过所属模块执行深度 fsck，排空已持久 outbox 并记录事件、审计及裁剪水位。
2. 枚举并核验 CAS 原件，将整个 Blob 清单持久写入源实例的 `backups/records/<backup_id>.json`。该记录是 `operations` 的 backup pin，复制未完成时不按 TTL 释放。
3. 在同一个独占窗口内对 `main`、`ledger`、`runtime`、`events` 执行 `VACUUM INTO`，包括已提交 WAL 内容；核对完整性、实例绑定及迁移摘要。复制 `instance.json`、可选 `config.yaml` 和 `projects/`、`trash/`、`staging/`、`quarantine/`、`audit/`、`logs/` 中的可变文件。每件记录相对路径、大小和 SHA-256。
4. 持久发布 `copying` 清单后才恢复写入。其后只复制已固定的不可变 Blob，流式读写，不把整个原件装入内存。
5. 四库及所有文件再次校验后写 `complete` 清单和绑定其精确字节摘要的 `COMPLETE` 文件，最后释放源 backup pin。应用层仅在此后向事件模块确认冻结的备份水位；确认失败可重试，不自动裁剪事件。

`index.db` 可重建，不进入备份；密钥、锁、旧备份也不复制。密钥仅保存 `key_id` 引用，须独立保存。备份与数据根必须是互不包含的目录，路径穿越、符号链接、非普通文件、重复路径、非法 CAS 路径或摘要不符均拒绝。M1 保守地保存所有 CAS 内容，不实现物理 GC。

```sh
lantai backup -home <data-root> -destination <empty-backup-dir>
lantai backup -home <data-root> -resume <backup-id>
lantai backup -home <data-root> -status
lantai backup-verify -backup <backup-dir>
lantai backup -home <data-root> -cancel <backup-id>
```

`backup_id` 即使失败也会输出。已发布 `copying` 的备份续跑同一冻结点，不重新快照。捕获阶段尚未形成共同点的中断会保留旧尝试到相邻 `.incomplete-<id>` 目录，通过持久重置日志重新捕获；绝不混入两次捕获的文件。`COMPLETE` 已写而源 pin 尚未释放时，重试先完整验证再释放。取消与复制串行，确认复制结束后才释放 pin，保留部分文件供诊断。取消的记录不可继续。

## 空目录恢复与凭据处理

```sh
lantai restore -home <empty-data-root> -backup <complete-backup-dir> \
  -key-dir <separately-recovered-old-key-dir> -minimum-epoch <latest-known-epoch>
lantai recover-admin -home <data-root> -admin <administrator-name>
```

仅接受完整且重新核验过的备份。目标必须为空，或是同一备份、同一清单摘要的本次中断恢复；不覆盖既有实例。复制前先写入恢复门闩 `instance.json.restore`，保留实例 ID，将 `recovery_epoch` 提升到备份与 `-minimum-epoch` 的最大值加一。更高的已知代际不能在重试中被静默忽略：需要以新的空目录重新恢复。旧服务器须持续停机，不能把一个恢复实例与原实例并行投入服务。

恢复阶段依次为 `copying → credentials_required → key_required → reconciliation_required`：

- 只复制清单声明文件，重建索引库，不跟随旧主机的绝对密钥路径；配置的密钥目录重置为本机 `secrets/`。
- 用单独恢复的旧密钥核验原有加密因子，随后身份模块原子登记恢复 run，撤销全部旧机器凭据、密码、TOTP、恢复/设置码、HumanGrant 和挑战，并提升主体授权代际。运行库独立结束旧代际会话；没有跨库事务，重试补做同一 run，不再次破坏新管理员的设置。
- 生成新的主密钥。旧会话、委托、操作接受上下文与签名下载能力不能恢复授权。管理员必须通过现有离线重置命令设置新密码并确认新 TOTP；其他用户与机器凭据后续按正常身份管理重新签发。
- 服务保持关闭。`doctor` 报告 `restore_incomplete`，`serve` 拒绝启动；不能把复制成功当作服务恢复完成。

## 完成恢复的本机对账

旧备份可能缺少后来发生的撤权、删除和外部副作用事实。维护者须取得最新记录，对账并应用需要保留的变更；无法证明完整性时保留维护状态。M1 尚无发布、物理清除或外部执行器，不能把相关未来状态机视为已实现。

准备符合 `lantai.recovery-review/v1` 的 JSON：从 `restore` 输出取 `run_id`、`backup_id`、`manifest_digest`、新 `recovery_epoch`；`administrator` 填本次重新设置认证的主体 ID；三个 `*_reconciled` 字段必须明确为 true。`evidence_digest` 为对账报告原始字节的 `sha256:<hex>`，`note` 说明依据。样例见 [`schemas/examples/operations/v1/recovery-review`](../../schemas/examples/operations/v1/recovery-review/)。样例标识不能原样用于实际恢复。

```sh
lantai schema validate lantai.recovery-review/v1 review.json
lantai restore-complete -home <data-root> -review review.json -evidence review.txt
lantai doctor -home <data-root> -json
lantai serve -home <data-root>
```

完成命令核验本次 run 后新管理员的密码/TOTP 事务证据，登记当前内置扩展、按原授权恢复未决操作、深度 fsck、收录 outbox、导出审计并重建索引。对账 JSON 和原始证据保存在 `logs/restore-<run_id>*`；全部通过才清除门闩。相同已完成请求可安全重放，改过的对账不能替代原回执。完成不会自行开放监听，须另行正常启动。

## 升级中断与诊断

```sh
# 旧 schema 等待迁移：停机捕获物理共同点，保留旧库与全部文件。
lantai backup -home <data-root> -destination <empty-backup-dir> -before-upgrade
lantai migrate -home <data-root> -backup <complete-backup-dir>
# 已持久绑定备份的中断迁移，继续同一次 run。
lantai migrate -home <data-root>

lantai fsck -home <data-root>
lantai recover -home <data-root>
lantai reindex -home <data-root>
```

`-before-upgrade` 仅适用于等待新迁移且尚无中断迁移的实例。它捕获四库与文件共同点、验证物理完整性及旧迁移历史，不运行依赖新 schema 的领域检查。常规 `backup` 执行完整领域核验。迁移要求同实例、同代际、匹配迁移前 schema 的完整备份，并把备份 ID/摘要持久绑定到迁移 run。迁移每库独立提交；中断保持维护状态，续跑不重复已提交的迁移。旧程序遇到较新格式或 schema 明确拒绝，不做破坏性降级；回退应向新空目录恢复经过核验的兼容备份。

`fsck` 只报告领域完整性和残留，不删除或修复权威文件；打开实例仍需要数据根锁，并可创建缺失的派生索引库。`recover` 才在屏障中分派原 operation、补 upload pin、修复派生说明快照和别名。原会话到期、被撤销或旧代际的操作保留待对账，不能换 ID 或假造授权续交。已提交文件、证明或已接受证据损坏属于硬失败，会阻止启动/常规备份；未接受残留单独报告，不当成已提交事实。

## 验证边界

自动化覆盖共同屏障/WAL、四库与可变文件一致点、捕获/复制/完成标记中断、pin 保留、空目录复制中断、摘要/路径/符号链接拒绝、升级中断、旧凭据拒绝、新管理员重置、索引重建和真实 HTTP 原件下载。主机断电、目标 NAS 文件系统故障与生产恢复演练仍需独立 TEST-M1 验收；本机合成测试和交叉编译不能替代这些证据。
