# 任务与业务流程契约

本篇是 [T05.1 M1](../tasks/T05-tasks-workflow.md) 的实现契约。权威字段在 [`schemas/tasks/v1`](../../schemas/tasks/v1/)，Go 类型、状态边和接受检查在 [`internal/contract/tasks`](../../internal/contract/tasks/)。M1 不启用任务服务、领取端点、租约定时器、Checkout 或流程引擎；`taskstest.Static` 只供领域消费者测试，不能接入服务组装。

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
| Task | waiting → todo / cancelled；todo → claimed / done（review/qa 权威完成）/ cancelled；claimed → reconciling / blocked / submitted；blocked → reconciling；reconciling → todo / cancelled；submitted → done / rework；rework → claimed / cancelled |
| Seat | open → claimed / cancelled；claimed → reconciling / submitted；reconciling → open / cancelled；submitted → done / open（返工新 Attempt） |
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
