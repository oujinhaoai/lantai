# M2 业务恢复与重同步的独立复核

本记录复核共同基线 `e1cf5206b7b4611d5a7779438246643798b86eaf` 上的两个实现候选：业务恢复 `c1d7a7ba91b5e9d32ed2e8584158de4260902420` 和重同步 `077550f3fb0571ce9747cc643255c421e8d0d1b9`。在新隔离分支依次组合为 `71f36c5`、`a376b57`；原候选、原失败记录均保留。共享 README 中英文说明和开发命令逐项核对，两项能力均保留。

## 代码复核

制作任务的完成命令从人审批准移到真实发布事实接受后生成。发布指针须匹配本轮批准版本；完成命令使用本轮审定 operation 的稳定键，在所属 runtime 事务中持久化。重复和迟到通知不会另建完成命令，后续显式派发复验当前任务事实。原有夹具增加一次派发，保留全部状态、权限和指针断言。

里程碑 REST 通过真实应用接入 T01 身份服务与 T05 任务端口。路由核对项目、条件修订和任务集合；权限、负责人资格、幂等回执与进度由现有领域服务接受。`meta` 仅在两个服务均配置时声明能力，无跨库 SQL 或另一份任务真源。

未发现新增产品缺陷。新增补测为本轮自测，针对原实现的代码与证据复核独立于两个原实施会话。

## 独立补测与复现

```sh
GOMAXPROCS=2 GOFLAGS=-p=2 go test -race -count=1 -timeout=10m \
  ./internal/workflow ./tests/integration \
  -run '^(TestPublicationIndependentReviewIgnoresInapplicableFacts|TestM2MilestoneIndependentReviewRevokedReplay)$'

GOMAXPROCS=2 GOFLAGS=-p=2 go test -race -count=1 -timeout=10m \
  ./tests/integration \
  -run '^(TestM2BusinessRecoveryAcceptance|TestM2ResyncAcceptance.*|TestM2MilestoneIndependentReviewRevokedReplay)$'
```

新增发布边界测试覆盖其他版本、未发布、旧制作轮次及已完成步骤，均保持工作流不变。真实里程碑补测先接受并重放同 key，再撤销写角色，确认返回 `FORBIDDEN`；撤销项目成员资格后，列表、进度和同 key 重放均返回 `NOT_FOUND`，所属里程碑事实未变。

固定 race 集成制品的六项用例通过，累计 130.515 秒。业务链通过 12 个独立 serve 中断/重放点、15 次启停；approved 未发布时 C 仍为 waiting。发布后及空目录恢复后，C 经新凭据/新 epoch Attempt/新 manual_cli TaskRun 读取精确 48 字节。共同备份后 25 张业务历史表逐表一致，原事件再次收录数为零。旧 token、真实 `tasks/renew` 和 `task-runs/seal` 分别以领域错误拒绝；历史审定目标、活动 C fence 和内置 activation 在记录的内部最终接受层拒绝。

正式事件裁剪后，REST/独立 CLI 的过期、分页及水位行为一致；两处真实并发变更和重复事件重放后，8 个客户端对象与新鲜权威快照及最终水位完全相同。context/decision 四份版本均经真实 corecheck、独立 QA、HumanGrant、人审与发布，旧任务固定输入和同 key 重放未漂移。

原业务候选的 1,159 个源码摘要、49 个附件，以及重同步候选的 19 个提交绑定源码和 61 个附件均只读核验一致。历史 heartbeat 路由不存在所产生的 404 不作为旧租约拒绝证明；本轮以真实 `renew` 为准。历史产品失败、夹具失败和早期驱动失败继续保留。

源码、固定制品、命令、原始业务/REST/CLI/serve 日志与备份摘要保存于维护者执行记录；公开摘录和源码绑定见 [证据目录](m2-07-08-independent-review/)。

## 适用范围

这是 macOS arm64 上合成实例的软件复核，内部事件注入、合法可控时钟前置、预批准上下文验收配置及内部拒绝层的边界仍保留。C 的新执行证明是 manual_cli 登记及生产读取，不扩写成另一条制作/人审闭环。跨平台 CI 只证明其实际运行的软件范围；设备部署、断电、持续 RPO、14 天长稳、实际产品 MCP、整卡和 M1/M2 门禁仍需各自验收及人工签署。

## 组合完整检查

同一冻结源码持有共享仓库外排他锁，实际执行 `GOMAXPROCS=2 GOFLAGS=-p=2 scripts/check.sh`，保留默认 race / 30m，退出 0，总 1582.142 秒。格式、依赖整洁、vet、staticcheck、生成物无漂移、Python SDK、全部 Go race 包测试及构建通过；受测源码检查前后无漂移。完整检查中的业务链与重同步另使用独立实例/ID和证据目录，不与定向实例混算。原始控制台日志见 [full-check.log](m2-07-08-independent-review/full-check.log)，执行参数及摘要见 [full-check-result.json](m2-07-08-independent-review/full-check-result.json)。

## 容器构建输入补充修复与最终检查

容器配方的 COPY 与专用 dockerignore 同时遗漏 `api/`，而 MCP 服务实际导入该包。关闭模块网络的原 COPY 隔离上下文真实 Go 编译退出 1；新增 `COPY api ./api` 与 `!api/**` 后，同一 Linux/amd64、CGO=0 编译退出 0。新增测试由真实 `cmd/lantai` 依赖图推导所需源码根目录，覆盖配方和 allowlist 两层；原配置准确失败，修复后部署包 race 通过。这里只验证源码输入与编译，本机没有 Docker 引擎；真实镜像/启动仍需目标环境验收，既有 Caddy 条件用例的 skip 不计网关通过。详见 [构建输入证据](m2-07-08-independent-review/docker-context-review.json)。

包含两行修复及最终新测试的第二次冻结源码持同一共享排他锁，默认 race/30m 完整 `scripts/check.sh` 退出 0，累计 **1520.502 秒**，全部检查通过，1173 个冻结源码文件前后无漂移。实际业务链与五份 M2-07 补验证据再次生成；首次组合结果不移作这次结果。记录见 [最终完整日志](m2-07-08-independent-review/final-check.log)、[参数与制品绑定](m2-07-08-independent-review/final-check-result.json)和[完整冻结源码摘要](m2-07-08-independent-review/final-source-files.json)。结果文档是在检查结束后补入，最终提交对所有受测程序/测试/配置逐件核对摘要。

## Windows CI 清理夹具修复

原候选2a53304的[CI 38066163874](https://github.com/oujinhaoai/lantai/actions/runs/38066163874)保留为失败：9/10成功，Windows TestM2BusinessRecoveryAcceptance在71.17秒的清理阶段出现TerminateProcess Access is denied。Go测试在Cleanup前取消T.Context，CommandContext自动Kill与既有显式stop竞争；[微软TerminateProcess文档](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-terminateprocess)说明已终止进程可返回ERROR_ACCESS_DENIED。

夹具改用exec.Command，由原有stop唯一管理生命周期；Kill错误、实际Wait、10秒截止及全部强杀重放断言保留。新增真实子进程回归在原夹具先准确失败（signal:killed），修复后在已取消测试context的Cleanup中用独立context读取真实API meta成功，再严格停止子进程。回归编写阶段的日志句柄遗漏和错误端口/healthz 404分别保留在私有原始记录，不计产品结论。

本修复源码默认race/30m完整检查退出0，耗时1460.355秒；1178文件摘要前后相同，12强杀/12重放/15启动/15实际退出全部再现。原始输出及摘要见[windows-cleanup/result.json](evidence/m2-07-08-independent-review/windows-cleanup/result.json)。该结果只绑定记录的受测源码；修复后最终PR head和main全部十项CI须另行确认。
