# 素材存储迁移前的软件补验

这些测试补充[联合恢复回归](asset-recovery.md)；使用独立临时实例和合成内容，不读取真实素材、现用实例或知识库。它们属于开发自测，不能签署独立验收、实际存储设备或迁移放行。

## 多输入版本与许可收紧

`TestReadinessMultiInputRightsTightening` 经原 storage/catalog/provenance 所有者建立图片、动作和配方三个固定输入，再建立引用三者的成片。载荷是明确标记的短合成字节，仅验证领域关系，不冒充可播放视频或原生模型。

- 输入新增版本后，已提交成片的三条输入版本不漂移；可反向找到使用该输入的成片。
- 原来允许 production 的输入新增 reference 限制后，成片的当前用途判断拒绝 production，rights epoch 增加。
- 限制前签发的 production grant 在真实 HTTP Range 请求时拒绝，新 grant 同样拒绝；archive_review 仍允许。
- 冻结清单保留原始权利声明及固定输入，不为反映当前限制重写历史。

## 自然会话到期后续办

`TestReadinessExpiredSessionResumesOriginalUpload` 用同一合成机器凭据经公共 exchange 接口取得 60 秒会话。首片被接受后，推进服务端受控时钟 61 秒，使后续请求得到 `TOKEN_EXPIRED`。再以同一凭据换取新的短会话，用原状态续办。

测试要求主体和 credential ID 不变、session ID 改变；权限只有请求的 read/ingest 及基础 self，不含 organize。upload/operation/create key/commit key/request hash 均不变，首片只发送一次，提交后保留 description_pending，重跑回放同一回执。本地状态不得含机器凭据或会话 token。这里没有延长原会话，也没有在续办阶段重新引导管理员；受控时钟不证明经过真实时间的长稳或实际登录负担。

```sh
go test -race ./tests/integration \
  -run '^Test(ReadinessMultiInputRightsTightening|ReadinessExpiredSessionResumesOriginalUpload|RemoteResumableTransfer)$' \
  -count=1 -timeout=5m -v
```

## 显式 1GiB HTTPS 断流、重开及并发查询

```sh
LANTAI_ACCEPT_READINESS_LARGE=1 \
  go test ./tests/integration \
  -run '^TestReadinessLargeHTTPSRestartAndConcurrentQueries$' \
  -count=1 -timeout=15m -v
```

此项复用原远程流式传输测试，生成 `1GiB + 12345` 字节文件，使用受信任测试证书的本机 HTTPS 单入口和分开的核心 API/transfer 监听。第二片在接收 1024 字节后断开；完整应用关闭并从原数据根重开，HTTPS 入口及客户端原状态保留。已接受首片必须只发送一次。下载在 256KiB 后截断，同一 grant 用一次 Range 续完，最终大小与 SHA-256 必须匹配已提交版本。原有 128MiB Go heap 增长上限保留；这不是 RSS 上限。

测试在传输期间每约 50ms 请求 whoami，要求存在与传输重叠的成功样本并报告数量、p95 和最大值。只在有意关闭/重开核心期间暂停观测，因此不能据此声称重启无中断可用。延迟只是本次机器上的观测，没有用它制定实际设备的性能阈值。

未显式启用时普通全仓检查跳过这项大文件测试；原 `TestRemoteResumableTransfer` 默认 32MiB，仍在普通回归中运行。本项使用有序关闭/重开，不是进程强杀、掉盘、断电或真实网关验收。临时目录需要容纳源、暂存、CAS 和下载等并存文件，建议准备至少 8GiB 空余空间；这只是本测试的保守执行余量，不是整馆容量公式。

## 复用容量和 I/O 失败测试

无需重复实现软件保护，现有测试可独立重跑：

```sh
go test -race ./internal/storage ./internal/operations \
  -run '^Test(ResumeAfterRestart|PartIntegrity|WholeFileMismatchIsNotStored|LimitsAndIdempotency|StorageFullIsReported|DiskSpaceAndNetworkFileSystem|RecoveryPackageFailedPublicationAndStableRetry)$' \
  -count=1 -timeout=5m -v
```

覆盖存储重开后持久分片续办、分片/全文件错误摘要拒绝、额度边界和幂等、注入 ENOSPC 后可重试、低空间阻止启动、模拟网络文件系统拒绝，以及恢复包发布 EIO/导出失败时不发布且重试稳定。恢复包测试只操作新建的合成夹具，不执行任何浏览器下载，也不替代独立下载步骤。

## 仍需实际环境的证据

实际目标的文件系统、卷身份、ACL、挂载、HTTPS 证书/限额/超时、并发和可用容量须在固定环境核对；软件注入不证明设备耐久性。整馆预算应计入主库、同时上传暂存、历史版本、内层快照与完整包、解包/恢复/验证副本及保留策略，不能按单个大文件推断。持续 RPO、离机副本、RTO 和长稳需要实际介质及真实时间记录。实际素材的许可映射、成片输入关系和工作流也须单独审阅。以上条件满足并正式放行前，不开始真实素材试迁移。
