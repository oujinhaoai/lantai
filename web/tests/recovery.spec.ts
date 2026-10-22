import { test, expect } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";

test("complete backup downloads and restores through the new-target wizard", async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(120000);
  const fixture = async (action: string, data?: unknown) => {
    const response = await request.post(`/__fixture/${action}`, {
      headers: { "X-Lantai-Fixture": "1" },
      data,
    });
    expect(response.ok()).toBeTruthy();
    return response.json();
  };
  const login = await fixture("login");
  await page.goto("/");
  await page.getByLabel("账户名称").fill(login.name);
  await page.getByLabel("口令", { exact: true }).fill(login.password);
  await page.getByLabel("验证器动态码", { exact: true }).fill(login.code);
  await page.getByLabel("管理员登录（含备份管理权限）").check();
  await page.getByRole("button", { name: "登录并浏览" }).click();
  await page.getByRole("link", { name: "备份与恢复", exact: true }).click();
  const approve = async () => {
    const { code } = await fixture("code");
    await page.getByLabel("确认操作的验证器动态码").fill(code);
    await page.getByRole("button", { name: "验证并执行" }).click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
  };
  await page.getByRole("button", { name: "立即备份", exact: true }).click();
  await approve();
  await expect
    .poll(
      async () =>
        (await (await page.request.get("/api/v1/admin/backups/history")).json())
          .items[0]?.state,
      { timeout: 60000 },
    )
    .toBe("complete");
  const history = await (
    await page.request.get("/api/v1/admin/backups/history")
  ).json();
  expect(history.items[0].package.sensitive).toBe(true);
  await page.getByRole("button", { name: "刷新状态" }).click();
  const row = page
    .locator(".backup-history > li")
    .filter({ has: page.getByText(history.items[0].id, { exact: true }) });
  await row.getByRole("button", { name: "下载完整恢复包" }).click();
  const completed = page.waitForEvent("download");
  await approve();
  const download = await completed;
  expect(await download.failure()).toBeNull();
  expect(download.url()).not.toContain("?");
  const path = await download.path();
  expect(path).toBeTruthy();
  const bytes = await readFile(path!);
  expect(createHash("sha256").update(bytes).digest("hex")).toBe(
    history.items[0].package.sha256,
  );

  await page.screenshot({
    path: testInfo.outputPath("backup-complete.png"),
    fullPage: true,
  });
  const target = await fixture("recovery-target");
  await page.goto("/setup/");
  await page.getByLabel("新设备安装引导码").fill(target.code);
  await page.getByRole("button", { name: "连接新设备" }).click();
  await page.getByRole("button", { name: "从完整备份恢复" }).click();
  await page.getByLabel("选择 .lantai 文件").setInputFiles(path!);
  await page.getByRole("button", { name: "上传并校验" }).click();
  await expect(
    page.getByRole("heading", { name: "确认恢复", exact: true }),
  ).toBeVisible();
  await page.getByLabel("确认包来源可信，恢复到当前新目标").check();
  await page.getByRole("button", { name: "开始恢复" }).click();
  await expect(
    page.getByRole("heading", { name: "重建恢复管理员" }),
  ).toBeVisible({ timeout: 60000 });
  await page.getByLabel("原管理员用户名").fill("ada");
  await page
    .getByLabel("新登录口令", { exact: true })
    .fill("browser recovered administrator password");
  await page
    .getByLabel("重复口令", { exact: true })
    .filter({ visible: true })
    .fill("browser recovered administrator password");
  await page.getByRole("button", { name: "设置新验证器" }).click();
  await expect(page.locator("#secret")).toHaveText(/^[A-Z2-7]{32}$/);
  const secret = await page.locator("#secret").textContent();
  const { code } = await fixture("recovery-code", { secret });
  await page.getByLabel("已保存登录恢复码").check();
  await page.getByLabel("当前动态码").fill(code);
  await page.getByRole("button", { name: "确认管理员" }).click();
  await expect(
    page.getByRole("heading", { name: "恢复检查与投入使用" }),
  ).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("recovery-review.png"),
    fullPage: true,
  });
  await page.getByLabel("已核对并处理已知的捕获后变更").check();
  await page.getByLabel("接受此备份为恢复点", { exact: false }).check();
  await page.getByLabel("原实例已停止对外写入", { exact: false }).check();
  await page.getByRole("button", { name: "完成恢复并启动" }).click();
  await expect(
    page.getByRole("heading", { name: "已完成", exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("recovery-complete.png"),
    fullPage: true,
  });
  expect(await fixture("recovery-verify")).toEqual({ ready: true });
});
