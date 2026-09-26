# CLAUDE.md

Claude Code 在本仓库工作时读取本文件。仓库规则以 [AGENTS.md](AGENTS.md) 为准，下面整篇导入，只维护那一份；本文件只补充 Claude Code 特有的做法。两处说法不一致时以 AGENTS.md 为准，并提醒维护者同步本文件。

@AGENTS.md

## Claude Code 补充

- **为什么要导入**：仓库根目录同时有 CLAUDE.md 和 AGENTS.md 时，Claude Code 默认只读 CLAUDE.md，所以上面用导入把 AGENTS.md 带进来；导入不会让 AGENTS.md 被读两遍。
- **子目录规则**：默认设置下，Claude Code 读到根目录的 CLAUDE.md 后，不会自动加载子目录里的 AGENTS.md。进入某个子目录工作前，先检查该目录到仓库根之间有没有 AGENTS.md，有就先读。新建子目录 AGENTS.md 时，在同一目录放一个只写一行 `@AGENTS.md` 的 CLAUDE.md，Claude Code 读到该目录的文件时会自动加载它。
- **提交与 PR 不带署名**：遵守 AGENTS.md 的「Git 与提交说明」，不加 `Co-Authored-By` 尾注、「Generated with Claude Code」或任何 Agent、模型、会话标识；这一条覆盖 Claude Code 默认附加的署名。只在用户明确要求时提交、推送或开 PR。
- **不虚构命令与结果**：仓库目前没有可运行的构建、测试或部署命令。需要验证时如实说明哪些没验证，不编造输出；实现阶段有了真实命令，写进 AGENTS.md 或对应目录的 README，不在本文件另写一套。
- **并行协作**：可能有其他 Agent 同时在改本仓库。编辑前重新读取文件，只改本次任务相关的部分；暂存时只加本次改动的文件。
- **公开仓库**：不写入主机地址、私有绝对路径、内部笔记原文或凭据。仓库外的私有设计笔记只作参考，需要的结论改写成通用表述再进仓库。
- **沟通语言**：用中文回复和写文档；代码标识与协议字段用英文。
