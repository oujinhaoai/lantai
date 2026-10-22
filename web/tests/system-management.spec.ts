import {
  test,
  expect,
  type APIRequestContext,
  type Page,
} from "@playwright/test";
import { readFile } from "node:fs/promises";

// Only the opt-in, temporary Go browser fixture may service these operations.
// Every created project is synthetic. No external deployment is addressed.
async function control(request: APIRequestContext, action: string) {
  const response = await request.post(`/__fixture/${action}`, {
    headers: { "X-Lantai-Fixture": "1" },
  });
  expect(response.ok()).toBeTruthy();
  return response.status() === 204 ? undefined : response.json();
}
async function login(page: Page, request: APIRequestContext, admin = true) {
  const credentials = await control(request, "login");
  await page.goto("/");
  await page.getByLabel("账户名称").fill(credentials.name);
  await page.getByLabel("口令", { exact: true }).fill(credentials.password);
  await page.getByLabel("验证器动态码", { exact: true }).fill(credentials.code);
  if (admin) await page.getByLabel("管理员登录（含备份管理权限）").check();
  await page.getByRole("button", { name: "登录并浏览" }).click();
  await expect(page.getByRole("heading", { name: "你好，Ada" })).toBeVisible();
}
async function openSystem(page: Page) {
  await page.goto("/#/admin/system");
  await expect(
    page.getByRole("heading", { name: "系统与协作管理" }),
  ).toBeVisible();
}

test("ordinary read scope cannot open system management or enumerate principals", async ({
  page,
  request,
  context,
}) => {
  await login(page, request, false);
  await expect(
    page.getByRole("link", { name: "系统管理", exact: true }),
  ).toHaveCount(0);
  expect(
    (await context.request.get("/api/v1/identity/principals?limit=1")).status(),
  ).toBe(403);
  await page.goto("/#/admin/system");
  await expect(
    page.getByRole("heading", { name: "需要相应管理范围" }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "请求登记确认" })).toHaveCount(
    0,
  );
});

test("new project members are manageable by explicit ID; a lost role response reuses the exact grant, body and key", async ({
  page,
  request,
  context,
}) => {
  test.setTimeout(90000);
  await login(page, request);
  const who = await (await context.request.get("/api/v1/whoami")).json();
  await openSystem(page);
  const key = `synthetic-system-${crypto.randomUUID().slice(0, 8)}`;
  await page.getByLabel("项目 key", { exact: true }).fill(key);
  await page.getByLabel("名称", { exact: true }).fill("合成成员授权验收");
  await page
    .getByRole("combobox", { name: "项目类别", exact: true })
    .selectOption("sandbox");
  await page.getByRole("button", { name: "核对创建请求" }).click();
  const created = page.waitForResponse(
    (r) =>
      r.url().endsWith("/api/v1/projects") && r.request().method() === "POST",
  );
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "确认执行", exact: true })
    .click();
  const response = await created;
  expect(response.status()).toBe(201);
  const project = await response.json();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  const before = await (
    await context.request.get("/api/v1/projects?limit=100")
  ).json();
  expect(
    before.items.some(
      (p: { project_id: string }) => p.project_id === project.project_id,
    ),
  ).toBe(false);
  await page.getByLabel("成员管理的明确项目标识").fill(project.project_id);
  await page
    .getByRole("button", { name: "读取此项目成员", exact: true })
    .click();
  await expect(
    page.getByText("访问控制修订：0。确认时和最终接受时均绑定该修订。", {
      exact: true,
    }),
  ).toBeVisible();
  await page
    .getByLabel("主体标识", { exact: true })
    .fill(who.principal.principal_id);
  await page.getByLabel("项目角色", { exact: true }).selectOption("viewer");

  const writes: { body: string | null; key: string }[] = [];
  let verified = 0;
  page.on("request", (r) => {
    if (r.url().endsWith("/verify")) verified++;
  });
  await page.route("**/api/v1/identity/commands", async (route) => {
    writes.push({
      body: route.request().postData(),
      key: route.request().headers()["idempotency-key"],
    });
    if (writes.length === 1) {
      const accepted = await route.fetch();
      expect(accepted.ok()).toBeTruthy();
      await route.abort("failed");
    } else await route.continue();
  });
  const challenged = page.waitForResponse(
    (r) =>
      r.url().endsWith("/identity/challenges") &&
      r.request().method() === "POST",
  );
  await page.getByRole("button", { name: "请求动作绑定的人类确认" }).click();
  const challenge = await (await challenged).json();
  const dialog = page.getByRole("dialog");
  await expect(
    dialog.getByText(challenge.summary, { exact: true }),
  ).toBeVisible();
  await expect(dialog.getByRole("row")).toHaveCount(
    challenge.targets.length + 1,
  );
  for (const target of challenge.targets)
    await expect(
      dialog.getByRole("cell", { name: target.id, exact: true }),
    ).toBeVisible();
  const { code } = await control(request, "code");
  await dialog.getByLabel("确认操作的验证器动态码").fill(code);
  await dialog.getByRole("button", { name: "验证并执行", exact: true }).click();
  await expect(
    dialog.getByRole("button", { name: "按原授权与键重试", exact: true }),
  ).toBeEnabled();
  expect(writes).toHaveLength(1);
  const sent = JSON.parse(writes[0].body!);
  const storage = await page.evaluate(() =>
    JSON.stringify([
      Object.entries(localStorage),
      Object.entries(sessionStorage),
    ]),
  );
  expect(storage).not.toContain(sent.grant_id);
  const downloaded = page.waitForEvent("download");
  await dialog
    .getByRole("button", { name: "保存原操作诊断", exact: true })
    .click();
  const file = await downloaded;
  const diagnostic = JSON.parse(await readFile((await file.path())!, "utf8"));
  expect(diagnostic.challenge.operation_id).toBe(challenge.operation_id);
  expect(diagnostic.key).toBe(writes[0].key);
  expect(JSON.stringify(diagnostic)).not.toContain(sent.grant_id);
  expect(diagnostic.execution_attempted).toBe(true);
  await dialog
    .getByRole("button", { name: "按原授权与键重试", exact: true })
    .click();
  await expect(dialog).toHaveCount(0);
  expect(writes).toHaveLength(2);
  expect(writes[1]).toEqual(writes[0]);
  expect(verified).toBe(1);
  const members = await (
    await context.request.get(
      `/api/v1/identity/projects/${project.project_id}/members`,
    )
  ).json();
  expect(
    members.items.find(
      (m: { principal_id: string }) =>
        m.principal_id === who.principal.principal_id,
    ).roles,
  ).toEqual(["viewer"]);

  await expect(page.getByLabel("动作", { exact: true })).toBeEnabled();
  await page.getByLabel("动作", { exact: true }).selectOption("revoke");
  await page.getByRole("button", { name: "请求动作绑定的人类确认" }).click();
  const next = await control(request, "code");
  await page
    .getByRole("dialog")
    .getByLabel("确认操作的验证器动态码")
    .fill(next.code);
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "验证并执行", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  const after = await (
    await context.request.get("/api/v1/projects?limit=100")
  ).json();
  expect(
    after.items.some(
      (p: { project_id: string }) => p.project_id === project.project_id,
    ),
  ).toBe(false);
  await expect(
    page.getByText(`成员管理项目：${project.project_id}`, { exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});

test("system queries expire the browser session instead of retaining stale authority", async ({
  page,
  request,
}) => {
  await login(page, request);
  await openSystem(page);
  await page.getByRole("button", { name: "主体与凭据", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "当前登记主体", exact: true }),
  ).toBeVisible();
  await control(request, "expire");
  await page.getByRole("button", { name: "刷新主体", exact: true }).click();
  await expect(page.getByLabel("账户名称")).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "当前登记主体", exact: true }),
  ).toHaveCount(0);
});
