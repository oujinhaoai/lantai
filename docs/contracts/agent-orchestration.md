# Agent 任务编排 MCP 适配

所有工具调用原公开 REST 和原领域服务，不增加状态机、审批库、Worker 身份、
自动 Runner 或业务触发器。工具存在不代表当前 Agent 具有对应角色或 scope。

| 领域 | 新增正式 MCP 入口 |
| --- | --- |
| Task | task_create、task_assign、task_answer、task_cancel、task_reconcile、task_complete、task_rework、task_attempts |
| Flow | flow_list、flow_start、flow_pause、flow_resume、flow_cancel、flow_retry、flow_retry_command、flow_dispatch |
| TaskRun | run_capabilities、run_cancel、run_reconcile |
| Job/Node | job_list、job_run、job_cancel、job_retry、job_reconcile、node_observe |

领取、续租、提交、记录进度等既有工具保留。Run 的人工答复仍要求非委托真人，
不注册 run_answer。task_answer 是原任务阻塞协调，不能代替 Run 人工输入或审定。
新增管理与 Worker 工具不加入原扩展贡献白名单，扩展不能因此取得额外别名。

写入使用公开 schema 的 request 和调用者保存的 idempotency_key；状态修改继续
显式带原 expected_revision、round、attempt、fence、input 与 recovery_epoch。
409/412 必须重读，不能更换旧键、重新领取或覆盖。Job 运行须通过原 Worker/Node
校验；node_observe 不会授予 Worker 权限。执行成功不等于任务完成、人审或发布。

flow_dispatch 是一次显式、有界的派发 pass，其请求键不是整次 pass 的持久回执。
每个原 Flow command 自有稳定 operation 和接受规则；未知响应先重读对应 Flow
命令与任务/Job 状态，再决定是否发起下一次 pass。该工具不声明 IdempotentHint。

取消先撤销结果接受权。未确认停止或副作用未知时保持 cancelling/reconciling/
needs_reconciliation，对账只能记录可观察事实，不能把请求取消当作实际进程退出。

Task、Flow、Run、Job 的列表返回 complete 与 next_cursor；请求仍以 after 继续。
完整页先返回 complete:false，即使下一页最终为空也必须读取到 complete:true。
Job 列表流式跳过当前不可见的设备任务，直到可见页满或查询耗尽，不暴露隐藏 ID
或数量；调用 context 取消会中止扫描。列表是实时授权视图，不是事务快照。

record_read_chunk 的 kind 为 task、task_attempts、flow、run、job，id 为准确 ULID。
每次重读授权记录，返回最多 8192 字节的 base64 分段、offset、next_offset、
total_bytes、sha256 与 complete。非零 offset 必须携带原 expected_sha256；记录
变化返回 PRECONDITION_FAILED，必须替换旧分段。重组核对总长度和完整 SHA 后再
解析。它不接受任意 URL、SQL 或本机路径，也不改变任何 operation 或 fence。
