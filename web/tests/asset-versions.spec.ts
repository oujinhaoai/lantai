import {
  test,
  expect,
  type Page,
  type APIRequestContext,
} from "@playwright/test";

const assetID = "01J0000000000000000000000A";
const otherAssetID = "01J0000000000000000000000B";
const version1 = "01J00000000000000000000011";
const version2 = "01J00000000000000000000012";
const projectID = "01J0000000000000000000000C";
const instanceID = "01J0000000000000000000000D";
const filePath = "工作流/readme.md";
const digest = `sha256:${"a".repeat(64)}`;
const hash = "b".repeat(64);
const bytes = new TextEncoder().encode("# 合成旧版本\n");
const harness = process.env.LANTAI_ASSET_VERSION_HARNESS === "1";
const entry = harness ? "/assets/version-fixture.html" : "/";
const version = (id: string, number: number) => ({
  version_id: id,
  version_number: number,
  asset_id: assetID,
  project_id: projectID,
  committed_at: "2026-01-02T03:04:05.000Z",
  uri: `lantai://${instanceID}/assets/${assetID}/versions/${id}`,
  manifest_digest: digest,
});
const exact = (id: string) => ({
  version: version(id, id === version1 ? 1 : 2),
  manifest: {
    instance_id: instanceID,
    asset_id: assetID,
    project_id: projectID,
    version_id: id,
    version_number: id === version1 ? 1 : 2,
    manifest_digest: digest,
    content: {
      asset_type: "doc",
      version_note: id === version1 ? "旧版工作流 v1" : "新版工作流 v2",
      files: [
        { path: filePath, size: bytes.length, role: "primary", sha256: hash },
      ],
    },
  },
});

async function login(page: Page, request: APIRequestContext) {
  if (harness) return;
  const response = await request.post("/__fixture/login", {
    headers: { "X-Lantai-Fixture": "1" },
  });
  expect(response.ok()).toBeTruthy();
  const user = await response.json();
  await page.goto("/");
  await page.getByLabel("账户名称").fill(user.name);
  await page.getByLabel("口令", { exact: true }).fill(user.password);
  await page.getByLabel("验证器动态码").fill(user.code);
  await page.getByRole("button", { name: "登录并浏览" }).click();
  await expect(page.getByRole("heading", { name: "你好，Ada" })).toBeVisible();
}

async function seed(page: Page) {
  const state = { latest: version1, metadataHidden: false };
  const calls: string[] = [];
  const grants: { version: string; purpose: string }[] = [];
  await page.route(`**/api/v1/assets/${assetID}?*`, async (route) => {
    calls.push("asset");
    if (state.metadataHidden) {
      await route.fulfill({
        status: 404,
        json: { error: { code: "NOT_FOUND" } },
      });
      return;
    }
    await route.fulfill({
      json: {
        asset_id: assetID,
        project_id: projectID,
        asset_type: "doc",
        description: {
          title: "合成版本资料",
          slug: "synthetic-version",
          summary: "可变著录摘要",
          tags: ["合成"],
          subjects: [],
          revision: 3,
        },
        latest: version(state.latest, state.latest === version1 ? 1 : 2),
      },
    });
  });
  await page.route(
    (url) => url.pathname.startsWith(`/api/v1/assets/${assetID}/versions/`),
    async (route) => {
      const url = new URL(route.request().url());
      const id = url.pathname.split("/")[6];
      if (url.pathname.endsWith("/read-grants")) {
        const body = route.request().postDataJSON();
        grants.push({ version: id, purpose: body.purpose });
        await route.fulfill({
          json: {
            url: `/xfer/v1/reads/${id}`,
            size: bytes.length,
            expires_at: new Date(Date.now() + 120000).toISOString(),
            path: filePath,
            sha256: hash,
            range: true,
          },
        });
      } else {
        calls.push(id);
        await route.fulfill({ json: exact(id) });
      }
    },
  );
  await page.route("**/xfer/v1/reads/*", async (route) => {
    await route.fulfill({
      status: 206,
      headers: {
        "Content-Range": `bytes 0-${bytes.length - 1}/${bytes.length}`,
      },
      contentType: "text/plain; charset=utf-8",
      body: Buffer.from(bytes),
    });
  });
  return { state, calls, grants };
}
const open = (page: Page, query = `version=${version1}`) =>
  page.goto(`${entry}#/assets/${assetID}${query ? `?${query}` : ""}`);

test("saved v1 link, manifest, preview and download remain v1 after latest advances", async ({
  page,
  request,
}) => {
  await login(page, request);
  const fixture = await seed(page);
  await open(page);
  await expect(page.getByText("旧版工作流 v1", { exact: true })).toBeVisible();
  const fixed = page.getByRole("link", { name: "固定此版本链接" });
  const saved = await fixed.getAttribute("href");
  expect(saved).toContain(`version=${version1}`);
  fixture.state.latest = version2;
  await page.reload();
  await expect(page.getByText("旧版工作流 v1", { exact: true })).toBeVisible();
  await expect(page.getByText("新版工作流 v2", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "预览文件", exact: true }).click();
  await expect(page.getByRole("heading", { name: "合成旧版本" })).toBeVisible();
  await page.getByRole("button", { name: "申请下载原文件" }).click();
  await expect(
    page.getByRole("link", { name: "下载原文件", exact: true }),
  ).toHaveAttribute("href", new RegExp(version1));
  expect(fixture.grants).toEqual([
    { version: version1, purpose: "archive_review" },
    { version: version1, purpose: "raw_export" },
  ]);
  expect(fixture.calls).not.toContain(version2);
  const storage = await page.evaluate(() =>
    JSON.stringify({ ...localStorage, ...sessionStorage }),
  );
  expect(storage).not.toContain("/xfer/");
  await page.getByRole("link", { name: "查看最新版" }).click();
  await expect(page.getByText("新版工作流 v2", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("link", { name: "下载原文件", exact: true }),
  ).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "合成旧版本" })).toHaveCount(
    0,
  );
  expect(await fixed.getAttribute("href")).toContain(`version=${version2}`);
  await page.goto(`${entry}${saved}`);
  await expect(page.getByText("旧版工作流 v1", { exact: true })).toBeVisible();
});

test("omitting version reads latest; version selection and return link preserve query context", async ({
  page,
  request,
}) => {
  await login(page, request);
  const fixture = await seed(page);
  fixture.state.latest = version2;
  await open(
    page,
    `from=${encodeURIComponent("/assets?q=角色&asset_type=doc")}`,
  );
  await expect(page.getByText("新版工作流 v2", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "返回列表" })).toHaveAttribute(
    "href",
    "#/assets?q=角色&asset_type=doc",
  );
  await page.getByText("按版本标识打开历史版本", { exact: true }).click();
  await page.getByLabel("版本标识", { exact: true }).fill("bad");
  await page.getByRole("button", { name: "打开固定版本" }).click();
  await expect(page.getByRole("alert")).toContainText("26 位大写版本标识");
  await page.getByLabel("版本标识", { exact: true }).fill(version1);
  await page.getByRole("button", { name: "打开固定版本" }).click();
  await expect(page.getByText("旧版工作流 v1", { exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "返回列表" })).toHaveAttribute(
    "href",
    "#/assets?q=角色&asset_type=doc",
  );
});

test("invalid or duplicate explicit versions reject without loading latest", async ({
  page,
  request,
}) => {
  await login(page, request);
  const fixture = await seed(page);
  for (const query of [
    "version=",
    "version=bad",
    "version=%2Fbad",
    `version=${version1}&version=${version2}`,
  ]) {
    await open(page, query);
    await expect(page.getByRole("alert")).toContainText("输入格式不正确");
    await expect(
      page.getByRole("heading", { name: "文件预览与下载" }),
    ).toHaveCount(0);
  }
  expect(fixture.calls).toEqual([]);
});

test("wrong asset, manifest or permanent instance binding never displays files", async ({
  page,
  request,
}) => {
  await login(page, request);
  await seed(page);
  for (const mismatch of [
    "version-asset",
    "manifest-version",
    "instance",
    "project",
    "digest",
  ]) {
    await page.route(`**/versions/${version1}?*`, async (route) => {
      const reply = exact(version1);
      if (mismatch === "version-asset") reply.version.asset_id = otherAssetID;
      if (mismatch === "manifest-version") reply.manifest.version_id = version2;
      if (mismatch === "instance") reply.manifest.instance_id = otherAssetID;
      if (mismatch === "project") reply.manifest.project_id = otherAssetID;
      if (mismatch === "digest")
        reply.manifest.manifest_digest = `sha256:${"c".repeat(64)}`;
      await route.fulfill({ json: reply });
    });
    await open(page);
    await expect(page.getByRole("alert")).toContainText("无法识别的内容");
    await expect(
      page.getByRole("heading", { name: "文件预览与下载" }),
    ).toHaveCount(0);
    await page.unroute(`**/versions/${version1}?*`);
  }
});

test("unavailable current metadata does not hide readable historical content", async ({
  page,
  request,
}) => {
  await login(page, request);
  const fixture = await seed(page);
  fixture.state.metadataHidden = true;
  await open(page);
  await expect(page.getByText("旧版工作流 v1", { exact: true })).toBeVisible();
  await expect(
    page.getByText("当前著录不可读取，只显示你有权读取的固定版本清单。", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.getByRole("heading", { name: "当前著录信息" })).toHaveCount(
    0,
  );
  await page.getByRole("button", { name: "申请下载原文件" }).click();
  expect(fixture.grants).toEqual([
    { version: version1, purpose: "raw_export" },
  ]);
});

test("not-found and permission failures do not substitute latest; retry is explicit", async ({
  page,
  request,
}) => {
  await login(page, request);
  const fixture = await seed(page);
  fixture.state.latest = version2;
  await page.route(`**/versions/${version1}?*`, async (route) => {
    await route.fulfill({
      status: 404,
      json: { error: { code: "NOT_FOUND" } },
    });
  });
  await open(page);
  await expect(page.getByRole("alert")).toContainText("内容不存在");
  expect(fixture.calls).not.toContain(version2);
  await page.unroute(`**/versions/${version1}?*`);
  await page.getByRole("button", { name: "重试", exact: true }).click();
  await expect(page.getByText("旧版工作流 v1", { exact: true })).toBeVisible();
});

test("route change cancels old requests and late responses cannot restore old content", async ({
  page,
  request,
}) => {
  await login(page, request);
  await seed(page);
  const cancelled: string[] = [];
  page.on("requestfailed", (request) => cancelled.push(request.url()));
  let started!: () => void;
  let release!: () => void;
  const pending = new Promise<void>((resolve) => {
    started = resolve;
  });
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(`**/versions/${version1}?*`, async (route) => {
    started();
    await gate;
    await route.fulfill({ json: exact(version1) }).catch(() => {});
  });
  await open(page);
  await pending;
  await page.evaluate(
    (id) => {
      location.hash = `/assets/${id.asset}?version=${id.version}`;
    },
    { asset: assetID, version: version2 },
  );
  await expect(page.getByText("新版工作流 v2", { exact: true })).toBeVisible();
  await expect
    .poll(() => cancelled.some((url) => url.includes(version1)))
    .toBeTruthy();
  release();
  await expect(page.getByText("旧版工作流 v1", { exact: true })).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: "固定此版本链接" }),
  ).toHaveAttribute("href", new RegExp(`version=${version2}`));
});

test("switching same-path versions cancels pending read grant and removes old download", async ({
  page,
  request,
}) => {
  await login(page, request);
  await seed(page);
  const cancelled: string[] = [];
  page.on("requestfailed", (request) => cancelled.push(request.url()));
  let started!: () => void;
  let release!: () => void;
  const pending = new Promise<void>((resolve) => {
    started = resolve;
  });
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(`**/versions/${version1}/read-grants`, async (route) => {
    started();
    await gate;
    await route
      .fulfill({
        json: {
          url: `/xfer/v1/reads/${version1}`,
          path: filePath,
          size: bytes.length,
          sha256: hash,
          range: true,
          expires_at: new Date(Date.now() + 120000).toISOString(),
        },
      })
      .catch(() => {});
  });
  await open(page);
  await page.getByRole("button", { name: "申请下载原文件" }).click();
  await pending;
  await page.evaluate(
    (id) => {
      location.hash = `/assets/${id.asset}?version=${id.version}`;
    },
    { asset: assetID, version: version2 },
  );
  await expect(page.getByText("新版工作流 v2", { exact: true })).toBeVisible();
  await expect
    .poll(() =>
      cancelled.some((url) => url.endsWith(`${version1}/read-grants`)),
    )
    .toBeTruthy();
  release();
  await expect(
    page.getByRole("link", { name: "下载原文件", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "申请下载原文件" }),
  ).toBeEnabled();
});

test("exact-version session expiry returns control to the original authentication boundary", async ({
  page,
  request,
}) => {
  await login(page, request);
  const fixture = await seed(page);
  await page.route(`**/versions/${version1}?*`, async (route) => {
    await route.fulfill({
      status: 401,
      json: { error: { code: "TOKEN_EXPIRED" } },
    });
  });
  await open(page);
  if (harness)
    await expect(page.getByRole("alert")).toHaveText("合成会话已过期");
  else
    await expect(page.getByRole("heading", { name: "登录兰台" })).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "文件预览与下载" }),
  ).toHaveCount(0);
  expect(fixture.grants).toEqual([]);
});
