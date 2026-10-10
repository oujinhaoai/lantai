# 开发与验证

本篇列出当前工程中真实可运行的命令。命令变化时同步修改本篇与 [AGENTS.md](../AGENTS.md)，不保留失效说明。

## 环境

- Go 由 `go.mod` 的 `toolchain go1.26.9` 固定；本机 Go ≥ 1.21 且 `GOTOOLCHAIN` 为默认的 `auto` 时，`go` 命令会自动下载并使用该版本。
- 纯 Go 构建，不需要 CGo 或 C 编译器；只有 `-race` 测试需要本机 C 工具链。
- 开发工具（oapi-codegen、staticcheck、govulncheck）锁定在独立的 [`scripts/tools/go.mod`](../scripts/tools/go.mod)，通过 `go tool -modfile=scripts/tools/go.mod <tool>` 运行，不影响运行时依赖的版本选择。
- Python ≥ 3.11 用于最小 SDK、契约元数据生成与联调测试；运行时仅用标准库，`scripts/check.sh` 也运行 Python 测试。
- 数据库、运行数据与真实配置放在仓库之外；测试只使用临时目录和合成数据。

## 常用命令

在仓库根目录执行：

| 目的 | 命令 |
|---|---|
| 全部检查（格式、依赖整洁、vet、staticcheck、生成物无漂移、`-race` 测试、构建） | `scripts/check.sh` |
| 同上，但不用 `-race` | `LANTAI_RACE=0 scripts/check.sh` |
| 只跑测试 | `go test -timeout=20m ./...` |
| TOTP 夹具碰撞与重放回归 | `go test ./tests/integration -run '^TestIntegrationFreshTOTP' -count=1`；固定合成种子覆盖相邻同码，保留真实重放拒绝，普通取码只推进一时间步 |
| 重新生成派生文件 | `scripts/generate.sh` |
| 构建入口 | `go build -o bin/lantai ./cmd/lantai` |
| 查看版本、契约与协议支持状态 | `go run ./cmd/lantai version` |
| 列出契约 schema | `go run ./cmd/lantai schema list` |
| 校验 JSON/YAML 文档 | `go run ./cmd/lantai schema validate lantai.error/v1 path/to/doc.json` |
| 校验定义库中的单个定义 | `go run ./cmd/lantai schema validate 'lantai.execution-common/v1#/$defs/task_fence' fence.json` |
| SQLite 能力实测（输出 JSON） | `go run ./scripts/probe/sqlite -dir <被测文件系统上的目录>` |
| 验证实例数据位置（能力项加 15 分钟并发读写与完整性检查） | `go run ./scripts/probe/sqlite -dir <数据位置下的目录> -stress 15m` |
| 本机初始化实例与首个管理员（交互式） | `go run ./cmd/lantai init -home <数据根> -admin <名称>` |
| 启动同进程 API / 传输 / 本机运维监听 | `go run ./cmd/lantai serve -home <数据根>` |
| 本机开发合并 API 与传输监听 | `go run ./cmd/lantai serve -home <数据根> -merged` |
| REST 与 CLI 纵向联调（merged / split、身份管理、提交丢响应后重启恢复） | `go test -run 'TestRemoteCLIStorage\|TestRemoteIdentityAdministration' -v ./tests/integration/` |
| HTTPS 客户端 1 GiB 上传/下载双向中断续传 | `LANTAI_TEST_LARGE_MB=1024 go test -run TestRemoteResumableTransfer -v ./tests/integration/` |
| 只读诊断数据根 | `go run ./cmd/lantai doctor -home <数据根> [-json]` |
| 应用待执行的迁移 | `go run ./cmd/lantai migrate -home <数据根> -backup <完整备份>` |
| 共同备份与完整校验 | `go run ./cmd/lantai backup -home <数据根> -destination <空备份目录>`；`go run ./cmd/lantai backup-verify -backup <备份目录>` |
| 空目录恢复（保持维护状态） | `go run ./cmd/lantai restore -home <空目录> -backup <完整备份> -key-dir <单独恢复的原密钥目录>` |
| 领域核验 / 恢复分派 / 索引重建 | `go run ./cmd/lantai fsck -home <数据根>`；`recover`；`reindex` |
| 完成恢复对账 | `go run ./cmd/lantai restore-complete -home <数据根> -review review.json -evidence review.txt` |
| 完整业务链、独立 serve 中断与空目录恢复 | `GOMAXPROCS=2 GOFLAGS=-p=2 go test -race -count=1 -timeout=10m -run '^TestM2BusinessRecoveryAcceptance$' ./tests/integration`；[层级与证据说明](testing/m2-business-recovery.md) |
| 共同备份恢复与 CLI 集成 | `go test -run TestCommonBackup ./tests/integration/`；`go test -run TestLocalBackupRestoreCommandLifecycle ./cmd/lantai/` |
| 真实 Caddy HTTPS 模板隔离验证 | `LANTAI_TEST_CADDY=<已校验的caddy绝对路径> go test -run TestCaddySingleHTTPSGateway -v ./deploy/` |
| 单管理员本机离线恢复（交互式） | `go run ./cmd/lantai recover-admin -home <数据根> -admin <名称>` |
| Argon2id 默认参数在本机的耗时 | `go test -run '^$' -bench Default ./internal/identity/password/` |
| 跨模块集成测试（真实实例、身份、存储、目录、台账、来源限制、事件与查询） | `go test ./tests/integration/` |
| 1 GiB 分片续传与流式内存（默认 32 MiB） | `LANTAI_TEST_LARGE_MB=1024 go test -run TestLargeResumableTransfer -v ./tests/integration/` |
| 提交定点子进程强杀与恢复矩阵（原生运行；可保存 JSON 证据） | `go test ./tests/integration -run '^TestCommitCrashMatrix$' -count=1 -v`；详见[故障矩阵驱动](testing/commit-fault-matrix.md) |
| 提交文件错误、非法路径与无权威孤立目录 | `go test ./tests/integration -run '^TestCommit(FileFaultMatrix\|RejectsUnsafeManifestBeforeWriting\|OrphansNeverCreateAuthority)$' -count=1 -v` |
| 台账/事件/查询单元与故障验证 | `go test ./internal/ledger ./internal/provenance ./internal/events ./internal/query` |
| T03/T04 真模块集成 | `go test -run 'TestLedgerEvents\|TestPersonalRead' ./tests/integration/` |
| M2 T01/T02 领域适配与失败场景 | `go test ./internal/identity ./internal/storage ./internal/catalog ./internal/contract/schema -run 'TestHuman\|TestMilestone\|TestLifecycle\|TestGC\|TestContext\|TestExamples'` |
| MCP 对象输入边界与四客户端上传/备份/恢复（合成隔离实例） | `go test -race -count=1 -run '^TestMCPPush(InputContract\|FourClientRestore)$' ./tests/integration` |
| 人审目标冻结、返工与批次 REST/CLI | `go test -race ./tests/integration -run '^TestRemoteReview' -count=1 -v`；[验证边界与证据](testing/review-acceptance.md) |
| T06/T07 真实模块联调 | `go test ./tests/integration -run 'Test(M2Manual\|M2Job\|M2Real\|RemoteM2)' -count=1` |
| T09 一次性宿主、包治理、熔断与插件 SDK（构建合成 fixture 并真实起进程） | `go test ./internal/extensions/... ./sdk/go/... -count=1` |
| T09 真实模块贯通与本机命令/MCP | `go test ./tests/integration -run 'TestM2Extension\|TestRemoteExtension\|TestM2Business\|TestM2Governance' -count=1` |
| 扩展治理/重启与宿主超时回收 | `go test -race ./tests/integration ./internal/extensions -run '^TestM2GovernanceRealScopesProbeAndRestart$\|^TestOneShotTimeoutAndCancelReclaimProcessTree$' -count=1`；治理用例使用常规夹具预算，专门超时/取消用例仍验证进程树回收 |
| 检查适用性、raw 协议与公开来源包装回归 | `go test -race ./tests/integration -run 'TestM2BusinessCompletedCheckRetirementBeforeReview\|TestM2BusinessPublicContractPreservesActualRun\|TestM2GovernanceProbeCompleteRawProtocol\|TestM2GovernanceJobRejectsRawForbiddenFields' -count=1` |
| M2 固定上下文、讨论、里程碑 REST 与正式裁剪后并发重同步 | `GOMAXPROCS=2 GOFLAGS=-p=2 go test -race -count=1 -run '^TestM2ResyncAcceptance' -v ./tests/integration`；合成实例、时钟和覆盖边界见[增量重同步验收](testing/m2-resync-acceptance.md) |
| T08 到期清除与 GC 调度 | `go test ./internal/operations -run Scheduler -count=1`；`go test ./tests/integration -run TestM2LifecycleSchedulerDuePurgeAndGC -count=1` |
| 服务停止时运行一次到期提醒/清除与 GC | `go run ./cmd/lantai lifecycle -home <数据根>` |
| 扩展包导入、启用诊断、命令列表（远程） | `go run ./cmd/lantai plugin import\|list\|enablements\|probe\|commands ...` |
| 本机扩展命令（显式安装、按摘要复验后运行） | `go run ./cmd/lantai ext install --package DIR --registry FILE ...`；`ext run <plugin-id> <command> --registry FILE --output DIR ...` |
| Python SDK 单元测试 | `PYTHONPATH=sdk/python python3 -m unittest discover -s sdk/python/tests -v` |
| 已知漏洞扫描 | `go tool -modfile=scripts/tools/go.mod govulncheck ./...` |
| 交叉编译示例 | `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/lantai` |
| 发布工具的离线门禁与归档验证 | `python3 -m unittest discover -s scripts/tests -v`；不代表真实镜像构建 |
| 精确 main 制品构建（维护者手动，需全 10 项 main CI） | [发布制品](release-artifacts.md)；实际 Docker 镜像、六平台二进制与源码归档，不自动发布或部署 |

`scripts/check.sh` 默认 race 测试的单包累计上限为 30 分钟；本脚本 `LANTAI_RACE=0` 及 Linux/macOS 普通 CI 测试保持 20 分钟，Windows 普通 CI 为 30 分钟。慢速目标机可显式设置 `LANTAI_TEST_TIMEOUT=60m scripts/check.sh`，完整执行相同检查与全部用例；报告必须注明覆盖预算，不得将它写成默认 30 分钟检查通过。较慢 runner 上的真实实例与进程强杀矩阵需要容纳执行开销；单个用例断言和性能验收阈值不变。Windows 手动全包验证可用 `go test -timeout=30m -count=1 ./...`。

本机实例/schema 命令退出码：0 成功；1 校验或实例状态拒绝；2 用法错误；3 读写或内部错误。远程 CLI 以 JSON 输出，0 成功，1 领域拒绝，2 输入错误，3 读写/协议错误，4 冲突或旧 ETag，5 认证失败，6 可重试/限流，130 取消。详见[薄 CLI](contracts/client.md)。

初始化、迁移与离线恢复命令只能在服务端本机、服务停止时对数据根运行（`doctor` 只读，运行中也可用；`serve` 持有同一数据根锁运行服务）；`-home` 缺省时取环境变量 `LANTAI_HOME`。数据根、主密钥与口令都是真实凭据相关资料，开发与测试只用临时目录。`init` 与 `recover-admin` 在终端上不回显口令，验证器种子与恢复码只展示一次；输入不是终端时（例如脚本化测试）会给出提示。备份、恢复阶段与升级步骤见[备份恢复](contracts/backup-restore.md)。规则见[实例生命周期](contracts/instance.md)与[身份与授权](contracts/identity.md)。

## 服务与远程 CLI

先使用本机 `init` 完成首个管理员的口令/TOTP 设置，再运行 `serve`。启动在共同维护上下文中检查身份状态、登记内置扩展、恢复原操作并深度核验领域文件，收录持久 outbox 并重建/追平索引，成功后才开放监听；后台用两个独立循环推进：一个收录 outbox 并追平查询投影，另一个推进收件箱与审计导出，慢的一方不拖住事件收录；另每分钟清扫到期上传会话，删除暂存并释放上传保留；不自动迁移或裁剪；完成备份的命令会确认其冻结水位。关闭时停止 HTTP、取消并排空后台任务，再关闭五库并释放锁。

数据根的可选 `config.yaml`（不含任何凭据）：

```yaml
contract: lantai.config/v1
storage:
  min_free_bytes: 1073741824
  max_file_bytes: 1099511627776
  max_upload_bytes: 4398046511104
  max_upload_files: 100000
  upload_idle_expiry_seconds: 86400
  upload_absolute_expiry_seconds: 604800
listen:
  api: 127.0.0.1:8080
  transfer: 127.0.0.1:8081
  operations: 127.0.0.1:9090
  merged: false
http:
  max_json_bytes: 8388608
  api_timeout_seconds: 30
  allowed_origins: [https://lantai.example.test]
transfer:
  interactive_slots: 4
  batch_slots: 8
  batch_per_principal: 4
  batch_bytes_per_second: 0
  batch_bytes_per_second_while_interactive: 0
```

`storage` 下是最低可用空间、上传限额与上传会话到期，以上均为默认值；超限返回 `QUOTA_EXCEEDED` 及具体原因，语义见[存储契约](contracts/storage.md)。文件数很多时，创建会话的 JSON 请求还受 `http.max_json_bytes` 限制。分片大小与版本清单字节上限不开放配置。`listen` 下是核心内部地址。对外只通过一个 HTTPS 网关路由 `/api/` 与 `/xfer/`，运维 `/healthz`、`/readyz`、`/metrics` 只在数字 loopback 地址开放；[部署模板](deployment.md)提供 Caddy、容器和 systemd 配置，实际证书安装与目标环境部署由运维执行。开发 `-merged` 共用 API 监听，客户端只有在显式 `--allow-http` 时才接受本机 loopback HTTP。API 的 JSON 限额不限制分片流；文件传输使用逐次 I/O 空闲超时。带宽 0 代表不限，具体限额须在目标环境实测。

CLI 会话保存在显式选择的私有文件中（Unix 0600、Windows 仅当前用户 ACL），凭据、口令和验证码不经命令行参数或普通输出。使用 `login --credentials-file` 或 `session exchange --token-file`，后续命令指定同一个 `--session-file`；通过 `meta` 查询能力。项目角色及 Agent 凭据通过 `identity challenge → verify → execute` 复用服务端 HumanGrant；管理员不会自动获得新项目的读取角色。`push` 固定原 create/commit 键到 `--state`，中断后重复同一命令；`pull` 只搬运清单声明文件并逐件验证 SHA-256。完整输入示例和命令见[客户端契约](contracts/client.md)，REST 见[HTTP 契约](contracts/http.md)。

T05/T06 的 M2 已提供任务/Flow、手动执行与检查作业（官方内置或经 T09 治理启用的外部一次性检查器）；T07 远程接线及实际命令见[手动执行与远程协作](contracts/manual-execution.md)。自动 Runner/触发与任务租约定时清扫未启用，生命周期调度默认关闭。共同备份/空目录恢复、内置扩展登记和单 HTTPS 网关模板已经实现；目标 NAS 流量 p95、Docker/systemd 实际部署和全面平台故障演练继续按独立测试任务验收，不能以存取冒烟通过宣称 M1 整体完成。

## 生成物

| 来源 | 生成物 | 生成器 |
|---|---|---|
| `schemas/common/v1/error-codes.json` | `internal/contract/errcode/codes_gen.go`、`docs/contracts/error-codes.md` | `scripts/gen/errcodes` |
| `internal/contract/ownership` | `docs/contracts/ownership.md` | `scripts/gen/ownership` |
| `internal/identity`（动作与策略登记） | `docs/contracts/identity-actions.md` | `scripts/gen/identity` |
| `api/openapi.yaml` + `schemas/` | `api/gen/openapi.bundle.json`（自包含，供生成器与外部 SDK 工具使用） | `scripts/gen/openapi` |
| `api/gen/openapi.bundle.json` | `sdk/python/lantai/_contract.py` | `python3 sdk/python/generate.py`（`--check` 检查漂移） |
| `api/gen/openapi.bundle.json` | `internal/apiv1/models.gen.go` | oapi-codegen（配置 `api/oapi-codegen.yaml`） |

生成物纳入版本控制，不手改。各生成器支持 `-check`，`scripts/check.sh` 与 CI 会确认重新生成没有差异。

## 修改契约

1. 在 `schemas/` 修改或新增 schema，文档类 schema 登记到 `schemas/index.json`；新增错误码改 `schemas/common/v1/error-codes.json`。
2. 在 `schemas/examples/` 补正反例：每个样例声明契约、是否有效，无效样例声明预期失败的位置与关键字。
3. 运行 `scripts/generate.sh`，再运行 `scripts/check.sh`。
4. 按[兼容规则](contracts/README.md#schema-版本与兼容)判断是否需要新主版本，并同步调用方、示例与相关任务文档。

## 持续集成

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml)：Linux 上运行 `scripts/check.sh`（race 单包累计上限 30 分钟）与 govulncheck；Linux、macOS、Windows 上按显式矩阵运行 `go test -timeout=<平台上限> -count=1 ./...` 并上传 SQLite 能力报告（Linux/macOS 为 20 分钟，Windows 为 30 分钟，均为单包累计防挂死上限，不替代业务性能阈值）；六个目标（linux/darwin/windows × amd64/arm64）交叉编译。第三方 action 固定到提交 SHA。

## 平台验证状态

| 平台 | 构建 | 单元与契约测试 | SQLite 能力实测 | 文件系统故障演练 |
|---|---|---|---|---|
| macOS arm64（开发机，APFS） | 通过 | 通过（含 `-race`） | 通过，见下 | [提交强杀与原生文件子集](testing/commit-fault-matrix.md)已运行；断电未测 |
| Linux amd64（测试容器，btrfs） | 测试二进制通过 | 提交矩阵与关联组件通过 | 未重测能力清单 | 强杀、句柄与跨卷子集已运行；真实满卷与断电未测 |
| macOS arm64（CI） | 通过 | 通过 | 通过 | 未开始 |
| Linux amd64（CI） | 通过 | 通过（含 `-race`） | 通过 | 未开始 |
| Windows amd64（CI） | 通过 | 通过 | 通过 | 未开始 |
| Linux arm64、Windows arm64、macOS amd64 | 交叉编译通过 | 未运行 | 未运行 | 未开始 |

CI 结果来自 2026-09-27 首次运行（提交 `985fa0f`，GitHub 托管的 `ubuntu-latest`、`windows-latest`、`macos-latest` 运行器）：全部作业通过，三个平台的 SQLite 能力报告均满足必需项，单行提交约 0.4 ms（Linux）、0.5 ms（Windows）、1.3 ms（macOS，fullfsync）。托管运行器不代表目标 NAS 或其挂载方式，部署环境仍需单独实测。

能力项都是低并发检查，不足以判定一个位置能否承载实例。验证数据位置时必须加 `-stress`（至少 15 分钟）：多连接并发读写、逐行核对内容摘要，结束后用新连接做 `integrity_check`。未通过即说明该位置不能使用；通过不能证明位置安全。2026-10 的 NAS 测试中，厂商经 FUSE 映射的共享文件夹能力项全部通过，却在并发读写下间歇性地把库写坏，同一位置也有并发实测通过的时段；同机的 Docker 命名卷通过，做法见[部署的数据位置](deployment.md#数据位置)。CI 在三个平台上各跑 30 秒并发实测，只验证实测本身可用，不代表部署位置合格。

开发机实测（2026-09-27，Go 1.26.8，modernc.org/sqlite v1.59.0，SQLite 3.53.4）：WAL、synchronous=FULL、fullfsync、STRICT 表、RETURNING、JSON 函数、FTS5、busy 超时后返回可分类错误、上下文取消可中断长查询、回滚、`wal_checkpoint(TRUNCATE)`、`VACUUM INTO` 快照与 `integrity_check` 均通过；写事务中强杀进程后重开，已提交数据完整、未提交数据不出现。启用 fullfsync 后单行提交约 4 ms（内置 SSD）至 10 ms（外置 SSD）。交叉编译通过不代表其他平台的文件语义、恢复或隔离已验证。

存储传输开发机实测（2026-09-27，同上环境，macOS 系统临时目录）：`LANTAI_TEST_LARGE_MB=1024` 的续传测试上传 1 073 754 169 字节（17 个 64 MiB 分片，第三片中途断开后只补传缺的 15 片）、分两段 Range 下载，整件 SHA-256 一致；最终复跑上传约 2.5 秒、全程约 3.1 秒，传输期间堆占用增长约 3.6 MiB（默认 32 MiB 规模约 1.6 MiB），不随文件大小线性增长。提交定点强杀、文件错误与原生句柄/跨卷/小容量满卷驱动见[提交故障矩阵](testing/commit-fault-matrix.md)。错误注入、真实进程终止、原生文件语义和断电分别记证据；现有结果不代表 TEST-M1-06 三平台全部通过。

容量与性能：最终性能取决于部署环境的系统与硬件。1 万资产 / 5 万版本公共合成集的查询、写入、重建与库增长基线，以及接口 p95 等阈值，只在实际部署环境测量；开发机只用小规模合成集做正确性回归，测得的时延与吞吐不作为基线或阈值。

M2 的 T01/T02 适配接口、存储迁移与接线边界见[协作适配契约](contracts/collaboration-foundation.md)。到期清除与 GC 由 T08 调度器执行，`serve` 中默认关闭，需在 `config.yaml` 设置 `lifecycle.scheduler: true`，见[生命周期调度](contracts/lifecycle-scheduler.md)；扩展包导入、启用、探测、熔断和 `lantai ext` 见[扩展包治理](contracts/extension-governance.md)。


一次性宿主的 Windows 临时目录回收使用原生文件占用反例验证：

```powershell
go test -count=1 -run '^TestOneShotWindowsCleanup' ./internal/extensions
```

测试分别持有已核验入口的无删除共享句柄，再验证释放后的有界回收和持续占用时的结果拒绝。进程退出不自动证明目录已删除；最多 2 秒的清理重试只处理 Windows 占用/访问错误，不能替代停止未知进程的对账或独立平台验收。

Windows 原子替换与审计占用恢复的原生验证：

```powershell
go test -count=1 -run '^TestWriteFileAtomicWindows' ./internal/platform/fsutil
go test -count=1 -run '^TestAuditWindowsOccupiedManifestRecovery$' ./internal/events
```

反例持有无删除共享句柄，核对释放后替换、持续失败时旧文件/水位保留、临时文件清理与同事实重试恢复。重试不改变仅创建一次文件的排他语义。
