> [!note] 原报告的脱敏归档副本
> 已验证完整原 ZIP 与全部清单哈希。下面描述的是源执行及原始完整包；本目录仅为选择性文本副本，数据库/二进制/源码压缩包及私有历史引用仍在仓库外原件。脱敏与文件名变换见 archive-manifest.json；本机未重跑产品测试。

# Lantai M2-10 Linux 熔断 owner 持久化、强杀与冷启动补验

本轮指定范围 **pass**，没有发现产品缺陷或测试阻塞。固定已发布提交 `bad6b8a54d9e48e8d8d85bb903d9257a5b06c7d7`，9 个实际独立 owner PID，4 个写进程在 durable marker 及独立已提交读核验后被真实 `SIGKILL`。其余 5 个进程验证完成后退出 0；9 个 PID 均已回收。本轮测试已停止。

这是 runtime SQLite 中熔断 owner 的 Linux 软件补验，结论仅适用于本报告列出的断言。**不签署整个 TEST-M2-10、GATE-M1 或 GATE-M2，不替代 RPO、真实掉电、15 分钟观察或长稳。** 上层调用授权与宿主结果观察为合成输入；未运行 activation、registry、Job、Review 或发布治理业务链。

## 基线与依据

- 原 checkout `<ORIGINAL_SOURCE_CHECKOUT>`：分支 `work`，开始和结束的 tracked/untracked 工作树均干净，HEAD 为指定基线。通过现有 GitHub 连接只读核验远端 `main`，其 HEAD 也为 `bad6b8a…`，差异 0。没有 fetch、reset、覆盖原改动、提交、推送、PR 或合并。
- 独立 detached worktree `<M210_SOURCE_WORKTREE>`：同一基线。仅新增 `internal/extensions/linux_m2_10_owner_test.go`；未改产品或共享 fixture。数据、驱动副本和报告均放仓库之外。本轮没有接入用户电脑、NAS、生产服务或新凭据。
- 树摘要 `055c669b56034e09315dc3dabfdade04ff4333ba`。按委派说明，该基线不含本地 BUG-20261008-01～05 五项修复；本轮未获取、评审或补改这些本地变更，也未运行 M2-03/09 或另一线程的 M2-11 工作。
- 已读 `AGENTS.md`、README、开发与验证、架构、T09 任务和提交故障矩阵说明。仓库及 `<EMPTY_SKILL_DIRECTORY>` 中无适用 `.agents/skills/SKILL.md`；未绕过空目录权限。
- 权威断言来自固定基线 [docs/extensions.md §11.2](https://github.com/oujinhaoai/lantai/blob/bad6b8a54d9e48e8d8d85bb903d9257a5b06c7d7/docs/extensions.md#112-m2-熔断默认值)。默认 5 次故障、600 秒窗、900 秒冷却、单个半开名额；未知停止保留占位，已确认停止的取消释放但不视为恢复，协议合法完成才 close，旧 epoch 仅记审计。
- 私有知识库可由现有授权读取；已核对 TEST-M2-10（私有历史引用；见仓库外原件） 与 RUN-20261007-71（私有历史引用；见仓库外原件），并读其 Windows 原始驱动、控制脚本、日志和摘要。均固定到 `f47c808…`，快照及 blob SHA 位于 `reference/manifest.json`。本轮未写知识库，也未改变旧运行记录。

## 环境与测试边界

Debian 13.6，Linux 6.18.44 x86_64，Go 1.26.8 linux/amd64，Python 3.12.14，modernc.org/sqlite v1.59.0 / SQLite 3.53.4。控制器只读核验使用 Python SQLite 3.53.1。实际文件系统为本地容器 `overlayfs`；挂载参数、工具链、源文件 SHA-256、go.mod/go.sum 均已留存于 `run-001/environment.json` 和 `run-001/source/`。不把该容器外推成目标 NAS 数据卷。

数据库通过产品 `sqlite.Open` 打开，逐次核对 WAL / synchronous=2（FULL）。只执行发布的 `runtime/0013.extensions.runtime.sql`，建立专用空库中的 extensions 表，未建立完整实例，也没有 activation 记录或其他领域运行数据。测试调用实际 `admit`、`breakerView`、`Manager.Settle` 和产品执行结果分类器；没有复制另一套熔断算法。

用 `clock.Fake`，逻辑原点 `2026-10-08T00:00:00Z`。每个新进程重新建立 Fake，`cold_start` 是设置阶段 offset 前的初始诊断；有效边界断言在下表列出的 offset 执行。真实运行时间 **14:36:32.761901–14:36:36.804995 UTC，约 4.04 秒（含构建）**，没有真实等候 899/900 秒或 15 分钟。

`Dispatched`、取消/停止确认和 `ResultValid` 等宿主观察是合成数据，限定于 breaker owner 端口。强杀的真实对象是持有 runtime SQLite 的 owner 进程；本轮没有证明真实检查器进程树已结束或真实 result.json schema 校验通过。调用的上层授权假定已通过，不能从本轮推导启用、代次、白名单、HumanGrant 或业务证据接受权。

## 独立 PID、durable marker 与终止时间线

所有时间均为 2026-10-08 UTC。控制器 PID 为 1028。marker 内记录真实 PID、逻辑时间、SQLite 状态、invocation 与 fault 行。写进程先完成产品事务，再写临时 marker、fsync 文件、改名、fsync 目录，最后输出 `LT10_READY`。控制器看到该 sentinel 后，以独立只读连接核对数据库与 marker 完全相同，随后才发送 SIGKILL。杀进程后等待 reap，保存 DB/WAL/SHM，再只读核验，下一阶段通过 exec 创建新 PID，重新打开真实数据库。

| 阶段 | PID | marker 观察时间 | SIGKILL 请求时间 | 退出结果 | 最终诊断 / epoch / 占位 |
|---|---:|---|---|---|---|
| seed | 1493 | 14:36:36.473338 | 14:36:36.477774 | -9 / SIGKILL | open / 2 / 0 |
| hold | 1500 | 14:36:36.523465 | 14:36:36.525718 | -9 / SIGKILL | half_open / 2 / 1 |
| reconcile | 1507 | 14:36:36.571050 | 14:36:36.573315 | -9 / SIGKILL | half_open / 2 / 0 |
| recover | 1515 | 14:36:36.609576 | — | 0 | closed / 3 / 0 |
| closed | 1522 | 14:36:36.654115 | — | 0 | closed / 3 / 0；新窗 4 |
| window_cold | 1529 | 14:36:36.678506 | — | 0 | closed / 3 / 0；新窗 4 |
| fault_seed（独立库） | 1536 | 14:36:36.722945 | 14:36:36.725259 | -9 / SIGKILL | open / 3 / 0 |
| fault_recover | 1543 | 14:36:36.760919 | — | 0 | closed / 4 / 0 |
| fault_closed | 1551 | 14:36:36.785570 | — | 0 | closed / 4 / 0 |

完整纳秒级 wall/monotonic 时间线在 `run-001/timeline.jsonl`；子命令、cwd、退出状态在 `commands.json` 和 `summary.json`。4 个预定 -9 是测试强杀结果，不是断言失败；被杀进程的 Go test 不会输出 PASS 或运行 Close/defer。5 个正常子进程有原始 PASS 日志。

## 冷启动、半开与 epoch 状态矩阵

`half_open` 是到期后诊断投影，持久状态仍为 `open`。有效窗口按当前 epoch 与时间计数，关闭后推进 epoch；历史 fault 行保留用于审计，不要求删除历史记录。每项 actual 与 expected 均有原始快照。

| 阶段 / 逻辑 offset | 观测及断言 | 诊断 / 持久行 | epoch / 占位 / 当前窗 | 结果 |
|---|---|---|---|---|
| seed / 0s | 第 4 次仍 closed，5 次事务提交后 open；同 fault 重复回调不新增 | closed → open / 同左 | 1 → 2 / 0 / 4 → 0 | pass |
| hold 冷启动 / 899s | 新 PID 保留 opened_at；派发拒绝，invocation 和 fault 行数不变 | open / open | 2 / 0 / 0 | pass |
| hold / 899s | epoch1 普通调用迟到 completed/runtime_fault，仅持久记录 stale_epoch，breaker 与故障表不变 | open / open | 2 / 0 / 0 | pass |
| hold / 900s | 到期只可试探；没有自动创建调用，显式调用只取一个槽 | half_open / open | 2 / 0 → 1 / 0 | pass |
| hold / 900s | dispatched+cancelled+stop未知分类 unresolved，保留槽，第二次派发拒绝 | half_open / open | 2 / 1 / 0 | pass |
| reconcile 冷启动 / 1200s | probe deadline 为 960s，已超 240s；原 invocation 状态和 ID 保留，3 次补发均拒绝 | half_open / open | 2 / 1 / 0 | pass |
| reconcile / 1200s | 合成停止确认分类 cancelled，释放槽，不 close；已取消调用的重复成功/故障不改变终态或审计 | half_open / open | 2 / 0 / 0 | pass |
| recover 冷启动 / 1201s | 上一释放事务强杀后仍 open、空槽；下一显式调用领取槽，completed 才 close | half_open → closed / open → closed | 2 → 3 / 1 → 0 / 0 | pass |
| recover / 1201s | 合成合法业务 fail 分类 completed，属于运行恢复 | closed / closed | 3 / 0 / 0 | pass |
| closed 冷启动 / 1202s | closed/epoch3 持久；另外两条未终态 epoch1 迟到成功/故障仅记 stale 审计 | closed / closed | 3 / 0 / 0 | pass |
| closed / 1202s | 新窗 4 次故障仍 closed；closed 普通成功不清窗，旧历史不污染新窗 | closed / closed | 3 / 0 / 4 | pass |
| window_cold 冷启动 / 1203s | 新窗 4 次故障跨进程保留 | closed / closed | 3 / 0 / 4 | pass |
| fault_seed 独立库 / 1200s | unresolved 半开经 runtime_fault 对账，重新 open 并重新记录 opened_at | open / open | 2 → 3 / 0 / 0 | pass |
| fault_recover 冷启动 / 2099s | 距新 opened_at 899s，拒绝；已终态 epoch2 half-open 迟到成功/故障均幂等忽略 | open / open | 3 / 0 / 0 | pass |
| fault_recover / 2100s | 距新 opened_at 900s，可领取；显式 completed 关闭 | half_open → closed / open → closed | 3 → 4 / 0 / 0 | pass |
| fault_closed 冷启动 / 2101s | epoch4 closed 保留，旧已终态 half-open 重复回调仍不污染 | closed / closed | 4 / 0 / 0 | pass |

完整 67 行矩阵：`run-001/state-matrix.tsv`；JSON 含完整 invocation/fault 审计：`run-001/state-matrix.json`。没有人为构造并行 Job 或有副作用检查调用。槽满的连续拒绝覆盖 owner 原子准入，不外推为多个并发宿主协调器的压力验证。

## SQLite 与原始证据

67 次子进程快照 `PRAGMA integrity_check`、4 次杀前独立只读核验、9 次退出后独立只读核验，均为 `ok`。另外对 9 份退出后数据库文件快照做只读验证也全部 `ok`，留 `artifact-verification.json`。这些证据只证明本轮进程终止后可恢复读取与一致性，不能证明设备缓存掉电持久性。

各阶段 `run-001/phases/<phase>/` 含 `events.jsonl`、已同步的 `marker.json`、原始 `stdout.log`/`stderr.log`、`independent-after-exit.json` 与 `sqlite-files-after-exit/`；4 个被杀阶段另有 `independent-before-kill.json`。SHM 是可重建的 SQLite 协调文件，核验可能更新 SHM，不作为已提交事实的真源。没有以同进程 Close/Open 替代上述 exec 冷启动。

3 个发布基线 breaker 测试均 pass：`TestBreakerWindowCountingAndExclusions`、`TestBreakerCooldownHalfOpenAndEpochs`、`TestBreakerPolicyOverride`。编译、测试原始输出和准确 argv 已保存。未运行全仓 tests 或 scripts/check.sh：本轮仅添加独立私有测试文件，没有待提交产品变化，全面检查会进入用户另行隔离的领域范围。

测试二进制 SHA-256：`c0c4ee7ff969538c7ed488dd36329f90b6adf00baf1818cd70068beefb360aa1`。

Go 驱动 SHA-256：`4671cf252154b30c1515077b464eda96af5db25aa54e945cd60df9fa3a027770`。

Python 控制器 SHA-256：`e94b967b4d77206aadd703e2212fb3f7568458b638c47a339fd40ec718bde374`。

`MANIFEST.sha256` 列出证据包成员的 SHA-256，排除清单自身。公共源码快照含 GPL LICENSE；完整构建源码可按固定 commit 从现有 Lantai 仓库取得，依赖版本由 go.mod/go.sum 固定。

## 可重放命令

在同一获准的 Linux 容器中，使用新的隔离 worktree 和新的输出目录；不要覆盖本轮证据目录。驱动会拒绝不符的 HEAD、tracked 改动、不同内容的已有驱动，以及已存在的输出目录。

```bash
git -C <ORIGINAL_SOURCE_CHECKOUT> worktree add --detach <M210_REPLAY_WORKTREE> bad6b8a54d9e48e8d8d85bb903d9257a5b06c7d7
source <PRECONFIGURED_TOOLCHAIN_ENV>
python3 <M210_EVIDENCE_ROOT>/driver/replay.py \
  --source <M210_REPLAY_WORKTREE> \
  --output <NEW_M210_EVIDENCE>
```

解压后的证据包可用自身 `driver/replay.py` 与同目录 Go 驱动重建测试二进制。控制器的环境变量仅是 Go test 子进程入口，产品入口没有新增故障开关。新 output 保存先前失败；若将来驱动或环境有问题，应另建运行并保留旧失败，不覆盖本轮。

## 通过、失败、未测与后续影响回归

**通过**：上表 owner 持久化/冷却/未知占位/停止释放/后续完成/重新 open/epoch/去重/窗口断言；9 个原生 PID 与 4 次实际 SIGKILL；各阶段 SQLite 完整性；3 个原有 breaker 单元测试。

**失败**：无。没有为通过而修改产品或共享 fixture，没有测试失败后改期望重跑。本轮仅一次 `run-001`；前期文件定位的一条不存在的 errcode 文件读取不属于测试执行失败，已按真实路径读取。

**未测或不适用本轮**：activation、registry、Job、人审、发布治理、M2-03/09、本地 BUG-20261008-01～05 修复和 M2-11 客户端；完整 serve 启动/业务链；实际一次性检查器停止确认与 result.json 验证；权限撤销/启用代次接受权；升级回滚或自有状态不兼容；真实 15 分钟等待、长稳、持续 RPO、设备断电、宿主机崩溃、Windows/NAS/其他平台与目标部署卷；同时运行多 owner 的并发压力。

本地五项修复合入后，应记录实际合入 commit/tree 并在新隔离 worktree 执行本驱动，以 `--expected-head <新合入SHA>` 显式固定新基线，另建输出，保留本轮历史。若涉及 `breaker.go`、SQLite 打开/迁移/事务队列、执行结果分类或时钟，复跑完整 9 阶段及 3 个 breaker 测试；若涉及调用入口、授权、activation、registry、Job、Review 或发布治理，其接线和影响由对应本地专项补验，不能仅用 owner pass 放行。M2-11 继续由已分配线程负责。

本轮所有测试子进程和控制器均退出，没有常驻监听、周期任务或继续运行的测试。保留隔离 worktree 和证据供复核；报告没有维护者签署，也没有更新任何知识库或验收状态。
