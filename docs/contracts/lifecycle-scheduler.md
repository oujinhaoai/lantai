# M2 到期清除与可恢复 GC 调度（T08）

本文记录 DEV-T08-05 已实现的调度器。业务是否允许清除仍由 T03 台账裁决，字节动作由 T02 按持久意图执行；回收站边界、文件动作与 GC 的领域规则见[协作适配契约](collaboration-foundation.md#回收站字节动作)与[审定与协作接口](review-collaboration.md)。本文不是独立 TEST 或 M2 门禁结论。

## 启用方式

调度默认关闭。数据根 `config.yaml` 显式开启后，`serve` 才按间隔在后台运行：

```yaml
contract: lantai.config/v1
lifecycle:
  scheduler: true        # 默认 false；开启是明确的部署决定
  interval_seconds: 300  # 10–86400，默认 300
  batch: 100             # 每类每次最多处理的条目，1–1000，默认 100
```

也可在服务停止时由本机维护命令运行一次，输出本次报告 JSON：

```sh
lantai lifecycle -home <数据根>
```

两种方式执行同一 `RunOnce`，都在维护屏障下写入；实例处于维护、备份或恢复状态时相关工作只延后，不越过屏障。项目策略 `trash.auto_purge`（默认 true）为 false 时，台账拒绝定时清除，条目保留到人工处理或策略恢复。

## 调度登记

runtime 库的 `operations_lifecycle_jobs` 由 operations 唯一写入。每项工作在交给所属模块前先登记固定的 operation、幂等键、请求摘要与恢复代次：

- 提醒 `ledger.purge_reminder` 与到期清除 `ledger.purge_due`：键为种类、回收条目、条目 revision 与恢复代次。条目变化（Hold/Unhold、恢复等）产生新 revision 和新登记；恢复后的新代次不能重放旧登记。
- GC `storage.gc`：键为内容哈希与 T03 候选 operation。

台账 `RunDueJob` 以调度器作为可信作业来源，调用 `CheckLifecycleJob` 核对：命令必须与已登记且未作废的记录完全一致，执行者为实例本身，没有会话或 HumanGrant。人或 Agent 会话不能冒充调度作业。重试总是复用原登记，响应丢失后按原 operation 取回台账回执并继续文件阶段。

## 每轮顺序

1. 以 storage 的 `deleting` 意图续完中断的 GC 删除；每次都重新读取候选、全部权威清单根与所有 pin。
2. 对台账记录的文件阶段失败，经 T03 `Resume` 按原意图与计划重放。
3. 到期前 72 小时起发送一次提醒（台账去重并产生 `trash.purge_due` 事件）；到期后请求清除。T03 在自己的锁内复验 Hold、到期、revision、项目策略、引用与首个清单基线；`NOT_DUE` 或策略关闭时同一登记一小时后再问，Hold、状态变化、前置条件失败或对象消失标记为作废，不重试。
4. 已达 24 小时等待的 GC 候选交给 storage `CollectBlob`。仍被存活版本、回收站、提交 pin、上传 pin 或备份 pin 引用时保留并一小时后复核；需要对账（例如台账仍有未决操作、存在权威根之外的清单）时按退避重试并在报告中列出；完成或已取消的候选不再调度。GC 从不根据索引计数或墓碑哈希删除。

失败按 1、2、4…分钟指数退避，最长一小时，原因保存在登记中。报告字段为 `reminders`、`purges`、`superseded`、`collected`、`retained`、`deferred`、`resumed_file_actions`、`failed` 与至多 20 条错误。

## 验证

```sh
go test ./internal/operations -run Scheduler -count=1
go test ./tests/integration -run TestM2LifecycleSchedulerDuePurgeAndGC -count=1
```

单元测试覆盖 72 小时提醒边界、执行前登记、崩溃后重试复用同一 operation、Hold 作废与新 revision 重新登记、授权核对拒绝会话/他人/换种类/未登记/换代次的命令、GC 等待/保留/对账重试/维护延后与完成后不再调度。集成测试用真实台账、存储、身份策略和可控时钟验证提醒、策略关闭时不清除、到期清除、24 小时前不回收、独有内容回收而与存活版本共享的内容保留，以及重复运行不扩大范围。

备份 pin 与上传 pin 的阻止回收、清单遗漏与损坏检测由 storage 的 GC 单元测试覆盖；真实平台上的中断演练、长期运行与恢复耗时仍属独立测试任务。
