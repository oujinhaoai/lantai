# M2 手动执行、检查作业与远程协作

本文记录 T06/T07 已接入的实现及调用方式。共享字段仍以 `schemas/` 为准，HTTP 请求/响应以 [OpenAPI](../../api/openapi.yaml) 为准；插件协议唯一规格仍是[扩展设计](../extensions.md)。这不是独立 TEST 或 M2 门禁结论。

## 所有者与能力边界

`tasks` 签发 Task/Seat/Attempt 和任务 fence。`agent_execution` 在 runtime 保存 TaskRun、步骤、工具 intent/receipt、检查点、预算、人机问答和 admission 历史；它不签发第二套任务租约。`jobs` 在 runtime 保存独立 Job/JobAttempt/作业租约；`node` 保存带会话与过期时间的能力观测。`extensions` 管理内置检查器与已启用外部包的一次性进程及其准入、熔断和最终门禁（见[扩展包治理](extension-governance.md)）。台账仍是候选提交、审定和发布真源。

`GET /api/v1/meta` 列出已组装能力；`GET /api/v1/task-runs/capabilities` 返回实际手动 adapter 的能力。当前支持 `manual_cli` 1.0.0、幂等 start/lookup、portable_artifacts/restart_safe 检查点格式 1、revoke_only 取消。managed_runner、自动触发、常驻控制和网页仍未启用；外部包只有经 T09 静态导入、台账审定与 HumanGrant 启用后才作为一次性检查器或本机命令运行。

手动会话不会自动调用模型，也不能保证用户在外部模型软件上的网络、token 或进程隔离。网络/工具声明限制接受的执行记录；模型调用、工具调用、墙钟与产物预算由观测和服务器接受边界核验。精确 token 计量与强制终止外部会话不受支持，不能将它们申报成已具备能力。执行档案允许 manual_cli 省略 `activation_snapshot`；managed_runner 仍必须提供。无插件生成来源的候选可省略 `producer`，但不能捏造与已提交 manifest 不符的来源。

## 手动执行与恢复

1. 创建并领取任务，取得当前 Attempt/fence。`POST /task-runs` 接受完整 `lantai.agent-request/v1` start/resume 文档（路径均位于 `/api/v1` 下）。execution_key 由 run/attempt 派生；request_hash 按共享 Go/JCS 规则计算，不是普通 JSON 字符串散列。同键异摘要拒绝；同键重放返回原 admission，即使同一个 run 已恢复到新 Attempt。
2. 使用 `/task-runs/progress` 追加单调预算与步骤观测，使用 `/tool` 先记录 intended，再记录 dispatched 和确定的回执。未知副作用阻止封存和新的调用；不能换 key 重试。工具需同时在 profile 白名单、核心非人审 action 登记和当前会话权限内。
3. 产物先经普通 upload/commit 提交，commit 的 `task` 绑定当前任务、Attempt、fence。`/candidate` 验证已提交版本、作者、manifest、真实字节、文件清单、来源与已有许可证据；候选状态只能为 pending。`/seal` 再次核对当前权限与文件，生成不可变结果摘要。execution_succeeded 不会完成任务、通过审定或发布。
4. `/checkpoint` 固定 input/profile/预算水位/完成步骤/已提交附件。只有明确 stopped 才暂停。释放并重新领取后 `/task-runs` 的 resume 引用原检查点，沿用 budget_id 和累积用量。输入/profile 或任务返工轮次变化时新建 run，服务器记录 supersedes_run_id；同输入/档案/轮次不能借新 run 清零预算。旧 run 必须停止且副作用已解决。
5. `/ask` 的目标摘要由服务器从固定执行上下文、问题和期限计算。`/answer` 只接受当前非委托人类、未过期问题和正确摘要；答复暂停等待显式恢复，不续租、不扩预算、不构成 HumanGrant。
6. `/cancel` 先撤回本次执行的接受权；未证明停止时保持 cancelling/needs_reconciliation，随后版本提交和任务交付也被 guard 拒绝。`/reconcile` 需要当前管理权限、原因和已提交证据引用；工具未知结果需明确对账为完成或未执行。非取消故障可同时提交停止检查点再恢复，取消不被改写成成功。

变更端点带 Idempotency-Key 与 body 中 expected_revision/fence。读取 `GET /task-runs/{id}` 获取当前状态；列表用 task_id、after 和 limit（1–100）。历史 admission/回执不被后续运行状态覆盖。

## 最小检查 worker

Flow 定义明确请求 `org.lantai.corecheck.manifest` 或已启用包的 `asset.validator` 贡献 ID，固定目标版本及制作轮次后进入 Job 队列；StartJob 即解析处理器，未启用、未获白名单或探测失效的包直接拒绝。worker 必须是已认证的 Worker 主体、具有项目 checker 角色和 worker session scope；普通 Agent 自报能力不能取得该权限。Worker scope 仅补充 catalog.read、tasks.read、ledger.append_check，不授权版本制作、人审或身份管理。

先 `POST /nodes/observe` 报告当前会话的 capability、slots、busy 和 memory_bytes，再 `POST /jobs/run` 显式派发。观测五分钟过期，能力为 1–16 个贡献 ID 且不授予权限；检查器全局并发为一，未决运行占用槽位。每次分配独立 job_fence、30 秒接受租约、固定包/入口摘要、配置和恢复代次。不会发出 Task fence。

官方宿主只运行当前已登记核心二进制的私有 `_processor-check` 入口。它将固定 `job.json`、manifest 写入私有临时目录，清空环境变量，不传会话、数据库句柄或数据根；10 秒期限控制实际进程，Wait 返回后才确认停止，输出限制 1 MiB。结果在反序列化前按原始完整协议验证，拒绝禁止字段，再匹配 operation、输入摘要、永久引用和 producer。该路径属于 `builtin_release`，不是通用安全沙箱。外部包由同一一次性宿主运行，准入、探测、熔断、排空/撤权与最终接受见[扩展包治理](extension-governance.md)。

核心额外复验实际文件字节与当前权利，产生 integrity、schema、license_evidence、purpose 四项证据。外部校验器可通过 `check_key` 返回一项命名业务检查；Job 将其绑定到 activation 的 `effective_config_digest`，并独立核验 manifest 结构，追加第五项业务证据。未命名结果保持原四项路径；核心检查使用核心配置摘要，Profile 的业务要求使用实际冻结配置摘要。每项新检查包装携带核心签发的 `check_run_id`（精确 JobAttempt ID）；最终人审还核对 Job 中实际接受的 evidence ID，结果内容相同也不能借用另一执行的授权。旧不可变记录不回填字段；公开 OpenAPI 的输入、已接受证据及内嵌 ReviewTarget 使用同一共享 `check_run_id` 定义，Go/Python 契约随工程生成。证据通过台账所有者幂等追加；runtime 先保存 accepting 意图，响应丢失可以用原 key 继续接受。合法 fail 是已完成检查的失败证据，不能当宿主故障重试，也不能批准资源。只有确认停止的 runtime_fault/host_start_failed，或未派发的准入拒绝（breaker_open、activation_stale、processor_not_allowed、processor_not_enabled）可显式重试，总计最多三次；外部包合法返回的 `unsupported` 记为 `unsupported_input`，不计宿主故障；没有自动重试调度。unknown 必须先 `/jobs/reconcile` 提供已提交的停止证据，仍有本机宿主时拒绝人工覆盖。取消先请求宿主停止，停止未知不冒充 cancelled。

恢复后旧恢复代次、Worker 会话、activation 和任务轮次在接受点失效。重启不会自动重派 running/accepting/needs_reconciliation；原 worker 会话仍有效时可重放 accepting，其他未决情况需要有权人员对账。Job 成功只交回证据，固定 Flow 再经独立质检、人审和发布。

## REST 与 CLI

应用组装启用任务、Flow、手动执行、Job、讨论、当前上下文、审定发布、生命周期、权利更正、权限事件和收件箱。所有入口调用同一领域 owner。后台只追平 outbox、查询/收件箱与审计；任务租约定时清扫与自动触发没有启用，到期清除与 GC 调度默认关闭，见[生命周期调度](lifecycle-scheduler.md)。`flow dispatch` 显式消费已收录的 Flow 事件、派发持久命令并同步 Job 结果；事件尚未收录或新命令待处理时需再次调用。其子命令各有稳定 operation/key，HTTP 调度请求本身不是一次业务事务。

```sh
lantai task list --server https://gateway.example --session-file session.json --project PROJECT_ID --limit 20
lantai task create --server https://gateway.example --session-file session.json --input task.json --idempotency-key create-1
lantai task claim --server https://gateway.example --session-file session.json --input claim.json --idempotency-key claim-1
lantai run capabilities --server https://gateway.example --session-file session.json
lantai run start --server https://gateway.example --session-file session.json --input start.json --idempotency-key start-1
lantai flow dispatch --server https://gateway.example --session-file session.json --input dispatch.json --idempotency-key dispatch-1
```

`dispatch.json` 为 `{"limit":20}`。任务/执行/作业请求文档直接使用 OpenAPI 的类型；任务提交/心跳等需要原 fence 和当前 revision，不使用示例伪造租约。

| CLI 组 | 子命令 |
|---|---|
| task | list/show/attempts/create/claim/renew/release/submit/block/handoff/assign/answer/cancel/reconcile/complete/rework |
| flow | list/show/start/pause/resume/cancel/retry/retry-command/dispatch |
| run | list/show/capabilities/start/resume/progress/tool/candidate/checkpoint/ask/answer/cancel/reconcile/seal |
| job、node | job list/show/run/cancel/retry/reconcile；node observe |
| message、review、evidence | message list/post；review show/submit/publish；evidence append |
| trash、rights | trash show/preview/own/restore；rights assert/cancel |
| human | prepare/verify/items/execute/rechallenge |
| context、events、resync、inbox | 读取；inbox read 更新已读位置 |
| plugin、ext | plugin import/list/enablements/probe/commands；ext install/list/remove/run（见[扩展包治理](extension-governance.md)） |

写入使用 `--input` 和 `--idempotency-key`；human、trash preview、inbox read 使用各自 owner 的专用幂等机制。show 用 `--id`，run list 的 `--id` 是任务 ID，message list 用 `--name KIND --id TARGET_ID`。context 用 `--project ID --type TYPE`。分页 after 通过 `--cursor`；events 的 cursor 是 sequence，resync 使用独立 opaque cursor。错误和退出码沿用[客户端契约](client.md)。

人审先 `human prepare --input intents.json`（items 中 kind 为 review/control/trash/force_trash/trash_mutation/release_name/rights/extension_enable/extension_disable），服务器生成固定目标与 challenge；再 `human verify --id CHALLENGE_ID --credentials-file code.json`；最后 `human items --id GRANT_ID` 读取各子 operation，`human execute --input child.json` 只传 grant_id/operation_id。执行不接收替换后的业务请求。人审只经这一显式流程；Agent 答复和 MCP 工具不提供授权。

## MCP 与 Python

`lantai mcp --server https://gateway.example --session-file session.json [--workspace ./working-copy]` 使用官方 Go SDK 的 stdio transport，stdout 仅协议。仅消费已有会话，不签发 token、不读数据库、不执行任意 shell。工具按服务器已启用能力注册，保留 REST 的 machine error；读取默认 20 条、最多 100 条，输出上限 64 KiB（超限报错），输入上限 256 KiB。大文件通过显式工作目录内的 resource_push/resource_pull 流式移动，工具只返回摘要，签名 URL 和文件字节不进入上下文。未给 workspace 时不注册搬运工具。工作目录约束不是第三方代码沙箱。

工具包含 whoami、资源搜索/精确读取、任务领取/心跳/交付/阻塞/交接、执行登记/观测/工具记录/候选/检查点/提问/封存、讨论、QA 证据追加以及状态读取。精确版本读取附带 lantai:// 永久资源链接。核心组装可通过 `mcpserver.Config.Contributions` 为 T09 添加带扩展命名空间的别名；该端口只允许选择已启用的核心安全写命令，不接受 URL、凭据或可执行入口，`--extensions-registry FILE` 与 workspace 同时给出时，另把本机已安装且服务器当前启用的 `cli.command` 投影为 `ext_…` 工具，调用与 `lantai ext run` 同一路径。人审、身份管理、插件启停和删除不进入 MCP。取消协议请求会取消 HTTP 调用；远端是否已提交仍按原 key/operation 对账，不盲目重放。

最小 Python SDK 见 [sdk/python](../../sdk/python/README.md)。它直接调用同一 REST，以标准库提供认证、资源、任务和精确单文件下载；版本/错误语义不另设一套。两种客户端都不把模型或外部完整平台作为前置。

里程碑配置与进度通过 REST 接入 T01/T05，见[协作适配契约](collaboration-foundation.md#项目里程碑)。
