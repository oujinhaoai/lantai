import { test } from "node:test";
import assert from "node:assert/strict";
import {
  batchCheckKeys,
  batchFromWorkspace,
  batchReadiness,
  bindReferenceResult,
  nextBatchRevision,
  parseBatchRefLines,
  referenceJob,
  validateBatchCheck,
  validateMigrationBatch,
  type BatchCheck,
  type MigrationBatch,
} from "../src/batch-acceptance-model";
import { canonicalJSON, sha256 } from "../src/managed-transfer";
import type { MigrationWorkspace } from "../src/migration-model";
const id = (n: number) =>
  `01J000000000000000000000${String(n).padStart(2, "0")}`;
const ref = (n: number) => ({ asset_id: id(n), version_id: id(n + 1) });
function complete(): MigrationBatch {
  const batch = batchFromWorkspace({
    projectID: id(1),
    instanceID: id(2),
    actorID: id(3),
  });
  batch.import_summary = {
    planned: 1,
    committed: 1,
    description_pending: 0,
    failed: 0,
    unknown: 0,
    not_attempted: 0,
  };
  batch.evidence.assets = [
    {
      key: "one",
      ...ref(4),
      manifest_digest: "sha256:" + "a".repeat(64),
      status: "committed",
    },
  ];
  batch.evidence.worksets = [ref(6)];
  batch.evidence.verifications = [ref(8)];
  batch.evidence.recovery = [ref(10)];
  batch.evidence.reference_jobs = [
    { job_id: id(12), action: "references.plan" },
    { job_id: id(13), action: "references.copy" },
  ];
  for (const key of Object.keys(batch.checklist))
    batch.checklist[key as keyof typeof batch.checklist] = true;
  batch.acceptance = "requested";
  return batch;
}
function check(batch = complete()): BatchCheck {
  return {
    schema: "lantai.batch-check/v1",
    instance_id: batch.instance_id,
    record_ref: ref(15),
    intended_purpose: batch.intended_purpose,
    batch_id: batch.batch_id,
    revision: batch.revision,
    checked_at: new Date().toISOString(),
    acceptance: "ready",
    checks: batchCheckKeys.map((key) => ({ key, status: "passed" })),
    assets: batch.evidence.assets.map(
      ({ asset_id, version_id, manifest_digest }) => ({
        asset_id,
        version_id,
        manifest_digest,
      }),
    ),
    worksets: batch.evidence.worksets,
    recovery_refs: batch.evidence.recovery,
  };
}
test("batch journal has stable identity and immutable conditional revision, never client accepted/complete", () => {
  const batch = complete(),
    next = nextBatchRevision(batch, ref(15), id(3));
  assert.equal(next.batch_id, batch.batch_id);
  assert.equal(next.revision, 2);
  assert.deepEqual(next.previous_ref, ref(15));
  assert.equal(next.acceptance, "not_requested");
  assert.equal(next.source_retained, true);
  assert.equal(batch.revision, 1);
  assert.throws(() =>
    validateMigrationBatch({ ...batch, acceptance: "accepted" }),
  );
  assert.throws(() => validateMigrationBatch({ ...batch, complete: true }));
  assert.throws(() =>
    validateMigrationBatch({ ...next, previous_ref: undefined }),
  );
});
test("missing or invalid owner/actor, source deletion and credentials are refused", () => {
  const batch = complete();
  for (const value of [
    { ...batch, owner_id: "" },
    { ...batch, last_action: { ...batch.last_action, actor_id: "" } },
    { ...batch, source_retained: false },
    {
      ...batch,
      last_action: { ...batch.last_action, note: "Bearer synthetic-secret" },
    },
  ])
    assert.throws(() => validateMigrationBatch(value));
});
test("partial import cannot disappear merely because only committed receipts have refs", () => {
  const workspace = {
    batch_id: "stable-batch",
    instance_id: id(2),
    plan: {
      project_id: id(1),
      name: "partial",
      assets: [{ key: "one" }, { key: "two" }],
    },
    rows: { one: { status: "committed" }, two: { status: "unknown" } },
    states: {
      one: {
        result: { ...ref(4), manifest_digest: "sha256:" + "a".repeat(64) },
      },
      two: {},
    },
  } as unknown as MigrationWorkspace;
  const batch = batchFromWorkspace({
    workspace,
    projectID: id(1),
    instanceID: id(2),
    actorID: id(3),
  });
  assert.equal(batch.evidence.assets.length, 1);
  assert.equal(batch.import_summary.planned, 2);
  assert.equal(batch.import_summary.unknown, 1);
  assert.ok(batchReadiness(batch).some((x) => x.includes("全部计划")));
});
test("failed, description pending, unknown and unattempted imports block acceptance", () => {
  for (const state of [
    "failed",
    "description_pending",
    "unknown",
    "not_attempted",
  ] as const) {
    const batch = complete();
    batch.import_summary.committed = 0;
    batch.import_summary[state] = 1;
    batch.evidence.assets[0].status = state;
    assert.ok(batchReadiness(batch).length);
    assert.throws(() => validateBatchCheck(check(batch), batch, ref(15)));
  }
});
test("explicit no selected notes does not invent reference migration coverage", () => {
  const batch = complete();
  batch.reference_scope = "none";
  batch.evidence.reference_jobs = [];
  assert.deepEqual(batchReadiness(batch), []);
  batch.reference_scope = "selected_notes";
  assert.ok(batchReadiness(batch).some((x) => x.includes("笔记")));
});
test("all fixed evidence categories and checklist declarations are required before server ready can display", () => {
  for (const category of ["worksets", "verifications", "recovery"] as const) {
    const batch = complete();
    batch.evidence[category] = [];
    assert.ok(batchReadiness(batch).length);
    assert.throws(() => validateBatchCheck(check(batch), batch, ref(15)));
  }
  const batch = complete();
  batch.checklist.entrypoints_opened = false;
  assert.throws(() => validateBatchCheck(check(batch), batch, ref(15)));
});
test("server native unknown remains blocked even with every human checkbox true", () => {
  const batch = complete(),
    value = check(batch);
  value.acceptance = "blocked";
  value.checks.find((row) => row.key === "native_continuation")!.status =
    "unknown";
  assert.equal(validateBatchCheck(value, batch, ref(15)).acceptance, "blocked");
  value.acceptance = "ready";
  assert.throws(() => validateBatchCheck(value, batch, ref(15)));
});
test("acceptance binds exact record revision, instance and complete manifest/record sets", () => {
  const batch = complete();
  assert.equal(
    validateBatchCheck(check(batch), batch, ref(15)).acceptance,
    "ready",
  );
  for (const mutate of [
    (x: BatchCheck) => {
      x.instance_id = id(30);
    },
    (x: BatchCheck) => {
      x.revision++;
    },
    (x: BatchCheck) => {
      x.record_ref = ref(30);
    },
    (x: BatchCheck) => {
      x.assets[0].manifest_digest = "sha256:" + "b".repeat(64);
    },
    (x: BatchCheck) => {
      x.worksets = [];
    },
    (x: BatchCheck) => {
      x.recovery_refs = [];
    },
    (x: BatchCheck) => {
      x.checks.pop();
    },
  ]) {
    const value = structuredClone(check(batch));
    mutate(value);
    assert.throws(() => validateBatchCheck(value, batch, ref(15)));
  }
});
test("any failed, missing, pending or unknown server check prevents ready", () => {
  const batch = complete();
  for (const status of ["failed", "missing", "pending", "unknown"] as const) {
    const value = check(batch);
    value.checks[0].status = status;
    assert.throws(() => validateBatchCheck(value, batch, ref(15)));
  }
});
test("fixed reference fields reject latest, duplicate and malformed records", () => {
  assert.deepEqual(parseBatchRefLines(`${id(4)}/${id(5)}`), [ref(4)]);
  for (const value of [
    "latest",
    `${id(4)}/latest`,
    `${id(4)}/${id(5)}\n${id(4)}/${id(5)}`,
  ])
    assert.throws(() => parseBatchRefLines(value));
});
const input = {
  schema: "lantai.reference-input/v1",
  notes: ["notes/a.md"],
  mappings: [
    {
      old_target: "old.png",
      fixed_ref: `lantai://${id(2)}/assets/${id(4)}/versions/${id(5)}`,
    },
  ],
  preserve: [],
};
const plan = {
  schema: "lantai.reference-plan/v1",
  digest: "a".repeat(64),
  input,
  complete: true,
  scope: "explicit_mappings_in_selected_notes_only",
  unresolved: [],
  notes: [
    {
      path: "notes/a.md",
      before_sha256: "a".repeat(64),
      after_sha256: "b".repeat(64),
    },
  ],
};
test("reference jobs only use explicit relative roots and preserve post-cutover order/duplicates", () => {
  const refs = [input.mappings[0].fixed_ref, input.mappings[0].fixed_ref];
  const job = referenceJob({
    action: "references.rollback",
    input: plan,
    batchID: "stable",
    directory: "copy",
    destination: "rollback",
    postCutoverRefs: refs,
  });
  assert.deepEqual(job.post_cutover_refs, refs);
  assert.equal(job.input, plan);
  for (const destination of ["copy", "copy/nested", "../outside", "/host"])
    assert.throws(() =>
      referenceJob({
        action: "references.copy",
        input: plan,
        batchID: "stable",
        directory: "copy",
        destination,
      }),
    );
  assert.throws(() =>
    referenceJob({
      action: "references.rollback",
      input: plan,
      batchID: "stable",
      directory: "copy",
      destination: "rollback",
    }),
  );
});
async function resultFixture(
  action:
    | "references.plan"
    | "references.copy"
    | "references.rollback" = "references.plan",
) {
  const job = referenceJob({
    action,
    input: action === "references.plan" ? input : plan,
    batchID: "stable",
    directory: "source",
    destination: "copy",
    postCutoverRefs: [],
  });
  const digest = await sha256(canonicalJSON(job));
  const result =
    action === "references.plan"
      ? plan
      : {
          schema: "lantai.reference-receipt/v1",
          plan_digest: plan.digest,
          mode: action === "references.copy" ? "copy" : "rollback",
          scope: plan.scope,
          complete: true,
          results: [
            {
              path: "notes/a.md",
              state: "copied",
              before_sha256: "a".repeat(64),
              after_sha256: "b".repeat(64),
              output_sha256:
                action === "references.copy" ? "b".repeat(64) : "a".repeat(64),
            },
          ],
          post_cutover_refs: [],
          inventory_scope: "caller_declared_only_not_whole_library",
        };
  return {
    job: job as unknown as Record<string, unknown>,
    envelope: {
      schema: "lantai.local-assistant-result/v1",
      job_id: job.job_id,
      action,
      job_digest: digest,
      status: "completed",
      exit_code: 0,
      result,
    },
    context: { job_id: id(12), job_digest: digest },
  };
}
test("device result binds actual queue job ID and original exact digest before plan/copy can continue", async () => {
  const f = await resultFixture();
  assert.equal((await bindReferenceResult(f)).completed, true);
  await assert.rejects(() =>
    bindReferenceResult({
      ...f,
      context: { ...f.context, job_id: "name-only" },
    }),
  );
  await assert.rejects(() =>
    bindReferenceResult({
      ...f,
      envelope: { ...f.envelope, job_digest: "b".repeat(64) },
    }),
  );
  await assert.rejects(() =>
    bindReferenceResult({
      ...f,
      envelope: {
        ...f.envelope,
        result: { ...plan, input: { ...input, notes: ["other.md"] } },
      },
    }),
  );
});
test("failed/pending/unknown/cancelled device observations never become complete", async () => {
  const f = await resultFixture("references.copy");
  for (const status of ["failed", "pending", "unknown", "cancelled"]) {
    const value = await bindReferenceResult({
      ...f,
      envelope: { ...f.envelope, status },
    });
    assert.equal(value.completed, false);
  }
  await assert.rejects(() =>
    bindReferenceResult({
      ...f,
      envelope: {
        ...f.envelope,
        result: { ...f.envelope.result, results: [{ state: "failed" }] },
      },
    }),
  );
});
test("rollback result cannot omit, reorder or deduplicate newer fixed version inventory", async () => {
  const f = await resultFixture("references.rollback");
  await assert.doesNotReject(() => bindReferenceResult(f));
  await assert.rejects(() =>
    bindReferenceResult({
      ...f,
      envelope: {
        ...f.envelope,
        result: {
          ...f.envelope.result,
          post_cutover_refs: [input.mappings[0].fixed_ref],
        },
      },
    }),
  );
});

test("import refresh preserves ledger revision, owner and stable batch while accounting for formerly unknown rows", async () => {
  const { syncBatchImport } = await import("../src/batch-acceptance-model");
  const batch = complete();
  batch.batch_id = "same";
  const workspace = {
    batch_id: "same",
    instance_id: batch.instance_id,
    plan: {
      project_id: batch.project_id,
      name: "source",
      assets: [{ key: "one" }],
    },
    rows: { one: { status: "description_pending" } },
    states: {
      one: {
        result: { ...ref(4), manifest_digest: "sha256:" + "a".repeat(64) },
      },
    },
  } as unknown as MigrationWorkspace;
  const next = syncBatchImport(batch, workspace, id(3));
  assert.equal(next.import_summary.description_pending, 1);
  assert.equal(next.import_summary.committed, 0);
  assert.equal(next.batch_id, batch.batch_id);
  assert.equal(next.owner_id, batch.owner_id);
  assert.equal(next.revision, batch.revision);
  assert.equal(next.acceptance, "not_requested");
  await assert.rejects(async () =>
    syncBatchImport(batch, { ...workspace, batch_id: "other" }, id(3)),
  );
});

test("acceptance purpose is explicit and current purpose denial cannot be bypassed by archive visibility", () => {
  const batch = complete();
  assert.equal(batch.intended_purpose, "production");
  assert.throws(() =>
    validateMigrationBatch({ ...batch, intended_purpose: undefined }),
  );
  assert.throws(() =>
    validateMigrationBatch({ ...batch, intended_purpose: "anything" }),
  );
  const value = check(batch);
  const item = value.checks.find((row) => row.key === "purpose")!;
  item.status = "failed";
  assert.throws(() => validateBatchCheck(value, batch, ref(15)));
});
test("blocked service response may expose only checked subsets while ready still requires full coverage", () => {
  const batch = complete();
  const value = check(batch);
  value.acceptance = "blocked";
  value.assets = [];
  value.worksets = [];
  value.recovery_refs = [];
  value.checks[1].status = "failed";
  assert.equal(validateBatchCheck(value, batch, ref(15)).acceptance, "blocked");
  value.acceptance = "ready";
  assert.throws(() => validateBatchCheck(value, batch, ref(15)));
});

test("native plan omitempty preserve normalization is lossless while missing notes cannot pass", async () => {
  const f = await resultFixture();
  const nativePlan = structuredClone(plan) as Record<string, unknown>;
  const nativeInput = { ...input } as Partial<typeof input>;
  delete nativeInput.preserve;
  nativePlan.input = nativeInput;
  assert.equal(
    (
      await bindReferenceResult({
        ...f,
        envelope: { ...f.envelope, result: nativePlan },
      })
    ).completed,
    true,
  );
  await assert.rejects(() =>
    bindReferenceResult({
      ...f,
      envelope: { ...f.envelope, result: { ...plan, notes: [] } },
    }),
  );
});
test("copy completion requires one correct output digest for every selected note", async () => {
  const f = await resultFixture("references.copy");
  for (const results of [
    [],
    [
      {
        path: "notes/a.md",
        state: "copied",
        before_sha256: "a".repeat(64),
        after_sha256: "b".repeat(64),
        output_sha256: "c".repeat(64),
      },
    ],
  ])
    await assert.rejects(() =>
      bindReferenceResult({
        ...f,
        envelope: { ...f.envelope, result: { ...f.envelope.result, results } },
      }),
    );
});

test("private LocalResultRef only advances with succeeded stopped original queue proof, never a fabricated plan", async () => {
  const f = await resultFixture();
  const local = {
    schema: "lantai.local-assistant-result-ref/v1",
    job_id: f.context.job_id,
    result_sha256: "d".repeat(64),
  };
  const context = {
    ...f.context,
    state: "succeeded",
    stopped: true,
    workspace_binding: "e".repeat(64),
  };
  const bound = await bindReferenceResult({
    ...f,
    envelope: { ...f.envelope, result: local },
    context,
  });
  assert.equal(bound.kind, "local_ref");
  assert.equal(bound.completed, true);
  assert.deepEqual(bound.result, local);
  assert.equal("notes" in bound.result, false);
  assert.equal("complete" in bound.result, false);
  for (const changed of [
    { ...context, state: "running" },
    { ...context, state: "needs_reconciliation" },
    { ...context, stopped: false },
    { ...context, workspace_binding: "" },
  ])
    assert.equal(
      (
        await bindReferenceResult({
          ...f,
          envelope: { ...f.envelope, result: local },
          context: changed,
        })
      ).completed,
      false,
    );
  await assert.rejects(() =>
    bindReferenceResult({
      ...f,
      envelope: { ...f.envelope, result: { ...local, job_id: id(30) } },
      context,
    }),
  );
  await assert.rejects(() =>
    bindReferenceResult({
      ...f,
      envelope: {
        ...f.envelope,
        result: { ...local, result_sha256: "sha256:" + "d".repeat(64) },
      },
      context,
    }),
  );
});
test("reference continuation preserves LocalResultRef exact private-byte hash instead of JSON-reserializing original plan", () => {
  const original = {
    schema: "lantai.local-assistant-result-ref/v1",
    job_id: id(12),
    result_sha256: "a".repeat(64),
  };
  const copy = referenceJob({
    action: "references.copy",
    input: original,
    batchID: "same",
    directory: "notes",
    destination: "copy",
  });
  const rollback = referenceJob({
    action: "references.rollback",
    input: original,
    batchID: "same",
    directory: "copy",
    destination: "rollback",
    postCutoverRefs: [],
  });
  assert.equal(copy.input, original);
  assert.equal(rollback.input, original);
  assert.equal(copy.job_id, "references-same-copy");
  assert.deepEqual(rollback.post_cutover_refs, []);
});

test("native evidence references preserve fixed reviewed artifacts and cannot supply client authority", () => {
  const batch = complete();
  batch.evidence.native_continuations = [ref(20)];
  assert.deepEqual(
    validateMigrationBatch(batch).evidence.native_continuations,
    [ref(20)],
  );
  assert.throws(() =>
    validateMigrationBatch({
      ...batch,
      evidence: { ...batch.evidence, native_continuations: [ref(20), ref(20)] },
    }),
  );
  assert.throws(() =>
    validateMigrationBatch({
      ...batch,
      evidence: {
        ...batch.evidence,
        native_continuations: [{ ...ref(20), approved: true }],
      },
    }),
  );
  const value = check(batch);
  value.checks = value.checks.filter(
    (row) => row.key !== "native_continuation",
  );
  assert.throws(() => validateBatchCheck(value, batch, ref(15)));
});
