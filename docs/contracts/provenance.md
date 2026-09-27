# M1 来源证据与用途限制

实现为 [`internal/provenance`](../../internal/provenance/)。文件保存不可变 manifest 中的 uses 与许可快照，以及 `storage.AppendRecord` 安装的追加证据；`ledger.db` 中 provenance 所属的表只保存待接受操作、已接受证据的摘要/修订/操作引用。未被本库提交接受的孤立证据不生效，普通著录与 correction 不覆盖旧证据。

`AppendEvidence` 先以请求摘要与幂等键保留稳定 operation/record ID、作者、时间和目标清单，再调用文件适配器排他写入；最终重新检查当前授权、生产者身份、预期修订及证据摘要，在所属库同事务保存生效引用、回执与 outbox。失败后保留同一操作；同键异摘要、陈旧修订和不同目标清单拒绝。正文协议只有 [`lantai.provenance-evidence/v1`](../../schemas/provenance/v1/evidence.schema.json) 一份。

当前用途判断每次读取已提交版本清单、已接受证据和身份模块当前授权，不读 index。`production`、`reference`、`restricted`、`noai`、`redistribute_raw` 与 `personal` 按声明用途检查。未知许可与已声明但尚未核验的外部输入，在非 `archive_review` 用途中返回 `RIGHTS_PENDING` 或拒绝；`archive_review` 允许有权主体读取这些待核验声明供审阅，不表示许可已通过。证据丢失/损坏、跨馆无法核验、循环或遍历预算耗尽仍返回 `RIGHTS_PENDING` 或拒绝。M1 尚无放宽 reference 关系的 Profile，三类来源关系都保守继承限制。许可表达式仅作声明与证据索引，不自动推导法律兼容性。

同哈希匹配只枚举当前可见的固定版本候选；多个来源返回 `Ambiguous`，预算不足返回 `Pending`，不自动选择许可最宽的版本。精确清单、查询关联与下载均受当前来源读取权限约束，不暴露无权来源详情。`rights_epoch` 随全库接受证据的序号单调前进，用于诊断，不替代逐次递归核验及实时授权，也不能单独作为授权缓存键。

personal 读取另外要求 `personal.read`，其身份规则是普通项目读取资格与项目策略 `personal.readers` 的显式主体 ID 名单同时满足。名单默认空，不能设置为实例级策略；变更复用身份模块已有的 `SetPolicy`、绑定目标/修订/摘要的 HumanGrant 和 security_guard 写锁。普通 viewer、owner 或管理员身份本身不隐含 personal 授权。撤权后新的 GET/Range 和查询立即重新判定。

启动时通过 `SetFiles(storage)` 与 `SetCatalog(catalog)` 接入权威文件及当前著录，在开放实例前完成接线；缺少 Catalog 时用途判断返回待核验。当前著录或冻结快照任一标记为 personal 都要求显式读取权。追加证据也同时要求当前目标读取权和 `provenance.append_evidence`，在写文件前及最终接受时复核。

`ProducerVerifier` 接 T09 的可信内置静态登记，核对扩展 ID、版本、包摘要、贡献及来源，内置产物还绑定当前 `core_release_digest`；没有登记时，带 producer 的证据明确拒绝，不信任自报 `builtin_release`。该核对不执行插件。旧 v1 记录缺少核心发布摘要仍保持可读且摘要不变，但新接受必须具有当前可信登记。M1 证据只记录未知外部输入和补充说明；RightsAssertion 的收紧/解除、安全用途确认和人审仍属于 M2，不通过普通证据接口启用。

`RecoveryInventory` 只读列出尚未接受的持久 record intent，以及已接受记录的固定引用、摘要与修订链。它经 ledger reader 定位版本、经 storage 文件接口核对字节与身份，不联表读取其他所有者的数据。已落位但未接受的文件不进入 rights 判断；已接受文件缺失/损坏或修订链断裂作为权威异常报告。

`RecoverOperation` 要求本实例有效维护上下文及原主体、会话、恢复代次，用原幂等键、operation/record ID 和冻结正文重放正常 `AppendEvidence` 接受流程，仍核对当前授权、目标清单与修订。application 先通过 identity `VerifySession` 取得可信上下文；旧代次、过期或撤权时保留待对账，不替换身份继续。

所有写入调用使用实例的维护屏障；最终权威接受与撤权共享同一协调器。调用只读 `EvaluateUse` 的下载/提交/查询入口持有 security_guard 读锁，计算器不再次取得维护屏障或安全锁。测试使用临时目录和合成输入：`go test ./internal/provenance ./tests/integration`。新增恢复用例覆盖未接受文件不推进修订、同 record 恢复、旧代次拒绝与已接受证据损坏；完整平台与部署验收仍须使用对应环境的执行证据。
