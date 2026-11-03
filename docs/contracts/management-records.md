# 管理档案与迁移批次客户端

管理档案仍是原核心管理的普通 `config` 资产，不新建台账、审批表或权限面。
共享 envelope 为 `schemas/catalog/v1/management-record.schema.json`，迁移批次为
同目录 `migration-batch.schema.json`。网页和正式客户端使用同一 kind、预算、
机密字段限制与批次枚举。递归机密字段/深度、UTF-16 文本预算、统计总数和
唯一证据键由共享规则的实现补充校验；JSON Schema 验证本身不能证明业务接受。

## 私有记录的原输入与原状态

在既有项目权限内，`management prepare/save/list/read/revise` 和对应 MCP
`management_*` 操作覆盖 `migration/workset/recovery/assistant` envelope。
每份记录最多 2 MiB；保存前冻结实例、项目、kind、payload、原资产/版本、
实际 record.json 字节数和 SHA256。新记录使用原有私有 rights 模板；修订要求
读取原固定版本并核对项目、类型及管理档案标识，不把冲突转成新资产。

以下命令的服务、会话和目录均由操作者显式选择。`--directory` 必须已存在；
使用独立的私有目录保存准备文件和原始 Push 状态。示例 spec 的 project_id
必须替换为已获准的项目：

```json
{
  "project_id": "01ARZ3NDEKTSV4RRFFQ69G5FAV",
  "kind": "assistant",
  "title": "续办记录",
  "payload": {"status": "unknown", "original_operation_id": "原操作引用"}
}
```

```sh
lantai management prepare --input spec.json --output private/prepared.json
lantai management save --input private/prepared.json --directory private/source --state private/push-state.json
lantai management list --project PROJECT --kind assistant --limit 10
lantai management read --asset ASSET --version VERSION --directory private/read
```

`prepare` 只读取元数据并保存不可覆盖的准备文件；不会上传。`save/revise`
使用原 `Push` 的 operation、上传/提交/著录键、私有 state 和真实回执。
相同准备文件允许继续；不同字节、范围、原基线或现有文件明确冲突。
超时/断线保留原输入和 state；`description_pending` 表示版本已经提交，
不能换键重建。修订 spec 另带 `asset_id/base_version_id`，重新显式准备到
新文件和新目录，再执行 `management revise`；旧准备文件及证据不覆盖。

MCP 文件操作需要显式 `--workspace`，所有路径相对于该根。使用
`management_prepare(spec_file, prepared_file)`，随后
`management_save/management_revise(prepared_file, directory, state)`。
`management_list` 和 `management_check` 无需本地工作区。列表继续原
`next_cursor`，空页有游标时也继续；投影异步更新，提交回执可先作准确读取。

`management_read` 通过原 `archive_review` 读取和传输核对固定 manifest、
实际文件大小及摘要。MCP 大记录用 `management_read_chunk`，每次最多
8192 原始字节，base64 返回；非首段必须带原 `expected_sha256`，最后核对
`total_bytes/sha256/complete`。每段重新经过实时授权和用途校验，私有本地
副本不能绕过撤权、rights 或恢复代次。整个 MCP 序列化结果仍受 64 KiB 限制，
超限明确拒绝，不截断后冒充完整。

## 批次构建、修订与核验

`management batch-create/batch-revise`、MCP
`management_batch_create/management_batch_revise` 读取私有 JSON 草稿：

```json
{
  "batch": {"schema": "lantai.migration-batch/v1"},
  "import_plan": {"schema": "lantai.asset-import-plan/v1"},
  "import_report": {"schema": "lantai.asset-import-report/v1"}
}
```

此处只示意外层结构，不是可执行完整输入。完整 batch 字段见共享 schema
及 `schemas/examples/catalog/v1/migration-batch/valid.json`。批次 ID、时间、
操作者、责任人、用途、原件保留、说明及逐项声明均须显式提供。实例和最后
操作者必须匹配当前会话；人类责任人的当前资格仍由 core check 重验。
不自动生成新的 batch ID/时间覆盖未知结果，也不代填真人原生续做见证。

可选 `import_plan` 必须保留原始 JSON 与摘要。缺少 report 时全部计划行计为
`not_attempted`；提供 report 必须与原计划、服务、行顺序和原回执一致。
所有计划行计入总数，`description_pending/failed/unknown/not_attempted`
原样保留。有固定回执的行读取准确版本，核对实例、项目、文件清单和摘要；
清单核对不是重新验证全部远端字节或证明用途已满足。没有 report 的零回执行
不会被推断为已成功。手工批次不提供 plan 时，显式统计仍须覆盖全部计划行。

```sh
lantai management batch-create --input private/draft.json --output private/batch-prepared.json
lantai management save --input private/batch-prepared.json --directory private/batch-source --state private/batch-state.json
lantai management check --asset BATCH_ASSET --version BATCH_VERSION
lantai management batch-revise --input private/next-draft.json --asset BATCH_ASSET --version BATCH_VERSION --directory private/previous --output private/next-prepared.json
```

批次草稿是私有 JSON，不经过 YAML 重排，避免改变原计划中 RawMessage 的
摘要输入。修订先读取原记录的实际字节，保留原 batch/项目/实例身份，递增
revision，钉住 `previous_ref`，重置为 `not_requested`；随后显式保存到原资产。

`management check` 与 MCP `management_check` 调用原
`POST /api/v1/management/batches/check`。它重新检查当前权限、rights、实际
字节、工作集、恢复及设备/原生证据、人类责任人和准确修订连续性。
保存的勾选、历史 check、模型结论、旧 epoch 记录均不能授予 `ready`。
完整恢复后的记录须重新发现、读取和 check；本文不授权现场恢复或部署。
