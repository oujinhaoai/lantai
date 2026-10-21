# 固定版本工作集拉取

实现：[本地输入与编排](../../internal/client/workset.go)、[合成与失败测试](../../internal/client/workset_test.go)。这是客户端工作副本指令，不增加服务端清单、HTTP 路由、权限或审定真源。调用方必须先取得普通会话，服务端照常核对每个精确版本及每份读取授权。

## 输入与映射

`lantai.client-workset/v1` 将若干具体版本还原到一个用户选定的工作根。每个 entry 的 `asset_id`、`version_id` 是规范 ULID，`destination` 是该版本独占的相对目录。目标文件始终为 `destination + "/" + manifest.content.files[].path`，不扁平化、不改名、不解析 `@latest`、不追随资产的新版本。

```json
{
  "schema": "lantai.client-workset/v1",
  "entries": [
    {
      "asset_id": "01J00000000000000000000001",
      "version_id": "01J00000000000000000000002",
      "destination": "project"
    },
    {
      "asset_id": "01J00000000000000000000003",
      "version_id": "01J00000000000000000000004",
      "destination": "textures"
    }
  ],
  "entrypoints": ["project/scene.blend", "project/workflows/ui.json"],
  "dependencies": [
    {"from": "project/scene.blend", "requires": ["textures/color.png"]}
  ]
}
```

以上 ID 和文件都是合成示例，不会在任何实际实例自动创建。假定第一个清单内有 `scene.blend` 和 `workflows/ui.json`，第二个有 `color.png`；工具会还原这三个目录位置。工程本身应已使用恰当的相对引用，例如相邻 `textures` 中的图片。不修改 Blender、ComfyUI、UE 或其他二进制/JSON 工程内的路径。

`entrypoints` 与 `dependencies` 可省略；提供后全部是工作根内的精确文件路径，必须由本次固定版本的清单提供。既有目录中碰巧存在的文件不能补齐缺失依赖。依赖的 `from` 也须存在，`requires` 不能为空。它们只表达必需文件的闭合，不执行入口、运行脚本、加载插件或自动打开软件，不解析任意工程格式。图中有环并不代表文件缺失，因此不禁止互相引用。清单 `uses` 的 `reference`、`derived_from` 等溯源关系不被擅自解释为必须复制的运行时依赖；所需输入必须在工作集显式列出。

## 调用接口与顺序

```go
in, err := client.DecodeWorkset(reader)
err = client.ValidateWorkset(in, directory)
plan, err := c.PlanWorkset(ctx, in, directory)
receipt, err := c.PullWorkset(ctx, in, directory, "production")
```

- `DecodeWorkset` 限制大小，拒绝未知字段、重复键、无效 Unicode、尾随文档。`ValidateWorkset` 是本地预检，不联网、不创建目录。
- `PlanWorkset` 在本地预检后先 GET 当前 origin 的 `/api/v1/meta`，严格检查 JSON、`api_version: "v1"` 和规范 ULID `instance_id`；metadata 缺失、重复键或无效时，连精确版本请求也不继续。随后仅 GET 每项 `/api/v1/assets/{asset_id}/versions/{version_id}?view=full`，核对响应中 version 和 manifest 的资产/版本身份、与 meta **完全相同**的实例 ID、相同 `manifest_digest`，并对原始 content 按 RFC 8785 重算摘要。不同有效 ULID 也属于错误实例；多项混入不同实例在任何 grant 或写入前拒绝，不接受“格式有效即可”。所有路径、依赖、目标冲突和已有文件检查完成后，才返回完整计划。失败时返回的未完整计划只用于诊断。
- `PullWorkset` 重做计划，不直接信任保存或编辑过的计划。成功预检后才创建缺失工作根，检查全部现存目标，再为每个原始 `source_path` 请求该精确版本的 read-grant。grant 的路径、大小、SHA-256 必须逐项匹配已钉住清单。
- 下载复用现有 `Download` 的同一内部实现（`downloadRoot`）：同源授权、普通会话、Range、持久 partial 元数据、逐文件锁、完整 SHA-256 和不覆盖发布行为均保持。工作集持有一个 `os.Root` 目录句柄，所有资产通过该句柄还原，不按每项目录重新解析根。
- 每次 grant 前后以及最终完成前重新核对根身份；路径及现存目标在预检、授权后、下载后反复检查。目录被替换后报错；即使下载期间根被重命名，写入仍留在原先钉住的目录，不能借新 symlink 越根。支持的本机平台是仓库现有 Linux、macOS、Windows；不把本功能宣称为任意本机进程或挂载点的安全沙箱。

工作根及其现存父级必须是真实目录，不接受 symlink；若操作系统提供 `/var`、`/tmp` 等别名，调用方应显式选择解析后的真实目录。路径中的任意已存在 symlink、非普通目标、非目录父级均拒绝。后代路径的逃逸还由 `os.Root` 持续约束。没有外部上传、NAS 访问、源文件修改或删除操作。

## 冲突、恢复与回执

所有相对目录与文件路径遵循 [catalog 路径规则](catalog.md)：NFC、`/` 分隔、跨平台保留名/字符及长度。冻结清单里的非 NFC 路径直接拒绝，不静默改写后再请求 grant。合成目标路径也受同一长度限制；嵌套目录过深时应选择较短 destination。额外拒绝传输占用的 `.lantai-part`、`.lantai-download.json`、`.lantai-lock` 后缀。

两个 entry 不得拥有同名、大小写/Unicode 等价或父子重叠的 destination。所有目标文件在下载前一起检查，包括文件/父目录冲突、完全大小写折叠冲突、同一中间目录的不同拼写（`A/x` 与 `a/y`）及与现存名称的冲突。即使字节相同，也不把两个 entry 合并写到同一个目录；应先调整工作集。

计划会流式核对所有已有目标的大小与完整 SHA-256；相同记为 `present`，不同返回 `PATH_CONFLICT`，保留原件，且不会先写前面的资产。下载时再次校验同一目标，相同不会重复下载；不同时保留并失败。中途失败可对原输入和原目录再执行 `PullWorkset`，沿用既有 partial 与绑定元数据，重新取得当前授权；撤权或到期不会靠已有部分文件绕过服务端。完成文件和 partial 不会被自动回滚或删除。

`WorksetPlan` / `WorksetReceipt` 的字段均可导出 JSON：

| 字段 | 内容 |
|---|---|
| `schema` | `lantai.client-workset-plan/v1` 或 `lantai.client-workset-receipt/v1` |
| `origin`、`input_digest` | 当前服务 origin、完整输入的 JCS SHA-256 |
| `state` | 计划为 `planned`；拉取失败为 `partial`；仅全部文件核对通过后为 `completed` |
| `file_count`、`total_bytes` | 全部目标文件的数量与逻辑字节，重复内容的工作副本仍按次数计算 |
| `entries` | 输入的固定 ID 与 destination，响应 `instance_id`、原 `manifest_digest` 及文件列表 |
| `files[].source_path`、`path` | 精确清单路径、工作根中的最终相对路径 |
| `files[].sha256`、`size`、`state` | 清单哈希/大小和 `planned` / `present` / `verified` |
| `entrypoints`、`dependencies` | 已验证闭合的原始声明 |

函数在错误时也返回部分回执，调用方必须同时检查 `error` 和 `state`；`partial` 不能当成功。工具不自动保存回执，CLI/批次入口可使用现有 `SaveState` 保存到用户显式选择的独立路径。回执没有会话令牌、授权 URL 或软件执行记录。重新运行始终重查服务端与磁盘，不把本地回执当授权或存储真源。它也不是整批事务：中断时已验证文件保留，用户修改过的文件不被覆盖，换到空工作目录可重新获取原版本继续制作。

一次工作集上限为 1,000 个 entry、10,000 个文件、1 TiB 逻辑字节；输入及输出计划各不超过 8 MiB。超过上限明确报错，不截断或抽样。原件按现有传输流式读取；这些预算不表示目标介质已通过容量或大文件性能验收。

## 合成验证

`go test -race ./internal/client` 覆盖：固定版本 + 多资产依赖目录；只读计划不创建目录；重跑不重复传输；中断后 Range 续传和撤权拒绝；同目标、父子、NFC/大小写及中间目录冲突；manifest 与 grant 的错误身份/摘要；错误有效实例、多项混实例及无效 metadata 在 grant/写入前拒绝；缺入口/依赖；原有不同字节保留；现有 symlink 和授权/传输期间替换目录均不越界；硬预算和严格 JSON。

这些测试使用本机 loopback 合成服务与临时目录，验证客户端协议和目录行为。真实服务联合入藏、Blender/ComfyUI 相对引用重开、九类素材以及新版本提交由集成夹具验收另行执行；本模块本身不宣称已完成真实软件兼容或目标 NAS 验收。
