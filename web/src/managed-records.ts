import { api, type Item } from "./api";
import {
  canonicalJSON,
  getExactManagedVersion,
  identifierPattern,
  newPushState,
  parseManagedJSON,
  preparePushInput,
  pushFiles,
  readManagedFile,
  sha256,
  validatePushState,
  type ManagedFile,
  type PushInput,
  type PushState,
} from "./managed-transfer";

export type ManagedRecordKind =
  "migration" | "workset" | "recovery" | "assistant";
export type ManagedRecordRef = { asset_id: string; version_id: string };
export type ManagedRecordItem = ManagedRecordRef & {
  project_id: string;
  title: string;
  record_kind: ManagedRecordKind;
};
export type ManagedRecordEnvelope = {
  schema: "lantai.management-record/v1";
  kind: ManagedRecordKind;
  instance_id: string;
  project_id: string;
  payload: unknown;
};
export type PreparedManagedRecord = {
  input: PushInput;
  files: ManagedFile[];
  state: PushState;
  envelope: ManagedRecordEnvelope;
};
export type ManagedRecordOptions = {
  projectID: string;
  title: string;
  kind: ManagedRecordKind;
  payload: unknown;
  assetID?: string;
  baseVersionID?: string;
  signal?: AbortSignal;
  checkpoint?: (state: PushState) => Promise<void>;
  prepared?: PreparedManagedRecord;
};
const kinds: ManagedRecordKind[] = [
  "migration",
  "workset",
  "recovery",
  "assistant",
];
const maxRecordBytes = 2 * 1024 * 1024;
const forbidden =
  /^(?:password|password_hash|secret_hash|totp_secret|totp_uri|recovery_codes|master_key|csrf_token|csrf|session_token|access_token|refresh_token|authorization|part_url_template|read_grant_url|download_token|grant_id|token)$/i;

function object(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
// Original idempotency keys and operation IDs are private work data, not bearer
// credentials. Preserve them; never archive authentication or transfer grants.
export function assertRecordPayload(value: unknown, depth = 0): void {
  if (depth > 64) throw new Error("档案嵌套超过限制。");
  if (Array.isArray(value)) {
    for (const item of value) assertRecordPayload(item, depth + 1);
  } else if (object(value)) {
    for (const [key, item] of Object.entries(value)) {
      if (forbidden.test(key))
        throw new Error(`档案不能包含凭据或短时授权字段：${key}`);
      assertRecordPayload(item, depth + 1);
    }
  } else if (
    value !== null &&
    typeof value !== "string" &&
    typeof value !== "boolean" &&
    !(typeof value === "number" && Number.isFinite(value))
  ) {
    throw new Error("档案只能包含有效 JSON 数据。");
  }
}

export async function prepareManagedRecord(
  options: ManagedRecordOptions,
): Promise<PreparedManagedRecord> {
  if (
    !identifierPattern.test(options.projectID) ||
    !kinds.includes(options.kind)
  )
    throw new Error("请选择合法项目和记录类型。");
  const title = options.title.trim();
  if (!title || title.length > 200)
    throw new Error("记录标题应为 1–200 个字符。");
  if (
    !!options.assetID !== !!options.baseVersionID ||
    (options.assetID &&
      (!identifierPattern.test(options.assetID) ||
        !identifierPattern.test(options.baseVersionID!)))
  )
    throw new Error("追加档案必须同时指定原资产和固定基线版本。");
  assertRecordPayload(options.payload);
  const meta = await api<{ api_version: string; instance_id: string }>(
    "/api/v1/meta",
    { signal: options.signal },
  );
  if (meta.api_version !== "v1" || !identifierPattern.test(meta.instance_id))
    throw new Error("当前实例身份无效。");
  // No local clock/random identity is put into the payload: an unchanged input
  // can be reconstructed exactly when resuming with its original Push state.
  const envelope: ManagedRecordEnvelope = {
    schema: "lantai.management-record/v1",
    kind: options.kind,
    instance_id: meta.instance_id,
    project_id: options.projectID,
    payload: parseManagedJSON(canonicalJSON(options.payload)),
  };
  const bytes = new TextEncoder().encode(canonicalJSON(envelope) + "\n");
  if (bytes.length > maxRecordBytes)
    throw new Error("管理档案超过 2 MiB，请显式拆分记录。");
  const hash = await sha256(bytes);
  const file = new File([bytes], "record.json", { type: "application/json" });
  const files: ManagedFile[] = [{ path: "record.json", role: "record", file }];
  const input: PushInput = {
    project_id: options.projectID,
    ...(options.assetID
      ? { asset_id: options.assetID, base_version_id: options.baseVersionID }
      : {
          slug: `management/${options.kind}-${hash.slice(0, 24)}`,
          describe: {
            title,
            tags: ["lantai-management-record", `record-kind:${options.kind}`],
          },
        }),
    content: {
      asset_type: "config",
      files: [
        {
          path: "record.json",
          role: "record",
          size: bytes.length,
          sha256: hash,
        },
      ],
      rights: {
        usage: "reference",
        license: "LicenseRef-Lantai-Private-Record",
        redistribute_raw: false,
        noai: true,
        sensitivity: "normal",
      },
      metadata: {
        tool: "lantai-management",
        extra: {
          management_record: { schema: envelope.schema, kind: options.kind },
        },
      },
    },
  };
  const prepared = await preparePushInput({
    input,
    files,
    signal: options.signal,
  });
  return {
    input: prepared,
    files,
    state: await newPushState({ input: prepared }),
    envelope,
  };
}

export async function saveManagedRecord(options: ManagedRecordOptions) {
  if (!options.checkpoint)
    throw new Error("保存前必须保全原操作状态；请使用档案保存确认框。");
  // Reconstruct against the current instance before any remote mutation. Keep
  // the original operation state and keys; a changed scope must fail closed.
  const expected = await prepareManagedRecord(options);
  const prepared = options.prepared ?? expected;
  assertRecordPayload(managedRecordResume(prepared));
  if (
    canonicalJSON(prepared.envelope) !== canonicalJSON(expected.envelope) ||
    canonicalJSON(prepared.input) !== canonicalJSON(expected.input)
  )
    throw new Error(
      "准备后的实例、档案或固定基线已变化，请保留原状态并重新审阅。",
    );
  const receipt = await pushFiles({
    input: prepared.input,
    files: prepared.files,
    state: prepared.state,
    checkpoint: options.checkpoint,
    signal: options.signal,
  });
  return {
    asset_id: receipt.asset_id,
    version_id: receipt.version_id,
    operation_id: receipt.operation_id,
    description_pending: receipt.description_pending,
  };
}

export function managedRecordResume(prepared: PreparedManagedRecord) {
  const resume = {
    schema: "lantai.management-record-resume/v1",
    input: prepared.input,
    state: prepared.state,
    envelope: prepared.envelope,
  };
  assertRecordPayload(resume);
  return resume;
}

export async function restoreManagedRecordResume(
  text: string,
  expected: ManagedRecordOptions,
): Promise<PreparedManagedRecord> {
  const value = parseManagedJSON(text);
  if (
    !object(value) ||
    value.schema !== "lantai.management-record-resume/v1" ||
    !object(value.input) ||
    !object(value.state) ||
    !object(value.envelope)
  )
    throw new Error("不是完整的管理档案原状态。");
  if (
    Object.keys(value).some(
      (key) => !["schema", "input", "state", "envelope"].includes(key),
    )
  )
    throw new Error("原状态含未知字段。");
  assertRecordPayload(value);
  const prepared = await prepareManagedRecord(expected);
  if (
    canonicalJSON(value.input) !== canonicalJSON(prepared.input) ||
    canonicalJSON(value.envelope) !== canonicalJSON(prepared.envelope)
  )
    throw new Error("原状态与当前项目、档案或固定基线不符，不能换键继续。");
  await validatePushState(prepared.input, value.state as PushState);
  prepared.state = value.state as PushState;
  return prepared;
}

export async function loadManagedRecord<T>(
  ref: ManagedRecordRef,
  signal?: AbortSignal,
  expected?: { kind?: ManagedRecordKind; projectID?: string },
): Promise<T> {
  const exact = await getExactManagedVersion({
    assetID: ref.asset_id,
    versionID: ref.version_id,
    signal,
  });
  const files = exact.manifest.content.files;
  if (
    exact.manifest.content.asset_type !== "config" ||
    files.length !== 1 ||
    files[0].path !== "record.json" ||
    files[0].role !== "record" ||
    files[0].size > maxRecordBytes
  )
    throw new Error("固定版本不是受支持的管理档案。");
  const bytes = await readManagedFile({
    assetID: ref.asset_id,
    versionID: ref.version_id,
    file: files[0],
    purpose: "archive_review",
    signal,
  });
  const envelope = parseManagedJSON(
    new TextDecoder("utf-8", { fatal: true }).decode(bytes),
  );
  if (
    !object(envelope) ||
    envelope.schema !== "lantai.management-record/v1" ||
    !kinds.includes(envelope.kind as ManagedRecordKind) ||
    envelope.instance_id !== exact.manifest.instance_id ||
    envelope.project_id !== exact.manifest.project_id ||
    Object.keys(envelope).some(
      (key) =>
        !["schema", "kind", "instance_id", "project_id", "payload"].includes(
          key,
        ),
    )
  )
    throw new Error("管理档案身份、范围或格式不匹配。");
  if (
    (expected?.kind && envelope.kind !== expected.kind) ||
    (expected?.projectID && envelope.project_id !== expected.projectID)
  )
    throw new Error("固定档案不属于所选记录类型或项目。");
  assertRecordPayload(envelope.payload);
  return envelope.payload as T;
}

export async function listManagedRecords({
  projectID,
  kind,
  signal,
  cursor,
}: {
  projectID: string;
  kind: ManagedRecordKind;
  signal?: AbortSignal;
  cursor?: string;
}): Promise<{ items: ManagedRecordItem[]; next_cursor?: string }> {
  if (!identifierPattern.test(projectID) || !kinds.includes(kind))
    throw new Error("请选择项目及记录类型。");
  const query = new URLSearchParams({
    project_id: projectID,
    asset_type: "config",
    limit: "50",
  });
  query.append("tag", "lantai-management-record");
  query.append("tag", `record-kind:${kind}`);
  if (cursor) query.set("cursor", cursor);
  const page = await api<{ items: Item[] | null; next_cursor?: string }>(
    `/api/v1/assets?${query}`,
    { signal },
  );
  return {
    items: (page.items ?? []).map((item) => ({
      asset_id: item.asset_id,
      version_id: item.version_id,
      project_id: item.project_id,
      title: item.title,
      record_kind: kind,
    })),
    next_cursor: page.next_cursor,
  };
}
