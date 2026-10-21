# 0008 首方出站助手与原作业权威

状态：已采纳（2026-10-20）。本条补充 [0004](0004-execution-common-protocol.md) 的首方执行分支，不改变原插件协议及其激活校验。精确 HTTP 接口见 [设备控制](../../api/device-control.md)，唯一领域归属仍见架构与扩展设计。

## 背景

网页需要向人明确启动的本机助手提交八种已有声明式动作。核心不得持有本机目录路径或另建任务真源；本机也不能接收任意 shell、远程 argv、传输令牌或服务端提供的 credentials。计划结果含私有绝对路径，不能直接回传网页再执行。

## 决定

1. 使用原 T01 口令/TOTP 登录取得独立 normal human CLI 会话，仅一个项目、最多十五分钟，拒绝 admin scope、长期 credential、父会话和委托。网页从不生成或保存设备会话；配对由另一个本人会话复核一次性 code、已选 workspace alias 及本机真实工作区摘要。两个会话撤销边界保持独立。
2. node 只拥有配对和项目/工作区约束。任务及执行唯一保存在原 T06 jobs_jobs/jobs_attempts；首方协议 lantai.local-assistant/v1 使用同一 JobAttempt/JobFence/lease/recovery_epoch，以 LocalExecutionBinding 绑定设备、工作区、原 job_digest 和 approval_digest。它没有第三方 extension activation，也不伪装成 corecheck。
3. 必须两阶段。网页先提交原 Job 和 JCS 摘要；设备仅 inspect，返回本机完整约束与 approval_digest；网页显式批准准确摘要后，设备出站 poll 才领取 T06 fence 执行。所有路径受本机已选目录别名根、项目、动作及原 REST 授权限制。
4. 本机私有 artifact 保存完整计划、结果与原 state/operation/keys。网络只携带 exact LocalResultRef，绑定原远程 Job ID 和完整私有结果 SHA-256。后续动作由助手从同工作区原 artifact 解析，inspect 和批准摘要绑定解析后的输入；服务端不接受本机绝对 source_root。
5. 作业结果是本机观察。准确且 stopped 的 completed/exit_code 0 才可记录 succeeded；停止不等于副作用已知。partial、unknown、过期 fence 或取消后仍 completed 保留 needs_reconciliation，阻塞同工作区第二执行者。不得清除未知后换键重跑。
6. 重启保留原 Job、operation 和 fence。queued/ready 可继续；已完成并保存的 pending 结果只重送，不执行第二遍。running 崩溃且没有结果 artifact 或租约过期时保留 unknown，须独立对账；本阶段不开放弱化该边界的 reconcile/clear 接口。
7. 撤销先持久化 node revoked，再由 T01 结束准确独立设备会话；用户网页登录不受该撤销影响。原 REST 和最终结果接受均复验当前授权。人启动的助手不是操作系统安全沙箱，不开启公共本机监听。

## 后果

新增 runtime node 自有配对表；没有第六库、跨库联查或第二 Job 队列。八动作可经真实核心 HTTPS 完成，但本机完成不替代中央字节验证、业务当前权限或审定。中断后未知外部副作用依旧是明确的验收门禁，不能据 stopped 或重启自动标记成功。
