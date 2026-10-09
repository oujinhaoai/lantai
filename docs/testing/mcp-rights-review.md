# MCP rights 独立复核与当前基线验收（2026-10-09）

本轮独立复核 [原修复自测](mcp-rights-input.md) 与 [NAS 历史补验](mcp-rights-linux.md)，将同一生产修复整合到 `45968f6b702d9b021147a9b7676bbff6181bb810`（包含 PR7 与 Go 1.26.9）。原工作树未改动；原失败和旧工具链结果保持原样，不能改写为新版本通过。私有完整证据索引为 `RUN-20261009-06`。

## 源码与独立复核

原 873 文件清单 SHA256 `6297e58855b52cd815fe5be52d9f8cfb8fe3f295d1a5b862111726d3552000b2` 与原工作树逐项一致；RUN04 的 415 个附件大小与 SHA256 全部匹配。原修复至新 main 仅增加文档/归档及 Go 工具链更新，MCP、客户端和领域源码未发生额外变化。

最终执行源码含 1152 文件，清单 SHA256 `beb18f22232615224e1eb92c7605880bfb9b100b0dffa689b50a85460d68a4c2`；源码归档 SHA256 `861adfd48d18aea9ed269d8b9295028b53d26410b75f82d2c0be47adf080d498`。本报告及 tests README 的索引链接在运行后产生，不在该执行清单中；提交与清单的对应关系另记于执行证据。

生产修改仅为 `resource_push` 指定从嵌入公共 `CommitRequest` 投影的输入 schema。真实 `tools/list` 的 14 个依赖定义与公共契约逐项比对，只有以下本机输入适配：

- 增加上传路由所需的 `project_id`。
- 允许省略文件摘要/大小，空摘要表示本机计算；非空摘要仍保留公共格式限制。
- rights 的两个布尔值允许省略；不插入默认值，不改变恢复状态的原请求摘要。
- 可选 raw JSON/指针字段保留 REST 原有的 null 输入语义；不改变最终领域接受规则。

其余类型、枚举、引用和未知字段限制无漂移；客户端 RawMessage、公共 schema、生成物和服务端规则均未改动。正常对象通过实际 stdio 上传；非法输入在创建恢复状态和上传之前拒绝。源码审阅与动态结果没有发现该修复的阻塞问题。

本轮仅补充新资产显式 null rights 的拒绝测试，并更正四上传来源的日志描述；没有追加生产修复。首轮补充测试误将省略/null 两个输入绑定到同一个恢复文件，正确触发客户端的不同请求拒绝；修正为独立状态文件后复验。该准备失败及两次旧 PATH/GOROOT 的启动失败均保留，不记成产品缺陷，也不覆盖原修复自测。

## 当前源码实测

| 项目 | macOS arm64 / Go 1.26.9 | NAS Linux amd64 / Go 1.26.9 |
|---|---|---|
| 定向 race | 24 叶通过、0 失败/跳过，49.875 秒 | 24 叶通过、0 失败/跳过，61.5 秒 |
| 合法对象/非法输入 | 上传 0→1；15 类非法输入无上传/两库回执/版本副作用 | 同左 |
| 可选 rights | 追加省略/null 继承；新资产省略/null 均由领域拒绝 | 同左 |
| 四上传来源与恢复 | 96 行字节/hash 一致，持久 rights 一致 | 同左 |
| 历史与旧授权 | 8 回执/4 operation/47 件备份不变；旧会话、push 状态与传输权拒绝 | 同左 |
| 完整检查 | 同源重跑默认 30m 未通过（两项治理探测失败）：1498.143 秒，集成包 1232.857 秒 | 显式 60m 通过：2641.309 秒，集成包 2174.880 秒 |
| 非 root 权限反例 | 默认非 root 运行；全量整体未通过 | root 全量中的跳过项以 uid 65534 单独补验，退出 0，4.778 秒 |

依赖沿用 `go.mod` 中的官方 Go MCP SDK `v1.8.0`、`google/jsonschema-go v0.4.3` 与 `santhosh-tekuri/jsonschema/v6 v6.0.3`。以上定向 race 覆盖 Go 测试进程；独立 CLI/stdio MCP 按现有 helper 原生构建，实际二进制 build info 与摘要分别归档。官方 Go SDK stdio 会话是协议客户端，不能作为实际产品 MCP 客户端的验收证据。

本机首次默认完整检查未通过：完整命令 1818.292 秒，集成包 1486.901 秒，唯一失败为既有 `TestM2GovernanceRealScopesProbeAndRestart` 在启用探测阶段返回 `EXTENSION_ACTIVATION_STALE`，早于该用例的 MCP 调用。该文件与 main 一致；正向夹具探测上限为 1 秒，但现有失败日志不足以判定具体原因。原样同源隔离三次均通过（18.87/17.58/18.43 秒），之后同源默认完整检查重跑再次失败，新增 `TestM2GovernanceRealJobFaultResults` 同类启用探测失败；不能用隔离通过覆盖首次失败，也不将资源争用推测写成根因。

## NAS 历史超时分析与有界完整验收

RUN04 标准 `scripts/check.sh` 的结果仍是**未通过**：整个命令耗时 2236.548 秒；50 个其他测试包及静态/Python 检查通过，集成包在 1800.308 秒触及默认 30 分钟上限。到期时父用例已运行约 601 秒，`/200` 子用例约 584 秒。

同源独立配额用例在 637.736 秒通过，其中 `/200` 为 619.39 秒；随后 51 个顶层用例在 281.782 秒完成（50 通过、1 root 跳过，后经非 root 补验通过）。原快照、首轮失败、后续日志和完整清单均已复核，分段结果不等于默认完整命令通过。

超时栈位于 `PreviewTrash` → `IncomingUses` → `ReadManifest` / `VersionControl` → SQLite。测试创建 201 个版本，反复预览和接受回收；来源核验扫描权威版本及 manifest，存在随版本量与回收次数增长的重复工作。它用注入时钟验证滚动一小时边界，没有实际等待一小时。运行期间有 CPU/I/O 推进，停止后五库 quick_check 为 ok，单独原用例能结束。这些证据支持累计预算不足，未证明死锁；也不足以把具体开销归咎于文件系统、CPU、SQLite 或某一个函数。

扣除已运行的配额部分，再加完整配额与后续用例，历史集成包约需 35 分钟。基于这个观测，`check.sh` 新增显式 `LANTAI_TEST_TIMEOUT`：默认 race 30m、非 race 20m、CI 配置均不变。NAS 本轮设置 60m，保留余量；外层完整命令另有 75 分钟截止。完整执行格式、依赖、vet、staticcheck、生成物、Python、全部 Go race 和构建，不筛选或跳过集成用例，不改变单用例断言或产品性能阈值。

```sh
# 使用实际 Go 1.26.9，先核对 go version 与 go env GOROOT。
# TMPDIR 需在新的本机 btrfs 测试目录，不能使用 zfuse 数据挂载。
export GOTOOLCHAIN=local GOMAXPROCS=3 CGO_ENABLED=1
export GOFLAGS='-p=2 -buildvcs=false'
export TMPDIR=/absolute/btrfs/new-test-tmp
LANTAI_M211_EVIDENCE=/absolute/new/evidence \
  go test -race -count=1 -timeout=8m -json \
  -run '^TestMCPPush(InputContract|FourClientRestore)$' ./tests/integration
LANTAI_TEST_TIMEOUT=60m scripts/check.sh
```

只使用本轮独立合成目录。原工具链、既有一万资产实例与 Compose 保持不变；btrfs 命名卷承载 SQLite，zfuse 不承载运行库。Go 1.26.9 Linux 工具链来自官方发布，下载包 SHA256 `42d158b4d8f7b61ac0a830567c940a86098fb7aac52e467a5ebec03ef5cc2f8d`，与官方清单一致。

## 结论与边界

本次 MCP 对象输入修复通过独立源码复核、两端定向验收和 NAS 显式 60m 完整检查；原始失败均保留。macOS 默认完整检查两次未通过，失败均在未修改的扩展治理启用探测路径；三次同源单项隔离通过不能替代全量结果。本轮日志没有保留这两次失败的底层宿主观察，尚不能直接定性为一秒预算超时，相关验收问题另行跟进。NAS 结果只证明显式预算下的完整执行，不改变旧默认 30m 超时结论。TEST-M2-11 整卡、目标产品 MCP 客户端、其他平台覆盖、GATE-M1/M2、设备 Sync/掉电、持续 RPO 与 14 天长稳仍分别验收。未推送、开 PR、合并或部署。
