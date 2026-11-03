# Agent 协作、生命周期与里程碑

本适配复用 T01/T03/T04/T05/T09 原所有者，不新建任务、审定、回收或启用状态。
CLI 新增 `milestone list/set/progress`；原协作 CLI 保持。MCP 新增：

| 入口 | 原领域行为 |
|---|---|
| `resync_read` | 精确项目范围重同步，固定 replay_after，完成后替换旧范围再重放事件 |
| `inbox_mark_read` | 当前本人收件箱的已观察 through 水位；超出已消费事件明确拒绝 |
| `review_submit/review_target_read` | 提交并读取冻结审定目标，保持原 key、固定版本、流程和证据 |
| `review_publish` | 已生效原审定、当前 rights 和发布修订均满足后才可发布 |
| `trash_preview/own/restore/read` | 普通本人回收、准确 entry 读取和恢复；原角色、配额、保护及宽限规则保持 |
| `milestone_set/list/progress` | T01 owner 条件配置、当前 T05 任务进度 |
| `extension_commands` | 当前调用者实际获准的命令发现；执行仍核对精确包和启用 |
| `extension_packages/extension_enablements` | 原治理诊断读取；普通 Agent 仍可能因 HumanOnly 被拒绝 |
| `collaboration_read_chunk` | 固定 allowlist 的准确对象或快照分段；无任意 URL 或可执行输入 |

普通工具不提供清除、Hold、人审批准、人类因素、权限签发、安装认领或任意
shell。新增入口不进入原扩展 contribution alias 白名单。模型成功、QA、
标签或旧审定记录都不能创建当前批准。普通 Agent 可在真实人审已生效后，
使用自己的既有权限继续发布；不能取得或模拟人的会话。

## 条件写入和续办

`review_submit/review_publish/trash_own/trash_restore/milestone_set` 都保留调用者
保存的 `idempotency_key` 和原 `request`；冲突时读取当前对象，显式决定新意图，
不自动换键。里程碑请求带 `expected_revision` 和原 `milestone_id`；没有 ID
的请求表示新建，不能把它误当成对旧配置的条件修订。发布使用当前
`expected_publication_revision`，实际有效 `review_id` 来自原领域事实。

```sh
lantai milestone list --project PROJECT
lantai milestone set --project PROJECT --input original-request.json --idempotency-key ORIGINAL_KEY
lantai milestone progress --project PROJECT --id MILESTONE
```

Flow 派发仍是显式的一轮处理：内部命令保留稳定 operation，一轮
`flow_dispatch` 自身不被标成幂等回执。未知响应后先读取原 Flow 命令和作业，
再决定是否发起下一轮。取消或进程停止不明时继续对账，旧 fence 不恢复执行权。

## 完整重同步和有界列表

`resync_read(project_id, cursor, limit)` 保留原服务的 `replace`、
`replay_after/next_cursor`。即使页面为空，只要有 next_cursor 就继续。
收齐所有页后替换原项目范围，再从固定水位读取事件；`CURSOR_EXPIRED` 或
范围变化时丢弃此前快照。`replace_required` 不能通过追加旧页面消除。

里程碑和扩展集合的默认 REST 响应继续为原 `{items: [...]}`。新增显式读取：

- `pagination=explicit&limit=10&offset=0` 返回 `sha256/offset/total_items/complete`
  及必要的 `next_offset`。每页最多 20 条，序列化页预算 16 KiB；实际页可能
  小于请求数，必须使用返回的 continuation。后续页带原 `expected_sha256`。
- 单项超过页预算时明确拒绝；使用 `pagination=chunk&max_bytes=8192&offset=0`
  读取完整快照 JSON 的 base64 字节段。后续段带原摘要，核对
  `sha256/total_bytes/complete` 后解析。整个集合超过客户端 8 MiB 的单响应
  上限时仍可分段，不必先下载完整列表。
- 每次由原所有者重新读取并授权；改变的快照返回 `PRECONDITION_FAILED`，
  撤权按原存在性策略拒绝。分页/摘要不创建访问授权或持久游标账本。

```sh
lantai milestone list --project PROJECT --pagination explicit --limit 10
lantai milestone list --project PROJECT --pagination explicit --offset NEXT --expected-sha256 ORIGINAL_SHA
lantai plugin commands --pagination chunk --max-bytes 8192
```

MCP 的 `milestone_list/extension_*` 默认最多 10 条，后续带 `offset` 和
`expected_sha256`。它识别 `/meta` 的 `bounded_collaboration_snapshots`，直接
请求服务端分页；`collaboration_read_chunk` 对集合直接请求服务端字节段。
旧服务缺少该能力时，适配器可在原单响应预算内对授权快照分页；更大集合需要
升级服务，不能以截断内容冒充完整。准确 review target、trash entry 和里程碑
进度也可用该分段工具读取，非首段必须带原摘要；每段复验当前权限。

MCP 核对完整 text + structured 序列化结果的 64 KiB 预算。过大的完整对象
明确拒绝并使用分段读取；不会返回截断的业务对象。普通恢复后应重读资源、
当前状态与实际文件大小/SHA256；历史回执不能替代当前用途和权限核验。
