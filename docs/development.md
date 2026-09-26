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
| 已知漏洞扫描 | `go tool -modfile=scripts/tools/go.mod govulncheck ./...` |
| 交叉编译示例 | `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/lantai` |

`lantai` 退出码：0 成功；1 文档未通过校验；2 用法错误；3 读写或内部错误。`-json` 输出供自动化使用。

## 生成物

| 来源 | 生成物 | 生成器 |
|---|---|---|
| `schemas/common/v1/error-codes.json` | `internal/contract/errcode/codes_gen.go`、`docs/contracts/error-codes.md` | `scripts/gen/errcodes` |
| `internal/contract/ownership` | `docs/contracts/ownership.md` | `scripts/gen/ownership` |
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
| Linux amd64/arm64 | 交叉编译通过 | CI 配置待首次运行 | CI 配置待首次运行 | 未开始 |
| Windows amd64/arm64 | 交叉编译通过 | CI 配置待首次运行 | CI 配置待首次运行 | 未开始 |
| macOS amd64 | 交叉编译通过 | 未运行 | 未运行 | 未开始 |

开发机实测（2026-09-27，Go 1.26.8，modernc.org/sqlite v1.59.0，SQLite 3.53.4）：WAL、synchronous=FULL、fullfsync、STRICT 表、RETURNING、JSON 函数、FTS5、busy 超时后返回可分类错误、上下文取消可中断长查询、回滚、`wal_checkpoint(TRUNCATE)`、`VACUUM INTO` 快照与 `integrity_check` 均通过；写事务中强杀进程后重开，已提交数据完整、未提交数据不出现。启用 fullfsync 后单行提交约 4 ms（内置 SSD）至 10 ms（外置 SSD）。交叉编译通过不代表其他平台的文件语义、恢复或隔离已验证。
