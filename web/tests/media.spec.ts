import {
  test,
  expect,
  type APIRequestContext,
  type Page,
  type TestInfo,
} from "@playwright/test";
import { mkdir } from "node:fs/promises";
import path from "node:path";
import AxeBuilder from "@axe-core/playwright";
async function control(request: APIRequestContext, action: string) {
  const response = await request.post(`/__fixture/${action}`, {
    headers: { "X-Lantai-Fixture": "1" },
  });
  expect(response.ok()).toBeTruthy();
  return response.json();
}
async function login(page: Page, request: APIRequestContext) {
  const assets = await control(request, "media");
  const user = await control(request, "login");
  await page.goto("/");
  await page.getByLabel("账户名称").fill(user.name);
  await page.getByLabel("口令", { exact: true }).fill(user.password);
  await page.getByLabel("验证器动态码").fill(user.code);
  await page.getByRole("button", { name: "登录并浏览" }).click();
  await expect(page.getByRole("heading", { name: "你好，Ada" })).toBeVisible();
  return assets as Record<string, string>;
}
async function open(page: Page, id: string, file?: string) {
  await page.goto(`/#/assets/${id}`);
  await expect(
    page.getByRole("heading", { name: "文件预览与下载" }),
  ).toBeVisible();
  if (file) await page.getByLabel("选择版本文件").selectOption(file);
}
async function preview(page: Page) {
  await page.getByRole("button", { name: "预览文件", exact: true }).click();
}
async function shot(page: Page, info: TestInfo, name: string) {
  if (process.env.LANTAI_WEB_EVIDENCE) {
    await mkdir(process.env.LANTAI_WEB_EVIDENCE, { recursive: true });
    await page.screenshot({
      path: path.join(
        process.env.LANTAI_WEB_EVIDENCE,
        `${info.project.name}-${name}.png`,
      ),
      fullPage: true,
    });
  }
}
test.afterEach(async ({ request }) => {
  await control(request, "media-off");
});

test("real authorized PNG, WAV and MP4 previews load on demand with native controls", async ({
  page,
  request,
}, info) => {
  const assets = await login(page, request);
  let grants = 0;
  const transfers: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("/read-grants")) grants++;
    if (r.url().includes("/xfer/")) transfers.push(r.headers().range || "full");
  });
  await open(page, assets.image);
  expect(grants).toBe(0);
  expect(
    (await page.getByLabel("选择版本文件").boundingBox())!.height,
  ).toBeGreaterThanOrEqual(44);
  await preview(page);
  await expect
    .poll(() =>
      page
        .locator(".media-image")
        .evaluate((image: HTMLImageElement) => image.naturalWidth),
    )
    .toBe(160);
  await shot(page, info, "media-image");
  for (const kind of ["audio", "video"]) {
    await open(page, assets[kind]);
    await preview(page);
    const media = page.locator(`.media-${kind}`);
    await expect
      .poll(() => media.evaluate((el: HTMLMediaElement) => el.readyState))
      .toBeGreaterThan(0);
    expect(await media.getAttribute("preload")).toBe("metadata");
    expect(await media.getAttribute("autoplay")).toBeNull();
    await media.evaluate(async (el: HTMLMediaElement) => {
      el.muted = true;
      await el.play();
    });
    await expect
      .poll(() => media.evaluate((el: HTMLMediaElement) => el.currentTime))
      .toBeGreaterThan(0);
    await shot(page, info, `media-${kind}`);
    await page.getByRole("button", { name: "关闭预览" }).click();
    await expect(media).toHaveCount(0);
  }
  expect(grants).toBe(3);
  expect(transfers.some((range) => range.startsWith("bytes="))).toBeTruthy();
});

test("safe Markdown, code and configuration render without executing or fetching embedded content", async ({
  page,
  request,
}, info) => {
  const assets = await login(page, request);
  const outside: string[] = [];
  page.on("request", (r) => {
    if (!r.url().startsWith("https://localhost:4180/")) outside.push(r.url());
  });
  await open(page, assets.text, "readme.md");
  await preview(page);
  await expect(
    page
      .locator(".markdown-preview")
      .getByRole("heading", { name: "安全阅读" }),
  ).toBeVisible();
  await expect(
    page.locator(
      ".markdown-preview img, .markdown-preview script, .markdown-preview iframe, .markdown-preview a",
    ),
  ).toHaveCount(0);
  expect(outside).toEqual([]);
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
  await page.getByRole("button", { name: "查看源码" }).click();
  await expect(page.locator(".code-preview")).toContainText("<script>");
  expect(await page.evaluate(() => "previewExecuted" in window)).toBe(false);
  await page.evaluate(() => {
    navigator.clipboard.writeText = async (value) => {
      (window as unknown as { copied: string }).copied = value;
    };
  });
  await page.getByRole("button", { name: "复制当前预览" }).click();
  await expect(page.getByRole("status")).toContainText("已复制当前预览文本");
  expect(
    await page.evaluate(() => (window as unknown as { copied: string }).copied),
  ).toContain("安全阅读");
  for (const file of [
    "source.py",
    "main.go",
    "index.html",
    "config.json",
    "config.yaml",
    "config.toml",
  ]) {
    await page.getByLabel("选择版本文件").selectOption(file);
    await preview(page);
    await expect(page.locator(".code-preview .line-number").first()).toHaveText(
      "1",
    );
    await expect(
      page.locator(".code-preview [class*=hljs-]").first(),
    ).toBeVisible();
  }
  expect(await page.evaluate(() => "previewExecuted" in window)).toBe(false);
  expect(outside).toEqual([]);
  await shot(page, info, "text-code");
  if (info.project.name !== "chromium") {
    const viewport = page.viewportSize()!;
    await page.setViewportSize({
      width: viewport.height,
      height: viewport.width,
    });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await shot(page, info, "text-code-landscape");
  }
  expect(
    (
      await new AxeBuilder({ page })
        .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
        .analyze()
    ).violations,
  ).toEqual([]);
});

test("text ranges are bounded, explicitly truncated and expanded only on demand", async ({
  page,
  request,
}, info) => {
  const assets = await login(page, request);
  const ranges: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("/xfer/")) ranges.push(r.headers().range || "");
  });
  await open(page, assets.text, "long.txt");
  await preview(page);
  await expect(
    page.getByText("仅预览前 64 KiB；后续内容已截断。"),
  ).toBeVisible();
  expect(ranges).toEqual(["bytes=0-65535"]);
  await page.getByRole("button", { name: "读取更多（最多 256 KiB）" }).click();
  await expect(
    page.getByText("仅预览前 128 KiB；后续内容已截断。"),
  ).toBeVisible();
  expect(ranges.at(-1)).toBe("bytes=0-131071");
  expect(await page.locator(".code-line").count()).toBeLessThanOrEqual(2000);
  await shot(page, info, "text-truncated");
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});

test("archives and unknown binaries never preview; encoding and decoder errors degrade safely", async ({
  page,
  request,
}, info) => {
  const assets = await login(page, request);
  let grants = 0;
  page.on("request", (r) => {
    if (r.url().includes("/read-grants")) grants++;
  });
  for (const file of ["archive.zip", "binary.bin"]) {
    await open(page, assets.text, file);
    await expect(page.getByText(/此格式不支持预览/)).toBeVisible();
    await expect(
      page.getByRole("button", { name: "预览文件", exact: true }),
    ).toHaveCount(0);
  }
  expect(grants).toBe(0);
  await shot(page, info, "binary-unsupported");
  await page.getByLabel("选择版本文件").selectOption("unknown.txt");
  await preview(page);
  await expect(page.getByRole("alert")).toContainText("无法按 UTF-8 安全读取");
  await page.getByLabel("选择版本文件").selectOption("broken.png");
  await preview(page);
  await expect(page.getByText(/浏览器不支持此文件的格式或编码/)).toBeVisible();
  const prior = grants;
  await page.route(
    (url) => /\/versions\/[A-Z0-9]+$/.test(url.pathname),
    async (route) => {
      const response = await route.fetch();
      const data = await response.json();
      data.manifest.content.files[0].size = 17 * 1024 * 1024;
      await route.fulfill({ response, json: data });
    },
  );
  await open(page, assets.image);
  await expect(page.getByText(/图片超过 16 MiB/)).toBeVisible();
  await expect(
    page.getByRole("button", { name: "预览文件", exact: true }),
  ).toHaveCount(0);
  expect(grants).toBe(prior);
});

test("download and preview grants stay bound to the session and recheck revocation", async ({
  page,
  request,
  playwright,
}) => {
  const assets = await login(page, request);
  await open(page, assets.image);
  await preview(page);
  await expect(page.locator(".media-image")).toBeVisible();
  const url = (await page.locator(".media-image").getAttribute("src"))!;
  const stranger = await playwright.request.newContext({
    ignoreHTTPSErrors: true,
  });
  expect((await stranger.get(url)).status()).toBe(401);
  await stranger.dispose();
  await page.getByRole("button", { name: "申请下载原文件" }).click();
  const link = page.getByRole("link", { name: "下载原文件", exact: true });
  await expect(link).toBeVisible();
  const download = await page
    .context()
    .request.get((await link.getAttribute("href"))!);
  expect(download.ok()).toBe(true);
  expect(download.headers()["content-disposition"]).toContain("attachment");
  const event = page.waitForEvent("download");
  await link.click();
  expect((await event).suggestedFilename()).toBe("sample.png");
  await control(request, "media-off");
  expect(
    (
      await page.context().request.get(url, { headers: { Range: "bytes=0-1" } })
    ).status(),
  ).toBe(404);
  await page.getByRole("button", { name: "重新读取", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("内容不存在");
  await expect(page.locator(".media-image")).toHaveCount(0);
});

test("untrusted transfer URLs and ignored ranges fail closed without an external request", async ({
  page,
  request,
}) => {
  const assets = await login(page, request);
  await open(page, assets.text, "long.txt");
  let external = 0;
  page.on("request", (r) => {
    if (r.url().startsWith("https://example.invalid/")) external++;
  });
  await page.route("**/read-grants", async (route) => {
    const response = await route.fetch();
    const grant = await response.json();
    grant.url = "https://example.invalid/file";
    await route.fulfill({ response, json: grant });
  });
  await preview(page);
  await expect(page.getByRole("alert")).toContainText("无法识别");
  expect(external).toBe(0);
  await page.unroute("**/read-grants");
  await page.route("**/xfer/**", (route) =>
    route.fulfill({ status: 200, body: "unexpected full body" }),
  );
  await preview(page);
  await expect(page.getByRole("alert")).toContainText("避免读取整个大文件");
});

test("preview grants expire visibly, duplicate clicks are bounded and late responses are discarded", async ({
  page,
  request,
}) => {
  const assets = await login(page, request);
  await open(page, assets.image);
  let grants = 0;
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/read-grants", async (route) => {
    grants++;
    await gate;
    const response = await route.fetch();
    const grant = await response.json();
    grant.expires_at = new Date(Date.now() + 1000).toISOString();
    await route.fulfill({ response, json: grant });
  });
  await preview(page);
  await expect(page.getByRole("button", { name: "正在读取…" })).toBeDisabled();
  await page
    .getByRole("button", { name: "正在读取…" })
    .evaluate((button) => (button as HTMLButtonElement).click());
  release();
  await expect(page.locator(".media-image")).toBeVisible();
  expect(grants).toBe(1);
  await expect(page.getByRole("alert")).toContainText("读取授权已到期");
  await expect(page.locator(".media-image")).toHaveCount(0);
  await page.unroute("**/read-grants");
  await open(page, assets.text, "disguised.txt");
  await preview(page);
  await expect(page.getByRole("alert")).toContainText(
    "检测到压缩包或二进制内容",
  );
  let releaseText!: () => void;
  const textGate = new Promise<void>((resolve) => {
    releaseText = resolve;
  });
  let reached = false;
  await page.route("**/xfer/**", async (route) => {
    reached = true;
    await textGate;
    await route
      .fulfill({
        status: 206,
        headers: { "Content-Range": "bytes 0-0/1" },
        body: "late",
      })
      .catch(() => {});
  });
  await page.getByLabel("选择版本文件").selectOption("source.py");
  await preview(page);
  await expect.poll(() => reached).toBe(true);
  await page.getByLabel("选择版本文件").selectOption("binary.bin");
  releaseText();
  await expect(page.getByText(/此格式不支持预览/)).toBeVisible();
  await expect(page.locator(".code-preview")).toHaveCount(0);
});

test("text module load failure stays readable and does not blank the workspace", async ({
  page,
  request,
}) => {
  const assets = await login(page, request);
  await open(page, assets.text, "config.json");
  await page.route("**/assets/text-preview-*.js", (route) => route.abort());
  await preview(page);
  await expect(page.getByRole("alert")).toContainText("文本阅读模块未能加载");
  await expect(page.getByRole("navigation")).toBeVisible();
  await expect(page.getByLabel("选择版本文件")).toBeVisible();
});
