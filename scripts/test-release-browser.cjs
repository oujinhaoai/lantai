// Actual packaged Caddy/core, no request mocks or dev server. Synthetic input
// arrives on stdin; no credentials/cookies/storage state are exported.
const fs = require("node:fs");
const path = require("node:path");
const { chromium, expect } = require("../web/node_modules/@playwright/test");
let phase = "bootstrap";
const privateValues = [];

(async () => {
  const input = JSON.parse(fs.readFileSync(0, "utf8"));
  privateValues.push(input.password, input.code);
  const browser = await chromium.launch({
    args: [`--ignore-certificate-errors-spki-list=${input.certificate_spki}`],
  });
  const result = {
    label: input.label,
    passed: false,
    tls: "Python verifies synthetic CA/hostname; Chromium exception pins this exact synthetic leaf SPKI",
  };
  try {
    // No ignoreHTTPSErrors and no system trust change.
    const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
    const page = await context.newPage();
    const errors = [];
    page.on("pageerror", () => errors.push("pageerror"));
    await page.goto(input.origin + "/");
    phase = "login";
    await expect(page.getByRole("heading", { name: "登录兰台" })).toBeVisible();
    await page.getByLabel("账户名称").fill(input.name);
    await page.getByLabel("口令", { exact: true }).fill(input.password);
    await page.getByLabel("验证器动态码").fill(input.code);
    await page.getByRole("button", { name: "登录并浏览" }).click();
    await expect(page.getByRole("heading", { name: `你好，${input.name}` })).toBeVisible();
    const who = await page.evaluate(async () => (await fetch("/api/v1/whoami")).json());
    expect(who.principal.principal_id).toBe(input.principal_id);
    expect(who.session.scopes.sort()).toEqual(["read", "self"]);
    expect(who).not.toHaveProperty("token");
    result.same_principal = true;
    const session = (await context.cookies()).find(c => c.name === "lantai_session");
    if (session) privateValues.push(session.value);
    expect(session).toMatchObject({ secure: true, httpOnly: true, sameSite: "Strict", path: "/" });
    expect(await page.evaluate(() => document.cookie)).not.toContain("lantai_session");
    result.cookie_protected = true;
    await page.screenshot({ path: path.join(input.screenshots, input.label + "-desktop-home.png"), fullPage: true });
    phase = "authorized resource detail and preview";
    await page.getByRole("navigation").getByRole("link", { name: "资源库" }).click();
    await page.getByRole("link", { name: /release-sample/ }).click();
    await expect(page.getByRole("heading", { name: "release-sample" })).toBeVisible();
    const grantResponse = page.waitForResponse(r => r.url().endsWith("/read-grants") && r.request().method() === "POST");
    await page.getByRole("button", { name: /预览文件/ }).click();
    const grant = await (await grantResponse).json();
    privateValues.push(grant.url);
    const range = await page.evaluate(async url => {
      const r = await fetch(url, { headers: { Range: "bytes=0-1" } });
      return { status: r.status, body: await r.text(), range: r.headers.get("Content-Range") };
    }, grant.url);
    expect(range.status).toBe(206);
    expect(range.body).toBe("# ");
    expect(range.range).toMatch(/^bytes 0-1\//);
    const anonymous = await browser.newContext();
    const anonymousPage = await anonymous.newPage();
    await anonymousPage.goto(input.origin + "/");
    expect(await anonymousPage.evaluate(async url => (await fetch(url)).status, grant.url)).toBe(401);
    await anonymous.close();
    result.range_and_session_binding = true;
    await expect(page.getByText("Synthetic release resource", { exact: true })).toBeVisible();
    await page.screenshot({ path: path.join(input.screenshots, input.label + "-desktop-resource.png"), fullPage: true });
    for (const width of [834, 390]) {
      await page.setViewportSize({ width, height: 1000 });
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      await page.screenshot({ path: path.join(input.screenshots, `${input.label}-${width}-resource.png`), fullPage: true });
    }
    phase = "help returns to the same session";
    await page.getByRole("link", { name: /使用帮助/ }).click();
    await expect(page.getByRole("heading", { name: "Lantai 用户手册" })).toBeVisible();
    await page.getByRole("link", { name: "返回兰台" }).click();
    await expect(page.getByRole("heading", { name: `你好，${input.name}` })).toBeVisible();
    await page.goto(input.origin + "/help/upgrade.html");
    await page.getByRole("link", { name: "返回兰台" }).focus();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("heading", { name: `你好，${input.name}` })).toBeVisible();
    // Browser cookie alone cannot authorize state changes without Origin/CSRF.
    phase = "CSRF denial";
    const denied = await page.evaluate(async () => (await fetch("/api/v1/sessions/current", {
      method: "DELETE", headers: { "Content-Type": "application/json" }, body: "{}",
    })).status);
    expect(denied).toBe(403);
    await page.reload();
    await expect(page.getByRole("heading", { name: `你好，${input.name}` })).toBeVisible();
    expect(errors).toEqual([]);
    result.no_csrf_post_denied = true;
    const noJS = await browser.newContext({ javaScriptEnabled: false, viewport: { width: 320, height: 850 } });
    phase = "nine static pages without JavaScript";
    const help = await noJS.newPage();
    for (const chapter of ["index", "install", "setup", "accounts", "daily", "codex-mcp", "upgrade", "backup", "troubleshooting"]) {
      await help.goto(`${input.origin}/help/${chapter}.html`);
      await expect(help.locator("main h1")).toBeVisible();
      await help.getByRole("link", { name: "返回兰台" }).click();
      expect(new URL(help.url()).pathname).toBe("/");
    }
    result.help_no_js_pages = 9;
    result.passed = true;
    process.stdout.write(JSON.stringify(result));
  } finally {
    await browser.close();
  }
})().catch((error) => {
  // Playwright assertion details can include synthetic form values. Keep only
  // the failed phase and nonzero exit; author diagnoses in the isolated runner.
  let message = String(error.message || error).slice(0, 4000);
  for (const value of privateValues) if (value) message = message.split(value).join("[REDACTED]");
  process.stderr.write(JSON.stringify({ phase, error_type: error.name, message }) + "\n");
  process.exitCode = 1;
});
