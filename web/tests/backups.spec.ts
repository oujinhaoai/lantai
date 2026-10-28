import {
  test,
  expect,
  type Page,
  type APIRequestContext,
  type BrowserContext,
  type TestInfo,
} from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { mkdir } from "node:fs/promises";
import path from "node:path";

test.setTimeout(90000);

async function control(request: APIRequestContext, action: string) {
  const reply = await request.post(`/__fixture/${action}`, {
    headers: { "X-Lantai-Fixture": "1" },
  });
  expect(reply.ok()).toBeTruthy();
  return reply.status() === 204 ? undefined : reply.json();
}
async function shot(page: Page, info: TestInfo, name: string) {
  if (!process.env.LANTAI_WEB_EVIDENCE) return;
  await mkdir(process.env.LANTAI_WEB_EVIDENCE, { recursive: true });
  await page.screenshot({
    path: path.join(
      process.env.LANTAI_WEB_EVIDENCE,
      `${info.project.name}-backups-${name}.png`,
    ),
    fullPage: true,
  });
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
async function openBackups(page: Page) {
  await page
    .getByRole("navigation")
    .getByRole("link", { name: "备份与恢复" })
    .click();
  await expect(
    page.getByRole("heading", { name: "备份管理", exact: true }),
  ).toBeVisible();
}
async function approve(page: Page, request: APIRequestContext) {
  const { code } = await control(request, "code");
  await page.getByLabel("确认操作的验证器动态码").fill(code);
  await page.getByRole("button", { name: "验证并执行" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
}
async function noOverflow(page: Page) {
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
}

test("ordinary browser scope has no backup entry and cannot use the API", async ({
  page,
  request,
  context,
}) => {
  await login(page, request, false);
  await expect(page.getByRole("link", { name: "备份与恢复" })).toHaveCount(0);
  expect((await context.request.get("/api/v1/admin/backups")).status()).toBe(
    403,
  );
  await page.goto("/#/admin/backups");
  await expect(
    page.getByText(
      "备份管理需要管理员身份与管理范围的浏览器会话。请退出后选择管理员登录。",
    ),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "立即备份" })).toHaveCount(0);
});

test("administrator creates a real verified backup and persists the daily schedule", async ({
  page,
  request,
  context,
}, info) => {
  await login(page, request);
  const who = await (await context.request.get("/api/v1/whoami")).json();
  expect(who.session.scopes).toContain("admin");
  await openBackups(page);
  await expect(
    page
      .getByRole("heading", { name: "备份目录", exact: true })
      .locator("..")
      .getByText("可用", { exact: true }),
  ).toBeVisible();
  await noOverflow(page);
  await page.getByRole("button", { name: "立即备份" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("操作摘要和全部目标");
  await expect(dialog.locator(".backup-server-summary")).not.toHaveText("");
  await expect(dialog.locator(".backup-targets li")).not.toHaveCount(0);
  await expect(
    dialog.getByRole("button", { name: "验证并执行" }),
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await shot(page, info, "confirmation");
  await approve(page, request);
  await expect
    .poll(
      async () => {
        const value = await (
          await context.request.get("/api/v1/admin/backups/history")
        ).json();
        return value.items?.[0]?.state;
      },
      { timeout: 60000 },
    )
    .toBe("complete");
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect(
    page.getByRole("heading", { name: "上次成功" }).locator(".."),
  ).toContainText("完整恢复包在完成时已生成并验证");
  await expect(
    page.getByRole("heading", { name: "上次成功" }).locator(".."),
  ).toContainText("下载时会重新核验当前文件");
  await expect(page.locator(".backup-history")).toContainText("已完成并验证");
  await page.getByLabel("每日时间", { exact: true }).fill("04:32");
  await page.getByLabel("IANA 时区").fill("Asia/Shanghai");
  await page.getByLabel("排空等待上限（秒）").fill("180");
  await page.getByLabel("整个任务超时（秒）").fill("9000");
  await page.getByLabel("开启每日备份").check();
  await page.getByRole("button", { name: "验证并保存计划" }).click();
  await expect(dialog.locator(".backup-server-summary")).toContainText(
    "Asia/Shanghai",
  );
  await approve(page, request);
  const schedule = await (
    await context.request.get("/api/v1/admin/backups/schedule")
  ).json();
  expect(schedule).toMatchObject({
    enabled: true,
    timezone: "Asia/Shanghai",
    time: "04:32",
    drain_seconds: 180,
    timeout_seconds: 9000,
  });
  expect(schedule.next_run_at).toBeTruthy();
  await page.reload();
  await expect(page.getByLabel("开启每日备份")).toBeChecked();
  await expect(page.getByLabel("每日时间", { exact: true })).toHaveValue(
    "04:32",
  );
  await expect(page.getByLabel("IANA 时区")).toHaveValue("Asia/Shanghai");
  await expect(page.getByLabel("排空等待上限（秒）")).toHaveValue("180");
  await expect(page.getByLabel("整个任务超时（秒）")).toHaveValue("9000");
  await page.getByLabel("开启每日备份").uncheck();
  await page.getByRole("button", { name: "验证并保存计划" }).click();
  await approve(page, request);
  await expect(page.getByText("计划已关闭", { exact: true })).toBeVisible();
  await noOverflow(page);
  await shot(page, info, "complete");
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

const runningJob = {
  id: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
  state: "draining",
  created_at: "2026-10-15T02:00:00Z",
  updated_at: "2026-10-15T02:00:01Z",
  source: "manual",
  attempt: 1,
};
function dashboard(current: typeof runningJob | null = runningJob) {
  return {
    availability: { configured: true, available: true },
    current,
    last_success: null,
    schedule: {
      enabled: false,
      time: "03:00",
      timezone: "Etc/UTC",
      revision: 1,
      next_run_at: null,
    },
    recent_schedule_events: [],
  };
}

test("real maintenance drains work and keeps cancellation available without a challenge", async ({
  page,
  request,
  context,
}) => {
  await login(page, request);
  await openBackups(page);
  await control(request, "backup-hold");
  try {
    await page.getByRole("button", { name: "立即备份" }).click();
    await approve(page, request);
    await expect(page.getByRole("button", { name: "立即备份" })).toBeDisabled();
    await expect(page.locator(".backup-current .backup-state")).toHaveText(
      "等待在途操作完成",
    );
    await expect(page.getByRole("progressbar")).toHaveCount(0);
    expect((await context.request.get("/api/v1/whoami")).status()).toBe(200);
    expect((await context.request.get("/api/v1/projects")).status()).toBe(503);
    await page.reload();
    await expect(page.locator(".backup-current .backup-state")).toHaveText(
      "等待在途操作完成",
    );
    let challenges = 0;
    await page.route("**/api/v1/identity/challenges", async (route) => {
      challenges++;
      await route.continue();
    });
    await page.getByRole("button", { name: "取消当前任务" }).click();
    await expect(
      page.getByText("取消请求已受理。请等待任务停止，期间业务仍暂停。"),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "取消当前任务" }),
    ).toHaveCount(0);
    expect(challenges).toBe(0);
    await expect(
      page.getByRole("button", { name: "续传此任务" }).first(),
    ).toBeEnabled();
    await noOverflow(page);
  } finally {
    await control(request, "backup-release");
  }
  await expect
    .poll(async () => (await context.request.get("/api/v1/projects")).status())
    .toBe(200);
  await page.getByRole("button", { name: "续传此任务" }).first().click();
  await approve(page, request);
  await expect
    .poll(
      async () => {
        const value = await (
          await context.request.get("/api/v1/admin/backups/history")
        ).json();
        return value.items?.[0]?.state;
      },
      { timeout: 60000 },
    )
    .toBe("complete");
});

test("uncertain response retries the same grant and idempotency key", async ({
  page,
  request,
  context,
}) => {
  await login(page, request);
  await openBackups(page);
  let verifies = 0;
  const submitted: { key: string | undefined; body: unknown }[] = [];
  await page.route("**/api/v1/identity/challenges/*/verify", async (route) => {
    verifies++;
    await route.continue();
  });
  await page.route("**/api/v1/admin/backups", async (route) => {
    if (route.request().method() !== "POST") {
      await route.continue();
      return;
    }
    submitted.push({
      key: route.request().headers()["idempotency-key"],
      body: route.request().postDataJSON(),
    });
    if (submitted.length === 1) await route.abort("failed");
    else await route.continue();
  });
  await page.getByRole("button", { name: "立即备份" }).click();
  const { code } = await control(request, "code");
  await page.getByLabel("确认操作的验证器动态码").fill(code);
  await page.getByRole("button", { name: "验证并执行" }).click();
  await expect(page.getByRole("dialog")).toContainText("无法连接服务");
  await expect(page.getByLabel("确认操作的验证器动态码")).toHaveCount(0);
  await page.getByRole("button", { name: "重试同一请求" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect(submitted).toHaveLength(2);
  expect(submitted[0]).toEqual(submitted[1]);
  expect(verifies).toBe(1);
  await expect
    .poll(
      async () => {
        const value = await (
          await context.request.get("/api/v1/admin/backups/history")
        ).json();
        return value.items?.[0]?.state;
      },
      { timeout: 60000 },
    )
    .toBe("complete");
});

test("availability, service failures and lost permission do not show stale backup data", async ({
  page,
  request,
}) => {
  await login(page, request);
  let denied = false;
  const value = dashboard(null);
  value.availability = { configured: true, available: false };
  await page.route("**/api/v1/admin/backups**", async (route) => {
    if (denied)
      await route.fulfill({
        status: 403,
        json: { error: { code: "FORBIDDEN" } },
      });
    else if (route.request().url().endsWith("/history"))
      await route.fulfill({
        json: { items: [{ ...runningJob, state: "failed", error: "timeout" }] },
      });
    else await route.fulfill({ json: value });
  });
  await openBackups(page);
  await expect(page.getByRole("button", { name: "立即备份" })).toBeDisabled();
  await expect(page.getByText("暂不可用", { exact: true })).toBeVisible();
  await expect(page.locator(".backup-history")).toContainText("任务超时");
  await noOverflow(page);
  denied = true;
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "当前会话没有备份管理权限",
  );
  await expect(page.getByText(runningJob.id, { exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "立即备份" })).toHaveCount(0);
});

test("dirty schedule keeps its revision and explicitly resets concurrent changes", async ({
  page,
  request,
}) => {
  await login(page, request);
  const value = dashboard(null);
  let submittedRevision: number | undefined;
  await page.route("**/api/v1/admin/backups**", async (route) => {
    await route.fulfill({
      json: route.request().url().endsWith("/history") ? { items: [] } : value,
    });
  });
  await page.route("**/api/v1/identity/challenges", async (route) => {
    submittedRevision = route.request().postDataJSON().command
      .schedule.revision;
    await route.fulfill({
      status: 412,
      json: { error: { code: "PRECONDITION_FAILED" } },
    });
  });
  await openBackups(page);
  await expect(page.getByLabel("每日时间", { exact: true })).toHaveValue(
    "03:00",
  );
  await page.getByLabel("每日时间", { exact: true }).fill("04:40");
  value.schedule.revision = 2;
  value.schedule.time = "08:00";
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect(
    page.getByText(
      "计划已在其他位置更新。当前草稿保留原修订号，请重置为已保存设置后重新编辑。",
    ),
  ).toBeVisible();
  await expect(page.getByLabel("每日时间", { exact: true })).toHaveValue(
    "04:40",
  );
  await page.getByRole("button", { name: "验证并保存计划" }).click();
  await expect(page.getByRole("alert")).toContainText("设置已被其他管理员更新");
  expect(submittedRevision).toBe(1);
  await page.getByRole("button", { name: "重置为已保存设置" }).click();
  await expect(page.getByLabel("每日时间", { exact: true })).toHaveValue(
    "08:00",
  );
  await expect(
    page.getByRole("button", { name: "验证并保存计划" }),
  ).toBeDisabled();
  await page.getByLabel("排空等待上限（秒）").fill("200");
  await page.getByLabel("整个任务超时（秒）").fill("100");
  await page.getByRole("button", { name: "验证并保存计划" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "排空等待上限不能大于整个任务超时",
  );
  expect(submittedRevision).toBe(1);
});

test("pending backup reads abort on navigation without populating another page", async ({
  page,
  request,
}) => {
  await login(page, request);
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/api/v1/admin/backups", async (route) => {
    await gate;
    await route.fulfill({ json: dashboard() }).catch(() => {});
  });
  const failed = page.waitForEvent("requestfailed", {
    predicate: (r) => r.url().endsWith("/api/v1/admin/backups"),
  });
  await openBackups(page);
  await expect(
    page.getByText("正在读取备份状态…", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("navigation")
    .getByRole("link", { name: "首页", exact: true })
    .click();
  await failed;
  release();
  await expect(page.getByRole("heading", { name: "你好，Ada" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "当前任务" })).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
});

test("active directory status is a prior check and schedule authorization failures are explicit", async ({
  page,
  request,
}) => {
  await login(page, request);
  const value = {
    ...dashboard(),
    availability: {
      configured: true,
      available: true,
      check_deferred: true,
      checked_at: "2026-10-15T02:00:00Z",
    },
    schedule: {
      ...dashboard().schedule,
      disabled_reason: "authorization_revoked",
    },
    recent_schedule_events: [
      {
        date: "2026-10-15",
        timezone: "Etc/UTC",
        state: "authorization_check_failed",
        error: "authorization_check_failed",
        at: "2026-10-15T02:00:00Z",
      },
    ],
  };
  await page.route("**/api/v1/admin/backups**", async (route) => {
    await route.fulfill({
      json: route.request().url().endsWith("/history")
        ? { items: [runningJob] }
        : value,
    });
  });
  await openBackups(page);
  await expect(page.getByText("开始前检查可用", { exact: true })).toBeVisible();
  await expect(page.getByText(/目录状态为开始前检查结果/)).toContainText(
    "实时探测已延后",
  );
  await expect(page.getByText(/原计划管理员权限已撤销/)).toBeVisible();
  await expect(
    page.getByRole("button", { name: "取消当前任务" }),
  ).toBeEnabled();
  await page.getByText("最近 20 条计划执行记录", { exact: true }).click();
  await expect(page.getByText(/暂时无法核验计划授权/)).toBeVisible();
  await noOverflow(page);
});

test("BM-UI-03: every configured directory and disk failure stays actionable without a false success", async ({
  page,
  request,
}) => {
  await login(page, request);
  let directoryError = "not_configured";
  let jobError = "";
  await page.route("**/api/v1/admin/backups**", async (route) => {
    const value = {
      ...dashboard(null),
      availability: {
        configured: directoryError !== "not_configured",
        available: !directoryError,
        error: directoryError,
      },
    };
    await route.fulfill({
      json: route.request().url().endsWith("/history")
        ? {
            items: jobError
              ? [{ ...runningJob, state: "failed", error: jobError }]
              : [],
          }
        : value,
    });
  });
  await openBackups(page);
  const directory = page.getByRole("region", { name: "备份目录", exact: true });
  for (const [code, text] of [
    ["not_configured", "尚未配置备份目录"],
    ["invalid_root_identity", "目录或标识格式无效"],
    ["root_identity_invalid", "标识不符或无法读取"],
    ["root_overlaps_data", "数据或密钥目录重叠"],
    ["root_replaced", "被替换或重新挂载"],
    ["root_unavailable", "无法访问备份目录"],
    ["root_permissions_unsafe", "权限不符合要求"],
    ["insufficient_space", "备份空间不足"],
    ["journal_invalid", "备份任务记录异常"],
  ]) {
    directoryError = code;
    await page.getByRole("button", { name: "刷新状态" }).click();
    await expect(directory).toContainText(text);
    await expect(page.getByRole("button", { name: "立即备份" })).toBeDisabled();
    await expect(page.getByText("尚无成功备份", { exact: true })).toBeVisible();
  }
  directoryError = "";
  for (const [code, text] of [
    ["timeout", "任务超时"],
    ["backup_failed", "备份或验证失败"],
    ["journal_write_failed", "任务记录写入失败"],
    ["process_interrupted", "core 在任务完成前停止"],
    ["capacity_check_failed", "备份空间检查失败"],
    ["unsafe_backup_target", "任务目录不符合安全约束"],
    ["resume_point_invalid", "原备份检查点不可用"],
    ["destination_permission_denied", "备份目标拒绝写入"],
  ]) {
    jobError = code;
    await page.getByRole("button", { name: "刷新状态" }).click();
    await expect(page.locator(".backup-history")).toContainText(text);
    await expect(
      page.getByRole("button", { name: "续传此任务" }),
    ).toBeEnabled();
    await expect(page.getByText("尚无成功备份", { exact: true })).toBeVisible();
  }
  await noOverflow(page);
});

test("BM-UI-05: keyboard confirmation, cancellation, resume and persistent live announcements", async ({
  page,
  request,
  context,
}) => {
  await login(page, request);
  const navigation = page
    .getByRole("navigation")
    .getByRole("link", { name: "备份与恢复" });
  await navigation.focus();
  await page.keyboard.press("Enter");
  await expect(
    page.getByRole("heading", { name: "备份管理", exact: true }),
  ).toBeFocused();
  const progress = page.getByRole("status", { name: "备份进度", exact: true });
  await expect(progress).toHaveAttribute("aria-live", "polite");
  await expect(progress).toHaveAttribute("aria-atomic", "true");
  const start = page.getByRole("button", { name: "立即备份" });
  await expect(start).toBeEnabled();
  await start.focus();
  await page.keyboard.press("Enter");
  const codeInput = page.getByLabel("确认操作的验证器动态码");
  await expect(codeInput).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(page.getByRole("button", { name: "验证并执行" })).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(codeInput).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(start).toBeFocused();
  await control(request, "backup-hold");
  let releaseVerification!: () => void;
  let holdFirstVerification = true;
  const verificationGate = new Promise<void>((resolve) => {
    releaseVerification = resolve;
  });
  await page.route("**/api/v1/identity/challenges/*/verify", async (route) => {
    if (holdFirstVerification) {
      holdFirstVerification = false;
      await verificationGate;
    }
    await route.continue();
  });
  try {
    await page.keyboard.press("Enter");
    await codeInput.fill((await control(request, "code")).code);
    await page.keyboard.press("Enter");
    await expect(page.getByRole("dialog")).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(page.getByRole("dialog")).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).toBeVisible();
    releaseVerification();
    await expect(
      page.getByRole("heading", { name: "当前任务", exact: true }),
    ).toBeFocused();
    await expect(progress).toContainText("等待在途操作完成");
    await page.keyboard.press("Tab");
    await expect(
      page.getByRole("button", { name: "取消当前任务" }),
    ).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(progress).toContainText("已取消");
    await expect(
      page.getByRole("status", { name: "备份操作结果", exact: true }),
    ).toContainText("取消请求已受理");
  } finally {
    releaseVerification();
    await control(request, "backup-release");
    await expect
      .poll(
        async () =>
          (await (await context.request.get("/api/v1/admin/backups")).json())
            .current,
        { timeout: 60000 },
      )
      .toBeNull();
  }
  const resume = page.getByRole("button", { name: "续传此任务" }).first();
  await resume.focus();
  await page.keyboard.press("Enter");
  await expect(codeInput).toBeFocused();
  await codeInput.fill((await control(request, "code")).code);
  await page.keyboard.press("Enter");
  await expect
    .poll(
      async () =>
        (
          await (
            await context.request.get("/api/v1/admin/backups/history")
          ).json()
        ).items[0].state,
      { timeout: 60000 },
    )
    .toBe("complete");
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect(progress).toContainText("已完成并验证");
  const toggle = page.getByLabel("开启每日备份");
  await toggle.focus();
  await page.keyboard.press("Space");
  await expect(toggle).toBeChecked();
  const save = page.getByRole("button", { name: "验证并保存计划" });
  await save.focus();
  await page.keyboard.press("Enter");
  await expect(codeInput).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(save).toBeFocused();
  await noOverflow(page);
});

async function expiredControlFlow(
  page: Page,
  request: APIRequestContext,
  context: BrowserContext,
  info: TestInfo,
  phase: "copying" | "draining",
) {
  await login(page, request);
  await openBackups(page);
  await control(
    request,
    phase === "copying" ? "backup-copy-hold" : "backup-hold",
  );
  try {
    await page.getByRole("button", { name: "立即备份" }).click();
    await approve(page, request);
    if (phase === "copying") {
      await expect
        .poll(
          async () => (await control(request, "backup-copy-status")).holding,
          { timeout: 60000 },
        )
        .toBe(true);
    } else {
      await expect(page.locator(".backup-current .backup-state")).toHaveText(
        "等待在途操作完成",
      );
    }
    const job = (
      await (await context.request.get("/api/v1/admin/backups")).json()
    ).current;
    expect(job.state).toBe(phase === "copying" ? "backing_up" : "draining");
    await control(request, "expire");
    await page.reload();
    await expect(
      page.getByRole("heading", { name: "登录兰台", exact: true }),
    ).toBeVisible();
    expect((await context.request.get("/api/v1/whoami")).status()).toBe(401);
    await page
      .getByRole("link", { name: "备份期间重新验证管理员身份" })
      .click();
    await expect(
      page.getByRole("heading", { name: "备份维护身份验证", exact: true }),
    ).toBeVisible();
    const credentials = await control(request, "login");
    await page
      .getByLabel("维护管理账户", { exact: true })
      .fill(credentials.name);
    await page
      .getByLabel("维护管理口令", { exact: true })
      .fill(credentials.password);
    await page
      .getByLabel("维护验证器动态码", { exact: true })
      .fill(credentials.code);
    const issuing = page.waitForResponse(
      (r) =>
        r.url().endsWith("/api/v1/admin/backups/control-session") &&
        r.request().method() === "POST",
    );
    await page.getByRole("button", { name: "验证并查看当前备份" }).click();
    const response = await issuing;
    expect(response.status()).toBe(201);
    expect(response.request().headers()["x-csrf-token"]).toBeUndefined();
    const restricted = await response.json();
    expect(restricted.scope).toEqual({ job_id: job.id, attempt: job.attempt });
    expect(restricted.capabilities.sort()).toEqual([
      "backup.cancel",
      "backup.read",
    ]);
    const cookie = (await context.cookies()).find(
      (c) => c.name === "lantai_backup_control",
    );
    expect(cookie).toMatchObject({
      secure: true,
      httpOnly: true,
      sameSite: "Strict",
      path: "/api/v1/admin/backups",
    });
    await expect(
      page.getByRole("heading", { name: "备份维护管理", exact: true }),
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "立即备份" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "续传此任务" })).toHaveCount(
      0,
    );
    await expect(
      page.getByRole("button", { name: "验证并保存计划" }),
    ).toHaveCount(0);
    await expect(page.getByRole("navigation")).toHaveCount(0);
    const stored = await page.evaluate(() =>
      JSON.stringify({ ...sessionStorage, ...localStorage }),
    );
    expect(stored).not.toContain(restricted.csrf_token);
    expect(stored).not.toContain(credentials.password);
    await page.reload();
    await expect(
      page.getByRole("heading", { name: "备份维护管理", exact: true }),
    ).toBeVisible();
    await expect(page.locator(".backup-current .backup-state")).toHaveText(
      phase === "copying" ? "正在复制与验证" : "等待在途操作完成",
    );
    await shot(page, info, `maintenance-${phase}`);
    const cancelling = page.waitForResponse((r) =>
      r.url().endsWith(`/${job.id}/cancel`),
    );
    await page.getByRole("button", { name: "取消当前任务" }).click();
    expect((await cancelling).status()).toBe(202);
    if (phase === "copying") {
      await expect(page.locator(".backup-current .backup-state")).toHaveText(
        "正在安全取消",
      );
      expect((await control(request, "backup-copy-status")).holding).toBe(true);
    }
    expect((await context.request.get("/api/v1/whoami")).status()).toBe(401);
  } finally {
    await control(
      request,
      phase === "copying" ? "backup-copy-release" : "backup-release",
    );
  }
  await expect(
    page.getByRole("heading", { name: "备份维护身份验证", exact: true }),
  ).toBeVisible({ timeout: 15000 });
  await expect(page.getByText(/任务已结束或授权已失效/)).toBeVisible();
  await page.getByRole("button", { name: "返回普通登录" }).click();
  await expect(
    page.getByRole("heading", { name: "登录兰台", exact: true }),
  ).toBeVisible();
  expect((await context.request.get("/api/v1/whoami")).status()).toBe(401);
  await noOverflow(page);
  await login(page, request);
  expect((await context.request.get("/api/v1/admin/backups")).status()).toBe(
    200,
  );
}

test("F1: expired ordinary session reauthenticates only for read and cancel during real copying", async ({
  page,
  request,
  context,
}, info) => {
  await expiredControlFlow(page, request, context, info, "copying");
});

test("F1: expired ordinary session reauthenticates while draining without releasing the held work", async ({
  page,
  request,
  context,
}, info) => {
  await expiredControlFlow(page, request, context, info, "draining");
});
