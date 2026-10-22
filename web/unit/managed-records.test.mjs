import assert from "node:assert/strict";
import { test } from "node:test";
import { assertRecordPayload, loadManagedRecord, saveManagedRecord } from "../src/managed-records.ts";

test("management records preserve idempotency facts but reject nested credentials and transfer capabilities", () => {
  assert.doesNotThrow(() => assertRecordPayload({ original: { create_key: "stable-create", commit_key: "stable-commit", operation_id: "public-id" }, uploaded: false, unknown: true }));
  for (const key of ["password", "session_token", "totp_secret", "master_key", "part_url_template", "csrf_token", "recovery_codes"]) {
    assert.throws(() => assertRecordPayload({ nested: [{ [key]: "synthetic-secret" }] }), /凭据|授权/);
  }
  assert.throws(() => assertRecordPayload({ result: Number.POSITIVE_INFINITY }), /JSON/);
  assert.throws(() => assertRecordPayload({ result: undefined }), /JSON/);
});

test("record save cannot create a remote operation without a checkpoint", async () => {
  const previous = globalThis.fetch;
  let requests = 0;
  globalThis.fetch = async () => { requests++; throw new Error("must not request"); };
  try {
    await assert.rejects(saveManagedRecord({ projectID: "01J00000000000000000000001", title: "synthetic", kind: "workset", payload: {} }), /原操作状态/);
    assert.equal(requests, 0);
  } finally { globalThis.fetch = previous; }
});

test("record read rejects an invalid fixed identity before any network access", async () => {
  const previous = globalThis.fetch;
  let requests = 0;
  globalThis.fetch = async () => { requests++; throw new Error("must not request"); };
  try {
    await assert.rejects(loadManagedRecord({ asset_id: "latest", version_id: "latest" }), /固定/);
    assert.equal(requests, 0);
  } finally { globalThis.fetch = previous; }
});

test("prepared records reject changed current instance, title and fixed baseline before mutations", async () => {
  const { prepareManagedRecord } = await import("../src/managed-records.ts");
  Object.defineProperty(globalThis, "location", { configurable: true, value: new globalThis.URL("https://synthetic.example") });
  const previous = globalThis.fetch;
  let instance = "01J00000000000000000000006";
  let writes = 0;
  globalThis.fetch = async (_url, init) => {
    if (init?.method && init.method !== "GET") writes++;
    return new globalThis.Response(JSON.stringify({ api_version: "v1", instance_id: instance }), { headers: { "Content-Type": "application/json" } });
  };
  const options = { projectID: "01J00000000000000000000001", title: "synthetic", kind: "workset", payload: { stable: true } };
  try {
    const prepared = await prepareManagedRecord(options);
    const original = globalThis.structuredClone(prepared.state);
    instance = "01J00000000000000000000007";
    await assert.rejects(saveManagedRecord({ ...options, prepared, checkpoint: async () => {} }), /实例|基线/);
    instance = "01J00000000000000000000006";
    await assert.rejects(saveManagedRecord({ ...options, title: "changed", prepared, checkpoint: async () => {} }), /实例|基线/);
    await assert.rejects(saveManagedRecord({ ...options, assetID: "01J00000000000000000000002", baseVersionID: "01J00000000000000000000003", prepared, checkpoint: async () => {} }), /实例|基线/);
    assert.equal(writes, 0);
    assert.deepEqual(prepared.state, original);
  } finally { globalThis.fetch = previous; }
});

test("resume export and import reject credentials anywhere in frozen input or result", async () => {
  const { managedRecordResume, restoreManagedRecordResume } = await import("../src/managed-records.ts");
  const prepared = { input: {}, state: { result: { access_token: "synthetic-placeholder" } }, envelope: { payload: {} } };
  assert.throws(() => managedRecordResume(prepared), /凭据|授权/);
  const previous = globalThis.fetch;
  let requests = 0;
  globalThis.fetch = async () => { requests++; throw new Error("must not request"); };
  try {
    await assert.rejects(restoreManagedRecordResume(JSON.stringify({ schema: "lantai.management-record-resume/v1", ...prepared }), {}), /凭据|授权/);
    assert.equal(requests, 0);
  } finally { globalThis.fetch = previous; }
});
