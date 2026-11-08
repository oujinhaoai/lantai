import { test as baseTest, expect } from "@playwright/test";
import type {
  APIRequestContext,
  Page,
  TestInfo,
  Route,
} from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { mkdir, writeFile } from "node:fs/promises";
import path from "node:path";
import { previewMockVersion, previewMocks } from "./file-preview-mocks";
import { registerPreviewSplitCases } from "./file-preview-split-cases";

async function control(request: APIRequestContext, action: string) {
  const response = await request.post(`/__fixture/${action}`, {
    headers: { "X-Lantai-Fixture": "1" },
  });
  expect(response.ok()).toBeTruthy();
  return response.json();
}
async function login(page: Page, request: APIRequestContext) {
  if (process.env.LANTAI_PREVIEW_MOCKS === "1") return previewMocks(page);
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
async function open(page: Page, id: string) {
  await page.goto(`/#/assets/${id}`);
  await expect(
    page.getByRole("heading", { name: "文件预览与下载" }),
  ).toBeVisible();
}
async function choose(page: Page, file: string) {
  const button = page.getByRole("button", { name: file, exact: true });
  // Capture only DOM input/scroll metadata, never credentials or requests.
  const diagnostic = await page.evaluateHandle(() => {
    const events: object[] = [];
    const types = [
      "pointerdown",
      "pointerup",
      "pointercancel",
      "touchstart",
      "touchend",
      "mousedown",
      "mouseup",
      "click",
      "focusin",
      "scroll",
    ];
    const capture = (event: Event) => {
      const target = event.target instanceof Element ? event.target : null;
      if (events.length >= 80) events.shift();
      events.push({
        type: event.type,
        target: target?.tagName,
        label: target?.closest("button")?.getAttribute("aria-label"),
        x: "clientX" in event ? event.clientX : null,
        y: "clientY" in event ? event.clientY : null,
        scrollY,
        listScroll: document.querySelector(".version-file-list")?.scrollTop,
        selected: document
          .querySelector('.version-file-row[aria-pressed="true"]')
          ?.getAttribute("aria-label"),
      });
    };
    types.forEach((type) => document.addEventListener(type, capture, true));
    return {
      events,
      stop: () =>
        types.forEach((type) =>
          document.removeEventListener(type, capture, true),
        ),
    };
  });
  try {
    // Touch projects exercise a touch activation, not a synthetic mouse hover.
    await button.scrollIntoViewIfNeeded();
    if (baseTest.info().project.use.hasTouch) await button.tap();
    else await button.click();
    await expect(button).toHaveAttribute("aria-pressed", "true");
  } catch (error) {
    await baseTest.info().attach("file-selection-events", {
      body: JSON.stringify(
        { file, events: await diagnostic.evaluate((value) => value.events) },
        null,
        2,
      ),
      contentType: "application/json",
    });
    throw error;
  } finally {
    await diagnostic.evaluate((value) => value.stop());
    await diagnostic.dispose();
  }
}

const screenshotWarnings = new WeakMap<Page, string[]>();
function captureScreenshotWarning(page: Page, message: string) {
  const warnings = screenshotWarnings.get(page);
  // Playwright WebKit inserts `body {}` while synchronizing screenshots.
  // Record this exact harness warning only inside page.screenshot; identical
  // messages during application interactions still fail the test.
  if (
    warnings &&
    page.context().browser()?.browserType().name() === "webkit" &&
    message ===
      "Refused to apply a stylesheet because its hash, its nonce, or 'unsafe-inline' does not appear in the style-src directive of the Content Security Policy."
  ) {
    warnings.push(message);
    return true;
  }
  return false;
}
async function shot(page: Page, info: TestInfo, name: string) {
  if (!process.env.LANTAI_WEB_EVIDENCE) return;
  await mkdir(process.env.LANTAI_WEB_EVIDENCE, { recursive: true });
  await page.getByRole("heading", { name: "文件预览", exact: true }).click();
  const warnings: string[] = [];
  screenshotWarnings.set(page, warnings);
  try {
    await page.screenshot({
      caret: "initial",
      path: path.join(
        process.env.LANTAI_WEB_EVIDENCE,
        `${info.project.name}-${name}.png`,
      ),
      fullPage: name.startsWith("mobile"),
    });
  } finally {
    screenshotWarnings.delete(page);
  }
  if (warnings.length)
    await info.attach(`${name}-screenshot-harness-warnings`, {
      body: JSON.stringify(warnings),
      contentType: "application/json",
    });
  await info.attach(name, {
    path: path.join(
      process.env.LANTAI_WEB_EVIDENCE,
      `${info.project.name}-${name}.png`,
    ),
    contentType: "image/png",
  });
}
export function registerFilePreviewCases(test: typeof baseTest) {
  registerPreviewSplitCases(test, { login, open, choose, shot });
  test.beforeEach(async ({ baseURL }, info) => {
    if (
      process.env.LANTAI_PREVIEW_MOCKS === "1" &&
      (baseURL !== "http://127.0.0.1:4188" ||
        !info.project.name.endsWith("-preview"))
    )
      throw new Error(
        "Synthetic frontend responses are forbidden in the HTTPS integration entry.",
      );
  });
  test("closing cancels a pending grant or byte transfer before late content can mount", async ({
    page,
    request,
  }) => {
    const assets = await login(page, request);
    await open(page, assets.text);
    let aborted = 0;
    page.on("requestfailed", (request) => {
      if (
        request.url().includes("/read-grants") ||
        request.url().includes("/xfer/")
      )
        aborted++;
    });
    for (const pattern of ["**/read-grants", "**/xfer/**"]) {
      let release!: () => void;
      let reached = false;
      const gate = new Promise<void>((resolve) => {
        release = resolve;
      });
      const pendingRoute = async (route: Route) => {
        reached = true;
        await gate;
        await route.fallback().catch(() => {});
      };
      await page.route(pattern, pendingRoute);
      await choose(page, "source.py");
      await page.getByLabel("自动预览", { exact: true }).check();
      await expect.poll(() => reached).toBe(true);
      const prior = aborted;
      await page.getByRole("button", { name: "收起预览" }).click();
      await expect.poll(() => aborted).toBeGreaterThan(prior);
      release();
      await page.unroute(pattern, pendingRoute);
      await expect(page.locator(".file-drawer, .code-preview")).toHaveCount(0);
      await choose(page, "config.json");
      await expect(page.locator(".code-preview")).toContainText("合成项目");
      await page.getByLabel("自动预览", { exact: true }).uncheck();
    }
  });
  test("closing during the automatic-preview delay cancels the scheduled read", async ({
    page,
    request,
  }) => {
    const assets = await login(page, request);
    await open(page, assets.text);
    let reads = 0;
    page.on("request", (request) => {
      if (request.url().includes("/read-grants")) reads++;
    });
    await page.clock.install();
    await page.clock.pauseAt(new Date());
    await page.getByLabel("自动预览", { exact: true }).check();
    await page.getByRole("button", { name: "收起预览" }).click();
    await page.clock.runFor(500);
    expect(reads).toBe(0);
    await expect(page.locator(".file-drawer")).toHaveCount(0);
  });
  test.afterEach(async ({ request }) => {
    if (process.env.LANTAI_PREVIEW_MOCKS !== "1")
      await control(request, "media-off");
  });

  test("preview width follows the available workspace during continuous resizing", async ({
    page,
    request,
  }, info) => {
    const assets = await login(page, request);
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => {
      if (
        message.type() === "error" &&
        !captureScreenshotWarning(page, message.text())
      )
        errors.push(message.text());
    });
    await page.setViewportSize({ width: 2047, height: 813 });
    await open(page, assets.image);
    if (process.env.LANTAI_PREVIEW_MOCKS === "1")
      await choose(page, "wide.png");
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect
      .poll(() =>
        page
          .locator(".media-image")
          .evaluate((image: HTMLImageElement) => image.naturalWidth),
      )
      .toBeGreaterThan(0);
    await shot(page, info, "responsive-wide-image");
    const widths = [3412, 2560, 2047, 1920, 1440, 1280];
    for (let width = 2560; width >= 360; width -= 80) widths.push(width);
    widths.push(1279, 1100, 760, 600, 390, 360, 1280, 2047);
    const measurements: object[] = [];
    for (const width of widths) {
      await page.setViewportSize({ width, height: 1000 });
      const layout = await page.evaluate(() => {
        const drawer = document.querySelector<HTMLElement>(".file-drawer")!;
        const list = document.querySelector<HTMLElement>(".version-files")!;
        const workspace = document
          .querySelector(".workspace")!
          .getBoundingClientRect();
        const box = drawer.getBoundingClientRect();
        const files = list.getBoundingClientRect();
        return {
          width: innerWidth,
          workspace: workspace.width,
          drawer: box.width,
          position: getComputedStyle(drawer).position,
          gutter: box.x - files.right,
          listWidth: files.width,
          stacked: box.top >= files.bottom,
          fits:
            document.documentElement.scrollWidth <= innerWidth &&
            drawer.scrollWidth <= drawer.clientWidth &&
            box.left >= workspace.left &&
            box.right <= innerWidth,
        };
      });
      measurements.push(layout);
      expect(layout.fits, `contained at ${width}px`).toBe(true);
      if (width >= 1280) {
        expect(layout.position).toBe("fixed");
        expect(layout.drawer / layout.workspace).toBeGreaterThan(0.37);
        expect(layout.drawer / layout.workspace).toBeLessThan(0.43);
        expect(layout.gutter).toBeGreaterThanOrEqual(24);
        expect(layout.gutter).toBeLessThanOrEqual(64);
        expect(layout.listWidth).toBeGreaterThan(360);
      } else {
        expect(layout.position).toBe("static");
        expect(layout.stacked).toBe(true);
        expect(Math.abs(layout.drawer - layout.listWidth)).toBeLessThan(1);
      }
      const image = await page
        .locator(".media-image")
        .evaluate((image: HTMLImageElement) => ({
          width: image.getBoundingClientRect().width,
          height: image.getBoundingClientRect().height,
          ratio: image.naturalWidth / image.naturalHeight,
          available: image.parentElement!.getBoundingClientRect().width,
        }));
      expect(image.width).toBeLessThanOrEqual(image.available);
      expect(Math.abs(image.width / image.height - image.ratio)).toBeLessThan(
        0.01,
      );
    }
    for (const width of [3412, 2047]) {
      await page.setViewportSize({ width, height: 1000 });
      const opened = (await page.locator(".version-files").boundingBox())!;
      await page.getByRole("button", { name: "收起预览" }).click();
      await expect(page.locator(".file-drawer")).toHaveCount(0);
      const closed = (await page.locator(".version-files").boundingBox())!;
      expect(closed.width).toBeGreaterThan(opened.width);
      measurements.push({ width, opened: opened.width, closed: closed.width });
      await page.getByRole("button", { name: "打开预览" }).click();
      await expect(page.locator(".media-image")).toBeVisible();
      const reopened = (await page.locator(".version-files").boundingBox())!;
      expect(Math.abs(reopened.width - opened.width)).toBeLessThan(1);
    }
    await info.attach("responsive-width-measurements", {
      body: JSON.stringify(measurements, null, 2),
      contentType: "application/json",
    });
    if (process.env.LANTAI_WEB_EVIDENCE)
      await writeFile(
        path.join(
          process.env.LANTAI_WEB_EVIDENCE,
          `${info.project.name}-responsive-width-measurements.json`,
        ),
        JSON.stringify(measurements, null, 2),
      );
    await page.evaluate(() => window.scrollTo(0, 0));
    await shot(page, info, "responsive-wide-image-final");
    expect(errors).toEqual([]);
  });

  test("long Markdown and code stay inside the responsive preview while file selection is retained", async ({
    page,
    request,
  }, info) => {
    const assets = await login(page, request);
    await open(page, assets.text);
    await page.getByLabel("自动预览", { exact: true }).check();
    for (const file of ["readme.md", "source.py", "config.json"]) {
      await choose(page, file);
      const content = page.locator(
        file === "readme.md" ? ".markdown-preview" : ".code-preview",
      );
      await expect(content).toBeVisible();
      for (const width of [3412, 2047, 1440, 1280, 1279, 760, 390, 360]) {
        await page.setViewportSize({ width, height: 1000 });
        expect(
          await page.evaluate(() => {
            const drawer = document.querySelector(".file-drawer")!;
            return (
              document.documentElement.scrollWidth <= innerWidth &&
              drawer.scrollWidth <= drawer.clientWidth
            );
          }),
          `${file} remains contained at ${width}px`,
        ).toBe(true);
        await expect(page.locator(".file-drawer-name")).toHaveText(file);
        await expect(
          page.getByLabel("自动预览", { exact: true }),
        ).toBeChecked();
        const scrollable =
          file === "readme.md" ? content.locator("pre") : content;
        if (file !== "config.json" && process.env.LANTAI_PREVIEW_MOCKS === "1")
          expect(
            await scrollable.evaluate(
              (pre) => pre.scrollWidth > pre.clientWidth,
            ),
          ).toBe(true);
        if (width === 2047 && file === "readme.md")
          await shot(page, info, "responsive-wide-document");
        if (width === 390 && file === "source.py")
          await shot(page, info, "mobile-responsive-code");
      }
    }
  });

  test("searchable single selection, opt-in automatic preview, manual fallback and responsive drawer", async ({
    page,
    request,
  }, info) => {
    const assets = await login(page, request);
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("console", (message) => {
      if (
        message.type() === "error" &&
        !captureScreenshotWarning(page, message.text())
      )
        errors.push(message.text());
    });
    const purposes: string[] = [];
    page.on("request", (r) => {
      if (r.url().includes("/read-grants"))
        purposes.push(r.postDataJSON().purpose);
    });
    await page.setViewportSize({ width: 1440, height: 1024 });
    await open(page, assets.text);
    await expect(
      page.getByLabel("自动预览", { exact: true }),
    ).not.toBeChecked();
    expect(purposes).toEqual([]);
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect(page.locator(".markdown-preview")).toContainText("安全阅读");
    await choose(page, "config.json");
    await expect(page.locator(".code-preview")).toContainText("合成项目");
    await expect(
      page.getByRole("button", { name: "config.json", exact: true }),
    ).toHaveAttribute("aria-pressed", "true");
    await expect(
      page.locator('.version-file-row[aria-pressed="true"]'),
    ).toHaveCount(1);
    expect(purposes).toEqual(["archive_review", "archive_review"]);
    await page.getByLabel("搜索版本文件").fill("DOES-NOT-EXIST");
    await expect(page.locator(".version-files-empty")).toContainText(
      "没有匹配的文件",
    );
    await expect(page.locator(".code-preview")).toContainText("合成项目");
    await page.getByRole("button", { name: "清除搜索" }).click();
    await page.getByLabel("自动预览", { exact: true }).uncheck();
    await expect(page.locator(".code-preview")).toBeVisible();
    await choose(page, "source.py");
    await expect(page.locator(".code-preview")).toHaveCount(0);
    expect(purposes).toHaveLength(2);
    await page.getByRole("button", { name: "预览文件", exact: true }).click();
    await expect(page.locator(".code-preview")).toBeVisible();
    await page.getByRole("button", { name: "收起预览" }).click();
    await expect(
      page.getByRole("complementary", { name: "文件预览" }),
    ).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "source.py", exact: true }),
    ).toBeFocused();
    await choose(page, "readme.md");
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect(page.locator(".markdown-preview")).toBeVisible();
    await shot(page, info, "desktop-document-preview");
    const list = (await page.locator(".version-files").boundingBox())!;
    const drawer = (await page.locator(".file-drawer").boundingBox())!;
    expect(list.x + list.width).toBeLessThanOrEqual(drawer.x);
    expect(await page.locator('[aria-modal="true"]').count()).toBe(0);
    await page.setViewportSize({ width: 390, height: 844 });
    await choose(page, "config.json");
    await expect(page.locator(".code-preview")).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await shot(page, info, "mobile-document-preview");
    await page.setViewportSize({ width: 1440, height: 1024 });
    await open(page, assets.image);
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect
      .poll(() =>
        page
          .locator(".media-image")
          .evaluate((image: HTMLImageElement) => image.naturalWidth),
      )
      .toBe(160);
    const sizes = page.getByRole("group", { name: "代理最长边" });
    await expect(sizes.getByRole("radio", { name: "1024 px" })).toBeChecked();
    await sizes.getByRole("radio", { name: "512 px" }).check();
    await expect(sizes.getByRole("radio", { name: "512 px" })).toBeChecked();
    await sizes.getByRole("radio", { name: "2048 px" }).check();
    await expect(sizes.getByRole("radio", { name: "2048 px" })).toBeChecked();
    await shot(page, info, "desktop-image-preview");
    // Assert application console/CSP errors before Axe injects audit code.
    expect(errors).toEqual([]);
    await page.setViewportSize({ width: 390, height: 844 });
    await open(page, assets.text);
    await page.getByLabel("自动预览", { exact: true }).check();
    await choose(page, "config.json");
    await expect(page.locator(".code-preview")).toBeVisible();
    expect(errors).toEqual([]);
    expect(
      (
        await new AxeBuilder({ page })
          .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
          .analyze()
      ).violations,
    ).toEqual([]);
  });

  test("automatic reads cancel on switching, turning off and closing; late responses cannot replace the selected file", async ({
    page,
    request,
  }) => {
    const assets = await login(page, request);
    await open(page, assets.textCompact || assets.text);
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect(page.locator(".markdown-preview")).toBeVisible();
    let release!: () => void;
    let reached = false;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    await page.route("**/read-grants", async (route) => {
      if (route.request().postDataJSON().path !== "source.py")
        return route.fallback();
      reached = true;
      await gate;
      await route.fallback().catch(() => {});
    });
    await choose(page, "source.py");
    await expect.poll(() => reached).toBe(true);
    await choose(page, "config.json");
    await expect(page.locator(".code-preview")).toContainText("合成项目");
    release();
    await expect(page.locator(".file-drawer-name")).toHaveText("config.json");
    await expect(page.locator(".code-preview")).toContainText("合成项目");
    await page.unroute("**/read-grants");
    let releaseAgain!: () => void;
    reached = false;
    const again = new Promise<void>((resolve) => {
      releaseAgain = resolve;
    });
    await page.route("**/read-grants", async (route) => {
      reached = true;
      await again;
      await route.fallback().catch(() => {});
    });
    await choose(page, "source.py");
    await expect.poll(() => reached).toBe(true);
    await page.getByLabel("自动预览", { exact: true }).uncheck();
    await expect(
      page.getByRole("button", { name: "预览文件", exact: true }),
    ).toBeEnabled();
    releaseAgain();
    await page.unroute("**/read-grants");
    await page.getByRole("button", { name: "预览文件", exact: true }).click();
    await expect(page.locator(".code-preview")).toBeVisible();
    await page.getByRole("button", { name: "收起预览" }).click();
    await expect(page.locator(".file-drawer")).toHaveCount(0);
    await choose(page, "config.json");
    await expect(page.locator(".code-preview")).toHaveCount(0);
  });

  test("unsupported and oversized files never auto-read, preview errors remain retryable and download uses its separate purpose", async ({
    page,
    request,
  }) => {
    const assets = await login(page, request);
    await open(page, assets.text);
    await choose(page, "archive.zip");
    let reads = 0;
    page.on("request", (r) => {
      if (r.url().includes("/read-grants")) reads++;
    });
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect(page.getByText(/此格式不支持通用预览/)).toBeVisible();
    expect(reads).toBe(0);
    await choose(page, "binary.bin");
    expect(reads).toBe(0);
    await choose(page, "disguised.txt");
    await expect(page.getByRole("alert")).toContainText(
      "检测到压缩包或二进制内容",
    );
    expect(reads).toBe(1);
    await page.route("**/read-grants", (route) =>
      route.fulfill({
        status: 403,
        json: { error: { code: "PURPOSE_DENIED" } },
      }),
    );
    await choose(page, "config.json");
    await expect(page.getByRole("alert")).toBeVisible();
    await page.getByLabel("自动预览", { exact: true }).uncheck();
    await page.unroute("**/read-grants");
    await page.getByRole("button", { name: "预览文件", exact: true }).click();
    await expect(page.locator(".code-preview")).toBeVisible();
    await open(page, assets.image);
    const versionURL = (url: URL) =>
      url.pathname.startsWith(`/api/v1/assets/${assets.image}/versions/`);
    await page.route(versionURL, async (route) => {
      const response =
        process.env.LANTAI_PREVIEW_MOCKS === "1"
          ? undefined
          : await route.fetch();
      const data = response
        ? await response.json()
        : previewMockVersion(page, assets.image);
      data.manifest.content.files.find(
        (file: { path: string }) => file.path === "sample.png",
      )!.size = 17 * 1024 * 1024;
      await route.fulfill({ ...(response ? { response } : {}), json: data });
    });
    await page.goto("/#/assets");
    await open(page, assets.image);
    await choose(page, "sample.png");
    const before = reads;
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect(page.getByText(/图片超过 16 MiB/)).toBeVisible();
    expect(reads).toBe(before);
    await page.unroute(versionURL);
    await open(page, assets.restricted);
    const purposes: string[] = [];
    page.on("request", (r) => {
      if (r.url().includes("/read-grants"))
        purposes.push(r.postDataJSON().purpose);
    });
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect(page.locator(".media-image")).toBeVisible();
    await page.getByRole("button", { name: "申请下载原文件" }).click();
    await expect(page.getByRole("alert")).toContainText(
      "此文件不允许用于当前读取用途",
    );
    expect(purposes).toEqual(["archive_review", "raw_export"]);
    for (const kind of ["audio", "video"]) {
      await open(page, assets[kind]);
      await page.getByLabel("自动预览", { exact: true }).check();
      const media = page.locator(`.media-${kind}`);
      await expect(media).toBeVisible();
      expect(await media.getAttribute("autoplay")).toBeNull();
      expect(await media.evaluate((el: HTMLMediaElement) => el.paused)).toBe(
        true,
      );
    }
  });
}
