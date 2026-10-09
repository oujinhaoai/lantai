# 0001 Go 工程基线、工具链与依赖锁定

状态：已采纳（2026-09-27）。相关：[开发与验证](../development.md)、[依赖与许可证](../dependencies.md)。

2026-10-09 安全补丁更新：工具链与容器构建统一由初始 Go 1.26.8 升至 1.26.9，采用[官方补丁](https://go.dev/doc/devel/release#go1.26.9)修复标准库漏洞；`go 1.26.0` 的语言基线与第三方模块版本不变。

## 背景

核心采用 Go 模块化单体、五个 SQLite 库与独立网页。开工前需要一个可重复构建、可重复生成、有静态检查与跨平台构建的工程，并锁定 HTTP/OpenAPI、SQLite、查询与 schema 校验的依赖。

## 决定

1. 单一 Go 模块 `github.com/oujinhaoai/lantai`，`go 1.26.0` 并以 `toolchain go1.26.9` 固定工具链；入口统一为 `cmd/lantai`，只做参数、组装与退出码，当前只提供已实现的 `version`、`schema` 子命令，不预留空命令。
2. 开发工具（oapi-codegen、staticcheck、govulncheck）放在独立的 `scripts/tools/go.mod`，用 `go tool -modfile=...` 运行，避免工具依赖经最小版本选择抬高运行时依赖。
3. HTTP 使用标准库；SQLite 使用纯 Go 的 modernc.org/sqlite，连接固定 WAL、synchronous=FULL、busy_timeout、外键、defensive、写事务 `BEGIN IMMEDIATE`，macOS 另开 fullfsync；SQL 采用 `database/sql` 显式语句，暂不引入 sqlc。
4. schema 校验用 santhosh-tekuri/jsonschema v6，YAML 用 go.yaml.in/yaml/v3 并按 JSON 数据模型转换。
5. 派生文件（错误码常量与表、所有权表、OpenAPI 打包文档、Go 传输类型）纳入版本控制，生成器支持 `-check`；`scripts/check.sh` 汇总格式、依赖整洁、vet、staticcheck、生成物漂移、`-race` 测试与构建，CI 在 Linux 执行全部检查，并在三平台测试、六目标交叉编译。

## 后果

- 新机器只需任意 Go ≥ 1.21 即可按 `go.mod` 自动取得同一工具链；不需要 C 工具链（`-race` 除外）。
- macOS 上开启 fullfsync 使单行提交变慢（开发机实测约 4–10 ms），换取断电后已提交事务仍在；其他平台的实际表现由 CI 报告与平台验收记录确认。
- 交叉编译通过只证明能产出二进制，不代表平台文件语义、恢复或隔离已验证。

## 未采纳

- mattn/go-sqlite3：需要 CGo，作为纯 Go 驱动不达标时的对照。
- 路由框架与通用业务框架：当前接口不需要，按需再评估。
- sqlc：已确认 v1.31.1 可无 CGo 构建，但 T00 只有三张基础设施表；首个采用它的模块负责锁定版本与接入生成检查。
