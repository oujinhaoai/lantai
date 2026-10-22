import assert from "node:assert/strict";
import { test } from "node:test";
import {
  assertNativeJSONRoundTrip,
  blankMigrationDraft,
  filesForAsset,
  freezeMigration,
  importMigrationWorkspace,
  makeAssistantJob,
  nativeImportInput,
  preflightMigration,
  validateMigrationDraft,
  type MigrationDraft,
} from "../src/migration-model";
import {
  browserFileLimit,
  canonicalJSON,
  sha256,
} from "../src/managed-transfer";

const origin = "https://synthetic.example";
Object.defineProperty(globalThis, "location", {
  configurable: true,
  value: new URL(origin),
});
const project = "01J00000000000000000000001";
const instance = "01J00000000000000000000002";
function draft(size = 12): MigrationDraft {
  return {
    ...blankMigrationDraft(project),
    assets: [
      {
        key: "reference",
        directory: "selected",
        classification: "unknown",
        push: {
          project_id: project,
          slug: "reference/synthetic",
          describe: { title: "Synthetic reference" },
          content: {
            asset_type: "doc",
            rights: {
              usage: "restricted",
              license: "LicenseRef-Synthetic",
              redistribute_raw: false,
              noai: true,
              sensitivity: "normal",
            },
            files: [{ path: "sample.txt", role: "primary", size, sha256: "" }],
          },
        },
      },
    ],
    excluded: [{ path: "excluded", reason: "保留源，另行备份" }],
    working_inputs: [{ path: "working.blend", reason: "可变工程输入另行灾备" }],
  };
}
const inventory = [
  { path: "selected/sample.txt", size: 12 },
  { path: "excluded/notes.txt", size: 8 },
  { path: "working.blend", size: 30 },
  { path: "unclassified.bin", size: 40 },
];
test("preflight preserves uncovered, excluded and working inputs without opting them into upload", () => {
  const plan = draft();
  const report = preflightMigration(plan, inventory);
  assert.equal(report.logical_bytes, 12);
  assert.equal(report.selected_files, 1);
  assert.deepEqual(
    report.uncovered.map((f) => f.path),
    ["unclassified.bin"],
  );
  assert.deepEqual(
    report.excluded.map((f) => f.path),
    ["excluded/notes.txt"],
  );
  assert.deepEqual(
    report.working_inputs.map((f) => f.path),
    ["working.blend"],
  );
  assert.equal(report.browser_eligible, true);
  assert.equal(report.capacity_failures.length, 0);
  assert.equal(report.capacity_unknown.length, 5);
  const native = nativeImportInput(plan);
  assert.equal(native.schema, "lantai.asset-import-input/v1");
  assert.equal(native.assets.length, 1);
  assert.equal(native.excluded.length, 2);
  assert.equal(plan.working_inputs.length, 1);
});
test("preflight rejects changed, missing, duplicate and multiply assigned sources", () => {
  assert.throws(
    () =>
      preflightMigration(
        draft(),
        inventory.map((item, i) => (i ? item : { ...item, size: 13 })),
      ),
    /大小已变化/,
  );
  assert.throws(
    () => preflightMigration(draft(), inventory.slice(1)),
    /缺少计划文件/,
  );
  assert.throws(
    () =>
      preflightMigration(draft(), [
        ...inventory,
        { path: "SELECTED/sample.txt", size: 12 },
      ]),
    /等价项/,
  );
  const repeated = draft();
  repeated.assets.push({
    ...structuredClone(repeated.assets[0]),
    key: "repeated",
  });
  assert.throws(() => preflightMigration(repeated, inventory), /重复选择/);
});
test("selection, exclusion and working-input responsibility cannot overlap", () => {
  const overlap = draft();
  overlap.excluded.push({ path: "selected", reason: "明确但冲突" });
  assert.throws(() => preflightMigration(overlap, inventory), /重叠/);
  const nested = draft();
  nested.working_inputs.push({ path: "excluded/nested", reason: "冲突" });
  assert.throws(() => preflightMigration(nested, inventory), /互相嵌套/);
  const noReason = draft();
  noReason.excluded[0].reason = " ";
  assert.throws(() => validateMigrationDraft(noReason), /原因/);
});
test("plan fixes a project, rights and native-safe paths before any write", () => {
  const crossProject = draft();
  crossProject.assets[0].push.project_id = instance;
  assert.throws(() => validateMigrationDraft(crossProject), /同一明确项目/);
  const noRights = draft();
  delete noRights.assets[0].push.content.rights;
  assert.throws(() => validateMigrationDraft(noRights), /rights/);
  const escape = draft();
  escape.assets[0].directory = "../outside";
  assert.throws(() => validateMigrationDraft(escape));
  const append = draft();
  append.assets[0].push.asset_id = instance;
  assert.throws(() => validateMigrationDraft(append), /base_version_id/);
});
test("capacity separately accounts for main staging, backup snapshot/package, restore and verification", () => {
  const plan = draft();
  plan.capacity = {
    existing_library_bytes: 100,
    free_main_bytes: 24,
    free_backup_bytes: 223,
    free_restore_bytes: 336,
    free_verification_bytes: 24,
  };
  const report = preflightMigration(plan, inventory);
  assert.deepEqual(report.budgets, {
    main: 24,
    backup: 224,
    restore: 336,
    verification: 24,
  });
  assert.deepEqual(report.capacity_failures, ["快照＋完整备份包"]);
  assert.deepEqual(report.capacity_unknown, []);
  plan.capacity.free_backup_bytes = 224;
  assert.deepEqual(preflightMigration(plan, inventory).capacity_failures, []);
  plan.capacity.existing_library_bytes = Number.MAX_SAFE_INTEGER;
  assert.throws(() => preflightMigration(plan, inventory), /可靠整数/);
});
test("large files remain intact in the native plan and are explicitly ineligible for browser transport", () => {
  const size = browserFileLimit + 1;
  const plan = draft(size);
  assert.equal(
    preflightMigration(plan, [{ path: "selected/sample.txt", size }])
      .browser_eligible,
    false,
  );
  assert.equal(
    nativeImportInput(plan).assets[0].push.content.files[0].size,
    size,
  );
});
async function frozen() {
  const file = new File(["synthetic123"], "sample.txt");
  const plan = draft(file.size);
  const files = [{ path: "selected/sample.txt", role: "source", file }];
  return {
    workspace: await freezeMigration({
      plan,
      files,
      instanceID: instance,
      origin,
    }),
    files,
    plan,
  };
}
test("freeze hashes selected files and imports the original keys with running changed to unknown", async () => {
  const { workspace, files } = await frozen();
  assert.equal(
    workspace.plan.assets[0].push.content.files[0].sha256,
    await sha256(files[0].file),
  );
  assert.equal(
    workspace.plan_digest,
    `sha256:${await sha256(canonicalJSON(workspace.plan))}`,
  );
  workspace.rows.reference.status = "running";
  const restored = await importMigrationWorkspace(workspace, origin);
  assert.equal(restored.rows.reference.status, "unknown");
  assert.equal(
    restored.states.reference.create_key,
    workspace.states.reference.create_key,
  );
  assert.equal(
    restored.states.reference.commit_key,
    workspace.states.reference.commit_key,
  );
  assert.equal(
    filesForAsset(workspace.plan.assets[0], files)[0].path,
    "sample.txt",
  );
  assert.equal(restored.plan.assets[0].push.content.files[0].role, "primary");
});
test("resume rejects origin changes, altered plans, dropped states and fabricated completed rows", async () => {
  const { workspace } = await frozen();
  await assert.rejects(
    () => importMigrationWorkspace(workspace, "https://other.example"),
    /origin/,
  );
  const changed = structuredClone(workspace);
  changed.plan.name = "edited";
  await assert.rejects(() => importMigrationWorkspace(changed, origin), /摘要/);
  const missing = structuredClone(workspace);
  delete missing.states.reference;
  await assert.rejects(
    () => importMigrationWorkspace(missing, origin),
    /缺少分组/,
  );
  const wrongKey = structuredClone(workspace);
  wrongKey.states.reference.request_hash = "0".repeat(64);
  await assert.rejects(
    () => importMigrationWorkspace(wrongKey, origin),
    /摘要/,
  );
  const complete = structuredClone(workspace);
  complete.rows.reference.status = "committed";
  await assert.rejects(
    () => importMigrationWorkspace(complete, origin),
    /缺少原固定版本/,
  );
});
test("known insufficient capacity stops freeze while unknown capacity stays explicit", async () => {
  const { files, plan } = await frozen();
  plan.capacity = { free_main_bytes: 0 };
  await assert.rejects(
    () => freezeMigration({ plan, files, instanceID: instance, origin }),
    /空间不足/,
  );
});
test("assistant jobs preserve original input and stable state without credentials or path escape", () => {
  const plan = {
    schema: "lantai.asset-import-plan/v1",
    digest: "sha256:" + "a".repeat(64),
    input: nativeImportInput(draft()),
  };
  const report = {
    schema: "lantai.asset-import-report/v1",
    results: [{ key: "reference", status: "unknown" }],
  };
  const job = makeAssistantJob({
    action: "import.verify",
    input: plan,
    report,
    source: "source",
    state: "state/original-batch",
    destination: "verify/batch",
    output: "results/verify.json",
    origin,
    jobID: "migration-stable-verify",
  });
  assert.equal(job.state, "state/original-batch");
  assert.equal(job.input, plan);
  assert.equal(job.report, report);
  assert.equal(job.purpose, "archive_review");
  assert.equal(job.origin, origin);
  assert.equal("token" in job, false);
  for (const source of [
    "../outside",
    "/home/private",
    "C:\\private",
    "source/../outside",
  ])
    assert.throws(() =>
      makeAssistantJob({ action: "import.plan", input: {}, source, origin }),
    );
});

test("reserved object property keys never produce an unexportable state", async () => {
  for (const key of ["__proto__", "constructor", "prototype"]) {
    const plan = draft();
    plan.assets[0].key = key;
    assert.throws(() => validateMigrationDraft(plan), /保留属性/);
  }
  const { workspace } = await frozen();
  const restored = await importMigrationWorkspace(
    JSON.parse(JSON.stringify(workspace)),
    origin,
  );
  assert.equal(
    restored.states.reference.create_key,
    workspace.states.reference.create_key,
  );
  const nativeOnly = structuredClone(workspace) as Partial<typeof workspace>;
  delete nativeOnly.state_format;
  await assert.rejects(() => importMigrationWorkspace(nativeOnly, origin));
});
test("credentials and short-lived authorization URLs cannot freeze or survive imported workspace plans", async () => {
  const { workspace, files, plan } = await frozen();
  plan.assets[0].push.task = { token: "synthetic-marker" };
  await assert.rejects(
    () => freezeMigration({ plan, files, instanceID: instance, origin }),
    /凭据/,
  );
  const changed = structuredClone(workspace);
  changed.plan.assets[0].push.content.metadata = {
    read_grant_url: "https://foreign.invalid/xfer/temporary",
  };
  await assert.rejects(
    () => importMigrationWorkspace(changed, origin),
    /短时传输/,
  );
});

test("recognizable Bearer strings and signed transfer URLs are rejected under arbitrary metadata keys", () => {
  for (const url of [
    "https://private.invalid/xfer/v1/reads/temporary",
    "https://private.invalid/object?sig=synthetic",
    "https://user:secret@private.invalid/file",
    "Bearer synthetic-token",
  ]) {
    const plan = draft();
    plan.assets[0].push.content.metadata = { source: url };
    assert.throws(() => validateMigrationDraft(plan), /凭据|短时传输/);
  }
});

test("native plan continuation rejects lexical loss without repairing original digest or keys", () => {
  for (const raw of [
    '{"metadata":{"n":1.0}}',
    '{"metadata":{"2":"b","1":"a"}}',
    '{"metadata":{"text":"\\u4e2d\\u6587"}}',
    '{"metadata":{"path":"a\\/b"}}',
  ]) {
    const parsed = JSON.parse(raw);
    const original = raw;
    assert.throws(() => assertNativeJSONRoundTrip(raw, parsed), /input_file/);
    assert.equal(raw, original);
  }
  const ordinary =
    '{ \n "schema": "lantai.asset-import-plan/v1", "input": {"metadata":{"n":1,"text":"中文"}} }';
  assert.doesNotThrow(() =>
    assertNativeJSONRoundTrip(ordinary, JSON.parse(ordinary)),
  );
});

test("device import private results bind actual queue identity and original input digest", async () => {
  const { bindImportDeviceResult } = await import("../src/migration-model");
  const job = makeAssistantJob({
    action: "import.plan",
    input: nativeImportInput(draft()),
    source: "source",
    origin,
    jobID: "migration-stable-plan",
  });
  const digest = await sha256(canonicalJSON(job));
  const reference = {
    schema: "lantai.local-assistant-result-ref/v1",
    job_id: project,
    result_sha256: "a".repeat(64),
  };
  const context = {
    job_id: project,
    job_digest: digest,
    state: "succeeded",
    stopped: true,
    workspace_binding: "b".repeat(64),
  };
  const envelope = {
    schema: "lantai.local-assistant-result/v1",
    job_id: job.job_id,
    action: job.action,
    job_digest: digest,
    status: "completed",
    exit_code: 0,
    result: reference,
  };
  const bound = await bindImportDeviceResult({ job, envelope, context });
  assert.equal(bound.completed, true);
  assert.deepEqual(bound.reference, reference);
  assert.equal("complete" in bound.reference, false);
  await assert.rejects(() =>
    bindImportDeviceResult({
      job,
      envelope: { ...envelope, job_digest: "c".repeat(64) },
      context,
    }),
  );
  await assert.rejects(() =>
    bindImportDeviceResult({
      job,
      envelope: { ...envelope, result: { ...reference, job_id: instance } },
      context,
    }),
  );
  for (const changed of [
    { ...context, state: "running" },
    { ...context, state: "needs_reconciliation" },
    { ...context, stopped: false },
  ])
    assert.equal(
      (await bindImportDeviceResult({ job, envelope, context: changed }))
        .completed,
      false,
    );
});
test("device apply and verify preserve exact private original plan/report refs and original state path", () => {
  const plan = {
    schema: "lantai.local-assistant-result-ref/v1",
    job_id: project,
    result_sha256: "a".repeat(64),
  };
  const report = {
    schema: "lantai.local-assistant-result-ref/v1",
    job_id: instance,
    result_sha256: "b".repeat(64),
  };
  const apply = makeAssistantJob({
    action: "import.apply",
    input: plan,
    source: "source",
    state: "state/original",
    origin,
    jobID: "migration-stable-apply",
  });
  const verify = makeAssistantJob({
    action: "import.verify",
    input: plan,
    report,
    source: "source",
    state: "state/original",
    destination: "verified/original",
    origin,
    jobID: "migration-stable-verify",
  });
  assert.equal(apply.input, plan);
  assert.equal(verify.input, plan);
  assert.equal(verify.report, report);
  assert.equal(apply.state, verify.state);
  assert.equal(verify.state, "state/original");
  assert.equal(verify.destination, "verified/original");
});
