# 严格 TLS 与沙箱浏览器验收

默认 Playwright 配置明确使用 `chromiumSandbox: true`、`ignoreHTTPSErrors: false`，不再带忽略证书的启动参数。不能提供原生沙箱的宿主会失败，不自动下载或切换不安全浏览器。

独立入口为 `web/playwright.secure.config.ts`。先在仓库之外的新私有目录生成一次性合成 CA 和 localhost 证书：

```sh
node web/tests/create-test-tls.mjs /explicit/new/private/browser-tls
```

该命令输出四个变量的路径。将它们作为此次 Playwright 进程的环境变量，特别是 `NODE_EXTRA_CA_CERTS` 必须在 Node 启动前设置。`LANTAI_WEB_TLS_CERT_FILE`、`LANTAI_WEB_TLS_KEY_FILE` 只供 opt-in Go 测试夹具加载，不改变产品 HTTPS 配置。SAN 明确包含 `localhost` 与 `127.0.0.1`。

```sh
cd web
LANTAI_WEB_CA_FILE=/explicit/private/browser-tls/ca.pem \
NODE_EXTRA_CA_CERTS=/explicit/private/browser-tls/ca.pem \
LANTAI_WEB_TLS_CERT_FILE=/explicit/private/browser-tls/localhost.pem \
LANTAI_WEB_TLS_KEY_FILE=/explicit/private/browser-tls/localhost-key.pem \
LANTAI_WEB_CHROMIUM_EXECUTABLE=/explicit/installed/chromium \
node node_modules/@playwright/test/cli.js test --config playwright.secure.config.ts
```

入口只使用已安装的浏览器。每个测试建立独立 `0700` persistent profile，通过 Chromium 自带证书管理器的受信任证书列表导入明确选择的合成 CA；不改宿主信任库或 HOME，不使用证书异常绕过。若所选 Chromium 不提供该 profile 信任界面或原生沙箱不可用，测试失败并保留阻塞，不降级。

独立用例检查正确 CA/主机名成功、空 CA 和错误主机名失败，然后由实际 Chromium 打开登录页。它不代表其他业务浏览器用例已经通过。Go、Node TLS 测试、页面构建或测试发现均不能替代实际浏览器执行。
