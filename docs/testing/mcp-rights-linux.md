# NAS Linux MCP rights 候选回归（2026-10-09）

本轮在维护者登记的 NAS Ubuntu 测试容器中，验证 `4b367503ddbb1f7c393166b24b337d5e5e76be53` 加 MCP rights 未提交修复。范围为原生 Linux 构建、输入契约及四客户端备份恢复回归、完整工程检查。执行记录为维护者知识库 `RUN-20261009-04`。

## 固定输入与环境

- 执行源码沿用上一轮 [macOS 修复自测](mcp-rights-input.md)，873 文件清单 SHA256：`6297e58855b52cd815fe5be52d9f8cfb8fe3f295d1a5b862111726d3552000b2`。
- 源码归档 SHA256：`1087992d8c34f2929024a65749d81f02b49179ee4884c7a0af1074789a915d30`。
- Ubuntu 24.04.1、共享宿主内核 Linux 5.17.13、linux/amd64；CPU 配额 3 核、内存上限 4 GiB。
- 原生 Go 1.26.8、gcc 13.3.0、Python 3.12.3；固定 Go 模块经离线文件代理供应，无新增系统包。
- 主要临时实例与数据库在 btrfs 命名卷独立子目录；最后的非 root 目录权限补测使用独立 overlay 临时目录。既有实例保持不变。zfuse 挂载不承载 SQLite。源码、日志、缓存位于测试容器的本轮工作目录。
- 测试使用真实 application/五库与文件、loopback HTTP、独立 CLI/stdio MCP 子进程，输入全部为合成数据。

## 已验证的定向结果

原生 CLI 构建通过。定向 `go test -race` 运行 103.605 秒退出 0：23 叶场景通过、0 失败、0 跳过。

- 正常三字段 rights 对象完成 stdio MCP 上传，持久布尔默认值正确；15 类非法输入在上传/状态创建前拒绝，上传、两库回执和版本计数不变。
- rights 省略/null 的追加继承，以及其他可选 null 输入兼容通过；新资产缺 rights 仍经领域校验拒绝。
- REST、CLI、Python SDK、官方 Go SDK stdio MCP 四类来源实际上传；恢复前后经四客户端读取 3 个文件，共 96 行全部 bytes_equal=true。
- 四组幂等重放与异摘要冲突、MCP 自身状态重放通过；恢复提升代次，旧会话、旧 push 状态及旧传输 capability 拒绝。
- 恢复前后 8 回执/4 operation 一致；47 件原备份文件字节不变，备份校验通过。

| 证据 | SHA256 |
|---|---|
| 实际 stdio CLI | `81bac3462910e2462a4a290f356b2dfa756904d76dca57b6d43139794d1bbf7d` |
| 定向 Go JSON 日志 | `47356732540ce9c6d17d2220de2190d39f6709ffa3be2b4f03a9c200ee54476e` |
| 96 行矩阵 | `3d33b445e03aa4603f82e1a4393ae9e431fe7b3772e0a35b3f4a98ad0a969365` |
| 回执/operation 前后相同清单 | `adfa4f362e3ca3ed44857a20b0ec1d7ac24985e877c04c15352da4bd96204ca8` |
| 备份前后相同字节清单 | `66b7eed1cb9c9ae6e655ce95a37773f174439a4a535f3eb4715e234b5fe42714` |

定向 race 覆盖 Go 测试进程；独立 CLI 按现有测试 helper 原生构建，二进制 build info 单独归档。

## 完整工程检查与超时补验

标准 `scripts/check.sh` **未通过**：格式、依赖整洁、`go vet`、`staticcheck`、生成物无漂移、Python 5 项和另外 50 个 Go 测试包通过；`tests/integration` 在 30 分钟包级时限触发超时。超时时正在运行第 64/115 个顶层用例 `TestM2AgentRealRollingQuotaBoundaries/200`，该子用例已执行 9 分 44 秒。首轮未出现独立断言失败或 race 报告，超时不改记为通过。

超时栈位于 `PreviewTrash` → 当前引用扫描 → `ReadManifest` / `VersionControl` → SQLite 查询。停止后的合成实例五库 `quick_check` 均为 `ok`，有 201 个版本和 173 条配额记录；运行期间有持续 CPU/I/O 活动。这些观察不等于已确定性能根因。

在同一源码、环境和资源配置下，独立补验结果：

| 补验 | 结果 |
|---|---|
| 单独配额测试，20 分钟上限 | 20/200 两个子用例通过，耗时 637.736 秒，0 fail/skip |
| 首轮尚未到达的 51 个顶层集成用例，30 分钟上限 | 50 项通过、1 项因 root 身份按设计跳过，耗时 281.782 秒，0 fail |
| 被 root 跳过的目录权限失败用例 | 临时 UID/GID 65534、清空附加组，在独立 overlay 临时目录运行 race 测试二进制；通过、无跳过 |
| 最终原生构建 | 通过 |
| 运行前后 873 文件清单 | 无漂移 |

root 跳过记录保持原样；非 root 补测不创建系统账号、不修改既有目录权限。补验补齐了剩余执行证据，标准完整检查仍保留超时结果。未修改产品源码、测试断言、标准 `check.sh` 或其时间上限。

首轮完整检查 stdout SHA256：`94b405306d376738beaa47777abdf4d7c8503f726fe614906477d7bc66d9d8b0`；stderr SHA256：`49a96e3c5e0fc8397059ef654f539fd51f9220d452c31282a97f1d09aa03375d`。源码快照、首轮失败、补验日志、精确用例清单及监测摘要均保存于 `RUN-20261009-04` 私有证据。

## 重跑与边界

从上述源码快照运行，使用一个新的 btrfs 临时目录与空证据目录：

```sh
export GOTOOLCHAIN=local GOMAXPROCS=3 CGO_ENABLED=1
export GOFLAGS='-p=2 -buildvcs=false'
export TMPDIR=/absolute/btrfs/test-tmp
LANTAI_M211_EVIDENCE=/absolute/empty/evidence \
  go test -race -count=1 -timeout=8m -json \
  -run '^TestMCPPush(InputContract|FourClientRestore)$' ./tests/integration
scripts/check.sh
```

本轮 MCP 定向结果通过；标准完整检查的 NAS 包级时限仍是限制。该证据只覆盖这份固定候选在指定 NAS Linux 容器的合成回归。没有验证实际 Codex/Claude/ChatGPT 产品客户端、NAS 宿主断电耐久性、持续 RPO、14 天长稳或正式部署拓扑；不代替独立评审、TEST-M2-11 整卡或 GATE-M1/M2 签署。未提交、推送、合并或发布。
