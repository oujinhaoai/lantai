# 新目标恢复与所选版本核验证据客户端

恢复控制复用 [现有安装接口](../../api/recovery-control.md)，不增加路由或放宽授权。`lantai recovery` 是独立的新目标安装认领客户端，不是普通会话命令，也不注册为 Agent MCP setup 工具；不接受普通 session 代替安装认领。每次请求保留精确 Host/Origin、HTTPS 验证、禁止重定向及原核心阶段/包摘要/检查/切换确认。

```sh
lantai recovery package-hash --package selected.lantai
lantai recovery status --server https://target.example --claim-file setup-code.json --output status.json
lantai recovery upload --server https://target.example --claim-file setup-code.json --package selected.lantai --package-sha256 SHA256 --output uploaded.json
lantai recovery restore --server https://target.example --claim-file setup-code.json --input restore.json
lantai recovery check --server https://target.example --claim-file setup-code.json --output review.json
lantai recovery complete --server https://target.example --claim-file setup-code.json --input complete.json
```

以上为通用示例。`setup-code.json` 必须是操作者已取得的新目标原私有安装文件，不自动创建、签发或扩大访问。私有包必须是普通文件、完整 SHA 与明确选择相符；流式上传后再次读取同一目标 status 核对服务器实际验证的包摘要/大小。`--ca-file` 只选择此客户端的信任，不修改系统 CA；不提供关闭 TLS 校验。`--allow-http` 只适用于显式 loopback 合成开发环境。

restore.json 为 `{package_sha256,confirm_new_target:true}`。恢复后台执行，status 返回原 phase/run；取消使用 `recovery cancel`，只请求原 worker 取消，必须继续查看原 status 确认退出。响应丢失不自动重试写请求，不换包或新建恢复 run。管理员口令、TOTP 与恢复码确认仍由本人在既有安装流程完成；客户端没有 admin、admin-confirm 或建库因素入口。

complete.json 为 `{review_id,known_changes_reviewed:true,source_offline:true,activate:true,accept_recovery_point:true}`。check 不是接受，complete 不自动填任何确认。服务器重新验证原核心检查、目标、阶段和因素；确认是操作者声明，不是客户端证明原设备隔离。完成后安装认领失效，业务须使用新管理员原登录流程。旧本机 restore/recover-admin/restore-complete 与 v1 对账语义保持；v2 保留捕获后未知，不伪写三个 reconciled=true。

## 当前实例与恢复证据

`lantai instance show` 读取原 `/api/v1/admin/instance`。现有本人管理员角色、scope 和 HumanOnly 均保持；普通 Agent 被拒绝，不因 CLI 存在而获权。

`lantai asset recovery-evidence prepare --input selection.json --directory verified-files --output prepared.json` 的私有输入为：

```json
{
  "project_id": "01J00000000000000000000001",
  "title": "所选固定版本的恢复核验",
  "run_id": "01J00000000000000000000002",
  "purpose": "archive_review",
  "workset": {
    "schema": "lantai.client-workset/v1",
    "entries": [{"asset_id":"01J00000000000000000000003","version_id":"01J00000000000000000000004","destination":"selected"}]
  }
}
```

准备时先读取真实已接受核心回执，再重新取得每个所选固定版本的当前用途授权，实际下载/核对全部 SHA 与大小，最后再读核心实例/epoch/回执。变更、缺回执、未就绪、撤权、不同字节和重复版本都拒绝；本地旧成功回执不能绕过。选择目录内原不同文件不覆盖；中断沿用原工作集副本重新核验。

输出使用网页同一 `lantai.recovery-evidence/v1`，范围恒为 `explicit_selected_versions_only`，保留原接受依据及 unknown_after_capture；`native_software_continuation_verified` 和 `source_retirement_authorized` 均为 false。核验不等于全馆对账、原生软件续做或删源授权。`lantai.browser-verification/v1` 是现有跨客户端字段名，CLI 生成仍由实际下载逐文件检查；不会虚构浏览器已运行。

`asset recovery-evidence save --input prepared.json --directory record-files --state original.state.json --output saved.json` 复用原管理记录 Push，仍需相应项目 ingest/organize 权限，系统管理员身份本身不替代项目角色。追加记录时 prepare 需同时提供原 `asset_id/base_version_id`。

`asset recovery-evidence open --input fixed-ref.json --directory record-read --output reopened.json` 实际下载 config 记录并核对封装。返回 `captured_evidence`、`historical_receipt:true`、顶层 `current_remote_verified:false`，不会把历史内部观察重标为本次通过。新鲜验收应重新 prepare；原通用管理 MCP 也能保存/读取相同管理档案。

所选版本实际下载后，证据准备逐文件重新取得同一显式用途的read-grant并核对SHA/大小，再复验核心实例、epoch和原恢复回执。元数据仍可见不表示该用途仍获准；下载后用途收紧、grant变更或epoch变化均拒绝产生新证据。

实际字节核验与保存config档案是独立动作；保存仍需当前项目写入角色。本人管理员仅有项目Viewer时可在原读取权限内核验选定版本，但不能借恢复流程写入新档案；客户端不调整角色或范围。
