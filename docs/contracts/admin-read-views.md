# 管理工作台的受权发现接口

这些接口复用 catalog、query、identity 的既有领域授权与读取，不创建权限、提交、审批或审计真源。HTTP 接线在 `internal/transport/httpapi/admin_views.go`，核心方法及测试分别在所属模块；只读接口同样要求有效会话。

## 精确标签组合查询

`GET /api/v1/assets?project_id=...&q=...&asset_type=...&tag=character%3Ahero-a&tag=source%3Astudio&limit=50&cursor=...`

- `tag` 可重复，按精确 AND；与现有客户端画像标签命名一致，NFC、去首尾空白、大小写敏感，最多 64 个输入标签、每个至多 64 个 Unicode 字符，拒绝空值、无效 Unicode 和控制字符。顺序和重复不会改变查询范围。
- 响应仍为原 QueryPage。筛选在完整授权分页范围内生效，不是在浏览器当前一页上过滤。每页仍最多扫描 1024 个候选；空 `items` 且有 `next_cursor` 时须继续，不能当成无结果。
- 原加密游标同时绑定规范化后的标签集合；改变标签、主体、会话、索引代次时返回 CURSOR_EXPIRED，不能拼接旧范围结果。标签顺序变化或重复不使相同范围失效。
- 返回的是可重建的当前目录投影，可能滞后于刚提交的著录。它不检索任意 extra、源路径或笔记正文，也不把业务标签变为正式审定、发布或许可事实。详情和操作仍使用原当前权限和用途复验。

## 历史版本发现

`GET /api/v1/assets/{asset_id}/versions?limit=50&cursor=...`

返回 `{ "items": [VersionView], "next_cursor": "opaque optional" }`，每项沿用原精确版本摘要：operation_id、project_id、asset_id、version_id、version_number、manifest_digest、committed_at、ref、uri。没有全量/隐藏计数，不返回冻结内容或当前著录。

- 按权威台账的版本号升序、键集分页；只枚举已 committed 版本。limit 为 1–100（HTTP 默认 50）。版本详情继续走原 `/versions/{version_id}?view=full`。
- 整个响应持既有 security_guard，复核当前项目读取授权；每项复用精确读取的当前 rights、用途及生命周期检查。最新版本不可读时，不据此隐藏其他仍可读的历史版本，也不返回不可读版本 ID。
- 每页最多扫描 1024 个台账候选，允许空页及不含隐藏位置的加密 continuation。客户端遍历到 `next_cursor` 缺省/空后才完成；恰好落在扫描边界时可有最后一页为空。
- 游标绑定资产、主体、会话、auth_epoch、recovery_epoch；使用进程随机密钥加密，重启后旧游标失效。游标过长、损坏、跨资产或跨会话均返回 CURSOR_EXPIRED。新页面重新核对授权，不因旧游标而延续权限。
- 此列表不建立时点快照，不把新版本并发提交改成旧链接漂移；列表版本 ID 可交给固定版本读取。既有冻结文件损坏仍由精确详情/文件校验报错，目录摘要不代替原件完整性验证。

## 实例管理员的主体与凭据发现

- `GET /api/v1/identity/principals?limit=50&cursor=<last-principal-id>` 返回 `{items: PrincipalView[], next_cursor?}`。
- `GET /api/v1/identity/principals/{principal_id}/credentials?limit=50&cursor=<last-credential-id>` 返回 `{items: CredentialInfo[], next_cursor?}`。

两者都由 identity 的原 `ActReadPrincipals` 授权，必须满足当前人类本人、普通非委托会话、系统管理员角色、admin scope；Agent、恢复/维护控制身份、普通成员、缺 admin scope 或已撤权会话不获权。域服务以 BeginRead 在既有 security_guard 内完成有界查询，撤权返回后的新请求不得继续读取。

按公开管理对象的 ULID 升序分页，每页 1–100，响应不返回总数。游标只是管理员已可见的最后一个对象 ID，不是权限凭据；每页重新授权。凭据范围限定指定主体，不联查其他领域数据库。

CredentialInfo 只含 credential_id、principal_id、prefix（非认证用的显示标签）、scopes、projects、label、created_at、expires_at，以及可选 revoked_at。永远不读取/返回 secret_hash、原始 token、口令或 TOTP。签发/吊销继续走原挑战、人类授权、幂等命令；本接口不会生成或恢复凭据秘密。

## 协作发现与审计

工作台继续复用既有 `/resync?project_id=...`、`/inbox?project_id=...` 和 `/events?project_id=...&after=...&limit=...`。CoreCollaborationObjects 已支持 project/asset/version/task/review/comment/trash，逐项读取权威当前态并过滤；空页仍须按 continuation 推进。

事件视图输出当前受权对象与 last_seq/replace_required，不暴露任意宿主日志或原始事件 payload。replace_required 要求替换可见范围；不能把已经撤权的旧对象继续留在浏览器缓存。

## 验证

`internal/query/exact_tags_test.go` 验证精确 AND、NFC、大小写、重复/排序等价、游标范围、跨空页、隐藏项及撤权。`internal/catalog/version_history_test.go` 验证历史读取、最新受限、当前用途变化、游标绑定和重启失效。`internal/identity/management_read_test.go` 使用真实身份/人类授权和凭据记录验证管理员分页、无秘密返回、撤权、Agent/普通 scope/委托拒绝。

`tests/integration/admin_read_views_test.go` 在真实实例、身份、台账、投影与 merged/split 两种内部 HTTP 接线上验证历史分页、精确组合、管理拒权、项目撤权和会话吊销。内部 loopback HTTP 测试不冒充外部严格 TLS/浏览器验收，也不接触 NAS 或真实素材。
