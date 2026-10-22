import { test, expect } from "./fixtures";
import { deviceJobDigest, type DeviceJobInput } from "../src/device-protocol";
import type { APIRequestContext } from "@playwright/test";

// Real HTTP authority + actual browser controls, using only the temporary Go
// fixture. This test simulates a device's inspection report; it does not claim
// the worker opened, edited, or executed a native file.
async function control(request: APIRequestContext, action: string) {
  const response = await request.post(`/__fixture/${action}`, {
    headers: { "X-Lantai-Fixture": "1" },
  });
  expect(response.ok()).toBeTruthy();
  return response.status() === 204 ? undefined : response.json();
}
test("synthetic device pairing, exact inspection approval and original fence cancellation use the protected API", async ({
  page,
  context,
  request,
  baseURL,
}) => {
  test.setTimeout(90000);
  const origin = new URL(baseURL!).origin;
  await control(request, "roles?enabled=true");
  try {
    const credentials = await control(request, "login");
    await page.goto("/");
    await page.getByLabel("账户名称").fill(credentials.name);
    await page.getByLabel("口令", { exact: true }).fill(credentials.password);
    await page
      .getByLabel("验证器动态码", { exact: true })
      .fill(credentials.code);
    await page
      .getByLabel("素材写入与整理（迁移、工作集档案和信息编辑）", {
        exact: true,
      })
      .check();
    await page.getByRole("button", { name: "登录并浏览" }).click();
    await expect(
      page.getByRole("heading", { name: "你好，Ada" }),
    ).toBeVisible();
    const cliCode = await control(request, "code");
    const login = await request.post("/api/v1/sessions/login", {
      data: {
        name: credentials.name,
        password: credentials.password,
        code: cliCode.code,
        channel: "cli",
        projects: [credentials.project],
        scopes: ["self", "read", "organize"],
        ttl_seconds: 900,
      },
    });
    expect(login.ok()).toBeTruthy();
    const session = await login.json();
    const worker = async (path: string, data: unknown) => {
      const response = await request.post(path, {
        headers: {
          Authorization: `Bearer ${session.token}`,
          "Idempotency-Key": crypto.randomUUID(),
        },
        data,
      });
      expect(response.ok()).toBeTruthy();
      return response.json();
    };
    const binding = "a".repeat(64);
    const paired = await worker("/api/v1/devices/claims", {
      project_id: credentials.project,
      alias: "synthetic-ui-device",
      workspace_alias: "synthetic-workspace",
      workspace_binding: binding,
      actions: ["references.plan"],
    });
    const id = paired.device.device_id;
    await page.goto("/#/admin/system");
    await page
      .getByLabel("已授权项目", { exact: true })
      .selectOption(credentials.project);
    await page
      .getByRole("button", { name: "设备与本机执行", exact: true })
      .click();
    await page.getByLabel("本机显示的设备标识", { exact: true }).fill(id);
    await page
      .getByRole("button", { name: "读取明确设备", exact: true })
      .click();
    await page
      .getByLabel("本机一次认领代码", { exact: true })
      .fill(paired.claim_code);
    await page.getByLabel(/已核对本机显示的同一主体/).check();
    await page
      .getByRole("button", { name: "核对并认领此设备", exact: true })
      .click();
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "确认此固定请求", exact: true })
      .click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    const active = await (
      await context.request.get(`/api/v1/devices/${id}`)
    ).json();
    expect(active.state).toBe("active");
    const csrf = await page.evaluate(
      () => JSON.parse(sessionStorage.getItem("lantai.csrf.v1")!).value,
    );
    const job: DeviceJobInput = {
      schema: "lantai.local-assistant-job/v1",
      job_id: `synthetic-inspection-${crypto.randomUUID()}`,
      action: "references.plan",
      input: { synthetic_only: true },
      directory: "sources/notes",
      output: "reports/plan.json",
    };
    const digest = await deviceJobDigest(job, origin);
    const created = await context.request.post(`/api/v1/devices/${id}/jobs`, {
      headers: {
        Origin: origin,
        "X-CSRF-Token": csrf,
        "Idempotency-Key": crypto.randomUUID(),
      },
      data: {
        project_id: credentials.project,
        expected_device_revision: active.revision,
        workspace_alias: active.workspace_alias,
        job,
        job_digest: digest,
      },
    });
    expect(created.ok()).toBeTruthy();
    const queued = await created.json();
    const pending = await worker(`/api/v1/devices/${id}/poll`, {});
    expect(pending.job.job_id).toBe(queued.job_id);
    expect(pending.job.job_fence).toBeUndefined();
    const inspection = {
      schema: "lantai.local-assistant-inspection/v1",
      job_id: job.job_id,
      action: job.action,
      job_digest: digest,
      approval_digest: "b".repeat(64),
      workspace_binding: binding,
      input_sha256: "c".repeat(64),
      paths: { directory: job.directory, output: job.output },
      network_required: false,
      remote_writes: false,
      source_deleted: false,
      timeout_seconds: 3600,
      warnings: [
        "Synthetic inspection report; no native file execution in this test.",
      ],
    };
    await worker(`/api/v1/devices/${id}/jobs/${queued.job_id}/inspect`, {
      inspection,
    });
    await page
      .getByRole("button", { name: "读取明确设备", exact: true })
      .click();
    await page
      .getByRole("row")
      .filter({ hasText: queued.job_id })
      .getByRole("button", { name: "读取原作业", exact: true })
      .click();
    await page.getByLabel(/已核对原作业、输入摘要、工作区绑定/).check();
    const approvals: { body: string | null; key: string }[] = [];
    await page.route(
      `**/api/v1/devices/${id}/jobs/${queued.job_id}/approve`,
      async (route) => {
        approvals.push({
          body: route.request().postData(),
          key: route.request().headers()["idempotency-key"],
        });
        if (approvals.length === 1) {
          const accepted = await route.fetch();
          expect(accepted.ok()).toBeTruthy();
          await route.abort("failed");
        } else await route.continue();
      },
    );
    await page
      .getByRole("button", { name: "批准原作业检查并允许领取", exact: true })
      .click();
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "确认此固定请求", exact: true })
      .click();
    await expect(
      page
        .getByRole("dialog")
        .getByRole("button", { name: "按原请求与键重试", exact: true }),
    ).toBeEnabled();
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "按原请求与键重试", exact: true })
      .click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    expect(approvals).toHaveLength(2);
    expect(approvals[1]).toEqual(approvals[0]);
    const claimed = await worker(`/api/v1/devices/${id}/poll`, {});
    expect(claimed.job.job_id).toBe(queued.job_id);
    expect(claimed.job.job_fence.job_id).toBe(queued.job_id);
    expect(claimed.job.job_fence.lease_fence).toBeGreaterThan(0);
    await page
      .getByRole("button", { name: "刷新原作业观察", exact: true })
      .click();
    await expect(
      page.getByText(claimed.job.job_fence.job_attempt_id, { exact: false }),
    ).toBeVisible();
    await page
      .getByRole("button", { name: "取消原队列作业", exact: true })
      .click();
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "确认此固定请求", exact: true })
      .click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await page
      .getByRole("button", { name: "请求撤销此设备授权", exact: true })
      .click();
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "确认此固定请求", exact: true })
      .click();
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await page
      .getByRole("button", { name: "刷新原作业观察", exact: true })
      .click();
    await expect(
      page.getByText(/结果不明，保留原作业对账/).first(),
    ).toBeVisible();
    const final = await (
      await context.request.get(`/api/v1/devices/${id}/jobs/${queued.job_id}`)
    ).json();
    expect(final.job_id).toBe(queued.job_id);
    expect(final.operation_id).toBe(queued.operation_id);
    expect(final.job_fence).toEqual(claimed.job.job_fence);
    expect(
      JSON.stringify(
        await page.evaluate(() => [
          Object.entries(localStorage),
          Object.entries(sessionStorage),
        ]),
      ),
    ).not.toContain(session.token);
  } finally {
    await control(request, "roles?enabled=false");
  }
});
