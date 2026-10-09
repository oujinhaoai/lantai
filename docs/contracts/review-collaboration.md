# 审定、发布与协作读取接口

本文说明核心内部已提供的领域接口及其接受边界。HTTP/CLI 路由由 T07 组装；接口存在不表示任务运行器、自动发布调度或里程碑门禁已经启用。

## 1. 审定与证据

`ledger.Service.NewReviews` 连接当前身份策略与 `ReviewSources`。`Submit` 固定版本清单、已批准的配置资产版本、检查要求、证据摘要及 T05 的 task/attempt/round/fence；同一资产新的正式提交撤回旧 submitted 目标，同任务同轮次的候选组保留并行候选。追加 draft 不撤回已有目标或批准。

验收配置作为 config 资产清单的 `metadata.acceptance_profile` 保存，字段由 `lantai.acceptance-profile/v1` 定义。固定的配置版本须处于 approved/enabled，不能把普通配置文件或数据库索引视为审定真源。配置至少要求 integrity、schema、license_evidence、purpose；处理器的扩展 ID、版本、包摘要、贡献 ID、来源及配置摘要必须精确匹配。不同配置版本不会重写历史审定。

项目没有活跃、已批准配置时，owner 可显式提交 `initial_profile: true`：目标和 Profile 必须是同一精确 config 版本，配置必须包含 config 类型及核心不可豁免检查。这个标记固定在 ReviewTarget 内；最终批准再次核对 owner、当前配置集合、任务/证据、自审策略和 HumanGrant。已有活跃 approved 配置时拒绝初始化路径，后续配置使用普通审定。初始化只提交候选，不直接制造批准；另一候选先获批后，旧初始化目标不能继续通过。

`lantai.review-target/v1`、`lantai.review/v1` 和 `lantai.waiver/v1` 定义固定目标、人审回执及豁免记录。Waiver ID 从审定 operation 与检查键摘要稳定派生，记录精确版本/清单、目标、Review、approver、HumanGrant、证据和有效期，scope 仅覆盖该 ReviewTarget。记录随所属 Review 不可变保存，不设第二份可修改的豁免真源。发布时复验绑定及有效期；不能把旧豁免移动到另一版本或扩大范围。

`FileReviewSources.AppendEvidence` 先持久化 ledger 中的 intent，再由 T02 写不可变记录，最后复验当前权限、目标、任务执行资格和文件摘要，在 ledger 本地事务接受记录、回执和 outbox。重试使用原 operation/record ID 与恢复代次；文件存在不能代替接受。`lantai.review-evidence/v1` 为核心证据包装；其中 `check` 复用扩展的 `lantai.check-result/v1`，没有第二套处理器协议。QA 报告必须包含工具、版本、观察说明和 verdict。

T05/T06 通过 `ReviewExecution` 只读接口提供当前任务身份、轮次、fence、检查执行完成与结果绑定；T05 的实现为 `workflow.ReviewExecution()`（见[任务与业务流程契约](tasks.md#m2-内部服务)），质检报告由其核对独立质检任务的执行者，检查结果仍委托 T06。`VerifyEvidence` 必须核实真实执行与 actor，不能只检查请求中的 producer。证据历史读取不要求已经完成的 Job 仍在运行。新检查包装的 `check_run_id` 固定核心 JobAttempt，不能混淆同输入/包/配置产生的相同结果；旧记录不回填，由 Job 已接受的 evidence ID 核对来源。`CheckApplicable` 在最终人审批准时另外核对该已完成 Job 的当前任务/目标、恢复代次及当前 activation（T09 `CheckCurrentSnapshot`）；正常停用/升级的排空资格只允许在途结果收尾，不能用于已完成检查的新批准或发布。包审定撤销、启用撤权或项目白名单移除立即拒绝新接受，同时不改写或隐藏既有证据。发布前仍复验检查适用性；同一生产轮次可在任务完成后发布，审定要求任务仍待接受，旧 Attempt/fence 或交付版本均不能借此恢复资格。未组装这些权威端口时，构造器或命令明确拒绝；测试替身不构成真实任务闭环。T05 另通过 `ReviewByOperation`/`ReviewByID`/`TargetFact`、`EvidenceByOperation`/`EvidenceIDByOperation` 与 `DiscussionMessage` 受信任读取审定、证据和讨论事实，调用方负责授权。

人审经 `Reviews.HumanAction` → identity Challenge/TOTP → `Reviews.Record`。HumanGrant 绑定完整请求、目标修订、清单、决定、原因和豁免项；最终接受再次检查当前目标、身份策略、任务资格、用途与证据。制作和 QA 按 principal 分离；人自审依当前 `review.allow_self_human` 策略记入回执。豁免只能覆盖配置明确允许的检查，不能绕过身份、完整性、目标或当前用途限制。过期豁免不能支持新批准或发布。

## 2. 发布与纠正

批准审定只改变 review_state；auto 配置同时在本事务中保存独立的 publication request。`RunPublication` 是显式调用的内部分发入口，T05 须为该持久流程命令提供当前授权；没有自动后台定时器。失败原因保存在队列中，重试不改变原 expected publication revision。持锁后先核对读取权限并查原回执：同一调用者重放已成功的请求时返回首次回执，不新增发布历史，流程已结束也一样；没有回执时才要求当前流程执行权，其他调用者因此被拒，不会再次发布。锁外读到的请求状态只作定位，不据此拒绝重放。失败诊断只由当前流程执行者写入。

Owner 可以通过 `Publish` 手动发布或回退到曾发布过的版本；接受时检查当前 approved/enabled/active、项目、锁、用途与验收要求。历史 Publication 追加保存，新的批准不自行替换当前指针。

`RevokeReview` 仅允许原审定人或当前 owner，经新的精确 HumanGrant 把当前批准变为 withdrawn；若它是当前发布版本，同一 ledger 事务暂停发布指针并取消未完成的发布请求。旧已完成命令可以重放历史回执，但不能重新激活指针。重新审定必须产生新目标。

`ChangeControl` 提供 unlock、disable/enable version、archive/unarchive 和 suspend。Unlock、停用、归档和暂停均经人类动作授权；归档额外要求 T05 当前无在用流程。Lock 覆盖资产或目录树，已有 prepared 提交也会在最终接受时复验，声明的过期时间不会静默解除锁。停用与暂停同事务记录历史；旧下载授权在新读取边界复验当前状态。

`NewArchival` 组合当前敏感来源读取和 T05 `Activity`。目录 `PreviewDirectory` 固定每个资产的控制修订与最新版本，`BatchActions` 生成精确人审项，`ChangeBatch` 只消费 identity 已存储的授权集合，返回独立子操作结果；新目录对象不会加入，确认后新增版本会使该项失败。单次最多 100 个资产，超限明确拒绝。每项接受时在 security guard 内复验当前权限与无在用流程，失败不撤销其他项，重试保留原子操作 ID。

项目归档使用 `PreviewProject`、`ProjectHumanAction` 与 `ChangeProject`，固定项目控制修订及整个保留资产集合，最多 256 项以遵守 HumanAction 的 64 KiB 上限；新增资产、最新版本或控制修订变化使旧确认失效。它还要求 T05 `ProjectActivity.RequireProjectIdle` 核验未绑定资产的项目流程，仅有逐资产检查不足以放行。项目生命周期、控制修订、回执和事件在同一 ledger 事务提交。归档使项目内资源只读并从默认搜索及默认上下文中隐藏，精确历史与显式 include_archived 读取保留；恢复项目不修改各资产自身的归档/停用状态，也不启动旧流程。项目控制事件要求客户端替换可见资源范围，当前对象状态与修订反映项目归档。

`provenance.ApplyAssertion` 只接受保守收紧或证据索引；更正有效来源边、确认未知来源或降低限制必须经 `ApplyHumanAssertion` 使用 owner 的精确 HumanGrant，并引用比上一断言更新的已接受证据。`lantai.rights-assertion/v1` 绑定永久引用、目标清单、预期修订、证据 ID、原因与操作。断言文件先持久写入，最终复验授权、文件和当前修订后才接受；冻结清单和旧证据不改写。用途判定沿当前来源关系传播限制，统一 `rights_epoch` 随证据或断言接受递增。

来源更正拒绝循环，确认未知来源只作用于明确的证据 ID，后续未知证据仍返回 pending。`VisibleUses`、在用关系盘点与索引重建读取当前生效图；关联查询在返回前复验每条候选边，所以滞后索引不能恢复已移除的关系。普通恢复仅重试原身份/会话/恢复代次的收紧意图；人审意图必须重新走身份模块的完整授权检查，维护权限不能代替过期 HumanGrant。`CancelAssertion` 允许当前 owner 按原 operation 与 request hash 显式取消尚未生效的意图，并保存取消回执和事件。取消与最终接受共用 security guard；旧重试返回终态失败，已接受断言不可取消。磁盘孤立证据继续保留对账，不因取消而删除。

## 3. 讨论、事件与收件箱

`PostMessage` / `Messages` 保存不可变的项目、任务、资产和版本讨论，包含回复、纠正、mentions 与结构化锚点。`NewDiscussionObjects` 使用真实台账、catalog 清单、provenance 当前用途及 identity 项目成员校验资源、文件锚点和 mentions；T05 通过 `DiscussionTasks` 校验当前任务可见范围及锚点归属，未接线时拒绝任务讨论。资产/版本锚点必须属于讨论对象，文件必须存在。读取时再次过滤已不可见的锚点和 mentions。记录结构由 `lantai.discussion-message/v1` 定义。事件不携带正文，讨论不推进审定或发布。

`query.NewCollaboration` 使用 T03/T05 的 `CollaborationObjects` 权威读端口提供权限过滤事件、最长 30 秒的长轮询、失效游标重同步和身份收件箱。即便本页事件全部不可见，扫描水位仍推进；已撤权或删除对象要求客户端执行替换。项目策略（如 `personal.readers`、`visibility`）、项目成员角色与项目控制的变化，以及作用于全部项目的系统策略变化，也要求替换：它们会收窄或放宽可见范围，而增量里没有对应对象的事件，只有替换才能移除已不可见的缓存条目、取回此前看不到的旧对象。登录、会话等与可见范围无关的事件不触发替换。重同步先捕获事件水位 S，再扫描当前快照，完成替换后重放 S 之后的事件。

`ledger.NewCollaborationSource` 与 `query.NewCoreCollaborationObjects` 组合真实台账、审定、讨论、回收状态和 identity 当前活跃项目成员。事件只提供定位，mentions、回复作者和审定角色均从当前权威记录展开；正文和锚点不进入通知。待审事项分配给当前 owner/reviewer，退回事项分配给提交者/制作者，到期回收事项分配给删除者/owner。T05 的 `CollaborationTasks` 独立提供任务可见性、当前归属及有序枚举；未配置时明确拒绝任务事件。组装不会启动消费者、任务执行器或调度。

runtime 中的收件箱只保存对象引用、事件去重和读取位置；排序与待办状态从当前权威对象读取，重新分派后不继续显示旧待办。维护模式可从权威快照替换投影并重放，消费者水位不能倒退；替换失败时旧投影、消费水位和已读位置均保留。权威重同步合并资源与任务的稳定分页，过滤隐藏对象仍推进扫描游标。重建扫描及收件人展开各自限制 100,000 项，超限明确返回错误，不发布截断投影。收件人解析失败与投影写入失败一样落入持久 consumer failure/backoff，重启不越过失败事件。`InboxMetrics` 提供聚合失败数；T08 可在有效维护上下文调用 `InboxFailures`，按事件序号分页查询事件 ID、失败类别、次数和重试时刻，不返回正文或原始错误。修复原因后调用 `RetryInboxEvent` 释放重试，不改变业务状态。业务状态不从事件序号或收件箱 SQL 推导。

`Reviews` 同时实现 T02 `ContextReviews`，只有当前有效批准且未归档的文档进入默认项目上下文。精确历史内容保留；撤销、停用和权限变化不继续提供当前批准。上下文包在返回前再次核对整个审定集合，变化时返回 `PRECONDITION_FAILED`。

## 4. 回收、恢复与清除

`NewLifecycle` 连接 T02 文件动作、当前 T05/溯源在用情况、实时权限与保留策略。`PreviewTrash` 固定版本修订、清单摘要、追加记录文件清单和受影响引用；人工授权绑定整个结果。`TrashOwn` 仅接受真实 Agent 主体，滚动小时配额包含尚未完成的持久预留：普通 20 个版本、宽限期 200 个版本。未完成预留不会因超过一小时而消失；完成时才记录成功删除时间，已完成回执重放不移动计数窗口。3 小时边界、本人提交、历史审定、目录、锁和当前发布均分别检查。普通保留期来自项目策略（7–365 天），宽限删除固定保留 7 天。

`PreviewTrashBatch` 接受明确的单项目目标；`PreviewDirectoryTrash` 按当前命名空间展开目录树，固定每项版本修订和字节清单。整目录始终需要人类授权，即便每项都符合宽限条件。单次确认最多 100 项，与 identity 批次上限一致，超限明确拒绝。`TrashBatchHumanActions` 构建精确子动作；`TrashHumanBatch` 只执行 identity 已保存的授权项，不能传入替换清单或加入确认后新建的对象。每项独立复验、提交并返回回执/错误码；部分失败后重试使用原子操作 ID。

接受与完成分为两次 ledger 本地事务：先保存不可变文件意图、配额预留和读写禁令，再调用 T02 移动文件，最后提交回收状态、回执、审定目标撤回及 outbox。文件动作故障记录在持久失败清单；`Resume` 只能继续原清单，不增加目标，不重新扣配额。恢复代次发生变化时必须先对账。完成的文件意图不能再次授权移动或删除。

`CancelTrash` 允许当前 owner 按项目、资产、原 operation、request hash 和原因取消未完成的初始删除。它在 security guard 与目标资产锁内调用 T02 `VerifyUnmovedTrash`，逐字节核对原版本和证据、确认回收目标不存在，并拒绝已经完成或摘要不同的文件动作。核对通过后，取消终态、解除禁令、释放 reserved 配额及事件在同一 ledger 事务提交；任何失败全部回滚。旧操作重放只返回失败终态，不能重新移动文件，新的删除须重新预览和授权。文件已部分/全部移动、原件不完整或目标存在时保留禁令和配额，继续原意图对账/恢复；不能把一次 I/O 错误直接结算为可取消。取消不发放文件权限，不能取消已完成删除、恢复或清除。

整资产目标包含仍保留的版本和独立历史条目：`targets` 固定本次要移动的版本，`history` 固定已回收/清除版本的修订、清单摘要及原回收条目。历史作者、审定与在用关系仍参与权限和宽限判定，接受时历史状态变化使旧确认失效；历史版本不重复移动或扣配额。恢复整资产只恢复本次移动的版本，旧回收条目的期限与 Hold 不变。清除资产容器前，其他历史回收条目必须各自完成清除；不能借整资产操作绕过旧保留期或 Hold。全部版本均已清除时，资产容器仍可按持久意图回收、恢复和清除，但不调用文件动作，其回执也不返回旧路径或用户填写的原因。首版清单仍承担初始说明回退来源时，清除须等待说明修订被台账接受。

`Restore` 限原删除者或当前 owner/admin，且仍须有项目读取、整理和当前敏感来源权限。原路径被占时返回 `PATH_CONFLICT`，调用者须明确选择新路径。普通整资产删除与清除继续占名；只有符合新建宽限条件的整资产删除自动释放名称。`ReleaseNameHuman` 要求人类 admin 的独立精确 HumanGrant，绑定资产、路径、代次、占名修订和原因；不能释放活跃资产的当前名称。恢复可以使用本资产仍保留的名称，遇其他资产占名时仍需明确新路径。整资产恢复分配新的别名代次，保留旧分配记录；原 ID、版本号、历史 Review、停用和归档限制保留。旧 submitted 目标保持撤回，重新审定须新目标。旧 prepared 版本/元数据操作和旧下载授权使用状态修订栅栏，恢复不能使它们重新生效。

`MutateTrashHuman` 的 Hold/Unhold 和提前 Purge 仅接受管理员本人的精确 HumanGrant。Hold 不延长原到期日，但阻止任何清除。`RunDueJob` 只供 T08 已注册服务端作业调用，必须提供独立的 `LifecycleJobs` 权威端口；人或 Agent 会话不能代替它。到期前 72 小时可持久生成一次提醒；到期、无 Hold 且项目策略 `trash.auto_purge` 为 true 才接受定时清除（策略关闭返回 `PRECONDITION_FAILED`/`auto_purge_disabled`）。调度器默认关闭，见[生命周期调度](lifecycle-scheduler.md)。

稳定回收状态的读取复验当前敏感来源权限；文件已清除或仍在移动时，只允许原删除者/owner/admin 在当前项目权限下读取有限墓碑，旧路径、恢复路径和删除原因置空。

清除保留台账墓碑与回执，将 Blob 登记为 GC 候选。物理 GC 仍需至少 24 小时、完整权威 manifest 根集和上传/提交/备份 pin 检查，清除一个版本不会直接删除共享 CAS。FSCK 区分保留回收区、移动中残留和已清除墓碑；查询重建排除回收/清除版本。FSCK 同时核验台账已接受的 Check/QA 与权利断言文件；真实接受记录不能作为孤立残留忽略，文件损坏必须报告完整性错误。

证据盘点覆盖活动记录区与 `trash/<trash_id>/records/<version_id>`。回收路径中的项目、资产与清单摘要由台账版本复验，并核对当前回收条目；已接受证据出现在错误位置也会报告完整性错误。未接受的正常/损坏记录进入待对账清单，移动中的记录按未完成意图报告；扫描不采纳或删除残留，也不跟随符号链接。

## 5. 集成边界

T06 执行运行态、T07 入口、动态插件、调度、部署和独立验收不由本文接口自动启用。T05 的任务可见性、讨论、收件箱归属、归档空闲判断与里程碑进度已有真实实现（`internal/tasks`、`internal/workflow`）；本文件原有归档与收件箱测试仍保留夹具用例，真实接线另见 T05 集成测试。未组装权威端口时拒绝相应命令。台账追加版本另可接入 T05 `CheckoutGuard`，在 Prepare 与最终 Commit 复验签出与租约。

可重复验证命令：

```sh
go test -race ./internal/ledger ./internal/catalog ./internal/query ./internal/events ./internal/provenance
go test -race ./tests/integration -run '^TestM2'
scripts/generate.sh
scripts/check.sh
```

集成用例使用真实身份/TOTP、存储、注册表和 SQLite；首次配置初始化用例通过真实配置入库、证据接受和人审获得批准。`TestM2Business*` 与 `TestM2Governance*` 使用组装后的真实任务/Job 所有者、初始 Profile 人审、合成包人审启用与实际一次性进程，不用 SQL 替代这些权威事实。本文件的旧用例中 T05/T06 当前任务与检查完成来源仍为显式夹具，部分旧审定用例保留预先 approved Profile 夹具；T05 真实任务端口的闭环见 `TestM2Flow*`，T06 检查完成仍为夹具。不得把这些测试记录为完整 M2 或独立验收门禁通过。
