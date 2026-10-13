# 浏览、入藏与下载资源

目标：找到有权限的项目和精确版本，安全上传或下载文件。下面的命令在**自己的电脑终端**运行，不在 NAS 数据卷内操作。前提：已安装匹配版本的 Lantai CLI，并按[账户](accounts.md)取得私有 `session.json`。

## 1. 确认我在哪个实例和身份下

```sh
lantai meta --server https://library.example:32444
lantai whoami --server https://library.example:32444 --session-file session.json
lantai project list --server https://library.example:32444 --session-file session.json --limit 20
```

核对 meta 的 `instance_id`、whoami 的主体、project list 中项目的 `project_id` 与 key。后续 `--project` 使用 **project_id，不是项目名称**。若列表为空，先让管理员核对项目角色、会话项目范围和 `read` scope；不要据此认定资料已被删除。

## 2. 搜索并查看精确版本

```sh
lantai project show --server https://library.example:32444 --session-file session.json --name sample-library
lantai search --server https://library.example:32444 --session-file session.json --project PROJECT_ID --query 示例 --limit 20
lantai show --server https://library.example:32444 --session-file session.json --asset ASSET_ID --version VERSION_ID --view full
```

把占位 ID 换成回执值。列表有下一页游标时用返回的游标作为 `--cursor` 继续，不把第一页当全部结果。`show` 的 full 视图用于核对精确版本与清单。永久 `lantai://…` 引用可用 `--ref` 读取，客户端会核对实例 ID，不能把另一实例的 ID 混过来。

成功标志：找到预期项目、资产 ID 和版本 ID，full 视图内容符合预期。`NOT_FOUND` 也可能是权限过滤；让管理员核对身份与项目，不对陌生 ID 反复猜测。

## 文件类型与媒体预览边界

rc.3 没有业务资源预览页。独立前端的媒体与文本预览仍处于开发阶段，尚未发行；不要因为看到资源文件名，就推断自己当前的部署已经支持在线预览。

压缩包及其他不支持预览的二进制只展示类型、大小和基础元数据，并明确提示“不支持预览”。不会解压、执行或增加十六进制视图。需要文件时，仍按下节的授权下载流程在自己的电脑处理。

图片、音频和视频分别属于媒体类型，不因它们是二进制就一概禁止预览。后续是否可预览、可用格式和浏览器支持范围，以实际发布功能及验证结果为准；本手册目前不承诺已经提供这些能力。

在建范围也包含文本、代码和配置文件：例如 txt/md、py/go/html、json/yaml/toml 统一只读显示；Markdown 提供安全渲染与源码切换，代码/配置提供高亮、行号和复制，HTML 只显示源码。预览不得运行文件里的脚本或自动加载第三方资源；大文件须限量读取并明确提示截断，未知编码须安全降级。以上是开发范围与验证边界，不能据此假定当前 rc.3 页面已有相应按钮。

### 开发预览说明：尚未发行

以下只适用于参与前端开发测试的版本，依据[前端 814eef6 的功能说明](https://github.com/oujinhaoai/lantai/blob/814eef6dcf8178ce23d52e6745b1ea3d5be3e535/web/README.md)与合成浏览器验证。对应 [PR20](https://github.com/oujinhaoai/lantai/pull/20) 尚未发行；它不属于已发布的 rc.3，也不证明现有 NAS 已部署。

在该开发版本中，打开项目和资源详情，选择文件后显式请求预览；列表只显示类型图标及著录摘要，不自动加载原件或缩略图。

| 类型 | 开发版本的只读展示与限制 |
| --- | --- |
| PNG/JPEG/GIF/WebP/AVIF | 原生图片展示；原图超过 16 MiB 时只给提示及受授权约束的下载入口 |
| 音频、视频 | 原生播放器，设置 `preload=metadata` 提示、不自动播放；该提示不保证只读取元数据或限制流量。PNG、WAV、H.264 MP4 是已验证的合成基线，其余已识别后缀的具体编码能否解码取决于浏览器 |
| txt/md、代码、JSON/YAML/TOML 等配置 | 严格 UTF-8 解码；初次最多 64 KiB，手动扩展至 256 KiB，最多显示 2000 行，超限明确提示截断；长文本停止高亮以保持响应，源码仍可选择和复制 |
| Markdown | 阅读与源码切换；禁止 HTML，内嵌图片和链接只显示文字，不自动请求资源 |
| HTML/SVG | 仅显示源码，不执行或嵌入网页 |
| 压缩包、PDF/Office、未知二进制 | 基础元数据与不支持提示；不解压、不执行、不显示十六进制；伪装成文本的压缩签名、控制字节和未知编码也会降级 |

预览使用 `archive_review`，下载按钮单独申请 `raw_export`；两者仍受原服务端权限和用途规则约束，不能通过切换按钮绕过拒绝。文件/页面切换会中止旧读取，过期时移除预览，不自动续期。该开发版本没有在线编辑、转码或列表缩略图。常规 rc.3 使用者仍按下面的 CLI 流程下载。

大媒体播放前的实际传输量与取消点尚未完成验证，不承诺音视频字节上限。独立的 Range 206 检查只证明授权的分段读取接口可用，不能代替播放器流量或取消行为的实测。

## 3. 把文件下载到电脑

先在电脑创建一个空的 `downloads` 文件夹，再运行：

```sh
lantai pull --server https://library.example:32444 --session-file session.json --asset ASSET_ID --version VERSION_ID --directory downloads --purpose archive_review
```

成功标志：命令成功，目标目录出现清单声明的文件，客户端已经校验大小与 SHA-256。途中中断时保留 `.lantai-part`、`.lantai-download.json` 和锁文件，再用同一命令继续。

用途必须准确：`archive_review` 档案查看、`reference` 参考、`production` 制作、`generative_input` 生成式输入、`raw_export` 原件导出。选择合法值仍需服务端按权利与权限批准。`PATH_CONFLICT` 表示已有不同内容，保留原文件并换空目录，不自动覆盖。

## 4. 将一份合成文件入藏

前提：该项目的 contributor 或 owner 角色、`ingest` scope；希望完成初始标题等著录时还要有 `organize`。在电脑私有工作目录建立 `sample.txt`，写入任意合成文字；在同目录建 `push.yaml`：

```yaml
project_id: PROJECT_ID
slug: sample
content:
  asset_type: doc
  files:
    - path: sample.txt
      role: source
  rights:
    usage: production
    license: CC0-1.0
    redistribute_raw: true
    noai: false
    sensitivity: normal
describe:
  title: 示例文本
```

将项目 ID 换成授权项目。权利字段只适用于这份自己创建的合成文本；真实资料须按其实际许可填写，不照抄 CC0。运行：

```sh
lantai push --server https://library.example:32444 --session-file session.json --input push.yaml --state push.state.json
```

成功标志：保存回执中的 `asset_id`、`version_id`、`operation_id` 与永久引用，然后用 `show` 和 `pull` 核对。**退出码 0 仍要检查 `description_pending`**：为 true 时文件已经提交，但初始著录未完成。先保留原回执与 state，补齐所需权限或单独处理著录；不要创建第二份资产。

## 5. 中断、修改和后续工作

上传中断后保留原 `push.yaml`、原文件、原 state 和幂等键。重跑同一命令会核对输入并补缺的分片；结果不确定时先用 `lantai operation --id OPERATION_ID`（带原 server/session）查询。改过文件却复用 state 会产生 `IDEMPOTENCY_CONFLICT`，应先恢复原输入并完成对账，不盲目换键。

修改著录先用 `metadata get --asset ASSET_ID` 取得当前描述和修订，再用 `metadata set --asset ASSET_ID --input patch.json --if-match '"REVISION"' --idempotency-key KEY`，各命令均带原 server/session。patch 字段按[客户端契约](../contracts/client.md)准备；冲突时重读并由本人合并，不覆盖他人修改。

任务、Flow、检查、讨论、人审和回收站有各自的 CLI，见[协作命令](../contracts/manual-execution.md)。上传成功不等于审定或发布；敏感人审动作仍由人审核。未发布的前端浏览/操作按钮不作为本版本入口。
