# 单次受控本机助手

`lantai assistant` 支持离线文件交接与显式启动的出站 HTTPS 设备 worker，直接复用 `internal/client` 的入藏、工作集与引用副本函数。它不监听端口、不运行 shell、不后台自动启动、不签发或续期长期生产凭据，不移动/删除源件，也不自动重试。

## 检查与批准

在操作者以后明确选择的**私有** workspace 中保留彼此独立的 `sources/`、`state/`、`working/`、`requests/`、`reports/`、`credentials/`。作业、输入和报告须是私有普通 JSON 文件；不接受链接、路径穿越、绝对的外层作业路径字段、重复键、未知字段或超过 8 MiB 的 JSON。程序不会修改现存目录权限。

```sh
lantai assistant inspect --workspace /explicit/private-workspace --job requests/workset-job.json
# 人核对 action、origin、相对目录和写入范围，并从本次检查结果取得完整摘要：
lantai assistant run --workspace /explicit/private-workspace --job requests/workset-job.json \
  --approve <approval_digest> --session-file credentials/session.json --ca-file credentials/ca.pem
```

`--approve` 不是凭据；它绑定确切 job 字节、解释后的 input/report 字节与规范 workspace 路径。文件变化须重新检查。执行网络操作时只使用显式会话文件，不读取 token 环境变量；会话必须绑定同一 origin，凭据独立于源、状态和工作副本。CA 只配置本客户端，TLS 证书及 hostname 始终校验。离线操作拒绝会话/CA 参数。

inspection 和 result 同时含 `job_digest`，为**原 job 文件 JSON 的 JCS 字节 SHA-256 小写十六进制**，无 `sha256:` 前缀，不使用补入默认值后的结构重新编码。页面用它关联原作业；格式化 JSON 不改变 job_digest，但会改变 approval_digest。外置 input_file/report_file 内容还分别由本次本机批准摘要绑定，页面导入仍须核对原输入及结果内容。两种摘要均不构成服务端签名或授权凭据。

程序和已有客户端重复核验目录与固定输入，并以原客户端的固定根/私有原子输出保护结果；它不是针对任意本机进程、管理员或恶意挂载变更的操作系统安全沙箱。

## 作业协议

```json
{
  "schema": "lantai.local-assistant-job/v1",
  "job_id": "synthetic-workset-v1",
  "action": "workset.pull",
  "origin": "https://gateway.example",
  "input": {
    "schema": "lantai.client-workset/v1",
    "entries": [{"asset_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","version_id":"01ARZ3NDEKTSV4RRFFQ69G5FAW","destination":"scene"}]
  },
  "directory": "working/scene-copy",
  "output": "reports/workset-result.json",
  "purpose": "archive_review",
  "timeout_seconds": 3600
}
```

上面的 ID 是合成例子，既不创建真实访问，也不会自动执行。job_id 为 1–100 个 ASCII 字母/数字/`_`/`-`，首位字母或数字。外层 path 字段使用 `/` 分隔的 workspace 内相对路径，不能用 `.` 代表整个工作根。`input_file` 可替代 inline `input`，二者只能选一个；文件必须是原客户端的纯输入/计划 JSON，不是助手结果 envelope。`report_file` 同样可替代 inline `report`。

| action | input | 外层路径及补充字段 | 网络/业务写 |
|---|---|---|---|
| import.plan | 原 ImportInput | directory=源目录，output=计划结果 | 无网络，仅输出计划 |
| import.apply | 原 ImportPlan | directory=原源目录，state=原状态目录，output=逐项结果，origin | 原上传/commit 写入；稳定原键 |
| import.verify | 原 ImportPlan | 上述字段加 report、destination=独立验证根、purpose | 真实远端与逐文件核验，不写服务端 |
| workset.plan | 原 WorksetInput | directory=计划工作根，output、origin | 受权只读规划，不创建工作根 |
| workset.pull | 原 WorksetInput | 上述字段加 purpose | 原 ReadGrant/Range 拉取到工作根 |
| references.plan | 原 ReferenceInput | directory=选定笔记源根、output | 无网络，输出可审阅计划 |
| references.copy | 原 ReferencePlan | directory=原源、destination=独立新副本、output | 无网络，源不改 |
| references.rollback | 原 ReferencePlan | directory=改链副本、destination=第三个独立目录、post_cutover_refs=显式数组、output | 无网络，不撤销服务端版本 |

import/reference plan 中原客户端持久化的 source_root 保持原值，但必须与 workspace 内选定源绑定，不能用一个外层相对目录把计划重定向到别处。output 必须在源/state/工作副本之外，不覆盖 job、输入、旧 report、会话或 CA；仅允许重放同 job/action/job_digest/approval_digest 的已有助手结果，其他文件不覆盖。

## 回执、取消与恢复

结果 schema 为 `lantai.local-assistant-result/v1`，含 job_id、action、job_digest、approval_digest、status、exit_code、时间、error_code 与 `result`。`result` 保留原客户端的计划或逐项回执。已知凭据键、Bearer 字符串和短期 transfer URL 在输入与输出中保守拒绝，原始错误消息不导出；这不等于通用秘密识别，任意笔记正文仍按私有数据保管。程序不签发设备凭据。

- `completed` 与 exit_code=0 才表示该**动作**完成。import.apply 的完成仍是历史回执，不会把 current_remote_verified 改成 true。
- `partial`：有明确的部分结果或待著录项；`unknown`：操作结果或结果保存不明；`cancelled`：上下文取消/超时。它们都不是整体成功或已回滚。
- 相同批准作业可以由人显式重跑以复用**原**客户端状态；不自动换键、换目录或创建替代操作。丢失原状态或捕获点外未知操作继续拒绝/对账。
- 失败仍尽力保存受控的部分结果；如果保存也失败，stdout 返回 unknown 和 `ASSISTANT_RESULT_NOT_SAVED`，保留全部原状态，不据此认定服务端没提交。
- 原始错误消息不进入回执，避免泄露 URL、凭据或本机路径；实际动作的规范结果仍可能含原计划中的私有来源路径，须私有保管。

## 保留的合成认领夹具

包内 `SyntheticClaims` 只在进程内合成夹具中测试：HTTPS loopback origin、短期随机挑战、job 摘要/设备别名绑定、过期、首次确认消费与并发防重放。它不发网络请求、不落盘、不签发 token，也不接入 RunFile 授权。receipt 明确 `synthetic_only=true`、`authorizes_execution=false`、`credential_issued=false`，不能代替真实设备授权或 `--approve`。

该夹具不代替下文真实 HTTPS 设备通道及 T01/T06 核心授权测试。本轮仅在云内临时合成目录验证，不安装到用户电脑或 NAS；生产部署和真实资料访问不属于本轮执行。

## 显式联网设备通道

本修订增加真实出站 HTTPS worker，不再限于文件交接。它使用原 T01 的独立 human CLI 会话：只允许一个项目、最长 15 分钟，按动作显式取得 read / ingest / organize；网页登录用同一主体的另一个 browser 会话，不把网页 Cookie、管理员身份或新长期设备密钥交给 worker。设备登记与一次码由 node 管理，作业与唯一 JobAttempt/fence 始终由 T06 jobs 管理。

先在明确私有 workspace 内准备原合法 `credentials/session.json` 与可信 `credentials/ca.pem`；会话 schema 保持 `lantai.client-session/v1`，origin 精确相同。只有配置路径进入 worker 配置，不复制 token。下面为合成配置范例，路径按本机明确选择调整：

```json
{
  "schema": "lantai.assistant-worker-config/v1",
  "origin": "https://gateway.example",
  "instance_id": "01ARZ3NDEKTSV4RRFFQ69G5FAV",
  "project_id": "01ARZ3NDEKTSV4RRFFQ69G5FAW",
  "alias": "selected-device",
  "workspace_alias": "selected-workspace",
  "roots": {
    "directory": "sources",
    "state": "state",
    "destination": "working",
    "output": "reports"
  },
  "actions": ["import.plan", "import.apply", "import.verify"],
  "session_file": "credentials/session.json",
  "ca_file": "credentials/ca.pem",
  "poll_seconds": 3,
  "heartbeat_seconds": 2
}
```

四个本机根须彼此独立，且与配置、凭据目录和 `.lantai-assistant/` 私有控制状态分离；路径不能为链接或逃出 workspace。actions 只能选择上表八个动作的子集。配置是操作者本机选择，网页任务不能更换它、扩大根目录/项目或提供任意 URL / session / CA / argv。

```sh
# requests/worker-input.json 须事先私有保存；创建原子私有配置，已有配置拒绝覆盖。
lantai assistant configure --workspace /explicit/private-workspace \
  --input requests/worker-input.json --config local/worker.json

# 显示一次认领码。将其与本机 workspace alias / binding 在设备网页逐项核对并批准。
lantai assistant enroll --workspace /explicit/private-workspace --config local/worker.json

# 人在终端显式启动。--once 处理一次观察后退出；不带它则仅在本进程存活时轮询。
lantai assistant worker --workspace /explicit/private-workspace --config local/worker.json --once
```

worker 不安装软件、不注册系统启动或后台 daemon、不监听本机端口。只连接配置中的精确 HTTPS origin，沿原客户端校验证书与 hostname，禁用重定向；没有 HTTP 降级或关闭 TLS 的开关。

每个作业执行经历独立的两阶段批准：网页提交原 Job 与 job_digest；worker 仅检查白名单/别名/项目及原输入，回传不含绝对路径的 inspection；网页由人明确批准本机 approval_digest 与 workspace_binding；后续 poll 才取得 T06 唯一 attempt/fence。执行前再 heartbeat 校验，同一 workspace 的 worker 与单次 `run` 共用跨进程锁。没有本机 inspection 或当前租约，八动作均不执行。

### 私有计划与结果引用

原 import/reference 计划包含本机绝对 source_root，完整原计划、回执、原生 JSON 词法及状态只保留在私有 workspace。网络结果的 `result` 只交付：

```json
{"schema":"lantai.local-assistant-result-ref/v1","job_id":"01ARZ3NDEKTSV4RRFFQ69G5FAX","result_sha256":"0000000000000000000000000000000000000000000000000000000000000000"}
```

上例零摘要只是合成占位，实际使用 worker 返回的完整摘要。下一动作的 `input` / `report` 可使用同一引用，worker 只从固定私有 artifact 中展开。引用必须匹配同项目、同别名、同配置的原任务与完整摘要；完整展开后的输入额外进入本机 approval_digest/input_sha256，job_digest 仍绑定原引用 Job，不修复或替换原计划摘要。`import.apply` 首次批准执行时固定原 plan 对应的 state 路径；后续 apply/verify 不能换 state 生成替代键。

这允许 plan→apply→verify、引用计划→副本/回滚联网衔接而不上传本机根或完整私有笔记。网络状态仍只是设备观察，不能作为服务端当前字节、人审、备份或恢复接受真值。

### 停止、失联与重启

heartbeat 持续复验 fence、job/approval 摘要、当前授权及本机配置。取消、过期、撤权、失去租约或控制连接不明会取消动作 context，保留原工作副本、上传状态、原 create/commit/upload/operation 身份。停止请求不等于副作用回滚；未知项不会自动重派或自动标成功。

完整结果先原子写本机 artifact，再保存原 fence 与待交付回执。结果响应丢失时，下次显式启动只重送该回执，核对服务端接受的精确原结果后才清待交付状态。崩溃时若只有 running 记录、没有已保存结果，则回报 unknown 并要求对账，不重新执行动作。后续确需续办时，须人重新派发/批准原输入与原 state；原客户端负责原 operation 对账与幂等续传。

`.lantai-assistant/`、配置、会话及原状态都须私有保全，不进入管理档案或一般日志。程序仍不是防御其他本机进程、管理员或恶意挂载的操作系统沙箱。

网页只收到私有结果引用时，用以下只读命令在原 workspace 核对完整原计划／差异，然后再在网页明确批准后续动作：

```sh
lantai assistant result --workspace /explicit/private-workspace \
  --job-id <ref.job_id> --result-sha256 <ref.result_sha256>
```

该命令不联网、不读取会话、不改文件，仅从固定私有 artifact 读取并复算与引用一致的完整 result.result 摘要。输出 `lantai.local-assistant-result-inspection/v1`，原计划在输出的 `result.result` 中；其中包含本机私有来源路径，留在本机审阅，不上传到网页或普通日志。缺失、错摘要、链接或非私有 artifact 会拒绝。

核心接受仍可能是 `needs_reconciliation`（例如结果晚到或取消竞态），即使本机动作返回 completed。worker 此时输出 unknown 与实际 core_state，保存原 fence/结果的未决关联；重启只读取原核心作业状态，不重执行或清除未决事实。只有核心在当前授权下真正进入终态，缓存关联才可释放。导入 commit 已接受但响应丢失时，父进程取消不会把原 unknown 降为无未决副作用的 cancelled。
