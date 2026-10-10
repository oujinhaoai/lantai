# 完整业务恢复的软件验收

`tests/integration/m2_business_recovery_acceptance_test.go` 使用独立 `lantai serve` 子进程、真实 REST/CLI 和合成临时实例，验证固定业务 Flow 的返工、审定、发布及共同备份恢复。只终止测试自己启动的进程。身份、Profile 和旧发布版本的引导通过合法领域接口、真实检查器及 HumanGrant 完成，Profile 明确记录引导测试制品与独立 serve 制品的两个实际检查来源摘要；SQL 仅作只读观察。

```sh
GOMAXPROCS=2 GOFLAGS=-p=2 go test -race -count=1 -timeout=10m \
  -run '^TestM2BusinessRecoveryAcceptance$' ./tests/integration
```

A 通过 REST 登记 manual_cli TaskRun，用 CLI 提交两个版本并接受候选、封存执行结果；真实 corecheck 各生成四项检查，B 通过 CLI 读字节后独立提交 QA。人类经真实挑战/TOTP/HumanGrant 退回第一轮，再批准第二轮，最后显式发布。TOTP 在独立 serve 中按真实时间核验，测试等待下一个可用计数器，保留重放拒绝。引导和恢复后的新凭据登记使用可控时钟及合法领域接口。

每个推进点保存 Flow、StepRun、Task、Attempt、Job、审定目标、operation、版本、修订和发布事实。Flow 启动后用独立只读 SQLite 连接确认待发命令已经持久化且尚未派发，再强制终止该 serve PID 并重新启动。其他推进点同样执行核心进程中断。

事件注入在 serve 停止期间使用内部 `events.Store.Collect`，没有对外事件伪造接口。先收录同 ID、相同规范字节的事件，断言新收录数为零；再把已发生的任务、审定和发布事件倒序复制为新 ID，保留 operation/aggregate/revision/payload，并以 causation 关联原事件。真实 Flow 消费后必须保持业务表快照不变，随后再次交付相同复制事件，断言仍不新增逻辑事件。新 ID 的迟到通知检验当前权威事实和轮次复验；它不代表远程事件接口测试。

第二轮 approved 未发布时，旧发布指针保持。下游 C 的任务依赖必须等待真实发布，随后用生产用途拉取第二轮的精确版本并核对字节。共同备份校验通过后恢复到空目录，未完成新管理员设置和恢复对账时拒绝启动。恢复后比对历史业务事实、重新交付原事件验证去重，再通过合法审批签发 C 的新生产凭据与会话，创建绑定已发布版本的新生产任务、领取新代际 Attempt 并经 REST 重新登记 manual_cli TaskRun，使用 CLI 复核同一版本字节。旧会话、旧租约、历史人审目标和旧 activation 分别验证拒绝；旧租约经真实 renew 接口拒绝；归档活动 C 的 fence 另在内部最终写入守卫核验，历史人审目标经新 HumanGrant 的领域接口拒绝，内置 activation 经内部接受守卫核验。具体层级保留在证据中。

可选保留原始证据和精确执行制品：

```sh
# 每次使用新的仓库外证据目录，测试拒绝覆盖已有 JSONL。
go build -trimpath -o /tmp/lantai-business-recovery ./cmd/lantai
LANTAI_REVIEW_BIN=/tmp/lantai-business-recovery \
LANTAI_BUSINESS_RECOVERY_EVIDENCE_DIR=/tmp/lantai-business-recovery-evidence \
GOMAXPROCS=2 GOFLAGS=-p=2 go test -race -json -count=1 -timeout=10m \
  -run '^TestM2BusinessRecoveryAcceptance$' ./tests/integration
```

证据包含实际请求和响应、CLI 输出、输入/二进制摘要、同步点、事件关联、业务快照、备份清单及旧权限拒绝。会话 token、长期凭据和 TOTP 不写入证据。运行材料和实际完成状态保存在维护者知识库；公开文档提供可重复方法。

这是本机软件进程故障验证。它不证明断电耐久性、目标设备部署、持续 RPO、14 天长稳、实际产品 MCP 客户端或 GATE-M1/M2 通过。其他平台需要实际运行证据。
