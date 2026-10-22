# 首方出站设备控制

精确 Go DTO 位于 `internal/contract/device/device.go`，HTTP 在 `internal/transport/httpapi/device.go`。所有 JSON 命令要求 `Idempotency-Key`，经过原身份、Origin、CSRF、JSON 配额与维护入口；成功返回 200。普通网页会话从不生成、读取或保存设备令牌。

| 方法和 `/api/v1` 路径 | 请求 | 响应 |
|---|---|---|
| POST `/devices/claims` | `Claim` | `ClaimResponse` |
| GET `/devices?project_id=&after=&limit=` | 项目 ULID、可选 after ULID、limit 1–100 | `DevicePage`（items/next_cursor） |
| GET `/devices/{device_id}` | — | `Device` |
| POST `/devices/{device_id}/approve` | `ApproveDevice` | `Device` |
| POST `/devices/{device_id}/revoke` | `Control` | `Device` |
| POST `/devices/{device_id}/jobs` | `CreateJob` | `JobView` |
| GET `/devices/{device_id}/jobs?after=&limit=` | 可选 after ULID、limit 1–100 | `JobPage`（items/next_cursor） |
| GET `/devices/{device_id}/jobs/{job_id}` | — | `JobView` |
| POST `/devices/{device_id}/poll` | `{}` | `Poll` |
| POST `/devices/{device_id}/jobs/{job_id}/inspect` | `InspectRequest`（完整 inspection） | `JobView` |
| POST `/devices/{device_id}/jobs/{job_id}/approve` | `ApproveJob` | `JobView` |
| POST `/devices/{device_id}/jobs/{job_id}/heartbeat` | `LeaseRequest` | `Poll` |
| POST `/devices/{device_id}/jobs/{job_id}/cancel` | `Control` | `JobView` |
| POST `/devices/{device_id}/jobs/{job_id}/result` | `ResultRequest` | `JobView` |

`after` 使用上一页 `next_cursor` 的最后可见 ULID。没有隐藏总数。设备与作业管理视图只属于同一实际项目成员本人；准确绑定且仍有效的设备 CLI session 还可读取自身精确 job_id 对账，已登记设备会话不能读取另一设备作业、充当另一设备管理者、枚举管理列表或批准。管理员身份不自动取得别人的工作区。

设备端先通过原 T01 口令/TOTP 显式取得独立 CLI 普通 human 会话：只列一个项目、有效期不超过 15 分钟，不使用长期 credential、浏览器 Cookie、委托或父会话。不接受 admin scope。设备视图状态为 pending、active、revoked，期限或恢复纪元失效派生 expired。需要 read、organize，实际 import.apply 还需 ingest。凭据留在本机私有 session 文件。claim 包含明确 alias、workspace_alias、本机真实工作区绑定 SHA-256 和八动作的显式子集。一次性 claim_code 只在第一次响应返回，回执不保存明文；响应丢失后保留原操作并对账，不能自动换键配对。配对必须由另一个正常本人会话复核准确 code/alias/binding；claim_code 两分钟有效。

权限矩阵唯一登记在 T01：`node.device_read` 为 read + 项目成员 + normal human；`node.device_manage` 为 organize + owner/coordinator/contributor/curator + normal human；`node.device_work` 为 read + 项目成员 + normal human。节点额外只核对同本人和准确设备 CLI session 绑定；实际文件读写仍受原 REST 权限、scope、用途和撤权约束。

两阶段：网页创建固定 `job` 与 canonical `job_digest` → queued；设备只读 inspect，回报完整 inspection → awaiting_approval；网页明确复核 `job_digest`、`approval_digest`、`workspace_binding` 和当前 revision → ready；设备出站 poll 才领取唯一 T06 JobAttempt、完整 job_fence 与 30 秒 lease → running。heartbeat 续原 fence；同工作区存在运行、取消中或未知 attempt 时不能领取第二执行者。引用前一私有计划可在原 job.input/report 中使用 `lantai.local-assistant-result-ref/v1`，由本机受限 artifact 解析，绝不上传 source_root 或本机绝对目录。

作业状态为 queued、awaiting_approval、ready、running、cancelling、succeeded、failed、cancelled、needs_reconciliation。只有准确绑定且 stopped 的 completed/exit_code 0 才能成为 succeeded。partial、unknown、过期 fence、取消后仍报告 completed 等一律保留 needs_reconciliation；不会重派或生成替代 operation。设备结果是带 fence 的历史本机观察，不能替代中央当前文件、权限、原生软件续做或恢复验收。

取消 queued 等未执行任务可直接 cancelled 且 stopped=true；未知作业取消仍保留 needs_reconciliation，stopped 仅表示本机进程停止，不代表外部副作用已知。已派发任务进入 cancelling，设备 heartbeat 停止本机 context 后回报实际结果。撤销先持久化 node revoked，再结束准确独立 CLI session；返回成功后原 REST 令牌也失效，不结束网页登录。掉线、撤权或过期时，worker 在私有状态保留原作业/输入/keys，未知副作用须先对账。核心重启保留原 jobs 与 attempts，queued/ready 按原键继续，已保存待发送结果只重送原结果。崩溃或过期而副作用未知的作业继续阻塞同工作区执行；本接口不提供清除未知、重派或跳过独立对账的恢复。

认领与节点观察保存在 runtime 的 node 自有表；所有任务状态、JobAttempt、lease、fence 和结果在原 jobs_jobs/jobs_attempts，未建立第六库、第二任务队列或任意 shell/本机 HTTP 监听。
