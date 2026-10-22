# 清单驱动的本机批次入藏

这是 `internal/client` 的版本化本机 JSON 与 Go 调用契约，复用既有 [Push/Pull](client.md)、[存储](storage.md)和[著录](catalog.md)接口。没有新的服务端提交、授权或幂等状态机。批次只保存本机编排信息，committed 事实仍以服务端固定版本回执为准。

本模块提供 `PrepareImport`、`ValidateImportPlan`、`Client.ApplyImport` 和 `Client.VerifyImport`；已通过 `lantai asset import plan/apply/verify` 接入[素材管理 CLI](asset-workflows.md)。不得因为一次开发测试通过就执行真实素材迁移。

## 1. 输入与只读计划

调用 `PrepareImport(ctx, input, sourceRoot)`。`sourceRoot` 必须是操作者单独指定的目录；本机输入、计划、回执、状态和验证副本不应放回源目录。输入示例只使用合成素材：

```json
{
  "schema": "lantai.asset-import-input/v1",
  "assets": [{
    "key": "example-scene",
    "directory": "scene",
    "classification": "final",
    "push": {
      "project_id": "01ARZ3NDEKTSV4RRFFQ69G5FAV",
      "slug": "example-scene",
      "content": {
        "asset_type": "scene",
        "files": [
          {"path": "main.blend", "role": "primary"},
          {"path": "textures/diffuse.png", "role": "source"}
        ]
      }
    }
  }],
  "excluded": [{"path": "cache", "reason": "可重建的中间缓存，不入藏"}]
}
```

- `key` 是稳定的批次本机键，使用单个安全路径组件，最多 100 字节，不以 `.` 开头；大小写折叠后不得重复。它不是服务端资产 ID。
- `directory` 相对源根；`.` 表示源根。`push` 完整沿用 `PushInput`。文件路径相对该资产目录；首次入藏或追加指定 `asset_id`/`base_version_id` 均由原提交接口裁决。业务字段适配应先由资产 profile 模块完成，再交给本模块冻结；本模块不推断审定/发布状态。
- `classification` 必须明确为 `final`、`intermediate` 或 `unknown`，仅表示源材料的本机分类。即使是 `unknown`，也必须在 `assets` 中显式逐文件选择才会入藏。
- `excluded` 对每个指定文件或目录提供非空理由。目录排除会停止遍历其子树；不声称掌握排除树内部的数量、容量或未知内容。排除项不得嵌套、重复或与选中路径重叠。
- 本机 JSON 拒绝未知字段和多个 JSON 文档；`metadata`、`rights` 等原有 RawMessage 继续服从原服务端契约。本机 JSON 不代替服务端全部 schema 校验。

计划 `lantai.asset-import-plan/v1` 保存规范化源根、完整输入、全部选中文件的大小/SHA-256、选中文件数/字节数以及未覆盖项的路径、类型、大小和 `unknown` 分类。计划摘要为去掉 `digest` 字段值后的固定 Go JSON 编码 SHA-256；这是本机变更检测，不是签名或权限证明。

计划不联网、不上传、不创建源文件、不移动或删除原件；未选择的文件只读元数据，不读取其负载。目录遍历不跟随符号链接，未选择链接会作为 `symlink` 报告；选中文件或资产目录中的任何链接均拒绝。文件路径必须准确匹配源目录中的拼写，同时拒绝跨平台大小写冲突、路径穿越和传输保留后缀。

选中凭据命名文件（如 `.env`、`credentials.json`、`token.json`、`session.json`、`.ssh`、`.aws`、`.key`、`.pem`）会拒绝，须显式排除并写理由。这只是保守命名检查；内容审阅仍由操作者负责，不能称为完整敏感内容识别。

硬限为 1,000 资产、30,000 选中文件、30,000 个未排除遍历项（包含目录）以及 8 MiB 本机 JSON；超限明确失败，绝不截断。大批次必须显式缩小源根或拆分清单，不能让程序自动把未知数据入藏。

`ValidateImportPlan(ctx, plan, sourceRoot)` 会重算完整计划。选中文件变化、源根变化、选项变化、排除理由变化或未覆盖元数据变化都会拒绝沿用计划。显式排除树内部的变化不会阻止计划，其内容没有被扫描。

## 2. 显式执行与断点恢复

只有 `client.ApplyImport(ctx, plan, sourceRoot, stateDirectory)` 发起写请求。计划不会自动执行；源根与状态根不得相同或互相包含。状态目录必须私有、不得是链接；Unix 新目录使用 0700，Windows 新目录使用现有私有安全描述符（当前用户显式 owner/ACL），已有目录只检查，不自动修改权限。

本机 `import.json` 绑定 schema、HTTPS origin、计划摘要和有序资产键。每个资产原封不动复用独立的 `<key>.push.json`，由既有 `Push` 在首次请求前保存 create/commit 幂等键并处理分片、原 operation 对账和提交重放。批次没有第二套服务端提交状态机。整个 Apply 固定同一个私有状态根目录句柄，批次报告与每项 Push 的读取、落盘和锁均从该根派生；网络请求期间状态根或祖先被换成链接/其他目录时返回非空错误与部分报告，不能把状态写入源目录。恢复原状态目录后仍使用原计划、schema、锁文件名和 create/commit 幂等键续跑，不新建替代操作。此锚定适配仅用于批次内部，公开 `Client.Push` 的签名及原路径状态行为保持原样。

| 每项 `status` | 含义与下一步 |
|---|---|
| `not_attempted` | 尚未开始；后续显式运行原批次继续。 |
| `committed` | 已保存固定 asset/version/operation 回执且初始著录完成。重复运行读取原 Push 回执，不再次发送写请求。 |
| `description_pending` | 文件版本已提交，初始著录未完成。保留固定回执；批次 `complete=false`，返回非空错误。重跑原计划/状态只重放原 commit 补著录，不创建另一份上传或版本，不自动扩权。 |
| `failed` | 服务端给出明确、有效的 4xx 拒绝。保留原状态，排除授权等问题后继续原操作。 |
| `unknown` | 响应丢失、协议错误或本机回执保存等结果不明。保留原 operation（如果已知）和原状态；不自动换键。 |

逐项保存并返回 `ImportReport`。单项失败不隐去其他项，调用方必须同时输出/保存 partial report 并保留非零退出。`complete=true` 仅表示保存的历史回执记录全部初始著录完成。输出固定带有 `completion_basis="historical_receipts"` 和 `current_remote_verified=false`，不证明当前服务器存在这些版本或当前身份仍可读取；也不表示已审定/发布、已备份或允许删除源文件。旧 v1 状态缺少这两个字段时按同一历史语义加载，不修改原 Push 键；声明远端已验证的导入状态会被拒绝。包含新增字段的报告/状态须由本版或更新版客户端读取；旧版严格解码器可能拒绝，不能删字段或改摘要绕过。

保存的原 Push 回执可恢复“服务端已接受但批次行尚未落盘”的情况。已有已尝试项丢失原 Push 状态时明确拒绝，不生成替代键。极小窗口内若进程在记录尝试后、创建 Push 状态前退出，也会安全拒绝并要求诊断；不以猜测未产生副作用为由生成新键。

每次执行和每项推送前重验源文件。状态与回执不包含会话 token、短时传输 URL 或原始错误消息；批次只输出已登记错误码或 `IMPORT_INCOMPLETE`。原 Push 状态仍使用其既有的私有文件格式，不能手工编辑、复制一部分或换服务器后继续。

## 3. 固定版本全量验证

调用 `client.VerifyImport(ctx, plan, report, sourceRoot, stateDirectory, destination, purpose)`，用途必须显式提供，不会为了下载成功自动换用途或扩大权限。

1. 重验计划及源文件，核对报告和原状态中保存的完整回执一致；源、状态、验证根三者两两不重叠，NFC/Unicode大小写等价的目录也视为同名（即便某个本机文件系统允许两种拼写并存）。
2. 目标必须为空，或带有与原 origin/计划摘要匹配的私有 `.lantai-import-verify.json`。已有未绑定内容、冲突字节、未声明文件/目录、链接和其他批次均拒绝；不覆盖或删除它们。
3. 读取回执中的固定版本 manifest，核对文件数、相对路径、角色、大小和 SHA-256 与计划完全相同；匹配后复用[工作集](workset.md)的 `PullWorkset`，以完整验证根为 root、该资产键为 entry destination。工作集重新验证 instance/asset/version/content digest 绑定，并在 read grant 和传输前后检查同一个固定 `os.Root` 的身份，不走 latest 别名，也不另建下载实现。
4. 复用下载的 Range 和私有本机状态续传；每个文件完成后再次全量散列，检查精确相对路径、数量、字节数和 SHA-256。传输锁文件不计作素材，残留 partial/下载状态不能算完成。

回传 `lantai.asset-import-verify/v1`，`completion_basis="current_remote_and_local_files"`；只有本次全部固定版本远端清单、权限与本地文件验证通过，才设置 `complete=true` 和 `current_remote_verified=true`。已有完整下载也必须重新请求远端，无法访问远端时不能只用本地缓存判定成功。这是本次验证观察，不保证后续永远可用。逐项列出 `verified`、`failed` 或 `not_committed`。`description_pending` 的固定版本可以校验字节，验证通过不把初始著录状态升级为完成。失败保留原件、完整回执和目标中的续传文件，修复问题后仅复用原验证目录继续。

验证文件字节和目录结构不等同于 Blender、ComfyUI 或剪辑软件能续做。工作集依赖恢复、固定版本浏览、字段组合查询、原软件打开/续做和离机备份恢复须由各自模块及独立验收补齐。本模块不解析或改写工程内引用，不转换素材格式，不部署 NAS，不实施真实迁移、主分支合并、发布或源文件清理。

## 4. 合成自动验证

```sh
go test -race ./internal/client -run Import -count=1
scripts/check.sh
GOOS=windows GOARCH=amd64 go test -c ./internal/client -o /tmp/lantai-import-client.test.exe
```

包内测试通过临时目录与 `httptest` 复用真实客户端协议路径，覆盖：同计划两次无新增版本、提交响应丢失后原 operation 恢复、待著录及原 commit 补办、明确拒绝与不明结果、输入/计划/源根/origin 变化、原状态丢失、创建上传/接受提交期间状态根被置换为源目录链接后源哨兵不变且恢复原键续跑、凭据命名/链接/排除项/未知字段、未覆盖文件、目标冲突/额外文件、manifest 变更先于下载、错误身份/摘要在 read grant 前拒绝、验证根在 grant 时被置换不产生越界下载、Range 续传及完整摘要、九种合成素材包装和新版本入藏后旧版本仍可验证。所有负载由测试生成，伪二进制扩展名不代表真实 DCC 文件；软件打开验证不能从这些测试推导。

下载继承工作集每个 entry 最多 10,000 文件和 1 TiB 的硬限；计划阶段已提前拒绝超过该验证能力的单资产。批次计划虽允许总计 30,000 文件，仍须把过大的单资产分解为经审阅的合理资产，不能在验证阶段静默省略。

Windows 另有私有目录 owner/ACL 测试；交叉编译只证明构建成功，Windows 本机执行仍须单独记录。本模块的完整检查证据应绑定实际提交和目标平台，不能替代既定迁移前门禁。


## 灾后恢复与客户端回执

原计划、Push 状态、批次回执和工作目录属于客户端，不由核心完整包自动恢复。同一 origin 恢复到较早捕获点后，旧回执可能晚于捕获点；`apply` 可以零网络返回历史完成，迁移验证必须使用 `verify` 读取真实远端，不能把历史 `complete` 当当前馆藏证明。

恢复后先私有保全原 plan/state/回执和未入藏成果，完成核心的恢复接受流程，以新合法身份核对固定 instance/asset/version、原件和操作状态。捕获点内的 pending/unknown 通过原 operation 查询及原 commit 键续办，禁止替换键；测试中的同一主体经正常管理员授权取得新凭据后可补著录和取得原回执。捕获点外的 pending/unknown 查询不到原 operation 时安全停止，不创建替代上传；已保存的 pending 固定版本身份继续保留。

读不到可能是无权或不在恢复点，不能仅凭 404 认定丢失。禁止手改 origin、摘要或原键，也不能为结果不明的项自动换 state 重传。确认未被捕获的成果须单列，经显式授权决定新操作与旧引用映射；此版本不自动侦测服务器恢复代次，也不额外申请读取权限来改变 `apply` 的既有行为。
