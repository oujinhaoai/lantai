# 0004 执行与扩展宿主的公共约定

状态：已采纳（2026-09-27）。规则全文见[执行与扩展宿主的公共约定](../contracts/execution.md)。

## 背景

任务（T05）、Agent 执行与作业（T06）、扩展宿主（T09）各自拥有状态机，但都要在最终接受边界判断“这份结果还能不能用”。若各自实现，容易出现只比较 fence 数值、把宿主健康当业务成功、把 checkpoint 回滚当成撤销外部动作等错误。

## 决定

1. 协议分三族：业务执行协议 `lantai.agent-execution/v1`（describe/start/lookup/status/resume/cancel/result），一次性处理器文件协议 `lantai.processor/v1`（无控制动作），常驻控制协议 `lantai.host-control/v1`（describe/negotiate/configure/health/drain/stop，按需实现）。动作不跨族使用。
2. 接受执行结果时先核对任务 fence（含 recovery_epoch），再核对扩展激活代次；两者都必须通过。撤权立即失效，正常升级或停用只允许排空名单内、未过期的旧代次收尾。
3. 恢复代次不符按对象类别映射错误码：会话 `TOKEN_REVOKED`、Attempt `LEASE_STALE`、HumanGrant `HUMAN_PROOF_REQUIRED`、BlobGrant `BLOB_GRANT_REQUIRED`、ReadGrant `FORBIDDEN`、扩展激活 `EXTENSION_ACTIVATION_STALE`、未提交操作 `OPERATION_NEEDS_RECONCILIATION`。
4. TaskRun 状态、宿主健康、审定与人类授权是互不相通的状态集合；`execution_succeeded` 不等于任务完成或审定通过；取消未确认停止时不报告 `cancelled`。
5. 一次性调用的运行结论（completed、not_dispatched、cancelled、runtime_fault、unresolved）与检查结论（pass、fail、unknown）分开：只有 runtime_fault 计入熔断，合法检查 fail 属于 completed；只有 completed 能给出 pass 或 fail。
6. 工具副作用按类别与进度决定恢复方式；结果不明且不可幂等或查询时阻塞恢复，禁止换键重试。
7. M1 只交付契约与判定规则，不启用任务服务、Agent 后端、一次性宿主或动态 loader；`lantai version` 报告各协议为 `contract_only` 或 `reserved`。

## 后果

- T05/T06/T09 的领域 schema 通过 `$ref` 引用 `lantai.execution-common/v1` 中的公共定义，Go 端复用 `internal/contract/execution` 的判定函数。
- M2 接入扩展激活与任务 fence 的联合检查时，只需提供两类权威状态，不另写判定逻辑。
