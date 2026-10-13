/* Reuse the frontend's installed Playwright + axe packages; do not install a
 * second web stack. Example: node browser-check.cjs /path/to/web/node_modules /tmp/help-evidence */
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const modules = path.resolve(process.argv[2]);
const { chromium, webkit } = require(path.join(modules, "@playwright/test"));
const AxeBuilder = require(path.join(modules, "@axe-core/playwright")).default;
const output = path.resolve(process.argv[3] || "/tmp/lantai-help-browser");
const root = path.resolve(__dirname, "../../web/public");
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'";
const pages = fs.readdirSync(path.join(root, "help")).filter((name) => name.endsWith(".html"));
const mime = { ".html": "text/html; charset=utf-8", ".css": "text/css", ".js": "text/javascript", ".svg": "image/svg+xml" };

async function main() {
  fs.mkdirSync(output, { recursive: true });
  const server = http.createServer((req, res) => {
    const pathname = new URL(req.url, "http://localhost").pathname;
    const file = path.join(root, pathname === "/help/" ? "/help/index.html" : pathname);
    if (!file.startsWith(root + path.sep) || !fs.existsSync(file) || !fs.statSync(file).isFile()) {
      res.writeHead(404).end();
      return;
    }
    res.writeHead(200, { "Content-Type": mime[path.extname(file)], "Content-Security-Policy": csp });
    res.end(fs.readFileSync(file));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const report = { csp, checks: [], screenshots: [] };
  const browsers = [];
  try {
    for (const device of [
      { name: "desktop-chromium", engine: chromium, viewport: { width: 1440, height: 1000 } },
      { name: "tablet-webkit", engine: webkit, viewport: { width: 834, height: 1112 }, isMobile: true, hasTouch: true },
      { name: "phone-webkit", engine: webkit, viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true },
    ]) {
      const browser = await device.engine.launch(device.engine === chromium ? { channel: "chromium" } : {});
      browsers.push(browser);
      const context = await browser.newContext({ viewport: device.viewport, isMobile: !!device.isMobile, hasTouch: !!device.hasTouch });
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      page.on("console", (message) => { if (message.type() === "error") errors.push(message.text() + " " + message.location().url); });
      page.on("request", (request) => assert.ok(request.url().startsWith(origin + "/"), "external request"));
      for (const name of pages) {
        console.log(`${device.name}: ${name}`);
        const response = await page.goto(`${origin}/help/${name}`);
        assert.equal(response.status(), 200);
        await page.locator(".search:not([hidden])").waitFor();
        assert.equal(await page.locator("h1").count(), 1);
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `${device.name}: overflow ${name}`);
        if (name === "accounts.html") await page.screenshot({ path: path.join(output, `${device.name}-accounts.png`), fullPage: true });
        // Legacy mode keeps axe in this same-origin document rather than adding
        // a finish iframe, which the production frame-ancestors policy forbids.
        const axe = await Promise.race([
          new AxeBuilder({ page }).setLegacyMode().withTags(["wcag2a", "wcag2aa", "wcag21aa"]).analyze(),
          new Promise((_, reject) => { const timer = setTimeout(() => reject(new Error(`axe timeout: ${device.name}/${name}`)), 30000); timer.unref(); }),
        ]);
        assert.deepEqual(axe.violations.map((item) => ({ id: item.id, nodes: item.nodes.length })), [], `${device.name}: axe ${name}`);
        report.checks.push({ device: device.name, page: name, http: 200, overflow: false, axeViolations: 0 });
      }
      await page.goto(`${origin}/help/index.html`);
      const indexShot = `${device.name}-index.png`;
      await page.screenshot({ path: path.join(output, indexShot), fullPage: true });
      report.screenshots.push(indexShot);
      // Touch WebKit does not tab through links by default. Verify the native
      // anchor's keyboard activation there; desktop also verifies Tab order.
      if (device.engine === chromium) await page.keyboard.press("Tab");
      else await page.locator(".skip").focus();
      assert.equal(await page.locator(".skip").evaluate((el) => el === document.activeElement), true);
      await page.keyboard.press("Enter");
      assert.equal(await page.locator("main").evaluate((el) => el === document.activeElement), true);
      const search = page.getByRole("searchbox", { name: "搜索全部帮助" });
      await search.focus();
      await page.keyboard.type("TOTP");
      await page.locator("#search-results:not([hidden])").waitFor();
      assert.ok(await page.locator("#search-list li").count() > 0);
      if (device.engine === chromium) await page.keyboard.press("Tab");
      else await page.getByRole("button", { name: "清除" }).focus();
      assert.equal(await page.getByRole("button", { name: "清除" }).evaluate((el) => el === document.activeElement), true);
      await page.keyboard.press("Enter");
      assert.equal(await search.inputValue(), "");
      await search.fill("不存在的合成查询abcdef");
      assert.ok((await page.locator("#search-status").textContent()).includes("没有找到"));
      await search.fill('<img src=x onerror="alert(1)">');
      assert.equal(await page.locator("#search-results img").count(), 0);
      await page.getByRole("button", { name: "清除" }).click();
      if (device.viewport.width < 760) {
        const summary = page.locator(".mobile-nav summary");
        await summary.focus();
        await page.keyboard.press("Enter");
        assert.equal(await page.locator(".mobile-nav").getAttribute("open"), "");
        await page.locator('.mobile-nav a[href="install.html"]').focus();
        await page.keyboard.press("Enter");
        await page.waitForURL("**/install.html");
      }
      await page.goto(`${origin}/help/setup.html`);
      const setupShot = `${device.name}-setup.png`;
      await page.screenshot({ path: path.join(output, setupShot), fullPage: true });
      report.screenshots.push(setupShot);
      assert.deepEqual(errors, [], `${device.name}: script/CSP errors`);
      await context.close();
      await browser.close();
    }
    const noJS = await chromium.launch({ channel: "chromium" });
    browsers.push(noJS);
    const context = await noJS.newContext({ javaScriptEnabled: false, viewport: { width: 320, height: 800 } });
    const page = await context.newPage();
    for (const name of pages) {
      await page.goto(`${origin}/help/${name}`);
      assert.equal(await page.locator("h1").count(), 1);
      assert.equal(await page.locator(".search").isVisible(), false);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `320px no-JS overflow ${name}`);
    }
    report.checks.push({ device: "320px-chromium-no-js", pages: pages.length, overflow: false, readable: true });
    await noJS.close();
    report.result = "passed";
    fs.writeFileSync(path.join(output, "browser-result.json"), JSON.stringify(report, null, 2) + "\n");
    console.log(`PASS: ${pages.length} pages × 3 devices, keyboard/search/CSP/axe, 320px no-JS. Evidence: ${output}`);
  } finally {
    for (const browser of browsers) await browser.close();
    server.close();
  }
}
main().catch((error) => { console.error(error); process.exitCode = 1; });
