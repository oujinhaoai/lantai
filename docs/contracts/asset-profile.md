# 素材字段适配与组合查找

`internal/client/asset_profile.go` 和 `asset_filter.go` 是公共 REST 的本机输入适配。它们不新增服务端协议、资产类型、权限索引或审批真源。正式权利仍放在 `content.rights`，文件角色、版本、提交和用途检查遵循[目录契约](catalog.md)、[客户端契约](client.md)与[查询契约](query.md)。

## 输入与字段

`DecodeAssetProfile` 严格读取以下 JSON；拒绝未知字段、null、未知 schema、安全字段和非法业务状态。`ApplyAssetProfile(PushInput, AssetProfile)` 返回一份可交给原 `Push` 的输入，不读源目录、不联网、不改变文件角色或 rights。

```json
{
  "schema": "lantai.asset-profile/v1",
  "characters": ["hero-a"],
  "source": "synthetic",
  "business_state": "pending_review",
  "legacy_state": "awaiting-human-review",
  "legacy_version": "v007",
  "source_paths": ["synthetic/motion/move.fbx"],
  "notes": ["synthetic-note.md"]
}
```

| 字段 | 规则和存放位置 |
|---|---|
| schema | 省略时补 `lantai.asset-profile/v1`；其他版本拒绝 |
| characters | 内容所指的角色，与 `files[].role` 无关。NFC、去首尾空白、去重、排序；每项最多54字符。投射 `character:hero-a` 标签及 `subjects` |
| source | 描述性来源名，每项最多57字符。投射 `source:synthetic`；不是许可证或许可证明 |
| business_state | `active/reference/rejected/superseded/archived/pending_review/unknown`；省略为 `unknown`。投射 `state:pending_review`。这些都是普通业务标签 |
| legacy_state | 原历史状态，最多200字符。待审可显式归入 `pending_review`；不认识的写法选择 `unknown` 并保留原值。适配器不猜测历史审批 |
| legacy_version | 原版本标签，最多49字符。投射 `legacy-version:v007`，不据此推断版本号或同一资产身份 |
| source_paths | 最多256项，每项最多2048字符，仅保留来源线索；不读文件、执行、解引用或添加进搜索投影 |
| notes | 最多64项，每项最多4000字符，保存相关笔记标识或简短说明；不读取笔记正文，不建立执行或授权入口 |

完整规范化 profile 同时保存为 `content.metadata.extra.asset_profile`（冻结版本快照）及 `describe.extra.asset_profile`（当前著录）。查询只依赖著录中的结构化标签，**不是检索全部 extra、原路径或笔记正文**。角色/来源/版本标签区分大小写，NFC后精确匹配；同义词和来源身份先由操作者明确统一。

上述双写只适用于新建资产。`PushInput.asset_id`非空的追加版本只写冻结metadata；既有核心明确禁止追加输入带slug/describe，适配器也明确拒绝而不静默删字段。追加v2不会自动改变当前可检索标签：需要改当前profile时，用独立的metadata PATCH、If-Match和稳定幂等键。v1/v2各自冻结的profile快照保留，不能把v2业务标签改动冒充v1的历史事实。

`active` 不表示可用于生产；`reference` 不赋予参考用途；`rejected` 不创建驳回记录；`archived` 不触发核心生命周期；`pending_review` 不创建任务；`approved/published` 不是允许的业务状态。正式权限、用途、审定和发布始终由原领域接口裁决。没有原许可证据时不能补成CC0或自有，也不能在 profile、tags 或 extra 中隐含放宽 rights。

既有普通 metadata、extra、title、summary、subjects 和无关标签会保留。两个 `extra` 必须是对象；已有 `asset_profile` 与本次输入不同，或保留标签命名空间出现本 profile 未声明的值时，返回冲突，不覆盖旧字段。metadata与describe内递归出现 rights/usage/license/sensitivity/defaults/redistribute_raw/noai/digital_source_type/restricted/restricted_source 均拒绝。既有数字经 `json.Number` 保留，不把大整数变成浮点近似值。

此函数是完整profile的**初始著录适配**，不是读取当前资产后合并修订的编辑接口。修改既有著录仍应先读当前description，用原 metadata PATCH、If-Match 和稳定幂等键；不能将一个旧 `describe.extra` 作为新当前修订盲目覆盖。profile字段为空不是清空当前服务器字段的指令。

`Push` 的 committed 回执仍可能含 `description_pending`。这表示字节/版本已经提交、初始标签和著录尚待补完：保留原输入/state/幂等键，按原操作续办；不能再上传一个新版本冒充恢复，也不能自动扩权。`FindAssets` 按当前已生效著录查找，所以待著录版本可能暂时不匹配标签。以精确资产/版本回执读取，不将“搜不到”解释为上传未发生。

## 资产单元、九类资料和文件角色

同一版本是一套能理解和继续使用的最终资料；分组由显式清单决定，不按后缀自动归档整工作区。跨资产依赖仍使用固定版本 `uses`，工程工作集由相应客户端还原。下表是分组规则，不是新增格式解码或工具载入承诺。

| 资料 | 建议资产单元 | 文件角色 |
|---|---|---|
| 图片、设计及参考 | 同一对象/用途的选定设计版本，预览和说明随附 | 原件 `primary` 或 `source`；预览 `preview`；说明 `doc` |
| 视频、审片与成片 | 一次明确的制作版本；选定审片/成片，配方和记录随附 | 成片 `primary`；审片 `preview`；输入 `source`；配方 `recipe`；记录 `record` |
| 音频及配乐 | 原音频、实际采用的编辑版及来源/署名证据；敏感训练语音不默认纳入 | 成品 `primary`；原件 `source`；说明/许可证据 `doc` |
| 配方与说明 | UI/API工作流、提示词、运行记录及选定输出保持对应；模型身份记录代替重复公开模型文件 | 工作流/提示词 `recipe`；run/元数据 `record`；说明 `doc` |
| Blender工程 | 工程、相对贴图/链接库、脚本和选定预览组成可复现版本 | BLEND `primary`；贴图 `texture`；脚本 `recipe`；预览 `preview` |
| 模型与贴图 | GLB，或OBJ+MTL+纹理等完整sidecar包；记录单位、骨架和来源 | 模型 `primary`；交换格式 `interchange`；MTL/纹理 `texture`；说明 `doc` |
| 动作与轨迹 | 同一动作的FBX/NPZ、骨架/帧区间记录及预览 | 动作 `primary` 或 `interchange`；轨迹 `source`；JSON `record`；预览 `preview` |
| 受限引擎原始组合 | UASSET+UEXP等完整组合；原始与提取产物保持关系和rights | 主件 `primary`；sidecar `source`；来源说明 `doc`；不在服务端自动执行/解包 |
| 原始发行包 | 发行ZIP和来源说明；原包身份与提取资产版本分清 | ZIP `source` 或 `primary`；说明 `doc`；需要还原时用受控工作集清单 |

不按文件名 `vNN` 自动叠版本，也不按相同哈希合并不同来源的资产身份。是否复用物理字节由现有服务端授权CAS决定；来源、版本和rights不会因去重而丢失。

中间渲染/深度/遮罩、工具缓存、实验与未选候选、可重新下载的公开模型通常不入本批素材库；必须有显式用途/目录规则及可重建前提。HDR/贴图等真实工程依赖不因扩展名误排；自训练/不可重获模型、唯一工程和未知资料保留为待判断，不静默抛弃。凭据、真实.env、令牌、恢复材料不能作为普通素材导入；它们按专用保护通道处理。排除入藏不等于允许删除或取消原备份责任。

## 精确组合查找

```go
result, err := c.FindAssets(ctx, client.AssetFilter{
    ProjectID: projectID, AssetType: "motion",
    Characters: []string{"hero-a"},
    BusinessState: "active", Source: "synthetic",
}, client.AssetFindOptions{})
// 必须先检查 err == nil && result.Complete && !result.Truncated。
// 结果中 asset_id/version_id 可交给现有精确 Pull。
```

`AssetFilter` JSON字段为 `project_id/asset_type/text/characters/source/business_state/legacy_version/tags`。项目、类型、text发给已有 `GET /api/v1/assets`；其余都转成标签精确AND。多个角色和多个自定义tags也取交集；空状态表示不筛状态，与写入时省略状态变 `unknown` 不同。text仍遵循服务端字面子串语义，不能解释成全文/语义/多个词AND。客户端不读取数据库、原素材或本地权限缓存。

`AssetFindResult` 返回公共QueryItem字段的 `items`，以及 `complete/truncated/pages/scanned/restarts`。scanned仅计本次收到的已授权项目数（包含被本地条件排除及重启前丢弃的项目），不是全馆或隐藏对象数量。不返回source_paths、自由extra、传输URL或隐藏候选计数。

| 行为 | 结果 |
|---|---|
| 空页且有next_cursor | 继续请求，不能误报没有结果 |
| 游标过期 | 只在已有游标时有界重启；丢弃此前积累的全部结果，从首页以当前身份重新读取 |
| 到期/撤销/拒权、取消、网络或协议异常 | 立即失败，返回零条累积结果；不自动登录、换身份、扩权或延长会话 |
| 重复/不递增资产、循环游标、跨范围/跨代次异常、缺必需字段 | 协议失败，不将畸形响应作完整结果 |
| 页/扫描/结果预算耗尽 | `complete=false,truncated=true`并返回 `ErrAssetFindIncomplete`（可取 `AssetFindIncompleteError.Reason`）；不得把部分结果作为全量批次 |
| 末页刚好达到预算 | 全部遍历完成，返回complete，不误报截断 |

默认 `page_size=100,max_pages=1000,max_items=100000,max_results=10000,max_restarts=2`；硬上限分别为100、10000、1000000、100000、8。这里采用公共HTTP的100条上限，不能沿用内部Query服务的200条上限。零选默认，负数或超上限拒绝；总请求和扫描预算包含游标重启的开销，避免失效循环无界访问。没有持久游标或权限缓存；预算不足时显式调高预算后重跑。

跨页是当前可见范围的实时遍历，不是精确时间点快照。查询过程中已经返回的字节不能被客户端收回；调用者收到失败应替换展示/缓存，不沿用旧结果。任何后续详情和拉取仍由服务端实时授权。需要精确时点/持续同步时，使用原Snapshot/Changes和范围替换契约。

## 验证

`go test ./internal/client` 覆盖九类合成分组适配、NFC、未知业务状态、错位安全字段、extra/profile冲突、字段保留及大整数、partial metadata语义，及1205条已授权资产的精确AND/空页/多页、游标过期范围替换、撤权/到期/取消、预算、畸形响应。测试仅用合成输入及loopback HTTP。它们不证明真实格式可载入、目标网关容量、NAS耐久性或真实素材迁移已通过。
