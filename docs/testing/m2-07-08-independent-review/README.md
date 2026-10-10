# 公开软件复核证据

本目录仅包含合成实例的软件证据和可核对摘录。受测方法与边界见[独立复核报告](../m2-07-08-independent-review.md)。

- `source-binding.json`：共同基线、两个原候选、组合代码、Go 版本、参数、固定 race 制品摘要及受测源码逐件 SHA-256。
- `targeted-race-results.jsonl`：固定制品 test2json 原始日志中完整保留的 run/pass/fail 和对应控制输出行；请求内容原日志单独归档，摘录不代替它。选择规则为 Action 属于 run/pass/fail，或 Output 为 RUN/PASS/FAIL 控制行。
- `proof-summary.json`：实际业务 ID、12 个中断/重放点、真实 REST 拒绝、C 字节摘要、保留历史表及并发重同步水位/集合摘要。
- `original-evidence-verification.json`：原候选提交源码及其既有附件的独立只读摘要核验结果。

全部原始请求/响应、子进程日志、备份清单、固定源码和制品保存在维护者执行记录，并经 Obsidian 原生二进制写入与 SHA-256 回读核验。该私有档案同时保留早期失败、错误 heartbeat 路由证明及夹具修正历史；公开摘录没有把它们改写为通过。

源码摘要可在仓库根目录对照实际文件计算。记录中的组合提交先于新增独立补测，`tested_source_sha256` 包含本轮两个新增补测文件，因此实际受测边界以逐件摘要为准。报告和本目录随后加入，均无运行行为变更。

`full-check.log` 是组合默认 race/30m 检查的完整原始控制台日志；`full-check-result.json` 固定参数、开始/完成时间、退出码、源码无漂移和 stdout/stderr 摘要。详细业务请求的另一完整检查实例独立归档。

第二次完整检查含 Docker 输入修复及最终依赖图测试。`final-check.log` 是完整原始 stdout；`final-check-result.json` 绑定其参数、时长、固定 CLI 与摘要；`final-source-files.json` 保存 1173 个检查前后相同的文件摘要。两次完整检查和定向运行各有独立实例，不混算事件/ID。`docker-context-review.json` 仅证明配方输入和实际 Go 编译；没有 Docker 引擎或 NAS 通过声明。本文及评审结果段在检查结束后补充，不是受测可执行源码变化。
