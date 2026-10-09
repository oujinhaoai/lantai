> [!note] 原报告的脱敏归档副本
> 已验证完整原 ZIP 与全部清单哈希。下面描述的是源执行及原始完整包；本目录仅为选择性文本副本，数据库/二进制/源码压缩包及私有历史引用仍在仓库外原件。脱敏与文件名变换见 archive-manifest.json；本机未重跑产品测试。

# Lantai M2-11 Linux 原生补验（2026-10-08 UTC）

结论：**partial**。已完成的 push→共同备份→空目录恢复→四客户端读取链通过 race 补验；固定基线的 MCP `resource_push` 正常对象输入被其公布的 schema 拒绝，最小复现保留为 fail。未改产品，不签署 TEST-M2-11 整卡或 GATE-M1/M2。

## 固定源码与授权边界

- 产品基线 `bad6b8a54d9e48e8d8d85bb903d9257a5b06c7d7`，tree `055c669b56034e09315dc3dabfdade04ff4333ba`。
- 原 checkout `<ORIGINAL_SOURCE_CHECKOUT>`：分支 work、同一 HEAD、启动及收尾均干净。隔离 detached worktree `<M211_SOURCE_WORKTREE>`；仅有两个本轮独立测试文件未跟踪，无 tracked diff。没有 reset、commit、push、PR、merge 或知识库写入。
- 启动、执行中及收尾的远端 main 均为指定基线。云端缓存 `origin/main` 为 `0ba278192d4851a52a0d0aa959edcadccfc2c820`，属于旧缓存，未以其代替已发布基线。本基线不含本地 BUG-20261008-01～05 修复；未复核这些修复或 M2-03/09，也未进行 M2-10 熔断测试。
- 已读根 AGENTS.md、tests/README.md、开发/架构/客户端/存储与备份恢复说明、T07 公开任务卡、扩展设计及相关代码；仓库没有 `.agents/skills/SKILL.md`，`<EMPTY_SKILL_DIRECTORY>` 为空，无子目录 AGENTS.md。
- 私有测试卡与 RUN-20261007-66（私有历史引用；见仓库外原件） 均已通过授权 GitHub connector 读取，固定知识库 ref `f47c80815fe85053ac261571ed56a814dd7efb60`。先前尝试错误 RUN 路径返回 404，随后在正确路径读取成功；未绕过权限。RUN66 正文用于范围对照，其附件与 Windows 原始日志本轮未下载或复核。
- 全部账号、口令、TOTP、资源与恢复对账为临时合成 fixture。网络仅为 loopback 测试服务及只读源码/历史证据读取；不接生产、用户电脑或 NAS，不申请凭据或权限扩大。

## 环境和构建

Linux amd64，内核 `6.18.44`，云端容器工作目录文件系统 `overlayfs`；Go `1.26.8`、GCC `14.2.0`、Python `3.12.14`、Python SDK `0.2.0`、MCP Go SDK `v1.8.0`、modernc.org/sqlite `v1.59.0`。SDK stdio 握手协议为 `2026-07-28`，服务端 `lantai 0.2.0`。完整依赖及 CGO/工具链信息见 build-info.txt、go-env.txt、go.mod/go.sum 和 environment.json；本轮没有重新执行 SQLite 压力探测。

实际客户端二进制 SHA-256：`41d534e5d03ffb446835e6b2eee2a6adf1dcc105573c074580c93021e5070451`。CLI/MCP 是独立进程；核心由测试进程内的完整 application 装配真实五库/文件/后台服务，不是独立 `lantai serve` 故障试验。REST 与 Python 在同一真实测试实例调用公共 API。fixture 使用低成本测试 Argon2id 参数和可控时钟，时钟业务时间为 2026-09-27，日志执行时间为本轮实际 UTC；不据其耗时判断生产性能。

最终 attempt5 实际运行 UTC `14:44:19` 至 `14:44:37`，包墙钟 `18.344s`。完成链 `16.08s`、最小复现 `2.19s`。`go test -race`：5 个链叶场景 pass、1 个复现 fail、0 skip；包退出 1（保留失败）。`go vet ./tests/integration` 退出 0，无 race 检测器报告。没有运行全套普通 CI。收尾已确认无 lantai 或 integration.test 进程。

## 协议依据与断言

- `docs/contracts/client.md` 与 `internal/client/workflow.go`：本机 state 在首个写请求前固定独立 CreateKey/CommitKey，绑定 origin、完整输入和文件摘要；committing 状态先读取稳定 operation，再重放权威 commit。state 不是授权凭据。
- `internal/storage/uploads.go`：CreateKey 作用域为 actor/project/command；同键同请求返回原 upload/operation，异请求为 IDEMPOTENCY_CONFLICT。重放响应反映当前 upload 状态，**未要求它与最初 open 响应逐字相同**。
- `internal/catalog/ingest.go` 与 commands.Store：CommitKey 同摘要返回原版本，异摘要拒绝；当前身份/权限先于回执读取。已提交历史回执可由同一 actor 的新、当前授权会话读取/重放，**没有将这种合法回执重放误判为旧权限复活**。
- `docs/contracts/backup-restore.md`：完整备份保持实例 ID，空恢复提升 epoch、旋转 key、撤销旧身份材料，并先保持维护门闩；新管理员设置密码/TOTP、绑定对账完成后才能启动。
- MCP resource_read 按当前实现返回 brief 元数据；resource_pull 只返回精确 ID/相对目的地。完整 manifest 在 REST/CLI/Python 逐项对照，MCP 用 brief 元数据和实际文件字节/摘要对照，未要求它返回未承诺的 full manifest。
- Python SDK 没有高层可恢复 push；本轮以官方 create_upload/commit/request 加独立标准库 HTTP 分片驱动实现其原生上传。该驱动不修改 SDK 或生成路由契约。

## 原生实际结果

| 场景 | 结果 | 原始证据 |
|---|---|---|
| CLI Unicode 工作副本上传；服务已提交后网关丢弃首次响应；同 state 对账重试 | pass，初次预期 exit3，重试 exit0，仅版本1 | attempt5/CLI-lost-commit-response.json、CLI-state-after-loss.json、CLI-push-result.json |
| REST、Python 各自创建/分片/complete/commit | pass，各生成一个版本1 | attempt5/REST-push-result.json、Python-push-result.json、gateway-requests.json |
| 三组 CreateKey 同请求复用 upload/operation；异请求冲突 | pass，201 / 409 IDEMPOTENCY_CONFLICT | attempt5/*-create-replay.json、*-create-conflict.json |
| 三组 CommitKey 经 Python 和 CLI state 跨入口重放；异请求冲突 | pass，同一回执/版本/operation；409 IDEMPOTENCY_CONFLICT | attempt5/*-python-commit-replay.json、*-CLI-state-replay.json、*-commit-conflict.json |
| 恢复前四客户端 brief 元数据、操作状态、落盘内容 | pass | attempt5/before-*.json、hash-matrix.json |
| 共同备份、空目录恢复、新管理员设置、绑定对账、新 CLI 登录 | pass，epoch1→2、key ID 变化、服务完成前拒绝启动、新会话四客户端 whoami 正常 | backup-manifest.json、restore-marker.json、restore-latch-refusal.json、epoch-key-rotation.json、fresh-login.json |
| 同一 gateway origin 的旧会话四客户端读取及 operation | pass，HTTP401 TOKEN_REVOKED、CLI exit5；机器字段除 request_id 外一致 | attempt5/*-old-*-read.json、*-old-operation.json、go-test.jsonl |
| 旧 CLI 已提交 state、旧 CreateKey/CommitKey 带旧会话重放 | pass，TOKEN_REVOKED；state 完整逻辑内容、键及 operation 不变 | attempt5/*-old-CLI-push.json、*-old-create-replay.json、*-old-commit-replay.json |
| 旧传输 capability：旧会话与新会话分别调用 | pass，旧会话401 TOKEN_REVOKED；新会话404 NOT_FOUND，未恢复旧 capability | attempt5/old-capability-*.json |
| 恢复后三来源 × 四读取客户端，合法新会话重放已提交历史回执 | pass，精确版本、文件和 operation 不变；新授权回执重放201，无新版本 | attempt5/after-*.json、*-fresh-commit-replay.json |
| 六份 CreateKey/CommitKey 持久回执及三个 operation 对照 | pass，恢复和拒绝重试后完整快照相等，每个资产仍一个版本 | attempt5/receipts-before.json、receipts-after.json |
| 源备份历史字节与完整核验 | pass，39 文件的路径/长度/SHA-256 完全相同，VerifyBackup 再次成功 | attempt5/backup-bytes-before.json、backup-bytes-after.json |
| MCP resource_push 正常 rights object | **fail**，schema 在进入上传前拒绝 | attempt3/MCP-wire-results.jsonl、attempt5/minimal-repro.json、MCP-tool-list.json |

72 行哈希矩阵 = 3 上传来源 × 4 读取客户端 × 3 文件 × 恢复前/后两阶段。所有行都核验实际文件字节相同，非仅比较申报摘要。

| 文件 | 字节 | REST/CLI/Go MCP/Python（前后所有来源一致）SHA-256 |
|---|---:|---|
| 资料/样本-é.txt | 262144 | e80acc9ce0cfc518d649e6822bcdcff8deeb269f2c0ba66e512f10813c3246d5 |
| 二进制/合成.bin | 77000 | 63f5930301e81b70004e691e1d5cbd1302b097baa98e03fdb0cb230b87b7b441 |
| 空白/空.txt | 0 | e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 |

## MCP push 失败及最小复现

`internal/mcpserver/local.go:70` 用 typed AddTool 注册 PushArgs，嵌入 `client.PushInput` 的 `json.RawMessage` 字段。实际公布的 `input.content.rights`（describe 同样）schema 是 `type: [null,array]`，元素是 0～255 integer；正常 PushInput 的 rights JSON object 因此被 SDK schema 验证拒绝。原错误明确为：object `want one of "null, array"`，`isError=true`，此类输入验证工具错误未携带 StructuredContent。

最小复现只准备一个合成 Unicode 文件、同一 owner 人员正常会话和实际 stdio 协议连接，调用 `resource_push` 时传入与公开 CLI PushInput/REST 权利形态一致的 object。服务端 `storage_uploads` 记录计数 `0→0`，失败发生在写入前；原请求、响应和 tools/list 完整保留。证据支持“跨入口正常输入形态不一致”，不支持权限绕过或恢复损坏。**未把缺少 StructuredContent 本身判为产品缺陷**，它是合法输入验证错误的输出形态。未以字节数组等替代对象绕过公布 schema。未关联或新建私有 BUG 卡，留给父会话去重既有本地修复。

因此 MCP 原生 push、MCP 同 state 重放及恢复后旧 MCP push 状态授权复验仍被该接口形态问题阻塞；MCP resource_read/resource_pull、operation_read 和旧会话拒绝已实测通过。

## 失败历史与驱动校准

1. preparation：首个版本只做 `go test -run '^$'` 编译，成功，不是功能通过。
2. attempt1：buildConsistencyCLI 子进程 Go VCS stamping 返回 status128，产品链未启动。仅将 GOFLAGS 加 `-buildvcs=false`，用显式 HEAD/tree/源码与二进制摘要记录版本；未改共享 helper。
3. attempt2：CLI 丢提交响应/重试和 Python commit 重放已成功；MCP 工具结果为空结构，驱动初始未打印完整 Content，停止。该症状随后由原始 MCP 响应定位。
4. attempt3：保留完整 MCP 原请求/响应，确认 rights object 被 null/array schema 拒绝，产品接口失败保持 fail。
5. attempt4：独立继续其余链时，驱动自行构造 REST/Python state 的 RequestHash 未先走 CLI YAML/JSON 解码；RawMessage 对象键序不同，CLI 正确拒绝 state 绑定。属于驱动问题，按仓库 yamljson.ToJSON 归一后修复，未改产品。
6. attempt5：最终链5叶场景 pass；独立最小复现 fail。此前每轮 source/log/exit 原文件保留，未覆盖失败。完整原 Go JSON 和 stderr 是权威，摘要/CSV 不增加验收计数。

## 已有 CI 覆盖映射与复用

本轮只读取得 [CI run 37642938180](https://github.com/oujinhaoai/lantai/actions/runs/37642938180)：PR6 head `635ed6f6e86d8a38c68a26d3bb9fcd48191ceb31` 的10个 jobs 均 success，其 tree 与指定合并基线 tree 完全一致。工具对 merge commit 的 PR 运行查询为空，故没有将空结果当失败；没有重跑普通 CI。ci-jobs.json 保留 job/step 原数据，这属于 job 级历史证明，不是本轮新跑的叶场景证据。

| 已有范围 | 基线测试入口 | 本轮处理 |
|---|---|---|
| 共同认证/读取/任务/分页/幂等、冲突、越权、撤权及 MCP 取消/大小限制 | client_consistency_test.go / TestClientConsistency | 引用 CI；仅在本轮链内验证必需版本/operation/会话 |
| 终审、提前清除、启停/探测、授权绑定/到期/撤回和无副作用矩阵 | client_consistency_sensitive_test.go / TestClientConsistencySensitive | 引用 CI，不重复 |
| 四客户端实际文件、资源链接、目标保护、工作区/符号链接及旧 URL 撤权 | client_consistency_files_test.go / TestClientConsistencyFiles | 引用 CI；补本轮多文件 Unicode + 恢复前后链 |
| 显式扩展安装、PATH/CWD 同名程序、argv shell 文本、env/cwd、入口摘要及 MCP 投影 | client_consistency_extensions_test.go / TestClientConsistencyExtensionBoundaries | 引用 CI，不改公共 oneshot、不重复 |
| 恢复与旧凭据/索引/HTTP文件 | backup_restore_test.go / TestCommonBackupRestoresWithNewCredentialsAndRebuiltIndex | 复用其协议流程，补同 origin + 三来源/四读取客户端与历史字节快照 |

## 可重放命令与交付内容

独立文件：`cloud_m211_linux_test.go`、`cloud_m211_mcp_schema_repro_test.go`；复制到指定基线 checkout 的 `tests/integration/`。共享 fixtures/SDK/产品源不需要编辑。Git 基线保护及新目录防覆盖见 replay.sh。

```bash
source <PRECONFIGURED_TOOLCHAIN_ENV>
bash <M211_EVIDENCE_ROOT>/replay.sh \
  <M211_SOURCE_WORKTREE> <M211_EVIDENCE_ROOT>/replay-new both
```

`both` 当前预期退出1：链通过、最小复现失败。`chain` 只重放已完成链（当前应退出0），`repro` 只重放最小失败（当前应退出1）。底层实际命令：

```bash
GOFLAGS='-mod=readonly -p=4 -buildvcs=false' \
LANTAI_M211_EVIDENCE=/absolute/new/evidence \
go test -race -count=1 -timeout=10m -json \
  -run '^TestCloudM211(LinuxPushRestore|MCPPushSchemaRepro)$' ./tests/integration
```

Go 标准测试工作目录自动为 tests/integration，不直接从仓库根启动 test 二进制。Python SDK 路径依赖该约定，遵循 RUN66 已知的 cwd 教训。

归档包含 REPORT.md、两个最终驱动、replay.sh、全部尝试的原日志/驱动/退出码、最终四客户端响应与 MCP wire/tool schema、72行 JSON/CSV、回执/operation 与39件备份摘要对照、environment/build/CI 元数据、863个 tracked 文件 SHA-256 与完整产品源码 `source-baseline.tar.gz`、SHA256SUMS.json。下载包省略实际 CLI 二进制（云端路径仍保留28MiB的 attempt5/lantai），不包含运行数据库、备份副本、会话文件、主密钥或临时认证材料。

## 未测与修复合入后回归

- 本轮 MCP 原生 push 没有成功；实际 Codex/Claude/ChatGPT 产品客户端未测，官方 Go SDK 协议客户端不能替代它们。后续正常 object schema 与对象输入反例、MCP Unicode 上传/重放/恢复旧状态均须在已记录的新候选基线回归。
- 本地五项修复合入后需更新基线保护、记录候选 HEAD/tree/driver hash，并重跑本轮 chain 与 repro；若接口修复可用，启用完整 MCP 上传来源，形成四上传来源 × 四读取客户端 × 三文件 × 两阶段 = 96行矩阵。原始基线失败继续保留。
- 共同能力、敏感接口、文件及 PATH/CWD/argv/env 等普通 CI 的候选版本适用性由候选 CI 与相应独立复核判断；本轮不重复或代替本地 BUG-01～05/M2-03/09 工作。
- 未测试真实 HTTPS/TLS网关、独立 serve 强杀、传输掉线/RPO 长时间持续采样、设备断电、NAS/Windows/macOS 原生文件行为、长稳14天或阶段门禁。多分片合成正确性不是1GiB吞吐或生产p95验收。
- 恢复对账仅证明此次合成无后续删除/外部副作用的封闭样本；不代表生产人工对账已完成。

本轮测试已停止，无产品修复或外部交付动作待执行；剩余阻塞为固定基线的 MCP push 输入形态问题与上述范围外验收。
