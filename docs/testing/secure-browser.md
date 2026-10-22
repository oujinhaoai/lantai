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

## Linux CI 的完整三浏览器回归

`python3 scripts/test-web-strict.py --evidence-file strict-web-ci.json` 在已安装 Node、Go、Playwright 浏览器和 `libnss3-tools` 的一次性 Linux CI 中运行完整套件。该入口需要 CI 用户已有的非交互 sudo/unshare 能力，仅用于合成测试，不在部署主机运行。

入口生成短期测试 CA/证书，为 Node 设置 `NODE_EXTRA_CA_CERTS`，给 Chromium 创建独立临时 HOME/NSS 数据库。WebKit 的系统 TLS 后端在新的私有 mount namespace 中读取“原 CA bundle + 合成 CA”；仅该子进程树可见，退出后自动释放。绑定前必须证明命名空间不同于父进程，浏览器启动前降回原非 root 用户；宿主 CA 文件不写入，前后摘要必须一致。临时目录退出后删除，不更改宿主 HOME、信任库或安全策略。

NSS 工具使用 Ubuntu 24.04 的 `libnss3-tools` 发行包（MPL-2.0），实际版本写入 `strict-web-ci.json`；`unshare`/`mount` 来自 runner 已有 util-linux。Chromium 显式选择 runner 已安装的 `/opt/google/chrome/chrome`，沿用其可用沙箱，不降级到无沙箱的下载版。制品浏览器验证用 `test-browser-trust.py` 给同一已安装浏览器提供独立临时 NSS HOME，并在导入后复核精确 CA 指纹；它不更改宿主证书库。

`ignoreHTTPSErrors: false` 和 Chromium sandbox 保持开启。完整套件增加每个浏览器对错误 CA、错误主机名的真实拒绝，以及拒绝后正确证书页面可用的检查。无 sudo/namespace 或浏览器安全能力时明确失败，不降级或跳过用例。原生 Chromium 证书界面专项仍使用上方独立入口，两个证据范围分别记录。
