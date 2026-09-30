# 依赖、版本与许可证

核对日期：2026-09-29（MCP 与 OpenAPI runtime 见下文；2026-09-27 T01/T08.1 新增 x/crypto、x/term，x/sys 升至 v0.48.0 并改为直接依赖；T02 没有新增模块，只多用了 x/text 的 `cases` 包）。版本由 `go.mod`/`go.sum` 与 `scripts/tools/go.mod`/`go.sum` 锁定；许可证依据各模块发布包中的许可证文件逐一核对。本项目以 GPL-3.0 发布，下列许可证均与之兼容。新增依赖须有实际用途，并在本篇记录版本、用途、兼容范围与许可证。

## 链接进程序的依赖

`go list -deps ./...` 使用的第三方模块（含生成类型包；实际入口按导入裁剪）：

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
| `golang.org/x/sync` | v0.23.0 | 可取消、公平的加权信号量（锁协调、口令计算并发上限） | BSD-3-Clause |
| `golang.org/x/text` | v0.42.0 | schema 校验错误信息的本地化打印；口令、路径与说明文本的 Unicode NFC 规范化；路径冲突判定的完全大小写折叠（`cases.Fold`） | BSD-3-Clause |
| `golang.org/x/crypto` | v0.57.0 | 只用 `argon2`：口令的 Argon2id（RFC 9106）校验值 | BSD-3-Clause |
| `golang.org/x/term` | v0.46.0 | 本机实例命令在终端读取口令时不回显 | BSD-3-Clause |
| `golang.org/x/sys` | v0.48.0 | 数据根单实例锁（Unix `flock`、Windows `LockFileEx`）、磁盘余量与文件系统类别探测；驱动依赖 | BSD-3-Clause |

新增 T07 依赖（2026-09-29 核对本地下载的指定版本发布包 LICENSE/go.mod；同时核对官方 [MCP v1.8.0 发布](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0)及 [runtime v1.7.0 发布](https://github.com/oapi-codegen/runtime/releases/tag/v1.7.0)）：

| 模块 | 版本 | 用途与兼容范围 | 许可证 |
|---|---|---|---|
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | 官方 MCP stdio transport/协议协商；Go ≥ 1.25，实际使用固定版本 | Apache-2.0 与未重新授权贡献的 MIT，保留完整过渡声明；文档 CC-BY-4.0 未复制 |
| `github.com/oapi-codegen/runtime` | v1.7.0 | OpenAPI 生成 anyOf 类型的 JSON 合并辅助；Go ≥ 1.24 | Apache-2.0 |
| `github.com/apapsch/go-jsonmerge/v2` | v2.0.0 | runtime 的 JSON 合并依赖 | MIT |
| `github.com/google/jsonschema-go` | v0.4.3 | SDK 输入 schema 推断/校验 | MIT |
| `github.com/segmentio/encoding` | v0.5.4 | SDK JSON 编码依赖 | MIT |
| `github.com/segmentio/asm` | v1.1.3 | encoding 的处理器辅助 | MIT |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 | SDK URI template 处理 | BSD-3-Clause |
| `golang.org/x/oauth2` | v0.35.0 | SDK 间接依赖；本项目 MCP 只消费既有 Lantai 会话 | BSD-3-Clause |
| `golang.org/x/time` | v0.15.0 | SDK 间接限流工具 | BSD-3-Clause |

MCP 不引入任何完整 Agent 平台。SDK 的其他 transport/认证能力不会因依赖存在自动启用。Python 客户端要求 ≥ 3.11、无运行时依赖；构建依赖 setuptools ≥ 68（MIT），项目测试直接从工作树导入。打包结果须保留仓库 GPL-3.0 许可证。

发布二进制时须随附上述依赖的许可证与版权声明（BSD/MIT/Apache 均要求保留声明）。发布流程归 T08，届时从 `go.sum` 生成第三方声明，不在仓库预填 `NOTICE`。

## 开发工具（不链接进程序）

| 工具 | 版本 | 用途 | 许可证 |
|---|---|---|---|
| `github.com/oapi-codegen/oapi-codegen/v2` | v2.8.0 | 从打包后的 OpenAPI 3.1 文档生成 Go 传输类型 | Apache-2.0 |
| `honnef.co/go/tools`（staticcheck） | v0.8.1（2026.2.1） | 静态检查 | MIT |
| `golang.org/x/vuln`（govulncheck） | v1.8.0 | 已知漏洞扫描 | BSD-3-Clause |

2026-09-27 使用 govulncheck v1.8.0 扫描：代码不调用任何已知漏洞（退出码 0）。引入 x/crypto 后模块级另报 GO-2026-5932（`golang.org/x/crypto/openpgp` 已不维护、没有修复版本）；本项目只导入 `x/crypto/argon2`，不导入 `openpgp`。

CI 使用的 GitHub Actions 固定到提交：`actions/checkout` v7.0.1、`actions/setup-go` v7.0.0、`actions/upload-artifact` v7.0.1。

## 可选部署组件

2026-09-28 核对：网关模板固定 Caddy 2.11.4（Apache-2.0），作为独立进程转发 API/传输，不链接进核心；[官方发布](https://github.com/caddyserver/caddy/releases/tag/v2.11.4)与[许可证](https://github.com/caddyserver/caddy/blob/v2.11.4/LICENSE)。隔离 HTTPS 验证使用官方 macOS arm64 二进制并核对发布 SHA-512 清单。兼容范围为模板已验证的该版本；更换版本须重跑网关测试。官方容器镜像和 systemd 模板的实际目标环境验收见[部署说明](deployment.md)。仓库不附带或自动安装第三方二进制。

## 选型依据

- **Go 版本**：1.26.8 为当时 1.26 系列的最新补丁；x/sync v0.23.0 要求 Go ≥ 1.26.0，驱动要求 ≥ 1.25.0。升级工具链时整体重跑 `scripts/check.sh` 与平台验证。
- **HTTP**：使用标准库 `net/http`（1.22 起支持方法与路径通配），不引入路由框架；生成的传输类型基于标准库。
- **SQLite 驱动**：modernc.org/sqlite 为纯 Go，六个目标均可在 `CGO_ENABLED=0` 下交叉编译，并在开发机通过能力实测（见[开发与验证](development.md#平台验证状态)）。连接基线为 WAL、synchronous=FULL、busy_timeout、外键、defensive 模式、写事务 `BEGIN IMMEDIATE`；macOS 另开 `fullfsync`，因为其 `fsync` 不保证数据落到介质。SQLite 的 busy handler 是定时轮询而非排队，多写者竞争时个别写者可能等满 busy_timeout，因此可写库的写事务和事务外写语句先在进程内按到达顺序排队（默认最长 15 秒，超时按可重试忙碌处理），busy_timeout 只兜底其他进程；只读事务与 `VACUUM INTO` 不排队。只读连接只打开已存在的文件，设置 `query_only`，不改日志模式也不用 `BEGIN IMMEDIATE`，核验快照与备份时不改动被核验的库。mattn/go-sqlite3 需要 CGo，作为实测不达标时的对照，未引入。
- **SQL 查询**：采用 `database/sql` 与显式 SQL，事务边界在调用处可见。sqlc v1.31.1 已验证可在 macOS/arm64 以 `CGO_ENABLED=0` 构建，但 T00 只有三张基础设施表，不为此引入生成器；首个采用 sqlc 的模块在 `scripts/tools/go.mod` 锁定版本，并把生成接入 `scripts/generate.sh` 与 `scripts/check.sh`，同时验证其对 STRICT 表等 SQLite 语法的支持。
- **JSON Schema**：santhosh-tekuri/jsonschema v6 支持 2020-12、格式断言与自定义加载器；本项目禁用远程加载，只从内嵌文件解析引用。
- **YAML**：go.yaml.in/yaml/v3 是 YAML 组织维护的 yaml.v3 后续版本；解析后再按 JSON 数据模型逐节点转换并拒绝锚点、别名、非字符串键等。
- **OpenAPI 生成**：oapi-codegen v2.8.0 不能直接跟随指向普通 JSON Schema 文件的外部引用，因此先由 `scripts/gen/openapi` 打包成自包含文档再生成。已知差异：可空类型生成为指针并省略空值；`format: date-time` 生成为 `time.Time`，其默认编码会省略末尾零毫秒，不满足契约时间格式，服务端输出须用契约包编码。生成代码不包含请求校验，校验与授权分别由 schema 注册表和领域检查完成。
- **ULID、规范化 JSON**：规则简单且是摘要安全的关键，自行实现并用 ULID 规范示例与 RFC 8785 附录样例测试，不引入额外依赖。
- **口令与密钥（T01，2026-09-27 核对）**：口令用 x/crypto 的 Argon2id（Go 维护的官方扩展库），不自制 KDF；默认 64 MiB、3 轮、单线程（RFC 9106 第二推荐方案的单线程形式），参数随每条记录保存，同时计算数默认 2，开发机（Apple 芯片）约 0.1 秒一次，目标 NAS 须实测后再调整。TOTP 种子加密用标准库经过验证的 AES-256-GCM，子密钥用标准库 `crypto/hkdf`；TOTP 本身按 RFC 6238 用标准库 HMAC-SHA1 实现，并以 RFC 6238 附录 B 的测试向量验证，不引入第三方 OTP 库。二维码生成暂未引入，登记时展示 Base32 种子与 otpauth 地址。
- **文件锁与平台能力**：标准库没有公开的文件锁与磁盘余量接口，改用 x/sys；只实现 Linux、macOS、Windows（锁另支持 BSD），其他平台明确返回不支持。
