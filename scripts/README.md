# scripts · 开发辅助

归属：[T00](../docs/tasks/T00-foundation.md)及相应验证任务。

存放可重复执行的代码生成、格式检查、构建、合成 fixture 生成与开发辅助脚本。脚本参数与依赖需要说明，运行不应依赖作者的机器路径。

正式备份、恢复、迁移和清除操作属于核心运维命令，不由临时脚本直接修改数据库或素材目录。当前内容：[`check.sh`](check.sh)（本地与 CI 共用的全部检查）、[`generate.sh`](generate.sh)（重新生成派生文件）、`gen/`（生成器，均支持 `-check`：错误码、所有权、身份动作与策略登记、OpenAPI 打包）、`probe/sqlite`（SQLite 能力实测，输出 JSON 报告）、[`tools/go.mod`](tools/go.mod)（锁定 oapi-codegen、staticcheck、govulncheck 版本）。这些脚本都不操作真实环境。
