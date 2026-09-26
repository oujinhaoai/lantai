# 执行与扩展宿主的公共约定

状态：**M1 共享执行契约，已实现规则与测试**（T00.6）。本篇规定任务、Agent 执行、作业与扩展宿主共同遵守的协议边界、凭据检查、错误映射、状态分离与副作用恢复规则。机器可校验的定义在 [`schemas/common/v1/execution.schema.json`](../../schemas/common/v1/execution.schema.json)，判定规则在 [`internal/contract/execution`](../../internal/contract/execution/)；领域完整 schema 由各自任务补充：Task/Seat/Attempt/Flow 归 [T05](../tasks/T05-tasks-workflow.md)，ExecutionProfile/TaskRun/AgentStepRun/Checkpoint/ToolOperation/JobAttempt 归 [T06](../tasks/T06-execution-nodes.md)，扩展清单与激活快照归 [T09](../tasks/T09-extension-platform.md)，插件规格以[扩展设计](../extensions.md)为准。

M1 只交付契约与判定规则：**不启用任务服务、Agent 后端、一次性处理器宿主或动态 loader**。`lantai version` 如实报告各协议状态。

## 对象归属

| 对象 | 唯一所有者 | 说明 |
|---|---|---|
| Task、Seat、Attempt、任务租约与 `lease_fence` | tasks | 只有它签发与失效执行权；执行模块只引用 `attempt_id` |
| TaskRun、AgentStepRun、adapter 映射、检查点、工具回执 | agent_execution | 执行成功只表示候选与结果已交回 |
| Job、JobAttempt、作业租约 | jobs | 与任务租约是不同类型 |
| 扩展登记、启用、激活代次、熔断 | extensions | 包审定引用台账，不另建审批真源 |
| 审定、发布 | ledger | 只有台账事实和发布命令能改变发布指针 |
| 人类授权（HumanGrant） | identity | 人工答复、模型建议都不能充当人审授权 |

## 协议族与动作边界

| 协议 | 用途 | 控制动作 |
|---|---|---|
| `lantai.agent-execution/v1` | 任务内 Agent 执行的业务协议（adapter） | `describe`、`start`、`lookup`、`status`、`resume`、`cancel`、`result` |
| `lantai.processor/v1` | 一次性处理器文件协议：`job.json` → spawn/run/exit → `out/result.json` | 无；超时、取消与回收由宿主处理进程树 |
| `lantai.host-control/v1` | 常驻实例控制，按真实需求实现 | `describe`、`negotiate`、`configure`、`health`、`drain`、`stop` |

两套动作互不相通：`health`、`drain` 不能借执行协议下发，`start`、`resume` 也不属于宿主控制；常驻控制协议不套用在一次性处理器上，不替代业务执行协议。执行协议请求的公共头 `execution_header` 包含 `protocol`、`operation_id`、`request_hash`、`fence`，来自扩展时再带 `activation`；授权材料不放在请求头或命令行参数中。

## 执行权凭据与接受检查

执行或作业结果在最终接受边界依次核对（与提交共用同一协调锁，时间取条件更新时的服务端时间；未给出时间时直接拒绝，不因零值时间放行）：

1. **任务 fence**（`task_fence`：`attempt_id`、`lease_fence`、`recovery_epoch`，可带 `task_id`）。先比较 `recovery_epoch`，因为恢复后可能出现重复的 fence 数值；再比较 Attempt、fence、是否已终止、是否到期。
2. **扩展激活**（`activation_ref`：扩展 ID、版本、包摘要、`activation_generation`）。撤权立即拒绝，排空中的调用也不例外；当前代次在启用时接受；正常升级后的旧代次、以及正常停用后的当前代次（停用时放入排空名单），只在排空名单内且未过期时接受；同版本换摘要一律拒绝。

两者是不同类型的凭据，必须分别通过；任一失败，结果只能隔离保存为证据，不能推进业务。缺少 fence 的执行结果直接拒绝。

| 失败 | 错误码 | `details.reason` |
|---|---|---|
| 恢复代次不同 | `LEASE_STALE` | `recovery_epoch_mismatch` |
| Attempt 已被替代 | `LEASE_STALE` | `attempt_superseded` |
| fence 不符 | `LEASE_STALE` | `fence_mismatch` |
| Attempt 已终止（取消、超时回收） | `LEASE_STALE` | `attempt_terminated` |
| 租约到期（`now >= expires_at`） | `LEASE_STALE` | `lease_expired` |
| 扩展已撤权 | `EXTENSION_ACTIVATION_STALE` | `revoked` |
| 扩展已停用且该代次不在排空名单 | `EXTENSION_ACTIVATION_STALE` | `disabled` |
| 包 ID、版本或摘要与代次不符 | `EXTENSION_ACTIVATION_STALE` | `package_mismatch` |
| 旧代次不在排空名单 | `EXTENSION_ACTIVATION_STALE` | `generation_stale` |
| 排空期已过 | `EXTENSION_ACTIVATION_STALE` | `drain_expired` |

同一 `operation_id` 携带不同请求摘要返回 `IDEMPOTENCY_CONFLICT`；同一 adapter `execution_key` 携带不同启动摘要返回 `START_KEY_CONFLICT`。不同 Attempt 必须使用不同启动键。

## 恢复代次的错误映射

整馆恢复会推进 `recovery_epoch`。带旧代次的对象一律失效，按类别返回：

| 对象 | 错误码 | 调用方下一步 |
|---|---|---|
| 会话 | `TOKEN_REVOKED` | 重新认证 |
| Attempt | `LEASE_STALE` | 重读任务状态，必要时重新领取 |
| HumanGrant | `HUMAN_PROOF_REQUIRED` | 重新完成人在场证明 |
| BlobGrant | `BLOB_GRANT_REQUIRED` | 重新上传或取得授权 |
| ReadGrant | `FORBIDDEN` | 重新申请下载授权 |
| 扩展激活 | `EXTENSION_ACTIVATION_STALE` | 按当前启用状态重新派发 |
| 未提交的持久操作 | `OPERATION_NEEDS_RECONCILIATION` | 对账后决定，不直接继续 |

## 状态分离

- **TaskRun 状态**：`pending`、`starting`、`running`、`waiting_input`、`paused`、`cancelling`、`needs_reconciliation`、`execution_succeeded`、`failed`、`cancelled`。终态只有后三个；`needs_reconciliation` 不是终态，对账前既不能宣称已停止，也不能自动重启。`execution_succeeded` 不等于任务完成、候选被审定或资产已发布。
- **调用运行结论与检查结论**：见上一节；运行故障与合法 fail 分开记录。
- **宿主健康**：`resolved`、`starting`、`ready`、`draining`、`stopped`、`failed`、`quarantined`，只描述常驻实例；只有 `ready` 可接新调用，且不代表任何业务完成。
- **审定与人类授权**分别由 ledger 与 identity 记录。三组状态在 schema 与 Go 类型上互不相通，测试确认互相不接受对方的取值。
- **取消**：先持久化 `cancelling`、撤销后续写入与工具授权，再请求后端停止。确认停止且没有未决副作用才转 `cancelled`；确认停止但仍有未决副作用，或超过宽限期仍不能确认，转 `needs_reconciliation`。adapter 声明自己能兑现的取消方式：`revoke_only`（只撤销兰台授权，停止需会话或操作者确认，例如 manual-cli）、`cooperative`、`enforced`。

## 调用运行结论与检查结论

一次性处理器或作业的每次调用先得到**运行结论**（`invocation_outcome`），再得到**检查结论**（`check_verdict`），两者不能混用：

| 观察 | 运行结论 | 计入熔断 | 检查结论 |
|---|---|---|---|
| 退出码 0 且结果完整、符合 schema | `completed` | 否 | 取结果中的 `pass`、`fail` 或 `unknown`（不支持的输入按 schema 报 `unknown` 并正常退出） |
| 派发前拒绝（不支持的输入、权限或配置拒绝）或排队失败 | `not_dispatched` | 否 | `unknown` |
| 取消且已确认进程树结束 | `cancelled` | 否 | `unknown` |
| 崩溃、非零退出、超时、结果缺失或畸形、可归因的启动故障 | `runtime_fault` | 是 | `unknown` |
| 已派发但停止状态未知 | `unresolved` | 暂不计，先回收或对账 | `unknown` |

合法的检查 `fail` 是业务不合格，不是运行故障；非零退出不能用来表达检查 fail；崩溃、超时或缺失结果永远不能变成 `pass`。熔断的窗口、冷却与半开规则以[扩展设计](../extensions.md)为准，本节只规定哪些结论计入。实现：`execution.ClassifyInvocation`、`CountsAsFault`、`Verdict`。

## 工具副作用与恢复

工具动作先持久化 intent 再发送。恢复时按副作用类别与进度处置（`execution.Recover`）：

| 进度 | 纯读 / 本地可替换 | 兰台命令 / 外部幂等 | 外部可查询 | 外部不可幂等 |
|---|---|---|---|---|
| `completed` | 复用回执 | 复用回执 | 复用回执 | 复用回执 |
| `intended`、`not_executed`、`failed`（外部明确失败且无副作用） | 重跑 | 沿用原键重发 | 重跑 | 重跑 |
| `dispatched`、`effect_unknown` | 重跑 | 沿用原键重发并核对回执 | 先按外部请求 ID 对账 | 阻塞，进入 `needs_reconciliation` |

结果不明时禁止换键、换版本、换实例或由模型自行重试；checkpoint 回滚不等于撤销已发出的外部动作，fence 只保护兰台写入。

## 检查点、恢复方式与预算

- 检查点只在稳定边界保存：固定输入、结构化计划摘要、已完成步骤、产物清单、工具回执水位和后端恢复指针；不保存模型隐藏思维与秘密。
- `resume_class`：`portable_artifacts`（换后端读摘要与产物继续）、`backend_checkpoint`（同一兼容后端恢复）、`restart_safe`、`manual_only`，由 adapter 实测声明，不能因后端框架自带检查点就推断可恢复。恢复前先对账副作用、产物与预算，再建立新 Attempt；旧检查点不恢复授权。
- 有效工具集合 = 项目策略 ∩ 身份权限 ∩ ExecutionProfile ∩ 本轮授权；子 Agent 只能进一步收窄。审定、发布、清除、发令牌、改策略永不进入 Agent 工具清单。预算跨重试、续跑、换模型与子 Agent 累计；无法精确计费时只声明可执行的调用或 token 上限。
