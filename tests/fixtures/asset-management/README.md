# 统一素材管理合成夹具

本目录只提交生成器和测试，不提交媒体二进制或运行态。生成器版本为 `1.0.1`；每次输出记录生成器、原生脚本 SHA-256 及工具版本。只使用 Python 标准库和本机已安装的 FFmpeg/ffprobe，可选使用已安装的 Blender，不安装、下载或读取已有素材。

## 生成与验证

输出必须是全新或空目录；不要指向真实素材目录。生成器拒绝覆盖已有内容。

```sh
python3 tests/fixtures/asset-management/generate.py generate \
  --output /tmp/lantai-synthetic-assets \
  --blender /path/to/installed/blender \
  --large-image
python3 tests/fixtures/asset-management/generate.py verify \
  --root /tmp/lantai-synthetic-assets \
  --blender /path/to/installed/blender --native
python3 -m unittest discover -s tests/fixtures/asset-management -v
go test ./tests/fixtures/asset-management -count=1 -v
# 显式启用真实Blender生成、异目录重开与继续制作回归（默认跳过）：
LANTAI_TEST_BLENDER=/path/to/installed/blender \
  python3 -m unittest discover -s tests/fixtures/asset-management -v
```

不用 Blender 时省略 `--blender` 和 `--native`。媒体、模型、轨迹、配方、opaque组合和ZIP仍可生成；BLEND/FBX条目记 `partial`，缺少入口明确列于 `missing_entrypoints`，严格检查不得当作原生格式通过。Blender 启动失败也会保留工具日志并继续生成其他类型，不用占位字节冒充BLEND/FBX。显式 `verify --native` 没有通过时输出 `blocked` 报告并返回非零退出码。

`verify` 对所有原件重新读出 size/SHA-256，核对文件角色、目录清单、相对依赖、两版差异、配方UI/API对应和已知轨迹数组；FFprobe检查有效媒体流，H.264样本应为160×96、10fps、1秒。它拒绝同大小异内容、缺文件、额外文件、路径越界和软链接。不表示Lantai入藏/回拉或浏览器播放已经通过。

对真实Lantai回拉件保留 `manifest.json` 和 `assets/` 原相对结构，再运行 `verify --assets-only --native`。该模式仍校验全部资产原件和原生工程，只将未入藏的 `negatives/` 明记为未运行；不要把生成目录的负例复制进去后声称其也经过了服务回拉。它仅额外认可与清单原件配对、零字节且非软链接的 `.lantai-lock` 客户端控制文件，并在每条结果的 `pull_control_files` 中单列；任何其他额外文件仍拒绝。异目录原生工作集只复制清单原件，保留原回拉目录和锁文件，不将本地控制文件误作下一版素材。

可选原生验证把生成目录复制到新的临时目录，暂时隐藏**仅该生成目录**下的 `assets/`，使其旧绝对路径不存在；在新目录打开相对贴图和链接库，CPU低分辨率渲染，修改并另存/重开场景；同时导入GLB、OBJ材质、HDR、FBX并用NumPy核对轨迹。验证结束恢复原合成目录，继续制作文件不写入原资产清单；完整续做工作集（含链接库和贴图）另存 `validation-artifacts/`，逐文件摘要见报告 `continued_workspaces`，可供下一版真实入藏测试。调用使用 `--factory-startup`，不读用户已有工程或startup配置。若无法启动Blender，报告保留 `blocked`。

## 输出约定（仅测试数据，不是新的公共API）

```text
manifest.json              # lantai.synthetic-fixtures/v1
validation.json            # verify生成的结果与限制
assets/<asset_key>/v1/...   # 每项root下可直接作为入藏目录
assets/<asset_key>/v2/...   # 相同路径、变化内容，不覆盖v1
negatives/...              # 不纳入正常资产，测试驱动显式选用
logs/...                   # 工具版本、探测、生成与原生验证输出
validation-artifacts/...   # 原生验证通过后保留的续做工作集及渲染图
```

`manifest.assets[]` 每条含 `coverage_id`（T01–T09）、`asset_key`、已有核心枚举 `asset_type`、`version_label`、相对 `root`、`files[]`（path/role/size/sha256）、`entrypoints`、`dependencies[]`（from/to/kind）、`expected_metadata`、`rights`、`status` 和 `limitations`。Lantai集成驱动读取 `root + files[].path`，使用自己的项目、资产、版本ID及正式提交协议；不要把本夹具清单当成 `lantai.manifest/v1` 直接提交。v2必须提交到对应v1的同一资产，回拉到两个空目录后分别校验；不能只用清单存在证明闭环。

| 覆盖 | asset_key / asset_type | 原件和依赖 | 验证边界 |
|---|---|---|---|
| T01 | image / image | PNG、JPEG、SVG源码、预览、许可 | PNG/JPEG有效；SVG只作源码，禁止执行 |
| T02 | video / video | 有帧序数字的H.264 MP4、MOV和合成正弦音 | FFprobe流/时长/帧率；浏览器播放另验 |
| T03 | audio / audio | 无人声WAV/MP3、署名/许可证据 | FFprobe；所有声音为程序生成 |
| T04 | recipe / config | ComfyUI UI/API JSON、prompt/run、PY、MD、TXT、TOML/YML、patch、输入PNG | 原生内置LoadImage→ImageScale→SaveImage；图结构和身份一致；`run.execution_status=not_run`，没有运行隔离ComfyUI则不声称执行复跑 |
| T05 | scene / scene | Blender场景、外部相对贴图、链接库、继续制作脚本 | 必须由实际Blender生成并从另一目录打开/续做才算通过 |
| T06 | model / model | glTF2 GLB、OBJ+MTL+PNG、2×2 HDR | 标准合法生成；原生导入另记，GLB无外部依赖、OBJ有贴图依赖 |
| T07 | motion / motion | Blender导出FBX骨架动画、NPZ、JSON、预览 | Root骨、1–12帧、24fps；NPZ为标准NumPy v1 float32数组；记录Blender导入`anim_offset=0`并核对末帧位移；FBX原生生成/导入另记 |
| T08 | restricted-opaque / model | uasset+uexp、uemodel/ueanim/strands及来源记录 | **不是有效UE引擎文件**；只测组合路径/字节和限制，不声称解析或提取兼容 |
| T09 | package / model | ZIP原包、成员清单、模型与相对贴图 | 确定性ZIP/CRC/安全成员名；不自动解包 |

所有样本来自新生成的几何、像素、数字、纯音或说明文本，没有真实人物、用户素材、游戏解包、模型权重或有效凭据。常规样本使用CC0-1.0测试声明；T08的`LicenseRef-Synthetic-Reference-Only`、restricted/noai仅用于拒权测试，不用于推断任何真实作品许可。`expected_metadata.characters`、`source`、`business_state`供客户端资产profile映射，`review_fact=not_reviewed`；v2的`pending_review`不等于人审事实。

## 负例和未覆盖项

- `corrupt.png`、`corrupt.mp4`：损坏字节，不应伪报可预览。
- `unsupported-browser-codec.mkv`：有效FFV1容器，用于明确下载降级。
- `over-preview-limit.png`：仅 `--large-image` 生成，2500×2500、无压缩、超过16MiB的有效PNG，不纳入普通入藏量。
- `missing-dependency.json`：在副本中去掉模型贴图，验证缺依赖；不得删原夹具。
- `motion-metadata-mismatch.json`：30fps、100–200帧的错误记录，与已知动作不匹配。
- `unsafe-path.zip`：含 `../outside.txt`，验证安全预检拒绝，**不要解包**。

本目录不实现权限、审定、批次编排、备份或迁移引擎，不提供1GiB/故障注入/容量结论。真实core入藏/回拉、浏览器预览、权限和恢复仍由相应集成验收负责。二进制生成结果不保证跨工具版本逐字节稳定；同一次运行的清单摘要和已固定工具版本才是验收依据。

## 可选：既有ComfyUI的无模型复跑

有已安装且依赖完整的ComfyUI时，可以显式提供其原有Python环境与源码，验证原始或Lantai回拉的合成配方。此命令不会自动安装依赖；没有环境就记未验证。

```sh
/path/to/installed/comfy/python -I \
  tests/fixtures/asset-management/comfy_native.py \
  --comfy-source /path/to/installed/ComfyUI \
  --fixture-root /tmp/lantai-synthetic-assets \
  --output /tmp/lantai-comfy-evidence
```

输出必须是新/空目录，且与安装目录、原始夹具分开。该工具使用实际 `execution.validate_prompt`、`PromptExecutor` 和三个内置节点，以内存事件接收器提供官方 `ExecutionServer` 协议；不启动HTTP服务或桌面会话。CPU运行，模型目录为空，custom nodes/API nodes禁用；input/output/user/temp/cache和数据库位置均指向新的证据目录。不执行安装、下载或修改全局配置。附加Python审计检查会拒绝网络监听/连接、子进程、模型权重及无关素材读取和证据目录之外的写入；它不是安全沙箱。

`validation.json` 记录实际ComfyUI/Python/Torch版本、校验器和节点源码摘要、输入清单摘要、正式执行状态、输出图片摘要/尺寸和工作流元数据一致性。两版输出应分别为64×64、128×64，符合已知最近邻像素结果。原清单的 `run.json` 保持原字节，实际运行记录写在证据目录，避免改变已入藏版本。

这只证明该无模型工作流经已记录版本的实际校验器/执行器复跑，不代表桌面前端交互、HTTP服务、任意第三方节点、模型扩散生成或所有ComfyUI版本兼容。环境不符时保留 `blocked` 和异常，不下载安装补齐。
