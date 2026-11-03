# 批量条件著录计划与回执

`asset metadata plan/apply` 与 MCP `metadata_batch_plan/apply` 是新增客户端效率能力，
没有新增批量服务端端点。每个资产仍经原条件 `PATCH /assets/{asset_id}/metadata`，
由当前角色、scope、修订和核心校验接受；不是整批事务，不改变版本、审定或 rights。

选择是私有 JSON，明确列出1–1000个不重复的资产与拟修改字段，总计不超过4MiB：

```json
{"schema":"lantai.metadata-batch-input/v1","items":[{"asset_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","patch":{"title":"Selected title"}}]}
```

plan 只读当前完整著录与 quoted ETag，冻结顺序、项目、各自 revision、原 patch，
绑定 origin、实例、主体和规范 JSON 摘要。它不会把未指定的 extra 往返写回；使用
profile 时由核心只替换 extra.asset_profile，其他未知值保留。显式 extra 仍是原来
的整个字段替换语义。sensitivity/defaults 仍须专用安全命令，普通批量入口不放宽。
类型/字段预检不代替核心对每项当前业务条件的最终校验。

```sh
lantai asset metadata plan --input SELECTION.json --output PLAN.json
lantai asset metadata apply --input PLAN.json --state ORIGINAL_STATE.json --output REPORT.json
```

连接参数沿用 server/session-file/ca-file。计划需先审阅。apply 在第一个 PATCH 前把
所有行的独立原 key 持久化于私有、锁定、固定根状态文件；未知结果保留原 key 和原
If-Match，不自动刷新 revision 或生成替代意图。每行独立执行并保存结果，冲突不删行，
一个失败不回滚或重新修改成功行。断线后重复 apply 以同键取回核心原接受回执，再对
每个已接受项重新 GET 完整当前著录、核对项目/revision/规范字节摘要。重放不会产生
第二次实际修订，也不信任调用方自改的历史接受摘要。

结果逐项保留 not_attempted、unknown、failed、conflict、applied、verified 或
verification_failed。已接受后的权限撤销、后续写者修改、文件/回执损坏都不能继续
报告当前验证成功；原接受事实和原状态保留。只有本次所有项确实完成重新读取核对时
complete/current_remote_verified 才为 true。来自原入藏的 description_pending 仍是
原提交回执的部分成功事实，本工具不创建第二版本，也不把历史初始著录或人审升级。

MCP 使用工作区内私有 `input_file`、`output_file`；apply 另须原 `state_file`。
计划排他保存，相同字节可恢复，不覆盖已有不同计划；部分执行也保存受控回执，
上下文只给摘要、计数、文件名/摘要/大小。输出不能覆盖输入、原状态或使用目录别名。

`metadata_batch_report_page` 每页最多20条，以 offset/expected_sha256 绑定原私有
计划或报告；文件变化须替换旧页。计划只返回资产/项目/revision及patch字段名/摘要，
不回传拟改值、未知extra或凭据。报告读取显式标记 historical_local_observation，
顶层 current_remote_verified=false；原验证字段仅在 recorded 中表示历史。读取回执
不构成当前授权或重新验证。丢失原状态、不同输入或409/412均需保留原证据并确认意图，
不得自行换键或以最新 revision 覆盖其他写者。
