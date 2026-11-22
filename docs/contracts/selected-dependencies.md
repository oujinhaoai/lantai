# 选定工程文本依赖分析

`lantai asset dependencies --input SELECTION.json --directory SOURCE --output REPORT`
离线读取明确选中的文件，CLI/MCP 共用 `internal/client` 解析库。选择为：

```json
{"paths":["scene/mesh.obj","scene/body.mtl","workflow.json"],"comfy_input_root":"inputs"}
```

paths 须为所选根内的规范 NFC 相对路径，不能重复、穿越、链接或跨平台大小写冲突。
只读取显式文件，不扫描/打开引用目标，不请求 URL，不加载模型、脚本或原生应用。
最多128文件、单个1MiB、总计4MiB；OBJ/MTL 最多10000行、10000依赖、1000诊断，
ComfyUI 最多4096节点与64层 JSON，拒绝重复键、损坏 UTF-8 和 NUL。

OBJ 识别 mtllib，MTL 识别原有贴图语句和明确支持的参数。未知语句/选项保留诊断，
不猜测文件名。ComfyUI 只解析 API/UI 图的 LoadImage，并明确区分 graph 所在目录
与操作者提供的 input 根；省略根、目录注记、模型及自定义节点均保持未解决。

结果沿用网页 `lantai.selected-dependency-analysis/v1`：逐文件 SHA256、格式、引用、
诊断、complete，另有显式依赖候选与 missing_selected_files。complete 仅表示选中文本
的语句处于已支持范围，不能解释为整个工程可运行，缺失依赖也不会自动下载或入藏。
只有操作者明确选择并映射准确固定版本后，才能加入工作集草稿。
网页和无页面工具用同一合成字节夹具校验摘要、依赖与未解决语义；诊断描述不是稳定错误码。

MCP 在配置 `--workspace` 时提供只读 `dependency_analyze`：`input_file` 为工作区内
私有 JSON 选择，`directory` 是明确的相对来源根。工具不写文件；完整 JSON 分段返回，
每段默认4096、最大8192字节，含 base64 data、sha256、total_bytes、offset、complete。
非零 offset 必须携带原 expected_sha256，每次重读实际选中字节；变化后丢弃旧段重来。
`analysis_complete` 与传输 complete 分开；重组后核对完整长度/SHA256 再读取分析结果。
CLI 的完整报告原子保存于私有输出，输出不得覆盖选择或写入源目录。

这是本机明确文件分析，不依赖远端资源授权。其结果不授予读取未选资料的权限；若随后
读馆内固定版本，仍按原项目、用途和当前 rights 重新授权。没有解析或原生运行证明
的资料保持未解决，不因工具退出0或文件名后缀看似正确而升级为验收通过。

## ComfyUI 固定工作集预检（本地 CLI）

`lantai asset comfy-preflight --input REQUEST.json --directory SOURCE [--output REPORT]`
只打开在 SOURCE 中明确选择的一个不超过 1 MiB 的 JSON 图，不打开其中引用的文件，
不查询服务器，也不扫描任何模型目录。REQUEST 包含相对于 SOURCE 的 `workflow`、
显式工作集相对根 `comfy_input_root`、可选分类根 `model_roots`，以及 `workset` 内嵌
此前由 `asset workset pull` 生成的完整回执对象。例如用已有回执组装：

```sh
jq -n --slurpfile receipt completed-receipt.json '{
  workflow: "scene/workflow.json",
  comfy_input_root: "input",
  model_roots: {checkpoints: "model/checkpoints", loras: "model/loras", vae: "model/vae"},
  workset: $receipt[0]
}' > request.json
lantai asset comfy-preflight --input request.json --directory SOURCE
```

API 图识别 `LoadImage.image`、`CheckpointLoaderSimple.ckpt_name`、
`LoraLoader.lora_name` 和 `VAELoader.vae_name` 的明确字面值；UI 图仅识别未连接的
`LoadImage` 图片控件。分类根不自动推断，`.` 表示显式工作集根。模型只接受常见权重文件后缀；运行时别名不猜测。链接输入、无法
解释的目录注记、自定义节点、UI 模型控件及路径拼写或层级冲突均报告 `manual`。
只有指定根下的精确路径才能匹配；不会按 basename 猜测、解码 URL 或搜索备用根。
节点序号是确定的解析顺序（API 图按键排序，UI 图按数组顺序），不是 ComfyUI 节点 ID。

报告 `scope` 为 `fixed-workset-receipt-only`。 `present` 表示回执声明曾核验，
不是当前本机文件存在、已获授权或可被 ComfyUI 加载。 `missing` 仅表示回执中无对应
精确条目，不要求文件不存在于磁盘； `manual` 需要操作者解释。如果图也在回执所列
路径里，预检要求其大小和 SHA-256 一致。报告返回固定匹配项的版本、路径、大小和摘要，
不使用回执的 origin、不把历史回执当签名凭据、不执行该图或改写工作集。现有 CLI/MCP
选定文本分析报告和网页语义保持不变。

报告中的 `draft_entries` 和 `draft_dependencies` 是供人工审阅的工作集片段，分别沿用
工作集 `entries` 和 `dependencies` 元素结构；只列回执精确匹配、去重后的候选项。
同一文件被多个节点引用时，`findings` 仍逐节点保留。草稿不包含 `missing` 或 `manual`
项，不是完整工作集；若图本身不在回执中，采纳依赖关系前也需先固定图的来源。
