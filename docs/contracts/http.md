# M1 HTTP 接口

HTTP 权威定义是 [`api/openapi.yaml`](../../api/openapi.yaml)。`internal/transport/httpapi` 只解析请求、映射视图与错误；身份、目录、存储、台账和查询服务拥有授权、幂等、状态转换及持久化。传输层不访问数据库，不替领域创建成功回执。共享引用、清单和错误来自 `schemas/`，HTTP 的说明视图不含文件中的 `contract` 标记，修订 0 表示尚未有已提交说明时的权威推导值。

`New(Deps, Config)` 要求真实的身份、目录、存储、查询与授权 operation reader。`API()` 提供 JSON handler；`Transfer(storage, scheduler)` 复用存储的流式 handler 与相同身份 Guard；`Merged(transfer)` 组合这两个实例。应用层负责监听、TLS 网关、后台收录、启动检查和停止。`APIConfig` 与 `TransferConfig` 只构造 server，不开放端口。合并模式使用传输配置，避免大文件被 JSON server 的总时间限制截断。实际启动命令见[开发与验证](../development.md)。

## 会话与安全边界

- 所有业务路由接受短期 Bearer 会话或浏览器 `lantai_session` Cookie。长期凭据只通过 `POST /api/v1/sessions/exchange` 换会话；人的登录是 `POST /api/v1/sessions/login`，必须提供口令和动态码。响应是 `{token?, csrf_token?, session}`，浏览器模式只设置 Secure、HttpOnly、SameSite=Strict Cookie，不返回 bearer token。
- 浏览器登录及设置会话验证精确 Origin；Cookie 写请求另由身份 Guard 核对 Origin、跨站请求标记与 CSRF。重复认证头、重复会话 Cookie、Bearer 与 Cookie 混用被拒绝。登录来源限速使用直接连接的对端，不信任客户端提交的转发头。
- `GET /api/v1/whoami` 返回当前身份与会话；`DELETE /api/v1/sessions/current` 结束本人会话。客户端自报的主体、权限和传输档位不进入可信上下文。M1 拒绝 `X-Lantai-Task`、`X-Lantai-Lease` 执行上下文；也不启用任务执行、审定、扩展路由、事件订阅或联邦请求。
- `/api/v1/meta` 无需会话，公布实例 ID、当前版本、实际能力、JSON 大小和分页上限；它不披露主机路径、监听配置、成员或凭据。其余读取仍由领域服务按当前权限判定。

## JSON、错误与条件写入

JSON 请求默认最大 8 MiB，可由应用配置调整；只接受 `application/json` 和单个对象，拒绝未知字段、重复键、不合法 Unicode、过深嵌套、超出精确整数范围及压缩正文。超限使用现有 `SCHEMA_INVALID`，细节 `reason=request_body_too_large`。领域服务继续校验必需值、路径、修订、用途与配额。默认 JSON 请求上下文 30 秒；API server 的读写限时随其配置留出响应余量。

`X-Request-Id` 可以由客户端提供（1–128 位 ASCII 字母、数字、点、下划线、冒号或连字符），否则随机生成；无效值不原样反射。成功和错误都返回请求编号。错误统一使用错误码注册表规定的 HTTP 状态、可重试性和恢复建议；未知内部错误不会带出堆栈、路径或凭据。429/503 等错误若带重试时长，同步返回 `Retry-After`。服务端 `time.Time` 输出统一为 UTC、固定三位毫秒；用户自由元数据中的字符串不被改写。

创建项目、创建上传、提交版本、修改著录与执行敏感管理命令必须带 `Idempotency-Key`（1–128 位，格式与共享命令契约一致）。响应丢失时保持原键；operation reader 只返回本人拥有且当前获权的操作及结果引用，不返回回执中的秘密或原始响应摘要。会话、挑战与短时读取授权每次签发有自身身份；分片、文件完成与取消绑定现有上传身份，不再创造新的业务操作。HTTP 不把未完成的领域错误改写为 202；共享 Accepted 模型供确实持久接受且未完成的后续业务使用。

著录写入还须带 `If-Match: "<revision>"`，包括初始修订 `"0"`；缺少返回 `PRECONDITION_REQUIRED`，落后返回 `PRECONDITION_FAILED`。成功返回当前 `ETag`。不接受弱 ETag、通配符或一组候选修订。说明中的敏感级别与默认许可字段仍由领域拒绝普通修改，不能借 HTTP patch 绕过专门命令。

## M1 业务路由

下表省略 `/api/v1` 前缀。具体 JSON 字段见 OpenAPI；所有资源 ID 为 ULID。

| 方法与路径 | 行为 |
| --- | --- |
| `GET /projects?cursor&limit&view` | 只枚举当前可读项目，`items` 与可选 `next_cursor`；游标不带隐藏项目 ID |
| `POST /projects` | 登记项目及初始说明；管理员不自动得到项目角色 |
| `GET /projects/{key}` | 按项目 key 权威读取 |
| `GET /types`、`GET /types/{type}` | 固定类型登记；详情附完整 schema 库及对应 JSON Pointer，局部引用可解析 |
| `POST /uploads`、`GET /uploads/{id}` | 创建或读取上传及已接收分片；固定 `operation_id` |
| `POST /uploads/{id}/check` | 返回本操作已获授权的内容或 `upload_required`，不泄露未获权内容是否存在 |
| `POST /uploads/{id}/files/{sha256}/complete` | 校验已上传文件，响应本操作 BlobGrant；正文 `{}` |
| `DELETE /uploads/{id}` | 经 catalog 取消上传及尚未提交的版本操作；终结状态按领域规则处理 |
| `POST /uploads/{id}/commit` | 使用冻结输入提交；`committed` 是可见点；初始说明待补时返回 `description_pending` |
| `GET /assets/{id}` | 精确资产读取，与索引是否追上无关 |
| `GET /assets/{id}/versions/{version}` | 精确版本读取；`view=full` 附满足共享 schema 的冻结 manifest |
| `POST /assets/{id}/versions/{version}/read-grants` | 为声明的文件及用途签发本人会话绑定的短时下载地址 |
| `PATCH /assets/{id}/metadata` | 通过 Idempotency-Key 与 If-Match 条件修改著录 |
| `GET /assets?q&project_id&asset_type&cursor&limit&view` | 当前权限过滤的查询投影，附代次与水位；不因 GET 自动重建 |
| `GET /operations/{id}` | 本人操作的安全视图，查询权限收紧后不继续泄露结果 |

列表 `limit` 默认 50、最大 100。`view` 默认 `brief`，另接受 `full`：项目/资产 brief 省去自由扩展字段，项目 full 还带建立者和时间；版本 full 增加 manifest。检索的两种视图均保持安全目录摘要，不展开文件、uses 或来源细节；需要详细内容时调用精确读取。查询游标绑定身份、会话、过滤条件和投影代次/水位；失效后按 `CURSOR_EXPIRED` 重新分页。

## 人类授权与初始设置

最小身份管理使用核心封闭命令集，仅开放主体登记、签发/吊销长期凭据、授予/撤回项目角色：

1. `POST /identity/challenges` 提交 `{action, command}`。重开同一操作的过期挑战时附 `operation_id`。服务端返回目标、预期修订、摘要、请求摘要和 operation。
2. 向本人完整展示服务端 `summary` 与 `targets`，再 `POST /identity/challenges/{id}/verify` 提交本人动态码 `{code}`。
3. `POST /identity/commands` 提交相同 action、command 以及 `grant_id`，并使用固定 Idempotency-Key。核心重新检查当前权限、修订和一次性授权。

`GET /identity/principals/{id}` 取得当前主体修订；`GET /identity/projects/{project_id}/members` 取得角色与项目访问控制修订。首次签发凭据或成员设置码可能在执行响应中含 `secret`，它只返回一次；重放和 operation 查询不能取回。丢失这次响应后须按回执识别已执行效果，再撤销并重新签发，不能换键盲重试。

新成员可用管理员发放的设置码调用 `POST /sessions/setup`，在受限设置会话中 `PUT /identity/password`、`POST /identity/factor/enroll`（正文 `{}`）、`POST /identity/factor/confirm` 完成设置。确认响应的恢复码只当场交付本人。初始管理员的本机 bootstrap 和离线恢复仍归 T08，不开放公网恢复/重置接口。

## 流式传输

上传视图每个文件的 `part_url_template` 是服务端给出的完整同源路径；客户端仅将 `{part_number}` 替换为有效分片号。PUT 携带 `Content-Length` 和 `Lantai-Part-Sha256`，重复分片依真实摘要核对。下载仅使用 ReadGrant 的 `url`；同一会话的新 GET/HEAD/Range 请求都重新检查授权、签名、到期、版本与用途。

API 与传输使用独立连接池、server 配置和服务端调度。传输档位只从认证上下文推导，客户端头不能提档。传输没有整个大文件的绝对完成时限，默认每次读取/写入最多空闲两分钟，持续进展会刷新连接 deadline；中止请求会释放调度票据。`ResponseController` 经可展开的 writer 抵达 net/http 连接，不依赖客户端提供的超时头。网关自身的缓冲、代理超时和 TLS 配置仍需部署侧匹配。

验证覆盖请求解析与限额、权限上下文传递、Cookie/CSRF、条件写入、敏感命令白名单、响应 schema 与 UTC 毫秒、brief/full、分片模板，以及 net/http 连接空闲超时和持续进展。真实 merged/split 纵向验证归应用层；1 GiB 压力、生产 TLS 网关与跨平台环境验收不由这些单元测试推断。
