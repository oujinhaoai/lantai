import {
  test,
  expect,
  type Page,
  type APIRequestContext,
  type TestInfo,
} from "@playwright/test";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import AxeBuilder from "@axe-core/playwright";

async function control(request: APIRequestContext, action: string) {
  const reply = await request.post(`/__fixture/${action}`, {
    headers: { "X-Lantai-Fixture": "1" },
  });
  expect(reply.ok()).toBeTruthy();
  return reply;
}
async function login(page: Page, request: APIRequestContext) {
  const credentials = await (await control(request, "login")).json();
  await page.goto("/");
  await page.getByLabel("账户名称").fill(credentials.name);
  await page.getByLabel("口令", { exact: true }).fill(credentials.password);
  await page.getByLabel("验证器动态码").fill(credentials.code);
  await page.getByRole("button", { name: "登录并浏览" }).click();
  await expect(page.getByRole("heading", { name: "你好，Ada" })).toBeVisible();
  return credentials;
}
async function shot(page: Page, info: TestInfo, name: string) {
  if (!process.env.LANTAI_WEB_EVIDENCE) return;
  await mkdir(process.env.LANTAI_WEB_EVIDENCE, { recursive: true });
  await page.screenshot({
    path: path.join(
      process.env.LANTAI_WEB_EVIDENCE,
      `${info.project.name}-${name}.png`,
    ),
    fullPage: true,
  });
}

test("real login: TOTP, least scopes, protected cookie, reload and duplicate submit", async ({
  page,
  request,
  context,
}, info) => {
  let logins = 0;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/v1/sessions/login", async (route) => {
    logins++;
    await gate;
    await route.continue();
  });
  const credentials = await (await control(request, "login")).json();
  await page.goto("/");
  await shot(page, info, "login");
  await page.getByLabel("账户名称").fill(credentials.name);
  await page.getByLabel("口令", { exact: true }).fill(credentials.password);
  await page.getByLabel("验证器动态码").fill(credentials.code);
  await page.getByRole("button", { name: "登录并浏览" }).click();
  await expect(page.getByRole("button", { name: "正在验证…" })).toBeDisabled();
  // A second submit in the same event turn must not replay the one-time TOTP.
  await page.locator("form").dispatchEvent("submit");
  release();
  await expect(page.getByRole("heading", { name: "你好，Ada" })).toBeVisible();
  expect(logins).toBe(1);
  const cookies = await context.cookies();
  const session = cookies.find((cookie) => cookie.name === "lantai_session");
  expect(session).toMatchObject({
    secure: true,
    httpOnly: true,
    sameSite: "Strict",
    path: "/",
  });
  const identity = await (await context.request.get("/api/v1/whoami")).json();
  expect(identity.session.scopes.sort()).toEqual(["read", "self"]);
  expect(identity).not.toHaveProperty("token");
  expect(await page.evaluate(() => document.cookie)).not.toContain(
    "lantai_session",
  );
  const storage = await page.evaluate(() => ({
    local: { ...localStorage },
    session: { ...sessionStorage },
  }));
  expect(storage.local).toEqual({});
  expect(JSON.stringify(storage)).not.toContain(credentials.password);
  expect(JSON.stringify(storage)).not.toContain("lts_");
  await expect(page.getByRole("link", { name: /盘丝洞/ })).toBeVisible();
  await shot(page, info, "home");
  await page.reload();
  await expect(page.getByRole("heading", { name: "你好，Ada" })).toBeVisible();
  await page.getByRole("button", { name: "退出", exact: true }).click();
  await expect(page.getByRole("heading", { name: "登录兰台" })).toBeVisible();
  expect((await context.request.get("/api/v1/whoami")).status()).toBe(401);
  expect(await page.evaluate(() => sessionStorage.length)).toBe(0);
});

test("Chinese browse, project isolation, real pagination, fixed detail and history", async ({
  page,
  request,
}, info) => {
  await login(page, request);
  await page
    .getByRole("navigation")
    .getByRole("link", { name: "我的项目" })
    .click();
  await page.getByRole("link", { name: /空白工作室/ }).click();
  await expect(
    page.getByRole("heading", { name: "暂无可见资源" }),
  ).toBeVisible();
  await shot(page, info, "empty");
  await page
    .getByRole("navigation")
    .getByRole("link", { name: "资源库" })
    .click();
  await expect(page.locator(".asset-list li")).toHaveCount(24);
  await page.getByRole("button", { name: "加载下一页" }).click();
  await expect(page.locator(".asset-list li")).toHaveCount(27);
  await expect(page.getByRole("button", { name: "加载下一页" })).toHaveCount(0);
  await page.getByLabel("搜索资源").fill("镜头");
  await page.getByRole("button", { name: "搜索", exact: true }).click();
  await expect(page.locator(".asset-list li")).toHaveCount(1);
  await expect(
    page.getByRole("link", { name: /镜头语言与场景参考/ }),
  ).toBeVisible();
  await shot(page, info, "search");
  await page.getByRole("link", { name: /镜头语言与场景参考/ }).click();
  await expect(
    page.getByRole("heading", { name: "镜头语言与场景参考" }),
  ).toBeVisible();
  await expect(page.getByRole("cell", { name: "content.txt" })).toBeVisible();
  await shot(page, info, "detail");
  await page.goBack();
  await expect(page.getByLabel("搜索资源")).toHaveValue("镜头");
  await expect(page.locator(".asset-list li")).toHaveCount(1);
  await page.goForward();
  await expect(
    page.getByRole("heading", { name: "镜头语言与场景参考" }),
  ).toBeVisible();
  await page.getByRole("link", { name: "返回列表" }).click();
  await page.getByLabel("资源类型").selectOption("image");
  await page.getByRole("button", { name: "搜索", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "没有匹配的资源" }),
  ).toBeVisible();
  const horizontalOverflow = await page.evaluate(
    () => document.documentElement.scrollWidth > window.innerWidth,
  );
  expect(horizontalOverflow).toBe(false);
});

test("real revoked project permissions hide entries and reject known details", async ({
  page,
  request,
  context,
}, info) => {
  await login(page, request);
  const assets = await (await context.request.get("/api/v1/assets")).json();
  const id = assets.items[0].asset_id;
  await control(request, "roles?enabled=false");
  try {
    await page
      .getByRole("navigation")
      .getByRole("link", { name: "我的项目" })
      .click();
    await expect(page.getByRole("link", { name: /空白工作室/ })).toBeVisible();
    await expect(page.getByRole("link", { name: /盘丝洞/ })).toHaveCount(0);
    await page.goto(`/#/assets/${id}`);
    await expect(page.getByRole("alert")).toContainText(
      "内容不存在，或你没有查看权限",
    );
    await expect(page.getByRole("heading", { name: "著录信息" })).toHaveCount(
      0,
    );
    await shot(page, info, "permission");
    await page
      .getByRole("navigation")
      .getByRole("link", { name: "资源库" })
      .click();
    await expect(
      page.getByRole("heading", { name: "暂无可见资源" }),
    ).toBeVisible();
  } finally {
    await control(request, "roles?enabled=true");
  }
});

test("loading, server failure, retry and late response after navigation", async ({
  page,
  request,
}, info) => {
  await page.route("**/api/v1/whoami", async (route) => {
    await route.fulfill({
      status: 503,
      json: { error: { code: "SERVER_ERROR" } },
    });
  });
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "暂时无法连接兰台" }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "打开受保护的设置页" }),
  ).toHaveAttribute("href", "/setup/");
  await page.unroute("**/api/v1/whoami");
  await page.getByRole("button", { name: "重试" }).click();
  await expect(page.getByRole("heading", { name: "登录兰台" })).toBeVisible();
  await login(page, request);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/v1/assets?*", async (route) => {
    await gate;
    await route.fulfill({
      status: 503,
      json: { error: { code: "SERVER_ERROR" } },
    });
  });
  await page
    .getByRole("navigation")
    .getByRole("link", { name: "资源库" })
    .click();
  await expect(page.getByRole("status")).toContainText("正在读取");
  release();
  await expect(page.getByRole("alert")).toContainText("服务暂时不可用");
  await shot(page, info, "error");
  await page.unroute("**/api/v1/assets?*");
  await page.getByRole("button", { name: "重试", exact: true }).click();
  await expect(page.locator(".asset-list li")).toHaveCount(24);
  // Delay a response while leaving its view. Its data must never replace the
  // new page, even if cancellation reaches the server too late.
  const later = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/v1/assets?*", async (route) => {
    await later;
    await route.fulfill({
      json: { items: [{ asset_id: "late", title: "不应显示的迟到数据" }] },
    });
  });
  await page.getByRole("button", { name: "刷新", exact: true }).click();
  await expect(page.getByRole("status")).toBeVisible();
  await page
    .getByRole("navigation")
    .getByRole("link", { name: "我的项目" })
    .click();
  release();
  await expect(page.getByRole("heading", { name: "我的项目" })).toBeVisible();
  await expect(page.getByText("不应显示的迟到数据")).toHaveCount(0);
});

test("real expiry clears protected UI and cannot silently reauthenticate", async ({
  page,
  request,
}, info) => {
  await login(page, request);
  await expect(page.locator(".asset-list li")).toHaveCount(6);
  let logins = 0;
  page.on("request", (request) => {
    if (request.url().endsWith("/sessions/login")) logins++;
  });
  await control(request, "expire");
  // An in-flight read may already have cleared navigation after real expiry.
  // Trigger the next route without requiring an obsolete control to survive.
  await page.evaluate(() => {
    location.hash = "/projects";
  });
  await expect(page.getByRole("heading", { name: "登录兰台" })).toBeVisible();
  await expect(page.getByRole("status")).toContainText("会话已过期");
  await expect(page.getByRole("navigation")).toHaveCount(0);
  expect(await page.evaluate(() => sessionStorage.length)).toBe(0);
  expect(logins).toBe(0);
  await shot(page, info, "expired");
});

test("server enforces exact Origin and CSRF; new tabs retain read-only browsing", async ({
  page,
  request,
  context,
}) => {
  await login(page, request);
  const { value } = JSON.parse(
    (await page.evaluate(() => sessionStorage.getItem("lantai.csrf.v1")))!,
  );
  const wrongOrigin = await context.request.delete("/api/v1/sessions/current", {
    headers: { Origin: "https://untrusted.invalid", "X-CSRF-Token": value },
  });
  expect(wrongOrigin.status()).toBe(403);
  const wrongCSRF = await context.request.delete("/api/v1/sessions/current", {
    headers: { Origin: "https://localhost:4180", "X-CSRF-Token": "invalid" },
  });
  expect(wrongCSRF.status()).toBe(403);
  const tab = await context.newPage();
  await tab.goto("/#/assets");
  await expect(tab.getByRole("button", { name: "重新验证" })).toBeVisible();
  await expect(tab.locator(".asset-list li")).toHaveCount(24);
  await tab.close();
  // Privacy modes may disable storage after login. The same session's
  // in-memory CSRF value must still support a real server-side logout.
  await page.evaluate(() => {
    Object.defineProperty(window, "sessionStorage", {
      get() {
        throw new DOMException("Storage disabled", "SecurityError");
      },
    });
    window.dispatchEvent(new Event("focus"));
  });
  await page.getByRole("button", { name: "退出", exact: true }).click();
  await expect(page.getByRole("heading", { name: "登录兰台" })).toBeVisible();
});

test("responsive portrait/landscape, long filenames, touch targets and keyboard focus", async ({
  page,
  request,
}, info) => {
  await login(page, request);
  await page
    .getByRole("navigation")
    .getByRole("link", { name: "资源库" })
    .click();
  await page.getByLabel("搜索资源").fill("声音素材");
  await page.getByRole("button", { name: "搜索", exact: true }).click();
  await page.getByRole("link", { name: /声音素材索引/ }).click();
  const longFile = page.getByRole("cell", {
    name: /very-long-multilingual-filename/,
  });
  await expect(longFile).toBeVisible();
  const initial = page.viewportSize()!;
  const orientations =
    info.project.name === "chromium"
      ? [initial]
      : [initial, { width: initial.height, height: initial.width }];
  for (const [index, size] of orientations.entries()) {
    await page.setViewportSize(size);
    await expect(longFile).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    const box = await longFile.boundingBox();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(size.width);
    const navigation = page
      .getByRole("navigation")
      .getByRole("link", { name: "首页" });
    expect((await navigation.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    await shot(
      page,
      info,
      index === 0 ? "long-file-portrait" : "long-file-landscape",
    );
  }
  await page.getByRole("link", { name: "跳到主要内容" }).focus();
  await page.keyboard.press("Enter");
  await expect(page.locator("main")).toBeFocused();
  await expect(
    page.getByRole("heading", { name: "声音素材索引" }),
  ).toBeVisible();
});

test("WCAG accessibility for login, home, search and fixed detail", async ({
  page,
  request,
}) => {
  const audit = async () => {
    const result = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
      .analyze();
    expect(
      result.violations.map((v) => ({
        id: v.id,
        nodes: v.nodes.map((n) => ({
          target: n.target,
          failure: n.failureSummary,
        })),
      })),
    ).toEqual([]);
  };
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "登录兰台" })).toBeVisible();
  await audit();
  await login(page, request);
  await expect(page.locator(".asset-list li")).toHaveCount(6);
  await audit();
  await page
    .getByRole("navigation")
    .getByRole("link", { name: "资源库" })
    .click();
  await expect(page.locator(".asset-list li")).toHaveCount(24);
  await audit();
  await page.locator(".asset-list a").first().click();
  await expect(page.getByRole("heading", { name: "著录信息" })).toBeVisible();
  await expect(page.locator("table tbody tr")).toHaveCount(1);
  await audit();
});
