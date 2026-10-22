# 显式出站设备 worker 设计

本修订把既有文件助手接到统一 HTTPS API。人从终端显式启动，进程不安装、不注册系统服务、不监听端口、不从网页执行 argv；停止后不后台恢复。既有八个动作仍由原 client 执行，T01/T06 的服务端租约与授权保持唯一真源。

## 本机批准边界

私有 workspace 内持久化精确 origin、受限项目、目录别名与动作白名单，凭据单独私有保存。网页任务不能自行指定本机绝对路径、URL、session/CA、可执行文件。每个实际动作须与本机只读 inspect 产生的 job_digest/approval_digest、选定 workspace、项目和目录别名一致；没有本机明确批准的任务不 claim、不执行。原 job 和原客户端 state 路径固定，重启不换 create/commit/operation 键。

服务端任务只能唤醒已批准本机输入，不能扩大本机目录和项目范围。所有控制请求通过固定的 /api/v1/ HTTPS 路由，无重定向、无弱 TLS、无代理配置或任意目标地址；证书与主机名由原客户端严格校验。设备认领需要短期挑战和显式一次性人确认，不由合成认领测试夹具替代真实核心授权。

## 运行与恢复

同一 workspace 仅一个 worker/单次 run 持有文件锁。配置、批准记录、待交付结果与租约关联均用既有私有原子输出；文件/父目录不跟随链接，不改现存权限。先持久化原租约与原 job/state 关联再执行；动作前、heartbeat、结果交付时复验租约与权限。heartbeat 取消、过期、撤权、失去租约或控制通道不明时取消动作 context，保留原状态并报告 unknown/取消观察，不声称副作用已回滚。

结果交付失败保留原结果与待交付关联，重启先对账原租约/结果。既有部分/unknown 业务状态继续由原 client 查询原 operation 并使用原键显式续办，不启动替代作业。不上传 session、token、CSRF、签名 URL、本机文件根或完整私有配置；结果不是服务端当前状态或人审结论。

## 验证与范围

协议与服务端负责人先对齐；测试使用真实 TLS 监听、受信 CA 和真实 client/动作函数，覆盖认领、轮询、claim、heartbeat、结果，失联/撤权/过期/取消/崩溃续办与单 workspace 锁。核心租约和权限端到端测试由双方共享，不以伪造 status 代替授权或实际动作。只使用临时合成工作区；不接入真实设备、真实目录、NAS 或生产授权。

## 冻结的线协议

纯 DTO 使用 `internal/contract/device`，避免客户端另定义中央状态。认领 `POST /api/v1/devices/claims`，网页一次码批准 `POST /devices/{id}/approve`；queued任务通过设备 `poll` 返回，本机提交 `/jobs/{id}/inspect` 的原 inspection，再由网页 `/jobs/{id}/approve` 批准原摘要。下一次 poll 执行 T06 claim，并返回原 JobAttempt fence；worker 在执行前及动作期间调用 `/heartbeat`，最后 `/result` 交付精确原 fence 与 `stopped=true`。控制写入均有保存/绑定的幂等键；未知控制响应不自动重跑动作。

八动作共享原 `Job` JSON。`input` / `report` 可为 `lantai.local-assistant-result-ref/v1`：原远端 job_id 与完整私有 result.result 字节 SHA256。服务端验证同设备/项目/别名/原任务的可用观察引用，worker 从固定私有 artifact 展开，job_digest 仍绑定原 ref Job；approval_digest/input_sha256 绑定实际展开的原文。网络仅上传安全 ref，不上传 source_root 或完整私有计划。

本机人审通过 `assistant result --workspace ROOT --job-id REMOTE_ULID --result-sha256 SHA256`，固定文件位置读取、复算原摘要后输出完整原 Result；没有 HTTP 原计划下载接口。网页要继续引用改链时，明确要求本机核对后再批准下一动作。

worker使用明确选择的原独立 T01 human CLI session，单项目、最长15分钟，不生成新长期设备凭据。过期/revoke不续期；网页使用同一human的另一browser会话。结果接受只清理本机待交付缓存，须响应回显精确原Result、job/approval摘要与stopped；不同或丢失的接受回执继续保留原状态。
