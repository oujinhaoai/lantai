# 开发与验证

本篇列出当前工程中真实可运行的命令。命令变化时同步修改本篇与 [AGENTS.md](../AGENTS.md)，不保留失效说明。

## 环境

- Go 由 `go.mod` 的 `toolchain go1.26.8` 固定；本机 Go ≥ 1.21 且 `GOTOOLCHAIN` 为默认的 `auto` 时，`go` 命令会自动下载并使用该版本。
- 纯 Go 构建，不需要 CGo 或 C 编译器；只有 `-race` 测试需要本机 C 工具链。
- 开发工具（oapi-codegen、staticcheck、govulncheck）锁定在独立的 [`scripts/tools/go.mod`](../scripts/tools/go.mod)，通过 `go tool -modfile=scripts/tools/go.mod <tool>` 运行，不影响运行时依赖的版本选择。
- 数据库、运行数据与真实配置放在仓库之外；测试只使用临时目录和合成数据。

## 常用命令

在仓库根目录执行：

| 目的 | 命令 |
|---|---|
| 全部检查（格式、依赖整洁、vet、staticcheck、生成物无漂移、`-race` 测试、构建） | `scripts/check.sh` |
| 同上，但不用 `-race` | `LANTAI_RACE=0 scripts/check.sh` |
| 只跑测试 | `go test ./...` |
| 重新生成派生文件 | `scripts/generate.sh` |
| 构建入口 | `go build -o bin/lantai ./cmd/lantai` |
| 查看版本、契约与协议支持状态 | `go run ./cmd/lantai version` |
| 列出契约 schema | `go run ./cmd/lantai schema list` |
| 校验 JSON/YAML 文档 | `go run ./cmd/lantai schema validate lantai.error/v1 path/to/doc.json` |
| 校验定义库中的单个定义 | `go run ./cmd/lantai schema validate 'lantai.execution-common/v1#/$defs/task_fence' fence.json` |
| SQLite 能力实测（输出 JSON） | `go run ./scripts/probe/sqlite -dir <被测文件系统上的目录>` |
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
| 共同备份恢复与 CLI 集成 | `go test -run TestCommonBackup ./tests/integration/`；`go test -run TestLocalBackupRestoreCommandLifecycle ./cmd/lantai/` |
| 真实 Caddy HTTPS 模板隔离验证 | `LANTAI_TEST_CADDY=<已校验的caddy绝对路径> go test -run TestCaddySingleHTTPSGateway -v ./deploy/` |
| 单管理员本机离线恢复（交互式） | `go run ./cmd/lantai recover-admin -home <数据根> -admin <名称>` |
| Argon2id 默认参数在本机的耗时 | `go test -run '^$' -bench Default ./internal/identity/password/` |
| 跨模块集成测试（真实实例、身份、存储、目录、台账、来源限制、事件与查询） | `go test ./tests/integration/` |
| 1 GiB 分片续传与流式内存（默认 32 MiB） | `LANTAI_TEST_LARGE_MB=1024 go test -run TestLargeResumableTransfer -v ./tests/integration/` |
| 台账/事件/查询单元与故障验证 | `go test ./internal/ledger ./internal/provenance ./internal/events ./internal/query` |
| T03/T04 真模块集成 | `go test -run 'TestLedgerEvents\|TestPersonalRead' ./tests/integration/` |
| M2 T01/T02 领域适配与失败场景 | `go test ./internal/identity ./internal/storage ./internal/catalog ./internal/contract/schema -run 'TestHuman\|TestMilestone\|TestLifecycle\|TestGC\|TestContext\|TestExamples'` |
| 已知漏洞扫描 | `go tool -modfile=scripts/tools/go.mod govulncheck ./...` |
| 交叉编译示例 | `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/lantai` |

本机实例/schema 命令退出码：0 成功；1 校验或实例状态拒绝；2 用法错误；3 读写或内部错误。远程 CLI 以 JSON 输出，0 成功，1 领域拒绝，2 输入错误，3 读写/协议错误，4 冲突或旧 ETag，5 认证失败，6 可重试/限流，130 取消。详见[薄 CLI](contracts/client.md)。

初始化、迁移与离线恢复命令只能在服务端本机、服务停止时对数据根运行（`doctor` 只读，运行中也可用；`serve` 持有同一数据根锁运行服务）；`-home` 缺省时取环境变量 `LANTAI_HOME`。数据根、主密钥与口令都是真实凭据相关资料，开发与测试只用临时目录。`init` 与 `recover-admin` 在终端上不回显口令，验证器种子与恢复码只展示一次；输入不是终端时（例如脚本化测试）会给出提示。备份、恢复阶段与升级步骤见[备份恢复](contracts/backup-restore.md)。规则见[实例生命周期](contracts/instance.md)与[身份与授权](contracts/identity.md)。

## 服务与远程 CLI

先使用本机 `init` 完成首个管理员的口令/TOTP 设置，再运行 `serve`。启动在共同维护上下文中检查身份状态、登记内置扩展、恢复原操作并深度核验领域文件，收录持久 outbox 并重建/追平索引，成功后才开放监听；后台继续推进 outbox、查询与审计导出，不自动迁移或裁剪；完成备份的命令会确认其冻结水位。关闭时停止 HTTP、取消并排空后台任务，再关闭五库并释放锁。

数据根的可选 `config.yaml`（不含任何凭据）：

```yaml
contract: lantai.config/v1
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

这些是核心内部地址。对外只通过一个 HTTPS 网关路由 `/api/` 与 `/xfer/`，运维 `/healthz`、`/readyz`、`/metrics` 只在数字 loopback 地址开放；[部署模板](deployment.md)提供 Caddy、容器和 systemd 配置，实际证书安装与目标环境部署由运维执行。开发 `-merged` 共用 API 监听，客户端只有在显式 `--allow-http` 时才接受本机 loopback HTTP。API 的 JSON 限额不限制分片流；文件传输使用逐次 I/O 空闲超时。带宽 0 代表不限，具体限额须在目标环境实测。

CLI 会话保存在显式选择的私有文件中（Unix 0600、Windows 仅当前用户 ACL），凭据、口令和验证码不经命令行参数或普通输出。使用 `login --credentials-file` 或 `session exchange --token-file`，后续命令指定同一个 `--session-file`；通过 `meta` 查询能力。项目角色及 Agent 凭据通过 `identity challenge → verify → execute` 复用服务端 HumanGrant；管理员不会自动获得新项目的读取角色。`push` 固定原 create/commit 键到 `--state`，中断后重复同一命令；`pull` 只搬运清单声明文件并逐件验证 SHA-256。完整输入示例和命令见[客户端契约](contracts/client.md)，REST 见[HTTP 契约](contracts/http.md)。

T05/T06 的 M1 仅为协议与静态桩；没有任务/执行运行服务。共同备份/空目录恢复、内置扩展登记和单 HTTPS 网关模板已经实现；目标 NAS 流量 p95、Docker/systemd 实际部署和全面平台故障演练继续按独立测试任务验收，不能以存取冒烟通过宣称 M1 整体完成。

## 生成物

| 来源 | 生成物 | 生成器 |
|---|---|---|
| `schemas/common/v1/error-codes.json` | `internal/contract/errcode/codes_gen.go`、`docs/contracts/error-codes.md` | `scripts/gen/errcodes` |
| `internal/contract/ownership` | `docs/contracts/ownership.md` | `scripts/gen/ownership` |
| `internal/identity`（动作与策略登记） | `docs/contracts/identity-actions.md` | `scripts/gen/identity` |
| `api/openapi.yaml` + `schemas/` | `api/gen/openapi.bundle.json`（自包含，供生成器与外部 SDK 工具使用） | `scripts/gen/openapi` |
| `api/gen/openapi.bundle.json` | `internal/apiv1/models.gen.go` | oapi-codegen（配置 `api/oapi-codegen.yaml`） |

生成物纳入版本控制，不手改。各生成器支持 `-check`，`scripts/check.sh` 与 CI 会确认重新生成没有差异。

## 修改契约

1. 在 `schemas/` 修改或新增 schema，文档类 schema 登记到 `schemas/index.json`；新增错误码改 `schemas/common/v1/error-codes.json`。
2. 在 `schemas/examples/` 补正反例：每个样例声明契约、是否有效，无效样例声明预期失败的位置与关键字。
3. 运行 `scripts/generate.sh`，再运行 `scripts/check.sh`。
4. 按[兼容规则](contracts/README.md#schema-版本与兼容)判断是否需要新主版本，并同步调用方、示例与相关任务文档。

## 持续集成

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml)：Linux 上运行 `scripts/check.sh` 与 govulncheck；Linux、macOS、Windows 上运行 `go test` 并上传 SQLite 能力报告；六个目标（linux/darwin/windows × amd64/arm64）交叉编译。第三方 action 固定到提交 SHA。

## 平台验证状态

| 平台 | 构建 | 单元与契约测试 | SQLite 能力实测 | 文件系统故障演练 |
|---|---|---|---|---|
| macOS arm64（开发机，APFS） | 通过 | 通过（含 `-race`） | 通过，见下 | 未开始（T02/T08） |
| macOS arm64（CI） | 通过 | 通过 | 通过 | 未开始 |
| Linux amd64（CI） | 通过 | 通过（含 `-race`） | 通过 | 未开始 |
| Windows amd64（CI） | 通过 | 通过 | 通过 | 未开始 |
| Linux arm64、Windows arm64、macOS amd64 | 交叉编译通过 | 未运行 | 未运行 | 未开始 |

CI 结果来自 2026-09-27 首次运行（提交 `985fa0f`，GitHub 托管的 `ubuntu-latest`、`windows-latest`、`macos-latest` 运行器）：全部作业通过，三个平台的 SQLite 能力报告均满足必需项，单行提交约 0.4 ms（Linux）、0.5 ms（Windows）、1.3 ms（macOS，fullfsync）。托管运行器不代表目标 NAS 或其挂载方式，部署环境仍需单独实测。

开发机实测（2026-09-27，Go 1.26.8，modernc.org/sqlite v1.59.0，SQLite 3.53.4）：WAL、synchronous=FULL、fullfsync、STRICT 表、RETURNING、JSON 函数、FTS5、busy 超时后返回可分类错误、上下文取消可中断长查询、回滚、`wal_checkpoint(TRUNCATE)`、`VACUUM INTO` 快照与 `integrity_check` 均通过；写事务中强杀进程后重开，已提交数据完整、未提交数据不出现。启用 fullfsync 后单行提交约 4 ms（内置 SSD）至 10 ms（外置 SSD）。交叉编译通过不代表其他平台的文件语义、恢复或隔离已验证。

存储传输开发机实测（2026-09-27，同上环境，macOS 系统临时目录）：`LANTAI_TEST_LARGE_MB=1024` 的续传测试上传 1 073 754 169 字节（17 个 64 MiB 分片，第三片中途断开后只补传缺的 15 片）、分两段 Range 下载，整件 SHA-256 一致；最终复跑上传约 2.5 秒、全程约 3.1 秒，传输期间堆占用增长约 3.6 MiB（默认 32 MiB 规模约 1.6 MiB），不随文件大小线性增长。空间不足、文件占用、硬链接不可用与改名失败只经故障注入测试覆盖，真实文件系统上的故障演练仍属 TEST-M1-06。

M2 的 T01/T02 适配接口、存储迁移与接线边界见[协作适配契约](contracts/collaboration-foundation.md)。它们尚未增加远程命令或自动 GC 调度。
