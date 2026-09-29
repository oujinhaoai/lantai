# M2 扩展包治理、一次性宿主与本机命令

本文记录 T09 在 M2 已实现的接口与调用方式（DEV-T09-03/04/05）。插件规格唯一权威仍是[扩展设计](../extensions.md)，机器字段以 `schemas/` 与 [OpenAPI](../../api/openapi.yaml) 为准；M1 清单与内置登记见[扩展契约](extensions.md)。本文不是独立 TEST 或 M2 门禁结论。

## 所有者与数据

| 事实 | 所有者与位置 |
|---|---|
| 包字节与版本 | 普通 `plugin` 资产的已提交版本（catalog/storage）；元数据 `extension_id`、`extension_version` |
| 包审定 | ledger 中该版本的当前审定事实（`approved` + `effective_review_id`）；T09 只引用 |
| 包登记、启用意图、配置 revision、历史 | main：`extensions_packages`、`extensions_enablements`、`extensions_enablement_history` |
| 探测证据、调用登记、熔断 | runtime：`extensions_activations`、`extensions_invocations`、`extensions_breakers`、`extensions_breaker_faults` |
| 业务结果与重试 | T06 Job/JobAttempt；检查证据经台账接受 |

跨库只经所属模块接口；启用不在同一 SQL 事务里“批准、启用并启动”。核心 `internal/extensions` 的 `Manager` 同时为 T06 提供处理器宿主端口，编译内置 `corecheck` 仍走发布绑定路径。

## 静态导入

管理员 `POST /api/v1/extensions/packages`（Idempotency-Key，body `{asset_id, version_id}`；CLI `lantai plugin import`）。服务端按调用者权限读取该 `plugin` 版本，经 storage 可信端口取回全部文件并逐件核验 SHA-256，再执行 `ReadPackage` 与 `CheckExternal`：

- 只接受 `server` 上的 `asset.validator` 与 `cli` 上的 `cli.command`；runtime `exec`、lifecycle `oneshot`、协议 `lantai.processor/v1`。
- node target 返回 `node_host_unavailable`，web target、非空插件依赖、`processor.requires`（M2 无管理员工具登记）、`permissions.api/network/secret_refs` 一律 `EXTENSION_POINT_UNSUPPORTED`，不静默忽略。
- 清单元数据须与资产元数据一致；编译内置包的 ID 保留给 `builtin_release`，外部包即使自称官方也不走自举例外。
- 同 ID/版本换摘要 `IDEMPOTENCY_CONFLICT`；相同字节再次导入返回原登记。

导入只登记字节与清单，不执行入口、安装脚本或探测，也不代表审定或启用。之后每次启用、探测和派发都重新读取包字节并复验包摘要，存储被替换即 `HASH_MISMATCH`。

## 启用、配置与停用

启停是管理员 HumanGrant 动作，经 `/api/v1/human/prepare` 的 kind `extension_enable` / `extension_disable`，再 verify 与 execute（与审定相同的 Challenge → TOTP → HumanGrant 流程）。启用请求字段：

```json
{"extension_id":"org.example.tools","extension_version":"0.1.0","package_digest":"sha256:…",
 "target":"server","scope_kind":"project","scope_id":"PROJECT_ID",
 "config":{},"config_revision":1,"trust":"trusted_unenforced","probe":true,"reason":"…"}
```

- 冻结的 `HumanAction` 绑定启用对象 ID（由实例 ID 与插件/target/作用域派生）与下一 revision、包摘要、target、作用域、配置 revision/摘要和实例 `plugins.*` 策略 revision。最终接受时由当前状态重算同一动作，任一变化即 `PRECONDITION_FAILED`，授权不能换用到其他包、配置或范围。
- 包审定必须当前有效并被记录其 review ID；撤销或重新审定后旧启用立即失效，需重新启用。
- 配置按包内 `config_schema` 离线校验；摘要不变沿用 revision，变化则 revision + 1。
- server 作用域为 instance 或 project；cli 为 instance 或 user。项目仍须在 `plugins.allowed` 中显式列出插件 ID，instance 启用不推出项目可用。
- 本宿主不能强制包声明的内存/CPU/网络/文件系统隔离与只读输入，因此每个外部包都需要请求中明确 `trust: trusted_unenforced`；客户端机器不受服务端控制，cli 启用同样需要。缺少即 `isolation_insufficient`。这是受信任部署决定，不是沙箱声明。
- server target 必须 `probe: true`，同一授权覆盖启用前的受限探测；cli target 不做服务端探测。

每次启用、升级（同一对象换版本）、配置变化或停用都产生新 `generation` 并追加历史。前一代的退场方式记录为 `drain`（正常升级/配置变化/停用排空）或 `revoke`（安全撤权）。停用请求 `{enablement_id, mode: drain|revoke, reason}`；revoke 立即取消本进程中该启用的在途宿主。

## 受限探测

server 启用提交后，在所有锁之外用同一授权运行一次合成调用：`job.json` 为 `lantai.processor-input/v1`、`mode: probe`、无业务输入，携带冻结配置；期限取 `timeout_seconds` 与 60 秒的较小者。结果必须协议合法（operation、producer 匹配，无文件、无检查）才记为 `ready`，否则 `probe_failed`，该代不可派发。探测不启用、不恢复已停用或撤权的包，也不授予新权限。

探测证据绑定环境摘要：平台、宿主能力、当前核心发布摘要、包摘要、入口摘要与配置摘要。任一变化即 `probe_stale`；管理员可 `POST /api/v1/extensions/enablements/{id}/probe`（CLI `lantai plugin probe --id`）在原启用授权允许探测时重跑。`GET /api/v1/extensions/enablements` 给出启用、审定、探测、熔断与宿主能力，并列出 `disabled`、`package_review_revoked`、`probe_required`、`probe_failed`、`probe_stale`、`isolation_changed`、`breaker_open` 等原因。

## 派发与最终接受

Flow 定义的 job 步骤可指定已启用包的 `asset.validator` 贡献 ID。T06 依次调用宿主端口：

1. `Snapshot`：StartJob 与每次 Run 前无副作用解析。项目作用域优先于 instance；核对项目白名单、当前审定、探测环境与信任决定，返回 `lantai.activation-snapshot/v1` 与 `source=package` 的 producer。
2. `Run`：在共享 security 锁内复验快照未变并占用熔断名额（调用 ID 为 JobAttempt ID），锁外读取并复验包字节，把目标 manifest 以只读 `in/manifest.yaml` 交给入口。结果须 schema 合法、operation/producer/输入引用与摘要一致、无文件与记录；`status: unsupported` 是合法业务结论（Job 失败原因 `unsupported_input`），不计故障。
3. `CheckSnapshot`：finish、证据接受和审定引用证据时的最终门禁。当前代直接有效；已排空的旧代只接受退场前准入、且未超过冻结期限的调用；revoke、审定撤回、白名单移除随时拒收，不等运行态追平。
4. `Settle`：`/jobs/reconcile` 确认停止后把未决调用记为取消或运行故障。

证据的 producer 由 `Registry.VerifyProducer` 核验：包登记存在、摘要一致且贡献已声明；这只证明身份，不代表启用或授权。Job 准入拒绝记为 `breaker_open`、`activation_stale`、`processor_not_allowed` 或 `processor_not_enabled`，未派发，可在条件恢复后显式重试。Worker 能力观测可报告 1–16 个贡献 ID，观测不授予任何权限。

启用中的包版本作为 `extension` 当前用途进入台账生命周期，普通回收返回 `ASSET_IN_USE`，引用保留由台账统一裁决。

## 熔断

参数取实例策略 `plugins.breaker_failures`（5）、`plugins.breaker_window_seconds`（600）、`plugins.breaker_cooldown_seconds`（900）、`plugins.breaker_half_open_max_calls`（1）。熔断键为包摘要、入口、target、宿主实例与启用作用域。

- 只计运行故障（崩溃、非零退出、超时、缺失/畸形输出）；合法 fail、unsupported、准入拒绝、取消与未派发不计。每个调用只结算一次，超时后迟到的退出不重复计数。
- closed 时按 `(now-600s, now]` 计数，业务成功不清窗；达到阈值原子转 open 并推进 epoch。
- open 在冷却内拒绝派发（`EXTENSION_BREAKER_OPEN`）；冷却到期只允许领取半开名额，不自动启动作业。半开调用即下一次正常获授权调用；协议合法完成（含合法 fail）关闭、推进 epoch 并清窗，运行故障重新 open 并重算冷却；取消或未派发释放名额；停止未知保留名额直到对账。
- 每次准入固定 epoch，旧 epoch 的迟到结果只留审计，不改变新窗口或状态。全部状态持久在 runtime，重启不丢冷却与名额。人工停用、撤权与审定撤回始终先于熔断判断。

## 一次性宿主

`RunOneShot` 为每次调用建私有目录：`job.json`（只读）、只读 `in/`、`out/`、`tmp/` 及 `pkg/`。`pkg/` 由包文件逐件边复制边核验摘要，入口为当前平台的平台制品，执行的是已核验副本，替换包存储不能换掉已核验字节。argv 只有入口与 `job.json` 绝对路径；环境只有 `TMPDIR/TMP/TEMP` 与 `LANTAI_PROTOCOL`，没有 PATH、HOME、会话或密钥；工作目录为私有目录。

标准输出丢弃，标准错误写入有界日志（尾部 64 KiB），日志不是协议。退出码 0 仍需完整结果；宿主拒绝符号链接、非普通文件、越界路径、超出数量/字节的输出及与 `result.json` 声明不一致的文件，`result.json` 上限 1 MiB。调用方可在交付前再校验结果 schema，拒绝时不向外交付任何文件。

超时与取消由宿主回收进程树：Unix 新建进程组并对组发 SIGKILL，正常退出后也回收残留成员；Windows 以挂起方式创建进程、加入 kill-on-close 作业对象后再恢复，关闭作业对象即回收。停止在 5 秒内无法确认时报告 unresolved，交由对账，不冒充已停止。

`HostCapabilities()` 如实发布：环境白名单、私有目录、核验副本、期限、输出限额与进程树回收（`process_group` 或 `job_object`）；内存、CPU、网络、文件系统隔离与只读输入均为 false，`sandbox` 恒为 false。Unix 上主动脱离进程组的后代可以逃逸，这是平台限制。

## 本机命令与 MCP

`cli.command` 扩展点（v1，所有者 cli）使用 [`lantai.cli-command-input/v1`](../../schemas/extensions/v1/cli-command-input.schema.json) 与 [`lantai.cli-command-result/v1`](../../schemas/extensions/v1/cli-command-result.schema.json)，同样是一次性文件协议。命令只读取用户显式选择的输入副本，只把核验后的文件写入调用者给出的空目录；它不接触兰台凭据，也不修改权威状态，需要入库时由用户另行 `push`/`commit`。

```sh
lantai plugin commands --server https://gateway.example --session-file session.json
lantai ext install --package ./pkg --registry ~/.config/lantai/extensions.json --server https://gateway.example --session-file session.json
lantai ext run org.example.tools inspect --registry ~/.config/lantai/extensions.json --input-file model.fbx --arg --strict --output ./out --server https://gateway.example --session-file session.json
```

- `GET /api/v1/extensions/commands` 只列出对调用者生效（instance 或本人 user 作用域）且审定有效的命令；服务端启用不等于本机信任。
- `ext install` 把本机目录的绝对真实路径、包摘要、入口摘要与平台写入私有注册表（Unix 0600 / Windows 当前用户 ACL），注册表绑定服务器 origin；服务器未启用该精确包时拒绝安装。
- `ext run <plugin-id> <command>` 只查注册表，不搜索 PATH 或当前目录；没有简写别名，不能遮蔽核心命令。每次运行前重新核验包与入口摘要（替换即 `HASH_MISMATCH`），并向服务器确认该精确包仍对本人启用（撤权即 `EXTENSION_ACTIVATION_STALE`）。参数逐项原样传递，不拼 shell；子进程拿不到 `LANTAI_SESSION_TOKEN` 或 HumanGrant。
- `ext list`、`ext remove --plugin ID` 只改本机注册表。

`lantai mcp --workspace DIR --extensions-registry FILE` 把已安装且当前启用的命令投影为 `ext_<贡献 ID 以下划线连接>` 工具：输入为工作目录内的相对文件、原样参数和已存在的空输出目录，调用走与 CLI 相同的 `local.Run`。行为注解由已核验契约生成：非只读、非破坏（只新增文件到空目录）、非幂等、封闭世界。结果受 64 KiB 限制；人审、凭据、插件启停与任意 shell 仍不进入 MCP。

## 插件 SDK 与示例

[`sdk/go/extension`](../../sdk/go/extension/) 是只依赖 Go 标准库的最小插件 SDK：读取 `job.json`、读取有界输入、在 `out/` 内创建输出、按实际文件生成 `files` 并原子写 `result.json`。它不携带凭据、不能提交或审定，也不是沙箱。[`tests/fixtures/public/oneshot`](../../tests/fixtures/public/oneshot/) 是用该 SDK 编写的合成检查器/命令，并按冻结配置或首个参数注入各类故障供测试使用。

## 验证

```sh
go test ./internal/extensions/... ./sdk/go/... -count=1
go test ./tests/integration -run 'TestM2Extension|TestRemoteExtension' -count=1
```

单元测试覆盖文件协议与各故障类、超时/取消的进程树回收、入口替换、环境白名单、导入不可变与拒绝项、启用门禁（审定、信任、探测、配置、白名单、授权换用）、探测失败与环境变化、排空/撤权/审定撤回、以及 §11.2 熔断的窗口、去重、冷却、单一半开、取消/未知、epoch 与重启。集成测试用真实实例、身份 TOTP、台账审定流程和一次性进程贯通：插件资产经真实检查/质检/人审批准 → 静态导入 → HumanGrant 启用与探测 → 项目白名单 → 外部检查器驱动第二条流程至发布 → 撤权后解析与最终接受拒绝；以及 CLI 安装/运行、MCP 工具、PATH 同名程序不执行、入口替换与撤权。

未验证：Linux/Windows 实机的隔离与回收行为只由 CI 托管 runner 的单元测试覆盖；内存/CPU/网络/文件系统强制约束不具备；常驻实例控制、跨插件依赖、节点侧宿主与第三方网页仍未启用。
