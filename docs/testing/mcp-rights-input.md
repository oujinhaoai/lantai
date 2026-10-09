# MCP rights 对象输入修复回归（2026-10-09）

本记录只证明以下固定源码上的本机合成回归；实现自测通过，独立评审另行完成。TEST-M2-11 整卡和 GATE-M1/M2 不据此放行。

## 源码、输入与实现

- 基线提交 `4b367503ddbb1f7c393166b24b337d5e5e76be53`，tree `91da7da357bdc6a9235785cd2d56bb4e4c00796d`，加本次未提交修复。该基线继承 PR7 已交付候选；此回归不代替其领域独立复核。
- 执行源码清单 SHA256：`6297e58855b52cd815fe5be52d9f8cfb8fe3f295d1a5b862111726d3552000b2`。清单包含当时全部 tracked 文件和本轮三个新增源码/测试文件；本报告是测试后生成的说明。原始清单、差异和新增文件副本保存在维护者的本轮执行证据。
- `internal/mcpserver/local.go`：`58bb100f7efd602632d61a9d40af5010b79d8b417a9744250121205b36bf5f11`；`push_schema.go`：`062f12c27867c09b1d277a025b0c603ccc3faf7d9e1248194cd3a93e7b311d08`。
- 两个测试文件 `mcp_push_input_test.go` / `mcp_push_restore_test.go` SHA256：`63450a31afb924c76b7d531462c00f15d659f18787837d4e15a05fc990458202` / `e9e6247ecd72e669442944e3ca75d44686ec25b67a8072f4eea62f87d2ebaad2`。
- 原 Linux 完整 ZIP SHA256：`a334abc6008559835aa0134c32441e5cdcb573e51c68aba7a9b476856a84373c`；本轮只读消费的归档复现驱动/恢复驱动 SHA256：`07475a5724c51b18fa91b0a9d4b511d4b1a28c0f7ba95bc7d2a1f2dda946c342` / `44fd94b1b058f58042f13a225599478b7df271d57af35594b1715a704b0b6680`，已与原归档清单核对。

根因是 typed MCP AddTool 对 `json.RawMessage` 使用 Go 切片推导，公布 null/array（0–255 整数）而非线上的 JSON 对象。修复从已有嵌入 OpenAPI CommitRequest 构造本机 push 输入 schema，复用字段、引用、枚举与未知属性限制；只适配本机计算的摘要/大小、可省略布尔值及服务端原有 null 输入语义。不取消校验，不新增第二份共享协议，不改客户端 RawMessage、服务端领域规则或生成物。

## 实际结果

环境：macOS / Darwin 27.0.0 arm64，Go 1.26.8，Python 3.14.7，MCP Go SDK v1.8.0。真实五库、文件和 application 服务；独立 CLI/stdio MCP 进程，经 loopback HTTP API。全部素材、身份与恢复输入为临时合成 fixture。

| 范围 | 结果 |
|---|---|
| pristine 候选真实 tools/list + tools/call | 原失败重现，正常 rights 对象被 null/array 拒绝，upload 0→0 |
| 修复后真实 tools/list + stdio resource_push | 正常三字段 rights 对象、metadata/describe、uses 可用；持久 rights 正确，upload 0→1 |
| 15 项非法输入 | rights array/string/number/bool、空/缺项/枚举/布尔类型/未知字段及 metadata/producer/uses/describe/task 错误形态均拒绝；上传、两库回执、版本计数不变，无恢复状态文件 |
| 可选输入兼容 | rights 省略/null 的追加版本继承正确；task/metadata/producer/uses 及著录字段的 null 保持支持 |
| 服务端最终领域验证 | 新资产未声明 rights 仍 SCHEMA_INVALID，不生成版本/提交回执；保留原工作流可恢复上传语义 |
| 四上传来源 | REST、独立 CLI、Python SDK、官方 Go SDK stdio MCP 均实际创建和提交一个版本 |
| 幂等与冲突 | 四组 CreateKey/CommitKey 同键重放/异摘要冲突；MCP 原状态自身重放；没有第二版本 |
| 共同备份/空目录恢复 | 新代次、新身份设置/对账和启动门闩验证；四读取客户端精确元数据/文件对照 |
| 96 行矩阵 | 4 上传来源 × 4 读取客户端 × 3 文件 × 前后两阶段，全部 bytes_equal=true，rights 持久值一致 |
| 旧权利拒绝 | 旧会话/旧 CLI 和 MCP push 状态拒绝，键和 operation 不变；旧传输 capability 在新会话下 NOT_FOUND |
| 恢复历史 | 8 回执/4 operation 不变，新授权可重放原回执；47 件源备份字节不变且 VerifyBackup 成功 |
| 最终定向 race | 23 叶场景 pass、0 fail、0 skip，无 race 报告 |
| 完整工程检查 | `scripts/check.sh` 退出 0：格式、依赖、vet、staticcheck、生成物无漂移、Python 5 项、Go 全套 race 和构建均通过 |

最终96行原始 JSON SHA256：`0c49dec2b1f15f16adc33981ffffa70a0e7395fdf114c94e97575e1ffb62fb24`；Go JSON日志 SHA256：`64ae97f5ed5cec8692b104cba99d5745a7d17ee5e0e81524e8f388fac09b8861`。回执 before/after 同为 `eab2525079b0596302840ce6b7b03d3bbb00cdbb8ddc7b07aadcfbfc3c052f3d`；备份字节清单 before/after 同为 `cb1159a5bb78a785e574af35e6b0c4014634d932bbfb811c09a6a6ba4c6c75a9`。

## 重跑

```sh
# 从仓库根目录运行；证据目录应位于仓库外且为空。
LANTAI_M211_EVIDENCE=/absolute/new/evidence \
  go test -race -count=1 -timeout=8m -json \
  -run '^TestMCPPush(InputContract|FourClientRestore)$' ./tests/integration
scripts/check.sh
```

本次为避免与独立复核争用资源，使用 `GOMAXPROCS=4`、`GOFLAGS='-p=2 -buildvcs=false'`。禁用 VCS stamping 只影响临时子进程构建的版本注入，源码按上述提交/tree/清单及实际二进制摘要单独绑定。没有修改旧 Linux replay.sh 的固定基线保护或覆盖旧证据。

原始日志、请求/响应、tools/list、矩阵、SHA、驱动适配及全部尝试保存在本轮执行证据，公开仓库只放合成回归代码及本说明。最初 Linux 文件名导致本机 helper 未编入、后续追加夹具误带创建 describe 的失败均保留；它们是测试准备/夹具问题，不能消除 pristine 候选的产品失败。

## 未覆盖范围

本轮未执行新候选 Linux 原生回归（本机 Colima 未运行，无 Docker/Podman 命令）；旧 Linux 证据仍绑定原 bad6b8a。未操作 NAS、生产或真实素材。官方 Go SDK stdio 协议客户端不代表 Codex/Claude/ChatGPT 产品客户端；Windows/macOS 部署行为、持续 RPO、设备耐久性、14天长稳、独立评审及阶段门禁均按各自任务继续。没有提交、推送、合并或发布。

实际 CLI 二进制 SHA256：`fb4c9b9efe5c3399ba9c18cb7342134a32dbc7f1e5751542531064135546965e`。

## 收尾源码适用性

收尾时主工作区已由另一会话推进到 `75de40433c156e773008d09d333bbd12c248efb3`（278个归档/说明文件，均在docs/）。该提交的产品基线仍为bad6b8a；本轮测试绑定4b367503候选加本次修复，未切换或重放到新的main，未修改另一会话分支。主工作区当前干净，本轮没有提交、推送或合并。
