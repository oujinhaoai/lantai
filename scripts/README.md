# scripts · 开发辅助

归属：[T00](../docs/tasks/T00-foundation.md)及相应验证任务。

存放可重复执行的代码生成、格式检查、构建、合成 fixture 生成与开发辅助脚本。脚本参数与依赖需要说明，运行不应依赖作者的机器路径。

正式备份、恢复、迁移和清除操作属于核心运维命令，不由临时脚本直接修改数据库或素材目录。当前内容：[`check.sh`](check.sh)（本地与 CI 共用的全部检查）、[`generate.sh`](generate.sh)（重新生成派生文件）、`gen/`（生成器，均支持 `-check`：错误码、所有权、身份动作与策略登记、OpenAPI 打包）、`probe/sqlite`（SQLite 能力实测，输出 JSON 报告；`-stress` 另做多连接并发读写与完整性检查，用于验证实例数据位置）、[`tools/go.mod`](tools/go.mod)（锁定 oapi-codegen、staticcheck、govulncheck 版本）。这些脚本都不操作真实环境。

## macOS 原生 HTTPS smoke（不使用 Docker）

`test-native-macos.py` 使用 Python 3.11+ 标准库、macOS 自带 `lsof`/`openssl`、自行准备且校验来源的 Caddy（本机验证版本 2.11.4）及同一源码构建的核心/Web：

```sh
go build -o bin/lantai ./cmd/lantai
npm ci --prefix web && npm run build --prefix web
python3 scripts/test-native-macos.py --lantai ./bin/lantai --caddy /path/to/verified/caddy
```

可用 `--web-dist /path/to/same-source/dist` 指定已有构建。脚本不下载依赖、不接受现有数据根；每次在系统临时目录创建全新合成管理员、数据和一天有效的测试证书。只监听四个随机 IPv4 loopback 端口；全程禁用 HTTP 代理，显式信任本次 CA 并校验证书与主机名，不修改系统信任库。检查 readiness、首页和全部构建 assets 的 HTTP 200 与字节一致、运维/安装入口拒绝、实际监听及 doctor。服务在成功或异常时停止，临时目录随后清除，只输出无凭据 JSON 摘要。端口分配与进程绑定之间仍有竞争窗口，冲突应失败后重跑，不自动开放其他地址。

这是 macOS 本机部署连通性检查，不是已认证浏览器登录/上传验收、SQLite 长时压力测试、恢复接管或生产部署许可。通用断言回归由 `scripts/check.sh` 的 Python 测试运行；真实 smoke 须在 macOS 显式运行，不要求 Linux CI 安装 Caddy。
