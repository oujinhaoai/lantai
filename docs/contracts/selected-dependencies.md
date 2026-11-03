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
