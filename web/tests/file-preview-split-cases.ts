import { expect, type test as baseTest } from "@playwright/test";
import type { APIRequestContext, Page, TestInfo } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { readFile } from "node:fs/promises";

const storageKey = "lantai.ui.file-preview-ratio.v1";
const splitter = (page: Page) =>
  page.getByRole("separator", { name: "调整文件列表与预览比例" });
async function scopedRules(page: Page) {
  return page.evaluate(
    () =>
      Array.from(document.styleSheets)
        .filter(
          (sheet) =>
            sheet.href && new URL(sheet.href).origin === location.origin,
        )
        .flatMap((sheet) => Array.from(sheet.cssRules))
        .filter(
          (rule) =>
            rule instanceof CSSStyleRule &&
            rule.selectorText.startsWith("main:has(#") &&
            rule.style.getPropertyValue("--file-drawer-ratio"),
        ).length,
  );
}
async function ratio(page: Page) {
  const drawer = (await page.locator(".file-drawer").boundingBox())!;
  const workspace = (await page.locator(".workspace").boundingBox())!;
  return drawer.width / workspace.width;
}
async function drag(page: Page, fraction: number, finish = true) {
  const handle = (await splitter(page).boundingBox())!;
  const workspace = (await page.locator(".workspace").boundingBox())!;
  const y = Math.max(150, handle.y + handle.height / 2);
  await page.mouse.move(handle.x + handle.width / 2, y);
  await page.mouse.down();
  await page.mouse.move(workspace.x + workspace.width * (1 - fraction), y, {
    steps: 8,
  });
  if (finish) await page.mouse.up();
}
async function stored(page: Page) {
  return page.evaluate((key) => localStorage.getItem(key), storageKey);
}
async function contained(page: Page) {
  expect(
    await page.evaluate(() => {
      const drawer = document.querySelector(".file-drawer")!;
      return (
        document.documentElement.scrollWidth <= innerWidth &&
        drawer.scrollWidth <= drawer.clientWidth
      );
    }),
  ).toBe(true);
}

export function registerPreviewSplitCases(
  test: typeof baseTest,
  helpers: {
    login: (
      page: Page,
      request: APIRequestContext,
    ) => Promise<Record<string, string>>;
    open: (page: Page, id: string) => Promise<void>;
    choose: (page: Page, file: string) => Promise<void>;
    shot: (page: Page, info: TestInfo, name: string) => Promise<void>;
  },
) {
  test("manual split drags to both limits, releases capture and shows two user proportions", async ({
    page,
    request,
  }, info) => {
    const assets = await helpers.login(page, request);
    await page.setViewportSize({ width: 2047, height: 1000 });
    await helpers.open(page, assets.image);
    if (process.env.LANTAI_PREVIEW_MOCKS === "1")
      await helpers.choose(page, "wide.png");
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect(page.locator(".media-image")).toBeVisible();
    await expect(splitter(page)).toBeVisible();
    await drag(page, 0.6);
    expect(await ratio(page)).toBeCloseTo(0.6, 2);
    await page.evaluate(() => window.scrollTo(0, 0));
    await helpers.shot(page, info, "manual-preview-60-percent");
    await drag(page, 0.3);
    expect(await ratio(page)).toBeCloseTo(0.3, 2);
    await helpers.shot(page, info, "manual-preview-30-percent");
    for (const desired of [2, -1]) {
      await drag(page, desired, false);
      await page.mouse.move(desired > 0 ? -80 : 3500, 200);
      await page.mouse.up();
      const list = (await page.locator(".version-files").boundingBox())!;
      const drawer = (await page.locator(".file-drawer").boundingBox())!;
      expect(list.width).toBeGreaterThanOrEqual(359);
      expect(drawer.width).toBeGreaterThanOrEqual(359);
      await contained(page);
      const released = await ratio(page);
      await page.mouse.move(1200, 600);
      expect(await ratio(page)).toBeCloseTo(released, 5);
    }
    expect(await page.evaluate(() => getSelection()?.toString())).toBe("");
    expect(await page.getByLabel("自动预览", { exact: true }).isChecked()).toBe(
      true,
    );
  });

  test("manual split keyboard, reset, refresh, resource switches and close reopen retain the preference", async ({
    page,
    request,
  }) => {
    const errors: string[] = [];
    page.on("pageerror", (e) => errors.push(e.message));
    page.on("console", (message) => {
      if (message.type() === "error") errors.push(message.text());
    });
    const assets = await helpers.login(page, request);
    if (process.env.LANTAI_PREVIEW_MOCKS === "1") {
      const caddy = await readFile(
        new URL("../../deploy/Caddyfile", import.meta.url),
        "utf8",
      );
      const policy = caddy.match(/Content-Security-Policy "([^"]+)"/)![1];
      await page.route("http://127.0.0.1:4188/", async (route) => {
        const response = await route.fetch();
        await route.fulfill({
          response,
          headers: { ...response.headers(), "content-security-policy": policy },
        });
      });
    }
    await page.setViewportSize({ width: 2047, height: 1000 });
    await helpers.open(page, assets.text);
    expect(await scopedRules(page)).toBe(1);
    await splitter(page).focus();
    await expect(splitter(page)).toBeFocused();
    await expect(splitter(page)).toHaveAttribute(
      "aria-orientation",
      "vertical",
    );
    const initial = await ratio(page);
    await page.keyboard.press("ArrowLeft");
    expect(await ratio(page)).toBeGreaterThan(initial);
    await expect(splitter(page)).toHaveCSS("outline-style", "solid");
    await page.keyboard.press("ArrowRight");
    expect(await ratio(page)).toBeCloseTo(initial, 3);
    await page.keyboard.press("End");
    expect(await ratio(page)).toBeGreaterThan(0.7);
    await page.keyboard.press("Home");
    expect(await ratio(page)).toBeLessThan(0.21);
    await page.getByRole("button", { name: "恢复默认比例" }).click();
    expect(await ratio(page)).toBeCloseTo(0.4, 3);
    expect(await stored(page)).toBeNull();
    await drag(page, 0.58);
    const saved = Number(await stored(page));
    expect(saved).toBeCloseTo(0.58, 3);
    await page.reload();
    await expect(splitter(page)).toBeVisible();
    expect(await ratio(page)).toBeCloseTo(saved, 3);
    await helpers.choose(page, "source.py");
    expect(await ratio(page)).toBeCloseTo(saved, 3);
    await page.getByRole("button", { name: "收起预览" }).click();
    await expect(splitter(page)).toHaveCount(0);
    expect(await scopedRules(page)).toBe(0);
    await page.getByRole("button", { name: "打开预览" }).click();
    expect(await ratio(page)).toBeCloseTo(saved, 3);
    expect(await scopedRules(page)).toBe(1);
    // No unrelated native image-tool select: its existing WebKit/CSP warning
    // is retained in the separate r2/r3 baseline diagnostic.
    await helpers.open(page, assets.unsupported);
    expect(await ratio(page)).toBeCloseTo(saved, 3);
    expect(await scopedRules(page)).toBe(1);
    expect(await page.locator("main").getAttribute("style")).toBeNull();
    await splitter(page).dblclick();
    expect(await ratio(page)).toBeCloseTo(0.4, 3);
    expect(await stored(page)).toBeNull();
    expect(
      errors,
      "CSP application interactions before accessibility audit",
    ).toEqual([]);
    expect(
      (
        await new AxeBuilder({ page })
          .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
          .analyze()
      ).violations,
    ).toEqual([]);
    expect(errors).toEqual([]);
  });

  test("manual split keeps the chosen ratio through resize clamps and narrow layout", async ({
    page,
    request,
  }) => {
    const assets = await helpers.login(page, request);
    await page.setViewportSize({ width: 3412, height: 1000 });
    await helpers.open(page, assets.text);
    await drag(page, 0.8);
    const saved = await stored(page);
    for (let width = 3412; width >= 360; width -= 80) {
      await page.setViewportSize({ width, height: 1000 });
      await contained(page);
      if (width >= 1280) {
        await expect(splitter(page)).toBeVisible();
        expect(
          (await page.locator(".version-files").boundingBox())!.width,
        ).toBeGreaterThanOrEqual(359);
        expect(
          (await page.locator(".file-drawer").boundingBox())!.width,
        ).toBeGreaterThanOrEqual(359);
      } else await expect(splitter(page)).toBeHidden();
      expect(await stored(page)).toBe(saved);
    }
    for (const width of [1280, 1279, 760, 600, 390, 360, 2047, 3412]) {
      await page.setViewportSize({ width, height: 1000 });
      await contained(page);
      expect(await stored(page)).toBe(saved);
    }
    expect(await ratio(page)).toBeCloseTo(0.8, 3);
    await page.getByLabel("自动预览", { exact: true }).check();
    for (const file of ["readme.md", "source.py", "config.json"]) {
      await helpers.choose(page, file);
      await expect(
        page.locator(
          file === "readme.md" ? ".markdown-preview" : ".code-preview",
        ),
      ).toBeVisible();
      await contained(page);
    }
  });

  test("manual split cancels the active drag without saving and stops on narrow resize", async ({
    page,
    request,
  }) => {
    const assets = await helpers.login(page, request);
    await page.setViewportSize({ width: 2047, height: 1000 });
    await helpers.open(page, assets.text);
    await drag(page, 0.55);
    const saved = await stored(page);
    await splitter(page).evaluate((handle) => {
      handle.addEventListener("gotpointercapture", (event) => {
        handle.setAttribute(
          "data-test-pointer",
          String((event as PointerEvent).pointerId),
        );
      });
    });
    await drag(page, 0.7, false);
    await expect(splitter(page)).toHaveAttribute("data-test-pointer", /\d+/);
    const pointerId = Number(
      await splitter(page).getAttribute("data-test-pointer"),
    );
    expect(
      await splitter(page).evaluate(
        (h, id) => h.hasPointerCapture(id),
        pointerId,
      ),
    ).toBe(true);
    await page.keyboard.press("ArrowLeft");
    await page.keyboard.press("Home");
    await page.keyboard.press("End");
    expect(await stored(page)).toBe(saved);
    await splitter(page).dispatchEvent("pointercancel", { pointerId });
    await page.mouse.up();
    expect(await stored(page)).toBe(saved);
    expect(await ratio(page)).toBeCloseTo(0.55, 3);
    expect(
      await splitter(page).evaluate(
        (h, id) => h.hasPointerCapture(id),
        pointerId,
      ),
    ).toBe(false);
    await drag(page, 0.65, false);
    await page.keyboard.press("Escape");
    await page.mouse.up();
    expect(await stored(page)).toBe(saved);
    expect(await ratio(page)).toBeCloseTo(0.55, 3);
    await expect(page.locator(".file-drawer")).toHaveCount(1);
    await drag(page, 0.65, false);
    const captured = Number(
      await splitter(page).getAttribute("data-test-pointer"),
    );
    await splitter(page).evaluate(
      (h, id) => h.releasePointerCapture(id),
      captured,
    );
    await page.mouse.move(1200, 500);
    await page.mouse.up();
    expect(await stored(page)).toBe(saved);
    expect(await ratio(page)).toBeCloseTo(0.55, 3);
    await drag(page, 0.65, false);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.mouse.up();
    await expect(splitter(page)).toBeHidden();
    await page.setViewportSize({ width: 2047, height: 1000 });
    expect(await stored(page)).toBe(saved);
    expect(await ratio(page)).toBeCloseTo(0.55, 3);
    await page.mouse.move(1400, 400);
    expect(await stored(page)).toBe(saved);
  });

  test("manual split rejects invalid stored values and remains usable when storage is blocked", async ({
    page,
    request,
  }) => {
    const assets = await helpers.login(page, request);
    await page.setViewportSize({ width: 2047, height: 1000 });
    await helpers.open(page, assets.text);
    for (const invalid of [
      "garbage",
      "NaN",
      "Infinity",
      "0",
      "1",
      "-0.2",
      '{"ratio":0.5}',
    ]) {
      await page.evaluate(
        ([key, value]) => localStorage.setItem(key, value),
        [storageKey, invalid],
      );
      await page.reload();
      await expect(splitter(page)).toBeVisible();
      expect(await ratio(page)).toBeCloseTo(0.4, 3);
    }
    await page.addInitScript(() => {
      Object.defineProperty(window, "localStorage", {
        get() {
          throw new DOMException("blocked", "SecurityError");
        },
      });
    });
    await page.reload();
    await expect(splitter(page)).toBeVisible();
    expect(await ratio(page)).toBeCloseTo(0.4, 3);
    await drag(page, 0.6);
    expect(await ratio(page)).toBeCloseTo(0.6, 3);
    await page.getByRole("button", { name: "恢复默认比例" }).click();
    expect(await ratio(page)).toBeCloseTo(0.4, 3);
    await page.addInitScript(() => {
      CSSStyleSheet.prototype.insertRule = () => {
        throw new DOMException("blocked", "SecurityError");
      };
    });
    await page.reload();
    await expect(
      page.getByText("此浏览器暂时无法调整比例，已使用默认布局"),
    ).toBeVisible();
    await expect(splitter(page)).toBeHidden();
    await expect(
      page.getByRole("button", { name: "恢复默认比例" }),
    ).toBeDisabled();
    await page.getByLabel("自动预览", { exact: true }).check();
    await expect(page.locator(".markdown-preview")).toBeVisible();
  });
}
