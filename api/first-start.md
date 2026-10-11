# 首次管理员设置

本协议仅在未完成初始化时由 `lantai start` 的专用 handler 提供，所有者为 T08 生命周期与 T01 身份模块。不属于常规业务 REST、公开 SDK 或公众注册。页面与静态资源作为最小安装入口随核心发布，不宣称交付 M3 业务网页或插件宿主。原有本机 `init` 保持兼容。

面向首次使用者的按钮、验证器与错误处理步骤见[首次管理员设置指南](../docs/first-setup.md)。页面没有二维码，TOTP 使用手动密钥添加。

## 前置与隔离

- 一个核心进程独占数据根锁，业务 gate 保持关闭。网关只发布既有 HTTPS 端口；后端复用 API 监听，不开启传输及运维监听。
- 明确配置至少一个 HTTPS allowed origin，精确包含外部端口。请求 Host 必须对应该 origin；POST 同时要求一个精确匹配的 Origin，拒绝跨站、重复认证头、query 和未列出的路由。
- 256 位随机引导码与实例 ID 绑定，保存在独立目录 `setup-code.json`。目录 0700，文件 0600，所有者为服务用户；拒绝链接、多硬链接、宽松权限、错误实例或格式。引导码不写标准日志、URL、cookie 或浏览器持久存储。
- 所有回复 `Cache-Control: no-store`；页面使用仅同源脚本/样式、禁止框架及表单导航的 CSP，无第三方资源。
- 页面离开时清空引导码、登记标识、口令、TOTP 与恢复码字段；从浏览器历史快照恢复时重新加载，不复用快照中的登记状态。

## HTTP

`GET /setup`、`GET /setup/` 和静态资源 `/setup/setup.js`、`/setup/setup.css` 仅返回安装页面，不含引导码。

两个 POST 都要求 `Authorization: Bearer <引导码>`、`Content-Type: application/json`。请求体最多 8192 字节，拒绝未知字段及额外 JSON。错误为不含秘密的纯文本。

| 请求 | JSON 字段 | 成功回复 |
|---|---|---|
| `POST /setup/begin` | `name`、`password` | `enrollment_id`、`totp_secret`、`totp_uri`、`recovery_codes`、`expires_in_seconds: 600` |
| `POST /setup/confirm` | `enrollment_id`、`totp_code`、`recovery_codes_saved: true` | `complete: true` |

用户名、口令、TOTP 沿用身份模块规则。每次 begin 至少间隔 10 秒，并使前一登记失效；同一时刻仅保留一个内存登记。登记 10 分钟过期，最多尝试 5 次 TOTP。尚未完成设置时，页面丢失或出现 `Enrollment expired; start again` 后刷新 `/setup/`，用原引导码重新 begin，替换验证器中旧的未完成登记密钥及整组恢复码；无需重启、重装或删卷。身份事务提交前不持久化口令登记、TOTP 种子或恢复码。

网页先展示该批恢复码，要求用户保存后再确认。身份模块将同一批码的校验值、管理员、口令和加密 TOTP 在一个主库事务提交，并永久关闭 bootstrap。确认响应丢失不会丢失用户预先保存的恢复码。提交后 handler 立即关闭，即使激活或清理失败也不能再次创建管理员；重启核验现有密钥并完成激活。

确认断线后先保留原密码、TOTP 与恢复码并检查状态：正常服务 `/api/v1/meta` 为 200 且 `/setup/` 为 404；根路径 404 是预期行为。只有 setup 404、meta 不可用时不能推断服务全就绪；`Administrator saved` 明确表示身份已提交，需排查同一实例的激活，不能指导用户重开初始化。

## 重启与升级

active 实例直接进入原有 serve，不依赖 setup 目录。完整的 initializing 实例只在五库都存在、绑定匹配、迁移已全部应用时续接。不完整库集、未标记的未知数据、待迁移、恢复中或密钥丢失均停止，保留数据供离线诊断；不会自动补库、迁移或替换已有管理员密钥。

prepare 按 data、secrets、setup 的顺序接受三个目录。只改空目录根。非空 data 根兼容服务所有且不允许组/其他人写入的旧权限；非空 secrets/setup 必须 0700。不递归修复。Compose 在未设置原路径变量时采用命名卷；原有 `LANTAI_DATA_DIR` / `LANTAI_SECRETS_DIR` 优先用于 prepare 和 core，已有部署升级必须保留原项目与卷身份。

目标 NAS 管理界面读取 0600 文件的能力须实际验收。证书签发、信任、续期和真实部署均由部署者管理，本流程不操作这些资源。

引导码落盘在极短的硬链接落位窗口中断时，可能留下 `.setup-code.json.tmp-*` 与正式文件的两个链接；此时同样失败关闭，不自动删除可疑链接。停机并备份后，管理员核验临时文件与正式文件确实为同一 inode、同一所有者、0600 且链接总数恰好为 2，才可移除该临时目录项；保留正式文件及数据库。不能证明这些条件时交由离线诊断，不删除码文件重建。测试包含该中断状态，不宣称所有早期创建失败都可无人值守恢复。
