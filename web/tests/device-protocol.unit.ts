import test from "node:test";
import assert from "node:assert/strict";
import {
  checkedDeviceResult,
  checkedInspection,
  deviceJobDigest,
  frozenJob,
  newDeviceRequest,
  restoreDeviceRequest,
  sendDeviceRequest,
  validDeviceJob,
  type Device,
  type DeviceJob,
  type DeviceJobInput,
  type DeviceRequest,
} from "../src/device-protocol";
import type { Transport } from "../src/system-operations";

const origin = "https://synthetic.example";
const project = "01J00000000000000000000001";
const deviceID = "01J00000000000000000000002";
const queueID = "01J00000000000000000000003";
const operation = "01J00000000000000000000004";
const owner = "01J00000000000000000000005";
const binding = "a".repeat(64),
  approval = "b".repeat(64);
const job: DeviceJobInput = {
  schema: "lantai.local-assistant-job/v1",
  job_id: "synthetic-reference-plan",
  action: "references.plan",
  directory: "sources/notes",
  output: "reports/plan.json",
  input: { schema: "lantai.client-reference-input/v1", entries: [] },
};
const device: Device = {
  device_id: deviceID,
  project_id: project,
  owner_id: owner,
  alias: "synthetic-device",
  workspace_alias: "synthetic-workspace",
  workspace_binding: binding,
  actions: ["references.plan"],
  state: "active",
  revision: 1,
  expires_at: "2099-10-20T12:00:00Z",
};
async function inspected(): Promise<DeviceJob> {
  const digest = await deviceJobDigest(job, origin);
  return {
    device_id: deviceID,
    project_id: project,
    job_id: queueID,
    operation_id: operation,
    workspace_alias: device.workspace_alias,
    workspace_binding: binding,
    job,
    job_digest: digest,
    state: "awaiting_approval",
    revision: 2,
    cancel_requested: false,
    stopped: false,
    inspection: {
      schema: "lantai.local-assistant-inspection/v1",
      job_id: job.job_id,
      action: job.action,
      job_digest: digest,
      approval_digest: approval,
      workspace_binding: binding,
      input_sha256: "c".repeat(64),
      paths: { directory: job.directory, output: job.output },
      network_required: false,
      remote_writes: false,
      source_deleted: false,
      timeout_seconds: 3600,
      warnings: [],
    },
  };
}

test("device dispatch retains immutable original body and key before a lost accepted response", async () => {
  let pending = newDeviceRequest(
    origin,
    project,
    deviceID,
    `/api/v1/devices/${deviceID}/jobs`,
    {
      project_id: project,
      expected_device_revision: 1,
      workspace_alias: device.workspace_alias,
      job,
      job_digest: await deviceJobDigest(job, origin),
    },
  );
  const writes: RequestInit[] = [];
  const request: Transport = async <T>(
    _path: string,
    options?: RequestInit,
  ) => {
    writes.push(options!);
    if (writes.length === 1)
      throw new Error("synthetic accepted response lost");
    return { job_id: queueID } as T;
  };
  await assert.rejects(
    sendDeviceRequest(pending, request, (value) => (pending = value)),
  );
  assert.equal(pending.attempted, true);
  await sendDeviceRequest(pending, request, (value) => (pending = value));
  assert.deepEqual(writes[0], writes[1]);
  assert.throws(() => {
    (pending.body.job as DeviceJobInput).action = "arbitrary.shell";
  });
});

test("two phase approval binds original JCS job, device workspace, input and complete local approval digest", async () => {
  const original = await inspected();
  const inspection = await checkedInspection(original, device, origin);
  assert.equal(inspection.approval_digest, approval);
  await assert.rejects(
    checkedInspection(
      { ...original, job: { ...job, directory: "sources/other" } },
      device,
      origin,
    ),
  );
  await assert.rejects(
    checkedInspection(
      original,
      { ...device, workspace_binding: "d".repeat(64) },
      origin,
    ),
  );
  await assert.rejects(
    checkedInspection(
      {
        ...original,
        inspection: {
          ...inspection,
          paths: { directory: "/private/absolute/source" },
        },
      },
      device,
      origin,
    ),
  );
  await assert.rejects(
    checkedInspection(
      { ...original, inspection: { ...inspection, approval_digest: "short" } },
      device,
      origin,
    ),
  );
});

test("safe result reference preserves server queue identity and original result binding", async () => {
  const original = await inspected();
  const completed: DeviceJob = {
    ...original,
    state: "succeeded",
    stopped: true,
    approval_digest: approval,
    result: {
      schema: "lantai.local-assistant-result/v1",
      job_id: job.job_id,
      action: job.action,
      job_digest: original.job_digest,
      approval_digest: approval,
      status: "completed",
      exit_code: 0,
      result: {
        schema: "lantai.local-assistant-result-ref/v1",
        job_id: queueID,
        result_sha256: "d".repeat(64),
      },
    },
  };
  assert.deepEqual(
    (await checkedDeviceResult(completed, device, origin)).result,
    completed.result!.result,
  );
  await assert.rejects(
    checkedDeviceResult(
      {
        ...completed,
        result: { ...completed.result!, approval_digest: "e".repeat(64) },
      },
      device,
      origin,
    ),
  );
  await assert.rejects(
    checkedDeviceResult(
      {
        ...completed,
        result: { ...completed.result!, result: { token: "synthetic-secret" } },
      },
      device,
      origin,
    ),
  );
});

test("fence remains the full T06 object; cross-project and credential/signed URL inputs fail closed", async () => {
  const original = await inspected();
  const fenced: DeviceJob = {
    ...original,
    state: "running",
    job_fence: {
      job_id: queueID,
      job_attempt_id: operation,
      lease_fence: 4,
      recovery_epoch: 1,
    },
  };
  assert.equal(
    validDeviceJob(fenced, project, deviceID).job_fence!.lease_fence,
    4,
  );
  assert.throws(() =>
    validDeviceJob(
      { ...fenced, job_fence: 4 as unknown as DeviceJob["job_fence"] },
      project,
      deviceID,
    ),
  );
  assert.throws(() => validDeviceJob(fenced, owner, deviceID));
  assert.throws(() => frozenJob({ ...job, action: "shell.run" }, origin));
  assert.throws(() => frozenJob({ ...job, directory: "../outside" }, origin));
  assert.throws(() =>
    frozenJob({ ...job, input: { token: "synthetic" } }, origin),
  );
  assert.throws(() =>
    frozenJob(
      { ...job, input: { url: "https://synthetic.example/xfer/v1/signed" } },
      origin,
    ),
  );
});

test("diagnostic resume preserves the original key and request and rejects another origin/project/control path", () => {
  const intent: DeviceRequest = {
    ...newDeviceRequest(
      origin,
      project,
      deviceID,
      `/api/v1/devices/${deviceID}/jobs/${queueID}/cancel`,
      { expected_revision: 4 },
    ),
    attempted: true,
  };
  const restored = restoreDeviceRequest(
    JSON.parse(JSON.stringify(intent)),
    origin,
    project,
  );
  assert.equal(restored.key, intent.key);
  assert.equal(restored.attempted, true);
  assert.deepEqual(restored.body, intent.body);
  assert.throws(() =>
    restoreDeviceRequest(intent, "https://another.invalid", project),
  );
  assert.throws(() => restoreDeviceRequest(intent, origin, owner));
  assert.throws(() =>
    restoreDeviceRequest(
      { ...intent, path: `/api/v1/devices/${deviceID}/worker/poll` },
      origin,
      project,
    ),
  );
  assert.throws(() =>
    restoreDeviceRequest(
      { ...intent, body: { bearer_token: "synthetic" } },
      origin,
      project,
    ),
  );
});
