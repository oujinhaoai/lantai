import { createHash } from "node:crypto";
import { test, expect, type Page } from "@playwright/test";

// Controlled UI responses exercise presentation/intent; real authorization is
// covered by the API tests, not inferred from this synthetic browser identity.
const assetID = "01J0000000000000000000000A",
  version1 = "01J00000000000000000000011",
  version2 = "01J00000000000000000000012",
  projectID = "01J0000000000000000000000B",
  instanceID = "01J0000000000000000000000C",
  sessionID = "01J0000000000000000000000D";
const digest = `sha256:${"a".repeat(64)}`;
const version = (id: string) => ({
  asset_id: assetID,
  project_id: projectID,
  version_id: id,
  version_number: id === version1 ? 1 : 2,
  committed_at: "2026-10-20T00:00:00Z",
  manifest_digest: digest,
  uri: `lantai://${instanceID}/assets/${assetID}/versions/${id}`,
});
async function controlled(
  page: Page,
  path = "synthetic.txt",
  data = Buffer.from("synthetic"),
) {
  const file = {
    path,
    role: "primary",
    size: data.length,
    sha256: createHash("sha256").update(data).digest("hex"),
  };
  let description = {
    title: "合成管理素材",
    slug: "synthetic-managed",
    summary: "原摘要",
    tags: ["source:synthetic"],
    subjects: [],
    revision: 3,
  };
  await page.addInitScript((session) => {
    sessionStorage.setItem(
      "lantai.csrf.v1",
      JSON.stringify({
        session,
        value: "synthetic-not-an-authentication-credential",
      }),
    );
  }, sessionID);
  await page.route("**/api/v1/whoami", (route) =>
    route.fulfill({
      json: {
        principal: { name: "synthetic-human", kind: "human" },
        session: {
          session_id: sessionID,
          channel: "browser",
          session_kind: "normal",
          expires_at: "2099-01-01T00:00:00Z",
          scopes: ["self", "read", "ingest", "organize"],
        },
        project_roles: { [projectID]: ["owner"] },
        system_roles: [],
      },
    }),
  );
  await page.route(`**/api/v1/assets/${assetID}?*`, (route) =>
    route.fulfill({
      json: {
        asset_id: assetID,
        project_id: projectID,
        asset_type: /\.obj$/u.test(path)
          ? "model"
          : /\.png$/u.test(path)
            ? "image"
            : "doc",
        description,
        latest: version(version1),
      },
    }),
  );
  await page.route(`**/api/v1/assets/${assetID}/versions?*`, (route) =>
    route.fulfill({ json: { items: [version(version1), version(version2)] } }),
  );
  await page.route(
    new RegExp(`/api/v1/assets/${assetID}/versions/[^/?]+\\?`),
    (route) => {
      const id = new URL(route.request().url()).pathname.split("/").pop()!;
      return route.fulfill({
        json: {
          version: version(id),
          manifest: {
            instance_id: instanceID,
            project_id: projectID,
            asset_id: assetID,
            version_id: id,
            version_number: id === version1 ? 1 : 2,
            manifest_digest: digest,
            content: {
              asset_type: /\.obj$/u.test(path)
                ? "model"
                : /\.png$/u.test(path)
                  ? "image"
                  : "doc",
              version_note: id === version1 ? "固定原版" : "对比版本",
              rights: { usage: "restricted", license: "LicenseRef-Synthetic" },
              files: [id === version1 ? file : { ...file, role: "source" }],
            },
          },
        },
      });
    },
  );
  await page.goto(`/#/assets/${assetID}?version=${version1}`);
  await expect(
    page.getByRole("heading", { name: "合成管理素材" }),
  ).toBeVisible();
  return {
    file,
    setDescription: (value: typeof description) => {
      description = value;
    },
  };
}

test("managed history discovery and frozen comparison keep current and historical metadata separate", async ({
  page,
}) => {
  await controlled(page);
  await page.getByRole("button", { name: "读取可见历史版本" }).click();
  await expect(page.getByRole("link", { name: "打开固定版本" })).toHaveCount(2);
  await page.getByRole("button", { name: "选为对比基线" }).nth(1).click();
  await page.getByRole("button", { name: "比较冻结清单" }).click();
  const diff = page.getByRole("region", { name: "冻结版本差异" });
  await expect(diff).toContainText("变化 1");
  await expect(diff).toContainText("不把当前著录称作历史著录");
  await expect(
    page.getByRole("link", { name: "固定此版本链接" }),
  ).toHaveAttribute("href", new RegExp(version1));
});

test("uncertain metadata response retries the exact original key, revision and payload", async ({
  page,
}) => {
  await controlled(page);
  const calls: { key?: string; revision?: string; body: unknown }[] = [];
  await page.route(`**/api/v1/assets/${assetID}/metadata`, async (route) => {
    calls.push({
      key: route.request().headers()["idempotency-key"],
      revision: route.request().headers()["if-match"],
      body: route.request().postDataJSON(),
    });
    if (calls.length === 1) await route.abort();
    else
      await route.fulfill({
        json: {
          asset_id: assetID,
          project_id: projectID,
          title: "已编辑资料",
          summary: "原摘要",
          tags: ["source:synthetic"],
          subjects: [],
          revision: 4,
        },
      });
  });
  await page.getByText("编辑当前素材信息", { exact: true }).click();
  await page.getByLabel("标题", { exact: true }).fill("已编辑资料");
  await page.getByRole("button", { name: "保存当前著录" }).click();
  await expect(
    page.getByText(/提交结果不明，草稿和原幂等键已冻结/),
  ).toBeVisible();
  await expect(page.getByLabel("标题", { exact: true })).toBeDisabled();
  await page.getByRole("button", { name: "按原键重试相同提交" }).click();
  await expect(page.getByText(/当前著录已保存为 r4/)).toBeVisible();
  expect(calls).toHaveLength(2);
  expect(calls[1]).toEqual(calls[0]);
  expect(calls[0].revision).toBe('"3"');
});

test("metadata conflict retains the draft until the operator explicitly adopts the new revision", async ({
  page,
}) => {
  const state = await controlled(page);
  const revisions: string[] = [];
  await page.route(`**/api/v1/assets/${assetID}/metadata`, async (route) => {
    revisions.push(route.request().headers()["if-match"]);
    await route.fulfill({
      status: 412,
      json: { error: { code: "PRECONDITION_FAILED" } },
    });
  });
  await page.getByText("编辑当前素材信息", { exact: true }).click();
  await page.getByLabel("标题", { exact: true }).fill("我的未决草稿");
  await page.getByRole("button", { name: "保存当前著录" }).click();
  await expect(page.getByLabel("标题", { exact: true })).toHaveValue(
    "我的未决草稿",
  );
  state.setDescription({
    title: "他人已更新",
    slug: "synthetic-managed",
    summary: "新摘要",
    tags: [],
    subjects: [],
    revision: 4,
  });
  await page.getByRole("button", { name: "读取最新著录（保留草稿）" }).click();
  await expect(page.getByLabel("标题", { exact: true })).toHaveValue(
    "我的未决草稿",
  );
  await page
    .getByRole("button", { name: "放弃本地草稿，采用服务器内容" })
    .click();
  await expect(page.getByLabel("标题", { exact: true })).toHaveValue(
    "他人已更新",
  );
  await page.getByRole("button", { name: "保存当前著录" }).click();
  expect(revisions).toEqual(['"3"', '"4"']);
});

test("proxy export cannot fall back from a rejected raw_export purpose", async ({
  page,
}) => {
  const png = Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/bZkAAAAASUVORK5CYII=",
    "base64",
  );
  await controlled(page, "synthetic.png", png);
  const purposes: string[] = [];
  let transferred = 0;
  await page.route(
    `**/api/v1/assets/${assetID}/versions/${version1}/read-grants`,
    async (route) => {
      purposes.push(route.request().postDataJSON().purpose);
      await route.fulfill({
        status: 403,
        json: { error: { code: "USE_RESTRICTED" } },
      });
    },
  );
  await page.route("**/xfer/**", async (route) => {
    transferred++;
    await route.abort();
  });
  await page.getByRole("button", { name: "申请导出并生成代理" }).click();
  await expect(page.getByRole("alert")).toContainText("不允许用于当前读取用途");
  expect(purposes).toEqual(["raw_export"]);
  expect(transferred).toBe(0);
  await expect(
    page.getByRole("link", { name: "下载 PNG 代理副本" }),
  ).toHaveCount(0);
});

test("OBJ preview verifies the fixed bytes and never fetches declared external material libraries", async ({
  page,
}) => {
  const data = Buffer.from(
    "mtllib https://untrusted.invalid/material.mtl\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n",
  );
  const state = await controlled(page, "synthetic.obj", data);
  let external = 0;
  await page.route("https://untrusted.invalid/**", async (route) => {
    external++;
    await route.abort();
  });
  await page.route(
    `**/api/v1/assets/${assetID}/versions/${version1}/read-grants`,
    (route) =>
      route.fulfill({
        json: {
          ...state.file,
          grant_id: version1,
          asset_id: assetID,
          version_id: version1,
          purpose: "archive_review",
          method: "GET",
          url: `/xfer/v1/reads/${version1}`,
          expires_at: new Date(Date.now() + 60000).toISOString(),
          range: true,
        },
      }),
  );
  await page.route(`**/xfer/v1/reads/${version1}`, (route) =>
    route.fulfill({ body: data }),
  );
  await page.getByRole("button", { name: "读取并显示线框" }).click();
  await expect(
    page.getByRole("img", { name: "OBJ 线框，3 顶点、1 个面" }),
  ).toBeVisible();
  expect(external).toBe(0);
});
