import { test, expect, type Page } from "@playwright/test";
import { createHash } from "node:crypto";
import {
  batchFromWorkspace,
  type BatchCheck,
} from "../src/batch-acceptance-model";
import { canonicalJSON } from "../src/managed-transfer";
const id = (n: number) =>
  `01J000000000000000000000${String(n).padStart(2, "0")}`;
const project = id(1),
  instance = id(2),
  principal = id(3),
  asset = id(4),
  version = id(5),
  grant = id(6),
  session = id(7);
const hash = (value: string | Buffer) =>
  createHash("sha256").update(value).digest("hex");
async function controlled(page: Page) {
  await page.addInitScript(
    ({ session }) =>
      sessionStorage.setItem(
        "lantai.csrf.v1",
        JSON.stringify({ session, value: "synthetic-csrf-only" }),
      ),
    { session },
  );
  await page.route("**/api/v1/whoami", (route) =>
    route.fulfill({
      json: {
        principal: {
          principal_id: principal,
          name: "synthetic human",
          kind: "human",
        },
        session: {
          session_id: session,
          channel: "browser",
          session_kind: "normal",
          scopes: ["self", "read", "ingest", "organize"],
          expires_at: "2099-01-01T00:00:00Z",
        },
        project_roles: { [project]: ["owner"] },
        system_roles: [],
      },
    }),
  );
  await page.route("**/api/v1/projects?*", (route) =>
    route.fulfill({
      json: {
        items: [
          {
            project_id: project,
            key: "synthetic",
            state: "active",
            description: {
              name: "合成项目",
              project_type: "production",
              revision: 1,
            },
          },
        ],
      },
    }),
  );
  await page.route("**/api/v1/meta", (route) =>
    route.fulfill({ json: { api_version: "v1", instance_id: instance } }),
  );
  await page.route("**/api/v1/devices?*", (route) =>
    route.fulfill({ json: { items: [] } }),
  );
  await page.goto("/#/migrations");
  await page
    .getByRole("combobox", { name: "目标项目", exact: true })
    .selectOption(project);
}
test("new journal fixes actual actor and preserves incomplete scope instead of showing accepted", async ({
  page,
}) => {
  await controlled(page);
  await page.getByRole("button", { name: "从当前明确批次建立台账" }).click();
  await expect(page.getByLabel("责任人主体 ID")).toHaveValue(principal);
  await expect(
    page.getByText("导入统计未覆盖全部计划的已提交项", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "服务端当前复核与接受判定" }),
  ).toHaveCount(0);
  await expect(page.getByLabel("接受使用的明确用途")).toHaveValue("production");
  await expect(
    page.getByRole("button", { name: "按原计划创建改链副本" }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "保留新增成果并创建回退副本" }),
  ).toBeDisabled();
});
test("real typed record loads into ledger and server native unknown remains blocked", async ({
  page,
}) => {
  const batch = batchFromWorkspace({
    projectID: project,
    instanceID: instance,
    actorID: principal,
    title: "合成批次台账",
  });
  const payload = {
    schema: "lantai.management-record/v1",
    kind: "migration",
    instance_id: instance,
    project_id: project,
    payload: batch,
  };
  const bytes = Buffer.from(canonicalJSON(payload) + "\n");
  const file = {
    path: "record.json",
    role: "record",
    size: bytes.length,
    sha256: hash(bytes),
  };
  const content = {
    asset_type: "config",
    files: [file],
    rights: {
      usage: "reference",
      license: "LicenseRef-Lantai-Private-Record",
      redistribute_raw: false,
      noai: true,
      sensitivity: "normal",
    },
  };
  const digest = "sha256:" + hash(canonicalJSON(content));
  await page.route("**/api/v1/assets?*", (route) =>
    route.fulfill({
      json: {
        items: [
          {
            asset_id: asset,
            version_id: version,
            project_id: project,
            version_number: 1,
            title: batch.title,
            slug: "management/batch",
            asset_type: "config",
            tags: ["lantai-management-record", "record-kind:migration"],
            subjects: [],
          },
        ],
      },
    }),
  );
  await page.route(`**/api/v1/assets/${asset}/versions/${version}?*`, (route) =>
    route.fulfill({
      json: {
        version: {
          asset_id: asset,
          version_id: version,
          project_id: project,
          version_number: 1,
          manifest_digest: digest,
          committed_at: "2026-10-20T00:00:00Z",
          uri: `lantai://${instance}/assets/${asset}/versions/${version}`,
        },
        manifest: {
          contract: "lantai.manifest/v1",
          instance_id: instance,
          project_id: project,
          asset_id: asset,
          version_id: version,
          version_number: 1,
          manifest_digest: digest,
          content,
        },
      },
    }),
  );
  await page.route(
    `**/api/v1/assets/${asset}/versions/${version}/read-grants`,
    (route) =>
      route.fulfill({
        json: {
          grant_id: grant,
          asset_id: asset,
          version_id: version,
          path: file.path,
          sha256: file.sha256,
          size: file.size,
          purpose: "archive_review",
          method: "GET",
          expires_at: "2099-01-01T00:00:00Z",
          url: `/xfer/v1/reads/${grant}?sig=${"a".repeat(64)}`,
        },
      }),
  );
  await page.route(`**/xfer/v1/reads/${grant}?*`, (route) =>
    route.fulfill({
      status: 200,
      body: bytes,
      headers: { "Content-Length": String(bytes.length) },
    }),
  );
  await page.route("**/api/v1/management/batches/check", async (route) => {
    expect(route.request().postDataJSON()).toEqual({
      asset_id: asset,
      version_id: version,
    });
    const value: BatchCheck = {
      schema: "lantai.batch-check/v1",
      instance_id: instance,
      record_ref: { asset_id: asset, version_id: version },
      intended_purpose: batch.intended_purpose,
      batch_id: batch.batch_id,
      revision: 1,
      checked_at: new Date().toISOString(),
      acceptance: "blocked",
      checks: [
        { key: "native_continuation", status: "unknown" },
        { key: "recovery", status: "missing" },
      ],
      assets: [],
      worksets: [],
      recovery_refs: [],
    };
    await route.fulfill({ json: value });
  });
  await controlled(page);
  await page.getByRole("button", { name: "读取本项目批次台账" }).click();
  await expect(page.getByRole("cell", { name: /合成批次台账/ })).toBeVisible();
  await page.getByRole("button", { name: "打开固定修订", exact: true }).click();
  await page.getByRole("button", { name: "服务端当前复核与接受判定" }).click();
  await expect(
    page.getByRole("heading", { name: "本次服务端接受观察：阻断" }),
  ).toBeVisible();
  await expect(page.getByText("native_continuation：unknown")).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "本次服务端接受观察：可用" }),
  ).toHaveCount(0);
});
