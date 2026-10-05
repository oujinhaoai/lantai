# 任务与业务流程契约

本篇是 [T05.1 M1](../tasks/T05-tasks-workflow.md) 的实现契约，并在末节说明 M2 已实现的内部服务。权威字段在 [`schemas/tasks/v1`](../../schemas/tasks/v1/)，Go 类型、状态边和接受检查在 [`internal/contract/tasks`](../../internal/contract/tasks/)。`taskstest.Static` 只供领域消费者测试，不能接入服务组装；M2 服务见 [`internal/tasks`](../../internal/tasks/) 与 [`internal/workflow`](../../internal/workflow/)，T07 已接入远程入口和显式消费/派发；租约定时器与业务自动触发未启用，见[手动执行与远程协作](manual-execution.md)。

## 对象与所有者

| 文档 | 唯一所有者 | 固定事实 |
|---|---|---|
| `lantai.task/v1` | tasks | 项目、类型、验收条件、输出要求、优先级、Seat 集合、具体版本输入及摘要 |
| `lantai.seat/v1` | tasks | Task 内席位、当前 Attempt、末次 fence、恢复代次、修订 |
| `lantai.attempt/v1` | tasks | 主体与 **Session** 绑定、领取操作、租约区间、任务 fence、停止与副作用对账事实 |
| `lantai.flow/v1` | workflow | 一次运行的定义版本及摘要、固定输入、StepRun 集合、待完成 operation 与事件去重位置 |
| `lantai.step-run/v1` | workflow | 业务步骤的一轮，绑定 Flow 定义、输入、Task/Job 引用及权威完成事实 |

`Attempt` 与执行模块的 `TaskRun`、`AgentStepRun`、`JobAttempt` 不互换。只有 tasks 签发任务 fence；jobs 签发自己的 `job_fence`；执行模块引用这些凭据。业务 StepRun 不承载 Agent 的内部步骤。定义更新或返工不能重写历史 StepRun：新一轮使用新的 ID 和递增 `round`。

持久输入使用完整 `(instance_id, asset_id, version_id)`，不保存 `latest`、别名或可漂移指针。`input_snapshot.digest` 为 `refs` 数组 JCS 的 SHA-256；数组顺序参与摘要。Flow、StepRun 的 `definition` 固定具体版本及定义摘要，`CheckStepBinding` 拒绝换定义。步骤输入可以包含之前步骤已固定的输出；变更运行输入必须建立新的相应运行。

## 状态与必需条件

以下是业务边，不是调用者可以任意赋值的字段。纯 `*Transition` 方法检查边；owner 还必须检查权限、修订、输入、停止事实和领域权威。它们不执行状态更新。

| 对象 | 允许的转移 |
|---|---|
| Task | waiting → todo / cancelled；todo → claimed / done（review/qa 权威完成）/ cancelled；claimed → reconciling / blocked / submitted；blocked → reconciling；reconciling → todo / cancelled；submitted → done / rework / cancelled（流程取消或审定拒绝传播）；rework → claimed / cancelled |
| Seat | open → claimed / cancelled；claimed → reconciling / submitted；reconciling → open / cancelled；submitted → done / open（返工新 Attempt）/ cancelled |
| Attempt | active → reconciling / submitted；reconciling → released / cancelled / expired；这些终态不可重启 |
| Flow | running → waiting / paused / completed / failed / cancelled；waiting → running / paused / failed / cancelled；paused → running / cancelled；failed → running（显式人工重试）/ cancelled |
| StepRun | pending → ready / cancelled；ready → running / cancelled；running → waiting / blocked / completed / failed / cancelled；waiting → running / blocked / completed / failed / cancelled；blocked → ready / cancelled；结束后新建轮次 |

`review` Task 不发生产租约：从 todo 到 done 需要已落盘的审定结论及其 operation；批准与拒绝都可以结束该审阅任务。`qa` Task 可由已落盘报告完成，不要求先人工提交。两者走 `CheckTaskCompletion` 的类型专用完成分支，普通产出任务仍须 submitted 和已接受输出。模型答复或 `TaskRun.execution_succeeded` 不满足这些条件。

`CheckStepCompletion` 按步骤类型要求对应 owner 的事实：task 已 done、job 已完成、gate 条件已成立、wait 条件已满足；review 需要 ledger 批准、publish 需要 ledger 发布成功和权威 operation。审定批准不能替代发布完成。M1 的 StepRun 仅声明 task/job/gate/review/publish/wait；parallel/join/foreach/subflow 等不受支持，schema 明确拒绝。

取消、超时、释放和阻塞不能直接把仍有执行的席位变为 open。先失效旧 Attempt 的写入权，再确认停止与副作用：

- `reconciliation.termination_confirmed=false`，或 `unresolved_effects>0`：保留 reconciling，`SafeToReclaim` 返回 `OPERATION_NEEDS_RECONCILIATION`；租约过期本身不是停止证据。
- 确认停止且没有未决副作用，才可结束旧 Attempt、重新开放席位。领取生成 **新 Attempt ID**，fence 必须严格大于该 Seat 的 `last_lease_fence`，即使回收后 fence 数值看似可复用也不能跳过恢复代次。
- `submitted/released/cancelled/expired` Attempt 的 schema 要求确认停止且未决副作用为零。取消意图不等于取消已完成。

## 命令、操作与最终接受检查点

`lantai.task-command/v1` 是 M1 的条件写入形态；`tasks.Commands.Execute` 和 `tasks.Reader` 是消费者端口。所有操作带 `operation_id`、`request_hash`、`expected_revision`、`recovery_epoch` 和已验证的 `session_id`。会话 ID 不提供授权；生产 owner 必须在协调锁内重新认证和授权。

| `command_type` | 条件目标 | 额外条件 |
|---|---|---|
| `tasks.claim` | Seat | Seat open、Task todo/rework 或其他席位已领取的 claimed、上一轮已安全回收；原子比较修订，创建新 Attempt/fence |
| `tasks.renew` | Attempt | 当前 Session、当前 Attempt、未过期且未失效的 fence |
| `tasks.release` | Attempt | 同上；撤销后续写入权，未确认停止先 reconciling |
| `tasks.submit` | Attempt | 同上；固定输出引用，重新检查版本可读与接受条件；提交不等于审定 |
| `tasks.cancel` | Attempt | 修订和 epoch；执行者带 fence，管理员无 fence 路径仍须独立授权，随后按停止事实收尾 |
| `tasks.complete` | Task | 按任务类型引用权威 operation；review/qa 可从 todo 完成，产出任务须 submitted 和已接受输出 |
| `tasks.requeue` | Seat | 当前修订、epoch、上一轮停止和副作用已对账；不能因定时器到期直接重领 |
| `workflow.advance` | StepRun | 当前修订、固定定义与输入、来自领域权威的完成事实 |

`HashCommand` 对命令的 JCS 求摘要，排除 `operation_id` 和 `request_hash`，其余字段均参与。相同 operation 和相同摘要返回原 `lantai.task-receipt/v1`；不同摘要为 `IDEMPOTENCY_CONFLICT`。旧修订为 `PRECONDITION_FAILED`，已有有效领取为 `TASK_ALREADY_CLAIMED`，旧 Attempt、Session、fence、到期或恢复代次为 `LEASE_STALE`。共同检查和错误细节复用[执行公共约定](execution.md)，不另立错误码。

最终接受顺序为当前身份权限 → 目标/固定输入绑定 → 当前 recovery_epoch → expected_revision → Session 与 Attempt → fence/到期 → 领域状态和输出资格。`AcceptCommand` 是其中的纯条件守卫；调用它不替代所属库中的原子条件更新、授权或资源复验。owner 的业务结果、回执和 outbox 必须同库同事务。流程跨边界命令用稳定子 operation（复用 `ids.DeriveChild`，步骤键含 step_key 和 round），未知副作用先按旧 operation 对账，不能换键重做。

已提交操作的响应重放先按当前读取权限授权，再返回原回执；不重新要求旧写租约仍有效。整馆恢复后的未完成操作通过公共 `CheckRecoveryEpoch(SubjectOperation, …)` 进入对账，不能把旧命令当作新命令继续执行。

## 契约桩与验证

[`taskstest.Static`](../../internal/contract/tasks/taskstest/static.go) 接受预设 Task/Seat/Attempt/Flow/StepRun、服务端时间、恢复代次、计划结果与已提交回执。它支持成功读取/回执、修订冲突、同键异 hash、旧 fence、旧恢复代次和未决副作用拒绝。每次读取返回副本，重放只返回预设原回执；它不发号、不推进持久状态、不启动进程、不模拟权限授予。消费者测试仍需独立设置授权依赖。

正反例在 [`schemas/examples/tasks/v1`](../../schemas/examples/tasks/v1/)。可重复验证：

```sh
GOCACHE=/tmp/lantai-go-cache go test -race ./internal/contract/tasks/... ./internal/contract/schema
```

这组测试验证契约、不变量与静态接口消费者；不代表 M2 的真实并发领取、心跳回收或工作流恢复已实现。

## M2 内部服务

M2 实现 T05.2–T05.5 的单席位任务与固定顺序业务流程，作为进程内领域服务供组装调用；REST/CLI/MCP 接线归 T07，定时清扫与后台消费归 T08 调度。`task.submitted → cancelled`（及对应席位边）为本轮补充：流程取消或审定拒绝需要结束已交付、尚未验收的任务，而不虚构返工轮次。

### 任务、租约与签出（`internal/tasks`）

`tasks` 拥有 runtime 迁移 `0008.tasks.core.sql` 中的任务、席位、Attempt、签出、阻塞、交接与资源引用表。所有写命令在维护屏障、security_guard 读锁、项目锁与任务锁内执行；回执、状态与 `task.*` outbox 同一 runtime 事务提交，同键同摘要重放原结果，异摘要 `IDEMPOTENCY_CONFLICT`。领取、续期与交付时由契约守卫依次核对当前权限、恢复代次、修订、Session/Attempt 与 fence；已失效的 Attempt 一律 `LEASE_STALE`，竞争领取的后到者得到 `TASK_ALREADY_CLAIMED`。

| 命令 | 要点 |
|---|---|
| `Create` | 固定精确输入版本与摘要、验收条件、指派或角色池、独立性、签出声明；可选固定 T02/T03 生效上下文集合及摘要：该读取自带 security_guard 读保护并在返回前复核整组权威集合，按全局加锁顺序在取项目锁之前完成，随后与任务同一事务提交；之后的上下文替换不改变已固定的集合。流程创建的任务经 workflow 核对步骤仍在推进且规格摘要一致，每个步骤轮次只有一个任务。M2 只建一个席位。 |
| `Claim` | 核对类型角色、指派/角色池、`distinct_from`（质检自动排除制作者）与每主体有效 Attempt 上限；新 Attempt 的 fence 严格递增，同一事务取得签出。空闲席位采用当前恢复代次；有未对账轮次时 `OPERATION_NEEDS_RECONCILIATION`。 |
| `Renew` / `Release` | 续期按策略 `task.lease_minutes`。释放先失效写入权；只有执行会话确认停止且未决副作用为零才结束轮次并重新开放，否则保留待对账。阻塞中的任务释放后仍等待答复。 |
| `Submit` | 输出必须是本人提交、当前可读的本项目精确版本，且不在其他任务的独占签出下；提交不等于验收。 |
| `Block` / `Answer` | 阻塞引用执行者在 T03 写入的提问消息；答复引用回复该提问的答复消息。答复把仍在执行的旧轮次转入对账，旧租约不复活；旧轮次已停止时任务回到待领。 |
| `Handoff` | 以本人提交的草稿版本与交接消息为界结束本轮，任务回到待领，接手者取得新 Attempt。 |
| `Cancel` / `Reconcile` | 取消在有执行轮次时只记录意图并失效写入权；`Reconcile` 记录操作者核实的停止与副作用，确认后结束轮次、刷新席位恢复代次、重新开放或完成取消。 |
| `SweepExpired` | 显式调用，把到期或属于旧恢复代次的有效轮次转入对账；到期计数达到 `task.max_attempts` 标记卡住。不推断外部执行已停止，也不重新派发。 |
| `Complete` / `Rework` | 完成与返工引用领域权威事实：制作类需要本轮交付被批准的审定记录，review 需要针对其对象的审定结论，qa 需要已接受的质检报告；返工依据为审定退回、未通过的质检报告或合法失败的检查证据。事实须绑定当前轮次、Attempt 与 fence，旧轮次的结论拒绝为 `REVIEW_TARGET_STALE`。完成后解除签出并解锁前置全部完成的等待任务。 |

签出生命周期随任务：领取时取得，任务交付、返工期间保留，回到待领、完成或取消时解除。`CheckVersionWrite` 是台账 `CheckoutGuard`，在 `Prepare` 与最终 `Commit` 各执行一次：未绑定任务的追加遇到独占签出返回 `ASSET_CHECKED_OUT`；绑定任务的写入（`catalog.VersionRequest.Task`，随请求摘要固定）须来自当前 Session 的有效 Attempt，新建资产同样核对租约。候选组例外只属于同一任务且须声明多候选输出，流程身份不能越过其他任务的独占签出。

只读端口：`Current`/`Refs`/`Recipients`（T04 `CollaborationTasks`，收件人按当前指派/角色池/执行者/创建者展开）、`CheckDiscussion`/`CheckDiscussionAnchor`（T03 `DiscussionTasks`，锚点须属于任务输入、输出、草稿、上下文或签出资产）、`MilestoneTasks`（T01 `TaskProgressReader`）。组装适配器 `LedgerAuthorities`、`CatalogContexts` 分别连接 T03 权威事实与 T02 上下文。

### 固定业务流程（`internal/workflow`）

流程定义为 [`lantai.flow-definition/v1`](../../schemas/tasks/v1/flow-definition.schema.json)，作为 config 资产清单的 `metadata.flow_definition` 保存，`Start` 固定精确版本与规范化摘要；版本须当前可读且未停用。M2 只接受 制作 →（检查 job）→（质检 qa，`distinct_from` 制作）→ 审定 → 发布，修改流程须声明签出；并行、分支、汇合与子流程拒绝。`BuiltinDefinition` 提供入库、新建、修改三个模板，实例仍须引用已入库的版本。修改流程在开始时把目标资产的当前最新版本固定为输入，制作任务领取即签出该资产。

`workflow` 拥有 runtime 迁移 `0009.workflow.core.sql` 中的流程、步骤轮次、绑定与待发命令表。推进规则：

- 事务消费者 `CatchUp` 按全局顺序处理 `task.*`、`review.submitted/withdrawn/recorded` 与 `publication.changed`；权威读取在 SQL 事务之外完成，流程修订在事务内复核，推进、待发命令、去重记录与水位同事务提交。
- 制作任务交付恰好一个输出后固定本轮对象（任务、Attempt、轮次、fence、精确版本）；检查、质检与审定都绑定该对象。审定提交载荷在创建命令时固定（版本控制修订、检查与质检证据）。
- 审定批准后追加制作任务完成与发布步骤；自动发布只执行台账已为该批准持久化的请求，手动模式等待负责人发布。流程仅在台账当前发布指针指向批准版本后完成，批准本身不完成流程。
- 审定退回、质检未通过或检查失败开启制作任务返工，新轮次使用新 StepRun，旧轮次与结论保留；超过定义的 `max_rework` 流程失败。审定拒绝使流程失败并取消已交付任务。
- `Dispatch` 以调用者身份执行待发命令，首个派发者固定为回执作用域；任务命令按步骤唯一或状态后置条件幂等，台账命令只能由同一主体重试。目标在最终接受时复验全部条件；暂时不可用按退避重试，领域拒绝使步骤阻塞，`RetryCommand` 以原 operation 释放。
- `SyncJobs` 读取 T06 作业结果；`Pause` 停止派发并使依赖流程授权的审定与自动发布被拒绝，`Resume` 继续；`Cancel` 取消未结束步骤与未派发命令，并为仍未结束的任务追加取消命令；`Retry` 只重跑阻塞步骤或宿主故障的检查作业，不推翻审定拒绝或返工上限。

端口：`ReviewExecution()` 实现 T03 `ReviewExecution`——submit/review/auto_publish 必须引用当前轮次的交付 Attempt，流程暂停或结束时拒绝；质检报告须来自独立质检任务的当前执行者，检查结果委托 T06 `CheckExecutions`，未接入时拒绝。`RequireIdle`/`RequireProjectIdle` 实现归档所需的 T05 空闲判断（未结束任务、签出与流程）；`VerifyStepTask` 供 tasks 核对流程创建的任务。检查作业端口 `Jobs` 与检查执行核实仍由 T06 交付，集成测试中为显式夹具。

### 验证与限制

```sh
go test -race ./internal/tasks ./internal/workflow ./internal/contract/tasks/...
go test -race ./tests/integration -run 'TestM2(Flow|Task|Modify)'
scripts/check.sh
```

单元测试使用真实 runtime 迁移、回执/outbox 与锁协调，T01/T03 为端口替身；集成测试使用真实身份/TOTP、存储、目录、台账审定与发布、来源、事件与 SQLite，覆盖首条闭环（含一次退回、签出守卫、旧轮次拒绝与自动发布）、检查/质检失败返工、审定拒绝、暂停、取消对账、服务重建后的重复派发、收件箱/讨论/里程碑及修改流程。T06 检查作业与执行核实为显式夹具，验收配置为预先批准的夹具。这些测试不代表 T06/T07 接线、`LifecycleUses` 组合、后台调度或独立 M2 测试卡与门禁已经通过。
