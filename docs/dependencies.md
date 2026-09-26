# 依赖、版本与许可证

核对日期：2026-09-27。版本由 `go.mod`/`go.sum` 与 `scripts/tools/go.mod`/`go.sum` 锁定；许可证依据各模块发布包中的许可证文件逐一核对。本项目以 GPL-3.0 发布，下列许可证均与之兼容。新增依赖须有实际用途，并在本篇记录版本、用途、兼容范围与许可证。

## 链接进程序的依赖

`go list -deps ./...` 得到的全部第三方模块：

| 模块 | 版本 | 用途 | 许可证 |
|---|---|---|---|
| `modernc.org/sqlite` | v1.59.0（内含 SQLite 3.53.4） | 纯 Go SQLite 驱动，无需 CGo | BSD-3-Clause；SQLite 本体为公有领域声明；随包的 sqlite-vec 为 MIT |
| `modernc.org/libc` | v1.75.7 | 驱动的 C 运行时转译 | BSD-3-Clause；第三方部分（Go、musl 等）为 BSD-3-Clause/MIT |
| `modernc.org/mathutil` | v1.7.1 | 驱动依赖 | BSD-3-Clause |
| `modernc.org/memory` | v1.12.1 | 驱动依赖 | BSD-3-Clause（另含 Go 与 mmap-go 的 BSD-3-Clause） |
| `github.com/dustin/go-humanize` | v1.0.1 | 驱动依赖 | MIT |
| `github.com/google/uuid` | v1.6.0 | 驱动依赖 | BSD-3-Clause |
| `github.com/mattn/go-isatty` | v0.0.24 | 驱动依赖 | MIT |
| `github.com/ncruces/go-strftime` | v1.0.0 | 驱动依赖 | MIT |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | 驱动依赖 | BSD-3-Clause |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 | JSON Schema 2020-12 校验 | Apache-2.0 |
| `go.yaml.in/yaml/v3` | v3.0.5 | YAML 解析（按 JSON 数据模型解释） | Apache-2.0；移植自 libyaml 的部分为 MIT |
| `golang.org/x/sync` | v0.23.0 | 可取消、公平的加权信号量（锁协调） | BSD-3-Clause |
| `golang.org/x/text` | v0.42.0 | schema 校验错误信息的本地化打印 | BSD-3-Clause |
| `golang.org/x/sys` | v0.47.0 | 驱动依赖 | BSD-3-Clause |

发布二进制时须随附上述依赖的许可证与版权声明（BSD/MIT/Apache 均要求保留声明）。发布流程归 T08，届时从 `go.sum` 生成第三方声明，不在仓库预填 `NOTICE`。

## 开发工具（不链接进程序）

| 工具 | 版本 | 用途 | 许可证 |
|---|---|---|---|
| `github.com/oapi-codegen/oapi-codegen/v2` | v2.8.0 | 从打包后的 OpenAPI 3.1 文档生成 Go 传输类型 | Apache-2.0 |
| `honnef.co/go/tools`（staticcheck） | v0.8.1（2026.2.1） | 静态检查 | MIT |
| `golang.org/x/vuln`（govulncheck） | v1.8.0 | 已知漏洞扫描 | BSD-3-Clause |

2026-09-27 使用 govulncheck v1.8.0 扫描，结果为未发现已知漏洞。

CI 使用的 GitHub Actions 固定到提交：`actions/checkout` v7.0.1、`actions/setup-go` v7.0.0、`actions/upload-artifact` v7.0.1。

## 选型依据

- **Go 版本**：1.26.8 为当时 1.26 系列的最新补丁；x/sync v0.23.0 要求 Go ≥ 1.26.0，驱动要求 ≥ 1.25.0。升级工具链时整体重跑 `scripts/check.sh` 与平台验证。
- **HTTP**：使用标准库 `net/http`（1.22 起支持方法与路径通配），不引入路由框架；生成的传输类型基于标准库。
- **SQLite 驱动**：modernc.org/sqlite 为纯 Go，六个目标均可在 `CGO_ENABLED=0` 下交叉编译，并在开发机通过能力实测（见[开发与验证](development.md#平台验证状态)）。连接基线为 WAL、synchronous=FULL、busy_timeout、外键、defensive 模式、写事务 `BEGIN IMMEDIATE`；macOS 另开 `fullfsync`，因为其 `fsync` 不保证数据落到介质。只读连接只打开已存在的文件，设置 `query_only`，不改日志模式也不用 `BEGIN IMMEDIATE`，核验快照与备份时不改动被核验的库。mattn/go-sqlite3 需要 CGo，作为实测不达标时的对照，未引入。
- **SQL 查询**：采用 `database/sql` 与显式 SQL，事务边界在调用处可见。sqlc v1.31.1 已验证可在 macOS/arm64 以 `CGO_ENABLED=0` 构建，但 T00 只有三张基础设施表，不为此引入生成器；首个采用 sqlc 的模块在 `scripts/tools/go.mod` 锁定版本，并把生成接入 `scripts/generate.sh` 与 `scripts/check.sh`，同时验证其对 STRICT 表等 SQLite 语法的支持。
- **JSON Schema**：santhosh-tekuri/jsonschema v6 支持 2020-12、格式断言与自定义加载器；本项目禁用远程加载，只从内嵌文件解析引用。
- **YAML**：go.yaml.in/yaml/v3 是 YAML 组织维护的 yaml.v3 后续版本；解析后再按 JSON 数据模型逐节点转换并拒绝锚点、别名、非字符串键等。
- **OpenAPI 生成**：oapi-codegen v2.8.0 不能直接跟随指向普通 JSON Schema 文件的外部引用，因此先由 `scripts/gen/openapi` 打包成自包含文档再生成。已知差异：可空类型生成为指针并省略空值；`format: date-time` 生成为 `time.Time`，其默认编码会省略末尾零毫秒，不满足契约时间格式，服务端输出须用契约包编码。生成代码不包含请求校验，校验与授权分别由 schema 注册表和领域检查完成。
- **ULID、规范化 JSON**：规则简单且是摘要安全的关键，自行实现并用 ULID 规范示例与 RFC 8785 附录样例测试，不引入额外依赖。
