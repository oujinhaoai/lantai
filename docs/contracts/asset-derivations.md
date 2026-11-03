# 固定版本比较与有界本地产物

`lantai asset derive` 和 MCP `asset_derive` 共用客户端与纯数据变换库。它们只处理调用方明确选择的固定版本和本地目录，不改原件、不提交新版本、不执行原生工程或第三方插件。实际实现为 [客户端编排](../../internal/client/derivations.go) 与 [受限图片/OBJ库](../../internal/assettools/media.go)。

```sh
lantai asset derive --input derive.json --directory verified-files --artifact preview.png --output receipt.json --server https://library.example --session-file session.json
```

MCP 参数为 workspace 内的 `input_file`、`directory`、`artifact_file`、`output_file`。完整产物和回执私有保存，工具输出摘要；`batch_report_page` 可分页读取回执输入证明、比较差异与OBJ数字结构。每次派生都重新读取核心固定清单、当前用途和read-grant，并实际核对下载字节；已存在本地缓存不能绕过撤权。发布产物前再复验当前用途授权。失败不自动更换purpose，未知/部分状态不等于通过。

## 私有输入

所有输入为 `schema: "lantai.asset-derivation-input/v1"`，字段由共用Go结构定义。按action只接受适用字段，不接受URL、宿主数据根或latest别名。

| action | 必需选择 | 用途 | 产物 |
|---|---|---|---|
| `compare` | `before`、`after` 的asset_id/version_id | `archive_review` | 文件增/删/改/不变与相关manifest字段JSON |
| `thumbnail` / `proxy` | `source:{asset_id,version_id,path}` | `archive_review` | PNG |
| `obj_structure` | 同上 | `archive_review` | 有界OBJ顶点/边/面JSON |
| `obj_wireframe` | 同上 | `archive_review` | 无外部资源的固定投影SVG |
| `workset_tar` | 原 `workset: WorksetInput` | `raw_export` | PAX TAR |

图像/线框的 `edge` 可为320、512、1024或2048；thumbnail默认320，其余默认1024，不放大输入图像。PNG/JPEG/WebP通过实际头与解码验证；每图最多16MiB、16,777,216像素、任一边16,384。拒绝APNG和动画WebP、损坏或超预算输入。PNG输出保留解码像素的透明度，使用明确记录的最近邻采样，不应用EXIF方向、不提供通用转码或AI图像处理。

```json
{
  "schema":"lantai.asset-derivation-input/v1",
  "action":"thumbnail",
  "purpose":"archive_review",
  "source":{"asset_id":"01J00000000000000000000001","version_id":"01J00000000000000000000002","path":"image.webp"},
  "edge":320
}
```

以上ID/域名均为合成示例。OBJ最多2MiB、100,000行、20,000顶点、30,000面、60,000唯一边，每面3–64个已有顶点，坐标有限且绝对值不超过1e9。负索引按当前顶点数量解析，曲线/曲面/线元素明确拒绝。mtllib仅记录有限文本及是否截断，绝不抓取外部材料或纹理；线框SVG不包含这些外部路径。

## 比较与归档

比较两个固定版本时，全部所选文件合计最多256个/64MiB。先核对预算再读取本地已有文件或取得传输授权，实际下载/逐文件核验两侧。文件路径相同但SHA、大小或role变化都记为changed；相关asset_type/version_note/uses/rights/metadata/producer按完整规范JSON比较，键顺序不制造变化。JSON产物保留固定值，MCP字段页只返回变化、大小及hash；完整值可从相应固定版本的 `resource_read_chunk` 获取。

工作集导出同样限制256文件/64MiB源字节，沿用原工作集目录冲突、固定入口/依赖、SHA与当前raw_export检查。只归档明确计划的路径，不枚举额外本地文件。TAR采用PAX保留Unicode和长相对路径；普通文件mode0600、uid/gid/mtime固定，无绝对目录、链接或宿主路径。对实际写入归档流重新计算SHA，变更、取消、损坏输入均不发布新产物。总产物预算65MiB；普通JSON恢复状态仍保持原8MiB限制。

工作副本根和产物父目录被钉住。产物必须在源/工作目录、输入和回执之外；已有不同产物、符号链接、目录替换或路径冲突均拒绝，不覆盖。相同产物可按相同输入重试，但仍取得新鲜远端授权。取消/失败保留既有完整文件和原下载partial，以原选择显式续办。

## 回执与MCP读取

`lantai.asset-derivation-receipt/v1` 记录工具 `lantai.bounded-asset-tools/v1`、action、purpose、参数、每份输入的实例/固定ID/manifest_digest、实际文件集合的规范摘要及数量/大小，以及产物实际SHA/大小/格式。PNG/OBJ/TAR只是本机派生产物；原生续做、人类见证与删源授权始终为false。`current_remote_verified:true` 仅描述此次实际检查，不是持续权限、核心审定或发布事实。

`batch_report_page` 的section：

- 回执默认rows：每份固定版本输入的证明元数据。
- 比较JSON默认rows：有界文件差异；fields：字段变化和前后摘要，不回显完整extra/备注。
- OBJ结构JSON默认rows：计数；vertices/edges：完整数字结构分段。

后续页带原 `expected_sha256` 与 `next_offset`，本地文件变化时必须替换旧页。每页最多20项并按实际模型结果预算缩小；历史页固定 `current_remote_verified:false`。二进制文件字节和传输URL不进入MCP上下文。
