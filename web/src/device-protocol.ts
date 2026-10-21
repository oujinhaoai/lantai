import {
  assertNoTransferSecrets,
  canonicalJSON,
  sha256,
  validateManagedPath,
} from "./managed-transfer";
import { identifier, revision, type Transport } from "./system-operations";

export const deviceActions = [
  "import.plan",
  "import.apply",
  "import.verify",
  "workset.plan",
  "workset.pull",
  "references.plan",
  "references.copy",
  "references.rollback",
] as const;
export type DeviceAction = (typeof deviceActions)[number];
export interface DeviceJobInput {
  schema: string;
  job_id: string;
  action: string;
  origin?: string;
  input?: unknown;
  input_file?: string;
  directory: string;
  state?: string;
  destination?: string;
  report?: unknown;
  report_file?: string;
  post_cutover_refs?: string[];
  output: string;
  purpose?: string;
  timeout_seconds?: number;
}
export interface Device {
  device_id: string;
  project_id: string;
  alias: string;
  workspace_alias: string;
  workspace_binding: string;
  actions: string[];
  state: "pending" | "active" | "revoked" | "expired";
  revision: number;
  expires_at: string;
  owner_id: string;
}
export interface DeviceInspection {
  schema: string;
  job_id: string;
  action: string;
  job_digest: string;
  approval_digest: string;
  workspace_binding: string;
  input_sha256: string;
  origin?: string;
  paths: Record<string, string>;
  network_required: boolean;
  remote_writes: boolean;
  source_deleted: boolean;
  timeout_seconds: number;
  warnings: string[];
}
export interface DeviceAssistantResult {
  schema: string;
  job_id: string;
  action: string;
  job_digest: string;
  approval_digest: string;
  status: string;
  exit_code: number;
  result?: unknown;
  error_code?: string;
  [key: string]: unknown;
}
export interface DeviceJob {
  job_id: string;
  operation_id: string;
  project_id: string;
  device_id: string;
  workspace_alias: string;
  workspace_binding: string;
  job: DeviceJobInput;
  job_digest: string;
  approval_digest?: string;
  state:
    | "queued"
    | "awaiting_approval"
    | "ready"
    | "running"
    | "cancelling"
    | "succeeded"
    | "failed"
    | "cancelled"
    | "needs_reconciliation";
  revision: number;
  job_fence?: DeviceFence;
  lease_expires_at?: string;
  cancel_requested: boolean;
  stopped: boolean;
  inspection?: DeviceInspection;
  result?: DeviceAssistantResult;
  failure_code?: string;
}
export interface DeviceResultContext {
  job_id: string;
  device_id: string;
  workspace_alias: string;
  workspace_binding: string;
  state: DeviceJob["state"];
  stopped: boolean;
  job_digest: string;
  approval_digest: string;
  operation_id: string;
  job_fence?: DeviceFence;
}
export interface DeviceFence {
  job_id: string;
  job_attempt_id: string;
  lease_fence: number;
  recovery_epoch: number;
}
const digestPattern = /^[a-f0-9]{64}$/;
const aliasPattern = /^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/;
const jobNamePattern = /^[a-zA-Z0-9][a-zA-Z0-9_-]{0,99}$/;
const states = [
  "queued",
  "awaiting_approval",
  "ready",
  "running",
  "cancelling",
  "succeeded",
  "failed",
  "cancelled",
  "needs_reconciliation",
];
function safeDevicePayload(value: unknown): void {
  assertNoTransferSecrets(value);
  const walk = (item: unknown) => {
    if (Array.isArray(item)) item.forEach(walk);
    else if (item && typeof item === "object")
      for (const [key, child] of Object.entries(item)) {
        if (
          /^(secret|session_file|ca_file|source_root|bearer_token|device_token|credentials)$/i.test(
            key,
          )
        )
          throw new Error(
            "设备通道不能携带秘密、本机会话文件或私有绝对来源根。",
          );
        walk(child);
      }
  };
  walk(value);
}
function frozenJSON<T>(value: T): T {
  const copied = JSON.parse(canonicalJSON(value)) as T;
  const lock = (item: unknown) => {
    if (item && typeof item === "object") {
      Object.values(item).forEach(lock);
      Object.freeze(item);
    }
  };
  lock(copied);
  return copied;
}
export function deviceDigest(value: string) {
  if (!digestPattern.test(value))
    throw new Error("设备摘要必须是完整的 64 位小写 SHA-256。");
  return value;
}
export function workspaceAlias(value: string) {
  if (!aliasPattern.test(value))
    throw new Error("请选择设备明确登记的工作区目录别名。");
  return value;
}
export function validDevice(value: Device, projectID: string): Device {
  identifier(value.device_id);
  identifier(value.owner_id);
  if (
    value.project_id !== identifier(projectID) ||
    !["pending", "active", "revoked", "expired"].includes(value.state) ||
    !Array.isArray(value.actions) ||
    value.actions.some((a) => !deviceActions.includes(a as DeviceAction)) ||
    !Number.isFinite(Date.parse(value.expires_at))
  )
    throw new Error("设备响应的项目、能力或生命周期不符合协议。");
  revision(value.revision);
  if (value.workspace_alias) workspaceAlias(value.workspace_alias);
  if (value.workspace_binding) deviceDigest(value.workspace_binding);
  return value;
}
export function frozenJob(
  value: DeviceJobInput,
  origin: string,
): DeviceJobInput {
  safeDevicePayload(value);
  const json = canonicalJSON(value);
  if (new TextEncoder().encode(json).length > 1024 * 1024)
    throw new Error("设备队列作业超过 1 MiB 上限。");
  const allowed = [
    "schema",
    "job_id",
    "action",
    "origin",
    "input",
    "input_file",
    "directory",
    "state",
    "destination",
    "report",
    "report_file",
    "post_cutover_refs",
    "output",
    "purpose",
    "timeout_seconds",
  ];
  if (
    Object.keys(value).some((k) => !allowed.includes(k)) ||
    value.schema !== "lantai.local-assistant-job/v1" ||
    !jobNamePattern.test(value.job_id) ||
    !deviceActions.includes(value.action as DeviceAction)
  )
    throw new Error("设备只接受固定的八类声明式助手作业。");
  if (value.origin && value.origin !== origin)
    throw new Error("作业 origin 与当前实例不一致。");
  for (const field of [
    "directory",
    "state",
    "destination",
    "output",
    "input_file",
    "report_file",
  ] as const)
    if (value[field] !== undefined) validateManagedPath(value[field]!);
  if (!value.directory || !value.output)
    throw new Error("作业必须明确选择工作区内的相对目录和回执路径。");
  return frozenJSON(value);
}
export async function deviceJobDigest(job: DeviceJobInput, origin: string) {
  return sha256(canonicalJSON(frozenJob(job, origin)));
}
export function validDeviceJob(
  value: DeviceJob,
  projectID: string,
  deviceID: string,
): DeviceJob {
  identifier(value.job_id);
  identifier(value.operation_id);
  if (
    value.project_id !== projectID ||
    value.device_id !== deviceID ||
    !states.includes(value.state) ||
    typeof value.cancel_requested !== "boolean" ||
    typeof value.stopped !== "boolean"
  )
    throw new Error("设备作业混入其他项目、设备或未知状态。");
  revision(value.revision);
  workspaceAlias(value.workspace_alias);
  deviceDigest(value.workspace_binding);
  deviceDigest(value.job_digest);
  if (value.job_fence !== undefined) {
    identifier(value.job_fence.job_id);
    if (value.job_fence.job_id !== value.job_id)
      throw new Error("T06 fence 不属于原队列作业。");
    identifier(value.job_fence.job_attempt_id);
    revision(value.job_fence.lease_fence);
    revision(value.job_fence.recovery_epoch, 0);
  }
  if (
    value.lease_expires_at &&
    !Number.isFinite(Date.parse(value.lease_expires_at))
  )
    throw new Error("设备租约时间不完整。");
  safeDevicePayload(value);
  return value;
}
export function deviceJobUnresolved(job: DeviceJob) {
  return (
    !["succeeded", "failed", "cancelled"].includes(job.state) || !job.stopped
  );
}
export async function checkedInspection(
  job: DeviceJob,
  device: Device,
  origin: string,
): Promise<DeviceInspection> {
  validDeviceJob(job, device.project_id, device.device_id);
  const inspection = job.inspection;
  if (
    !inspection ||
    inspection.schema !== "lantai.local-assistant-inspection/v1" ||
    inspection.job_id !== job.job.job_id ||
    inspection.action !== job.job.action ||
    inspection.job_digest !== job.job_digest ||
    job.job_digest !== (await deviceJobDigest(job.job, origin)) ||
    inspection.workspace_binding !== device.workspace_binding ||
    job.workspace_binding !== device.workspace_binding ||
    job.workspace_alias !== device.workspace_alias ||
    !inspection.paths ||
    typeof inspection.paths !== "object" ||
    Array.isArray(inspection.paths) ||
    !Array.isArray(inspection.warnings) ||
    inspection.warnings.some((warning) => typeof warning !== "string") ||
    typeof inspection.network_required !== "boolean" ||
    inspection.remote_writes !== (job.job.action === "import.apply") ||
    (inspection.origin && inspection.origin !== origin) ||
    !Number.isSafeInteger(inspection.timeout_seconds) ||
    inspection.timeout_seconds < 1 ||
    inspection.source_deleted !== false
  )
    throw new Error("本机检查没有绑定当前原作业、工作区或非破坏性范围。");
  deviceDigest(inspection.approval_digest);
  deviceDigest(inspection.workspace_binding);
  deviceDigest(inspection.input_sha256);
  const paths: Record<string, string> = {};
  for (const key of [
    "directory",
    "output",
    "state",
    "destination",
    "input_file",
    "report_file",
  ] as const)
    if (job.job[key]) paths[key] = job.job[key]!;
  if (canonicalJSON(paths) !== canonicalJSON(inspection.paths))
    throw new Error("本机检查的相对目录集合与原作业不符。");
  for (const path of Object.values(inspection.paths)) validateManagedPath(path);
  safeDevicePayload(inspection);
  return inspection;
}
export async function checkedDeviceResult(
  job: DeviceJob,
  device: Device,
  origin: string,
) {
  const inspection = await checkedInspection(job, device, origin);
  const result = job.result;
  if (
    !result ||
    result.schema !== "lantai.local-assistant-result/v1" ||
    result.job_id !== job.job.job_id ||
    result.action !== job.job.action ||
    result.job_digest !== inspection.job_digest ||
    result.approval_digest !== inspection.approval_digest ||
    !["completed", "partial", "unknown", "cancelled", "failed"].includes(
      result.status,
    ) ||
    !Number.isSafeInteger(result.exit_code)
  )
    throw new Error("设备结果没有绑定原作业与本机批准摘要。");
  safeDevicePayload(result);
  return result;
}

export interface DeviceRequest {
  schema: "lantai.device-operation/v1";
  origin: string;
  project_id: string;
  device_id: string;
  path: string;
  body: Record<string, unknown>;
  key: string;
  attempted: boolean;
}
function validPath(path: string, deviceID: string) {
  const base = `/api/v1/devices/${identifier(deviceID)}`;
  if ([`${base}/approve`, `${base}/revoke`, `${base}/jobs`].includes(path))
    return;
  const rest = path.slice(base.length);
  if (
    !path.startsWith(base) ||
    !/^\/jobs\/[0-7][0-9A-HJKMNP-TV-Z]{25}\/(approve|cancel)$/.test(rest)
  )
    throw new Error("设备请求路径不在封闭控制接口中。");
}
function validBody(
  path: string,
  body: Record<string, unknown>,
  origin: string,
  projectID: string,
) {
  let allowed: string[];
  if (path.endsWith("/jobs")) {
    allowed = [
      "expected_device_revision",
      "project_id",
      "workspace_alias",
      "job",
      "job_digest",
    ];
    revision(body.expected_device_revision as number);
    if (body.project_id !== projectID)
      throw new Error("派发请求项目与当前范围不符。");
    workspaceAlias(String(body.workspace_alias));
    frozenJob(body.job as DeviceJobInput, origin);
    deviceDigest(String(body.job_digest));
  } else if (/\/jobs\/[^/]+\/approve$/.test(path)) {
    allowed = [
      "expected_revision",
      "job_digest",
      "approval_digest",
      "workspace_binding",
    ];
    revision(body.expected_revision as number);
    for (const key of ["job_digest", "approval_digest", "workspace_binding"])
      deviceDigest(String(body[key]));
  } else if (/\/devices\/[^/]+\/approve$/.test(path)) {
    allowed = [
      "expected_revision",
      "claim_code",
      "workspace_alias",
      "workspace_binding",
    ];
    revision(body.expected_revision as number);
    if (
      typeof body.claim_code !== "string" ||
      !body.claim_code ||
      body.claim_code.length > 256
    )
      throw new Error("认领代码不完整。");
    workspaceAlias(String(body.workspace_alias));
    deviceDigest(String(body.workspace_binding));
  } else {
    allowed = ["expected_revision"];
    revision(body.expected_revision as number);
  }
  if (Object.keys(body).some((key) => !allowed.includes(key)))
    throw new Error("设备控制请求含未开放字段。");
}
export function newDeviceRequest(
  origin: string,
  projectID: string,
  deviceID: string,
  path: string,
  body: Record<string, unknown>,
): DeviceRequest {
  identifier(projectID);
  validPath(path, deviceID);
  safeDevicePayload(body);
  validBody(path, body, origin, projectID);
  return frozenJSON({
    schema: "lantai.device-operation/v1",
    origin,
    project_id: projectID,
    device_id: deviceID,
    path,
    body: JSON.parse(canonicalJSON(body)),
    key: crypto.randomUUID(),
    attempted: false,
  });
}
export async function sendDeviceRequest<T>(
  intent: DeviceRequest,
  request: Transport,
  retain: (intent: DeviceRequest) => void,
): Promise<T> {
  validPath(intent.path, intent.device_id);
  retain({ ...intent, attempted: true });
  return request<T>(intent.path, {
    method: "POST",
    headers: { "Idempotency-Key": intent.key },
    body: canonicalJSON(intent.body),
  });
}
export function restoreDeviceRequest(
  value: unknown,
  origin: string,
  projectID: string,
): DeviceRequest {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("设备诊断不是对象。");
  const v = value as DeviceRequest;
  if (
    Object.keys(v).some(
      (k) =>
        ![
          "schema",
          "origin",
          "project_id",
          "device_id",
          "path",
          "body",
          "key",
          "attempted",
        ].includes(k),
    ) ||
    v.schema !== "lantai.device-operation/v1" ||
    v.origin !== origin ||
    v.project_id !== projectID ||
    typeof v.attempted !== "boolean" ||
    typeof v.key !== "string" ||
    !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(v.key) ||
    !v.body ||
    typeof v.body !== "object" ||
    Array.isArray(v.body)
  )
    throw new Error("设备诊断不属于当前实例、项目或原请求。");
  validPath(v.path, v.device_id);
  safeDevicePayload(v);
  if ("claim_code" in v.body) throw new Error("认领代码不能保存在续办诊断中。");
  validBody(v.path, v.body, origin, projectID);
  return frozenJSON(v);
}
