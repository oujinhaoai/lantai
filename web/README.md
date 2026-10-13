# web · 兰台浏览工作台

TypeScript / React / Vite 独立静态网页，只调用现有同源 `/api/v1/`。首版提供口令＋TOTP 登录、首页、当前授权范围内的项目及资源列表/详情、著录文字检索、类型筛选、分页，以及加载、无权限、空数据、错误和过期提示。

本次是 M3 基础浏览切片，不代表 M3/T09.7 全部完成。编辑、人审、回收站、文件下载、媒体预览、插件 registry 和第三方网页尚未接入。固定版本详情展示真实清单，不把文件路径当作可公开下载 URL。

## 构建

Node.js 24（CI 固定主版本）、npm；Go 工具链由仓库固定。依赖精确版本见 package.json 和 package-lock.json。

```sh
cd web
npm ci
npm run lint
npm run build
```

产物是 web/dist，不需要运行 Node 服务。生产环境由已有 HTTPS 网关提供静态文件，与 API 完全同源。路由使用 `/#/projects` 和 `/#/assets`，刷新或深链接无需 catch-all；未知 API 和运维地址仍返回 404。

`npm run dev` 只监听 loopback，向 127.0.0.1:8080 代理 API。真实登录仍要求 HTTPS/安全 Cookie 与精确允许的 Origin。不要为 HTTP 调试关闭 Secure、TOTP 或来源校验。可重复验证优先使用自带 HTTPS 合成实例。

## 浏览器验证与预览

```sh
cd web
npm run build
npx playwright install chromium webkit
npm test
```

测试自动启动 Go TestWebBrowserFixture，使用临时真实实例、SQLite、文件存储、随机主密钥、合成管理员和资源；不读取实际实例配置或凭据。Chrome、手机 WebKit 与平板 WebKit 覆盖登录、Cookie、CSRF、中文搜索、分页、详情、导航、空数据、撤权、过期、失败、重复提交和迟到响应，含横竖屏、长文件名、触控目标与键盘焦点，另以 axe 扫描登录/首页/检索/详情的 WCAG 2.1 A/AA 自动规则。自动扫描不替代屏幕阅读器和真实设备人工验收。自签名 TLS 仅测试上下文接受；不修改系统证书信任。测试结束关闭实例。

独立保留本机预览：

```sh
# 仓库根，只用于合成测试，不绑定外部接口
LANTAI_WEB_FIXTURE=1 go test -run '^TestWebBrowserFixture$' -count=1 -timeout=30m ./tests/integration
# 另一个终端在 web/ 执行以复用实例
LANTAI_WEB_REUSE=1 npm test
```

地址 https://localhost:4180。它不使用真实凭据，测试工具通过仅存在于测试二进制中的控制接口生成合成登录输入。不要将此服务用于部署。`LANTAI_WEB_EVIDENCE=/path/out npm test` 保存不含登录输入的截图。测试不自动重试 TOTP。

## 认证与数据边界

- 登录固定 channel=browser，只申请 self/read。服务端发 Secure/HttpOnly/SameSite=Strict/host-only Cookie，JS 不读取会话令牌，也不接收主密钥、长期凭据或 HumanGrant。
- CSRF 值与会话 ID 只存在当前标签页 sessionStorage（存储受限时在内存）。刷新先用 whoami 复验；CSRF 必须匹配会话 ID。新标签页利用 Cookie 浏览；没有 CSRF 时提示回原登录标签页退出，或手动重新验证，不声称清 UI 就已结束服务器会话。
- 退出走既有 DELETE /sessions/current，带原有 CSRF。过期/撤销清空视图和 CSRF，不自动签发、交换或续期。返回应用时复验身份，新页面/筛选向服务端请求，不持久化业务数据，无 Service Worker 或离线缓存。
- 页面切换取消旧读取并卸载旧视图，迟到结果不装入新视图。分页显式加载下一页；过滤后空页仍允许继续，游标失效后回第一页。
- 用户字段作为文本渲染，无外部 CDN、埋点或第三方脚本。CSS/脚本均同源，网关静态路由提供 CSP 与禁止缓存头。
- 第一版没有权限自动推送。已展示的字节不能收回；刷新与新请求由服务端重新过滤。不引入全馆计数、隐藏项数量或权限推断。

## 部署接线

[compose.web.yaml](../deploy/compose.web.yaml) 只把已构建的 web/dist 以只读方式挂到现有网关，不修改核心镜像、数据/密钥/首次设置卷。现场升级与发布新版本须另行审查，本仓库构建不会自动部署。

第三方网页和未来 UI 插槽仍遵守[扩展规格](../docs/extensions.md)，本版不激活扩展脚本。

## 独立帮助页集成约定

帮助任务只维护 `docs/user-guide/` 与 `web/public/help/`；主应用拥有认证、布局、导航和网关。入口为普通静态路径 `/help/`，资源使用相对路径，不依赖 React、Cookie 或业务 API。现有 CSP 不接受内联脚本/样式，建议 `index.html` 配合同目录外置 CSS；不放宽主应用安全策略。

构建时只有检测到 `web/public/help/index.html` 才显示“使用帮助”链接；帮助文件尚未合并时不展示死链接。Vite 将 public/help 原样产出到 dist/help，Caddy 显式开放 /help 与 /help/*，不放开未知 API 或运维路径。帮助内容合并后需重新构建并统一验收导航、CSP、资源及三设备布局，不能以本次预留接线声明帮助内容已发布。
