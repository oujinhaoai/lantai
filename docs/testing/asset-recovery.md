# 素材管理与完整备份的联合验证

此项是开发组合回归，不代替独立验收或 NAS 现场验证。只使用[合成生成器](../../tests/fixtures/asset-management/README.md)的九类语料，不读取真实素材、知识库或现用实例。

## 联合路径

`TestAssetRecoveryJointPackage` 通过同一个核心和公共客户端执行：

1. 九类素材各入藏 v1/v2。v1 先用仅 ingest 的身份得到已提交但著录待完成的回执，再用合法 organize 身份续办；asset/version/operation ID 必须保持不变。
2. 两批均重跑并在独立目录验证。当前画像通过原条件 PATCH 更新，精确 AND 查询返回九项，错误角色不匹配；18 个历史清单、文件角色、rights 和固定工作集均核对。
3. 将只含预期 ID、清单和摘要的合成核对文档作为普通 config 资产入藏，再通过原 core 维护协调器生成完整包。
4. 原核心停止后，在新空目标仅用完整包恢复、建立新管理员、执行领域恢复与深度检查，再接受恢复点。恢复实现不取得源 fixture、源目录、源会话、独立 oracle 或额外密钥。
5. 新目标经公共 API 自行发现已恢复的核对文档，重复精确检索、18 个固定版本、全部原件和工作集拉取，确认受限来源仍拒绝 production 用途。
6. 仅以已回拉的 v2 原件创建工作副本，各加一份文本侧车，以原 asset ID 和 v2 base ID 追加 v3。同计划/状态重跑不得创建 v4；同时验证全部 27 个固定版本、177 个文件和当前著录，确认原 v1/v2 未改变。

本地导入计划、Push 恢复状态和工作目录属于客户端，不被冒称为服务端备份内容。新目标使用新的客户端状态和新管理员身份；不把原 origin 的状态文件改写到新 origin，也不重发旧密钥。

## 运行

先在独立目录生成并校验九类合成语料。包含原生格式时使用已安装的 Blender；缺少原生工具会明确标为部分覆盖，不能以占位文件替代。

```sh
LANTAI_ASSET_FIXTURE_ROOT=/absolute/synthetic-corpus \
  go test -race ./tests/integration -run '^TestAssetRecoveryJointPackage$' \
  -count=1 -timeout=10m -v
```

未设置 fixture 根时普通全仓检查明确跳过此项，因此必须另行启用。日志中的 source、restore 和原资产 v3 续办步骤都通过，才算这条组合路径通过。

隔离容器驱动使用两个互斥模式：

- 源容器：`LANTAI_ASSET_RECOVERY_MODE=capture`、fixture 根、空的 `LANTAI_ASSET_RECOVERY_EXPORT`。只导出 `recovery.lantai`。
- 销毁源容器后，新目标：`LANTAI_ASSET_RECOVERY_MODE=restore` 和 `LANTAI_ASSET_RECOVERY_PACKAGE`。不得设置 fixture 根。可选的空 `LANTAI_ASSET_RECOVERY_OUTPUT` 仅保存回拉结果，不能提供恢复输入。

目标镜像仅含测试程序，根文件系统只读、无外网、无额外能力，并以非 root 用户运行。容器输入挂载和源容器已销毁的证据由驱动另存。目标产出的 `roundtrip/` 保留原语料的 `assets/` 相对结构及恢复出来的生成清单，可再运行生成器的 `verify --assets-only`；原生工具复开与真实设备断电验证仍分别给出结论。

## JSON 计划的稳定性

客户端生成的 JSON 计划包含已绑定摘要。素材 CLI 在执行原有严格 JSON/YAML 校验后保留合法 JSON 的成员表达，避免重排嵌套的冻结内容导致未改动计划失效。大小上限、重复字段、未知字段、篡改摘要和源文件变化仍会拒绝。生成的计划应按原 JSON 保存，不编辑其中内容或把它转换为另一份 YAML 计划后沿用旧摘要。


## 同 origin 旧客户端状态

`TestAssetRecoveryOriginalClientState` 使用真实核心与 HTTP，在相同网关 origin 将已停止源端替换成较早捕获点的恢复端。六个场景分别保留捕获点前/后的 complete、description_pending 和已提交但响应丢失的 unknown 原始状态；不手工补客户端回执、origin 或幂等键。新凭据经恢复后正常身份服务签发给同一合成主体，旧凭据必须失效。

- 捕获点内 pending/unknown 用原操作和键补办，固定 asset/version 不变。
- 捕获点外 pending/unknown 查询失败后不能发送新写入；原状态保全。
- 捕获点外 complete 仍可零网络返回历史回执，但必须明确远端未验证。即使验证目录已有完整原件，`verify` 仍请求真实远端并失败。
- 最后远端只能保有捕获点内三个原资产，不得产生替代资产。

```sh
go test -race ./internal/client ./tests/integration \
  -run 'TestImportHistoricalReceipt|TestAssetRecoveryOriginalClientState' -count=1
```

这组测试证明明确的历史/当前语义与所列恢复路径，不建立自动恢复代次侦测，也不代表任意 404 都是丢失。服务器实时可用性不能由过去一次验证永久保证。


## 恢复前后的审批、保留和权限

`TestAssetRecoveryGovernanceAndPermissions` 使用真实应用组装、corecheck 一次性进程、TOTP 和 HumanGrant；不以 SQL 设置审批/任务/生命周期状态。代表性流程为人退回 v1、实际检查并批准 v2、显式发布 v2、保留待审 v3，然后完整包恢复。恢复后核对历史结论和发布指针不漂移；新人审证明也不能复活旧任务 fence/审定目标。新任务产生 v4，经新检查和人审后仍保持 v2 发布指针，直到显式发布 v4。

同一测试把两个可清除对象和一个 Hold 对象放入回收站。到期任务清除允许的对象，Hold 保留，GC 的 24 小时等待跨越完整包恢复。恢复后只收集无引用的唯一内容，共享内容因存活版本保留，Hold 内容仍在；有权人解除 Hold 后恢复误回收对象，固定版本字节不变。这里使用受控时钟，不代表经过真实时间的长稳。

恢复前后均通过真实 HTTP 检查：无项目权限的主体查不到列表/固定版本，不能使用他人的 Range 授权、探测/复用项目内容；受限资料不允许 production 用途，普通入藏身份不能清除或代替人审。恢复后的旧会话失效；合法新会话可调阅。实时撤销项目角色后，已有 Range grant 和固定版本详情均拒绝。

```sh
go test -race -v ./tests/integration \
  -run '^TestAssetRecovery(GovernanceAndPermissions|OriginalClientState)$' \
  -count=1 -timeout=10m
```

六种旧状态测试末尾还逐资产核对捕获内三项的固定 version ID、version number=1；捕获外三项不得出现。每项返回的 operation ID 必须与原 Push 状态一致。该断言补足“资产数没变但意外多出版本”的检测。

这些是共用控制管线的代表性组合，不是九类素材×全部权限/生命周期的机械全交叉验证。原生软件场景见[原生续做](asset-native-continuation.md)。未覆盖平台、实际存储故障、容量、外网 HTTPS 和独立浏览器许可仍单独验收。
