# 帮助手册维护与验证

用户入口为 [README.md](README.md)。本文件给维护者说明生成与验证，不发布到帮助页面。

## 文件与路由边界

- 用户内容唯一来源是本目录九篇 Markdown。`build.py` 用 Python 标准库生成 `web/public/help/*.html` 和 `search-data.js`，不要手改这些生成物。
- `help.css` 和 `help.js` 是手写静态资源。无第三方运行时、外部字体、分析脚本、网络检索或身份接口调用；查询只在当前页面内存中匹配公开文档，不写浏览器存储。
- 前端 Vite 将 `public/help/` 原样放入 `dist/help/`。主前端拥有主布局、认证、导航、构建脚本及网关；该分支只提供 `/help/` 内容。集成时由前端保证 `/help` 与 `/help/*` 静态路由、MIME 和既定安全头，不让帮助路由进入登录守卫。
- 所有资源用相对路径，兼容 `script-src 'self'; style-src 'self'`。HTML 无内联 style/script/事件处理。无 JavaScript 时目录、文章、分页链接和打印仍可用；搜索作为渐进增强。
- 每页公共页头的“返回兰台”是原生 `<a href="/">`，仅导航到同源网关根路径。主应用重新校验现有会话：有效会话进入首页，未登录进入登录页，失效或连接错误由主应用既有流程处理。该入口不读取会话、不携带凭据，也不依赖帮助脚本；主应用本身的运行要求不因此改变。
- 一般列表保持扁平；生成器仅支持文档用到的有限 Markdown 子集，不承诺通用 Markdown 插件、嵌套列表、原始 HTML 或动态渲染。

## 本地检查

在仓库根目录执行，Python 3.11 及以上：

```sh
python3 docs/user-guide/build.py
python3 docs/user-guide/build.py --check
python3 docs/user-guide/check.py
go build -o /tmp/lantai-help-bin ./cmd/lantai
python3 docs/user-guide/check.py --binary /tmp/lantai-help-bin
scripts/check.sh
```

`check.py` 校验生成物漂移、Markdown 文件/锚点、静态页面全部本地链接、单一 h1、CSP 结构、合成数据边界、shell 语法和文档中的命令 flag。给二进制时只执行 `--help`，不接入服务或执行示例写入。部分旧本机命令的帮助返回 usage 退出码 2，按其现有行为检查，未改产品命令。

前端已有依赖后，可复用它的 Playwright 和 axe，不另装文档框架：

```sh
node docs/user-guide/browser-check.cjs web/node_modules /tmp/lantai-help-browser
```

浏览器脚本只监听随机本机回环端口，发送与主站一致的 CSP，测试九页在 Chromium 桌面、WebKit 平板/手机的状态、溢出、WCAG 2/2.1 A/AA axe 检查、搜索、清空、键盘与无 JS 的 320px 阅读。九页均验证返回入口可见、至少 44px 高并能导航至同源根；另验证从搜索结果进入章节后用键盘返回，以及九页在无 JS 时返回。测试根页为合成占位，只证明原生导航，真实会话行为由主前端联合验收。桌面验证 Tab 顺序；触屏 WebKit 验证聚焦后的键盘激活。结果及合成截图写入指定目录。这里只测试同源文档，因此 axe 使用同源 legacy 模式，无需添加测试 iframe 或降低 CSP；自动扫描不能替代人工阅读、屏幕阅读器和真实设备验证。

WebKit 的 Playwright 截图过程会注入临时 `body {}` 样式同步动画，该样式被 CSP 拒绝；脚本仅在截图调用期间单独记录这一精确提示为 `screenshotHarnessWarnings`，不忽略导航/交互期间的相同错误或其他控制台错误。页面 CSS 仍从外置文件加载，不放宽 CSP。

合并入前端后还须在 Vite 产物及真实网关下重跑，确认主站入口有文件时出现、无文件时隐藏，以及 `/help/` 不泄露实例私有数据。该分支的页面测试不证明主应用认证或目标 NAS 的部署成功。

## 内容核对来源

- 首次设置：`internal/firststart/setup.html`、`setup.js`、`http.go`、`state.go`，对照原有 `docs/first-setup.md`；时限、次数、确认结果不凭 UI 文案猜测。
- 身份与命令：`internal/cli`、`internal/identity/admin.go`、`docs/contracts/client.md`、`identity.md`、`identity-actions.md`、`api/openapi.yaml`，以及 `tests/integration/remote_identity_test.go`。
- MCP：`cmd/lantai/mcp.go`、`internal/mcpserver`、`docs/contracts/manual-execution.md`；Codex 配置引用正文中核对日期的官方文档。
- 部署与备份：`deploy/compose.yaml`、`config.container.yaml`、`cmd/lantai/maintenance.go`、`internal/operations`、`docs/contracts/backup-restore.md`。
- 升级版本对：rc.2 `676de7a` → rc.4 直接升级的实际发行 manifest/delivery_smoke；旧 rc.3 仅历史参考；不将私有部署路径、主机地址或密钥复制入手册。

更新服务器/CLI 版本或增加网页能力时先重新核对实际发布物，再更新能力表和步骤。设计完成、实现合并、发行包含、目标机器部署成功是不同结论；不能因有帮助页就把在建功能标成可用。
