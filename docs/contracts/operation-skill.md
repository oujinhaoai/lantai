# Lantai 操作 skill 的分发与验证

正式操作说明放在 [`sdk/skills/lantai-operations`](../../sdk/skills/lantai-operations/SKILL.md)，
供 Lantai 使用方加载；仓库根 AGENTS.md 继续只管开发协作。skill 不携带账号、
主机、会话、生产目录、脚本或隐含权限，完整入口以连接实例的目录及实际注册为准。

在支持 SKILL.md 的 Codex 环境，可由用户将整个 `lantai-operations` 文件夹复制到
其 `$CODEX_HOME/skills`，未设置 CODEX_HOME 时使用 `~/.codex/skills`。保留 references
相对结构，避免与同名本地 skill 覆盖合并。重新打开支持技能发现的会话后，检查
目录中名称和描述是否出现，再用 `$lantai-operations` 执行当前已授权的业务任务。
本仓库不自动安装到全局、不修改个人配置；其他客户端按其实际支持的技能格式安装。

普通 CLI 只需正式构建的 `lantai`、选定 origin 和既有私有会话。MCP 是 stdio
适配同一 REST 的进程，不是自建 HTTP server。Codex 客户端若支持下列正式命令，
可由用户按自己的配置范围登记（先看本机 `codex mcp add --help`）：

```sh
codex mcp add lantai -- /PATH/TO/lantai mcp --server ORIGIN --session-file PRIVATE_SESSION --workspace SELECTED_DIR
```

私有 CA 可追加 `--ca-file PEM`。所有路径是用户显式选择的占位符；不把密码、TOTP、
Bearer 明文写入命令或配置示例，不把备份/设备真人因素交给 Agent。配置登记语法
通过不表示产品客户端已经成功协商、工具运行或验证权限。

验收应在临时合成实例中，分开记录 CLI、协议 SDK 与真实 Codex MCP 客户端：
先核对 whoami 的普通 Agent 身份、meta 和完整能力目录，再按已安装工具的 input
schema 走一次入藏、固定版本读取、字节下载验证和原状态重放。大结果必须跟到
complete；对失联、撤权、过期游标、未知副作用和真人因素停交分别保留证据。
再由独立评审者按真实任务使用 skill，仅通过正式帮助与客户端完成相同目标。
不能把 MCP SDK 的内存/stdio 测试写成真实 Codex 客户端实测，也不能将一次合成
闭环升级成生产、真实原生软件续做或整体门禁通过。
