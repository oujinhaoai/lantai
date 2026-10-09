# 人审目标与批次远程验证

`tests/integration/remote_review_acceptance_test.go` 验证真实组装应用的 HTTP 监听和独立 CLI 进程。使用隔离临时实例、合成主体与数据，调用固定 Flow、真实 corecheck 进程、独立 QA 和人审；不需要预先启动服务或配置真实凭据。

```sh
go test ./tests/integration -run '^TestRemoteReview' -count=1 -v
go test -race ./tests/integration -run '^TestRemoteReview' -count=1 -v
```

测试覆盖冻结字段替换、跨会话兑换与执行、挑战和未完成授权到期、已完成项在授权到期后的回执重放、同主体换会话质检拒绝、实际退回与新版提交、旧目标/旧授权/错误 revision 拒绝，以及普通评论和问答不能产生审定事实。

批次通过 `human/prepare` 固定两个审定子项，再用 `human/execute` 逐项执行。分别覆盖首项成功后剩余目标返工，以及第二项请求到达测试代理同步点后真实撤销角色、再恢复权限。代理只暂停请求转发，不替换服务或授权判定；用通道确认请求到达和撤权完成，不以 sleep 推测顺序。

当前 API 没有单请求执行整批或返回整批汇总的入口。逐项响应和 CLI 退出码是服务端契约；测试日志的 `batch-summary` 是测试驱动汇总。已完成项也要求原人类会话及当前权限。角色撤销后返回拒绝；恢复权限后重放只返回原回执，不重新批准。

前置的身份注册、Profile 初始化与 Flow 定义使用既有合法领域 API；Profile 由真实 corecheck 和人审批准，不直接写 SQL。被验收的制作、上传、检查、QA、挑战、审定和角色撤销走真实 REST/CLI。SQL 仅用于只读核对领域事实、计数与取得 QA 证据的 operation。应用与 HTTP 服务在测试进程内运行，CLI 和检查器为独立进程；不代表独立 `serve` 强杀、部署、掉电恢复或其他平台验收。

到期验证使用同一实例的受控时钟。操作、持久化、HTTP 和 CLI 真实执行，但不等待真实五分钟，也不据此推导墙钟耐久性。

可选保存原始远程响应与 CLI 标准输出/错误输出，或复用固定 CLI 制品：

```sh
# 路径位于仓库之外；每次选择新的空证据目录。
go build -trimpath -o /tmp/lantai-review-cli ./cmd/lantai
LANTAI_REVIEW_BIN=/tmp/lantai-review-cli \
LANTAI_REVIEW_EVIDENCE_DIR=/tmp/lantai-review-evidence \
go test -race ./tests/integration -run '^TestRemoteReview' -count=1 -v
```

证据包含合成对象 ID、目标绑定、完整原始响应及 SHA-256、CLI 输出、只读计数与每个场景的观察值。不会记录会话 token、长期凭据或 TOTP；仍应按运行材料保存在仓库之外。脚本不以 HTTP 成功或 CLI 退出码单独判定通过，还核对错误码、版本状态、回执相等及拒绝/重放前后事实不变。执行结论、源码/制品摘要和正式评审按知识库测试任务维护。
