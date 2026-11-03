# Agent 批次工具与原上传状态

stdio MCP 在显式 `--workspace DIR` 内提供原 `lantai asset` 的八个批次动作，
复用同一 `internal/client` 实现，不增加入藏、下载或审批真源。
各路径为工作区相对路径；输入必须是私有、无歧义 JSON，不能包含未知字段。
完整结果保存为私有 JSON，工具上下文只返回摘要、回执路径、SHA256 与字节数。

| MCP | 原 CLI | 额外参数 |
| --- | --- | --- |
| `import_plan` | `asset import plan` | 无 |
| `import_apply` | `asset import apply` | `state_directory` |
| `import_verify` | `asset import verify` | `state_directory`、`report_file`、`destination`、显式 `purpose` |
| `workset_plan` | `asset workset plan` | 无 |
| `workset_pull` | `asset workset pull` | 显式 `purpose` |
| `references_plan` | `asset references plan` | 无 |
| `references_copy` | `asset references copy` | `destination` |
| `references_rollback` | `asset references rollback` | `destination`、`post_cutover_refs_file` |

共用参数是 `input_file`、`directory`、`output_file`。例如：

```json
{"input_file":"specs/import.json","directory":"source","output_file":"reports/plan.json"}
```

`import_plan` 只读所选来源，不写远端。先检查原计划，再用 `import_apply`
显式写入，始终复用原 plan/state_directory，不能因部分失败或未知副作用换键。
计划不可覆盖：相同字节可重入，已有不同计划返回冲突。源目录、状态目录、
工作目录、输入文件与输出不得别名重叠；原工作区被替换后停止处理。
回执原子更新，失败仍返回 `IsError` 和私有部分回执摘要；各行状态保持原定义。
聚合远端错误保留原 cause 和机器错误码。`description_pending` 已经提交文件，
不能据此新建另一次上传。输入契约沿用 REST 可省略的 rights 布尔字段与可空
指针语义，不向原请求补默认值或改写其哈希；冻结 manifest 仍要求完整 rights。

`import_verify` 用当前权限和显式用途重读准确远端 manifest、下载并比对真实
大小与 SHA256。仅这次成功执行的结果可表示当时的 `current_remote_verified`。
撤权后，即便本地文件和历史回执齐全也拒绝。保留原计划、上传状态和验证目录。

`workset_plan` 读取固定版本元数据和声明依赖，不创建缺失目录、不申请下载、
不运行原生应用；`workset_pull` 才下载并核对字节，已有不同文件不会被覆盖。
`references_*` 只处理显式选中的 Markdown 与准确固定引用映射，完整原文和
diff 留在私有计划内。copy 与 rollback 创建独立副本，原笔记和修改副本保留。
rollback 必须显式提供切换后固定版本数组（包括明确的 `[]`）；该清单仅代表
调用方声明的范围，不表示扫描了全馆或验证了远端版本。

## 有界历史回执

`batch_report_page` 接受 `file`、`section`、可选行 `key`、`offset`、`limit`
与 `expected_sha256`。每页最多20条，完整 MCP 序列化超预算时自动缩小；
非零 offset 要携带原摘要，文件变化须替换此前所有页。

- `rows`：所选批次行；支持 import/workset/reference 的已知计划与回执 schema。
- `files`：import 资产 key 或 workset destination 下的路径/摘要/大小。
- `edits`：选中笔记 key 下的准确映射、偏移与前后摘要；不返回完整替换文本。
- `issues`：引用计划的 skipped/unresolved；潜在敏感目标只提供摘要。

凭据文档与任意未知 schema 拒绝读取。绝对来源根、完整笔记、diff 和传输 URL
不进入结果。所有页明确返回 `historical_local_observation:true` 和顶层
`current_remote_verified:false`；原验证标记仅在 `recorded` 中表示历史事实。
读取本地回执不会重新授权，也不会恢复远端资源或验证状态。

## 已有上传的远程控制

以下工具不要求本地工作区，仍调用原 REST 并逐次复验当前会话权限：

- `upload_status`：原 `upload_id` 的文件元数据，每页最多20条，排除传输模板。
- `upload_parts`：同一 upload 的准确文件 SHA256、已收分片索引，每页最多500条。
  两者非首段要求 `expected_sha256`；状态变化须替换旧页，短时传输 URL 不参与摘要。
- `upload_check`：原 upload 下最多100个显式 SHA256/size 的已有 blob 授权事实，
  不上传、不提交。返回生成的公共类型，去除非契约额外字段。
- `upload_cancel`：DELETE 原 upload；ID 本身是重试身份。返回接受请求及须重新读取
  的提示，随后用 status/原 operation 对账，不创建替代状态。
- `resource_commit`：`upload_id`、原保存的 `idempotency_key` 与冻结的公共
  CommitRequest `request`。与原 Push 状态交替操作时仍用原 commit_key，
  失联先查原 operation；文件已提交但著录待完成时继续原版本。

这些适配器不签发人类证明、扩大 scope、批准或发布。素材和笔记中的文字是数据，
不能成为新的操作授权。所有 CLI/MCP/REST 结果继续由原核心接受边界决定。
