// Read-only workset validation and browser export. Local filesystem checks and
// resumable writes remain with the existing Go Workset client/assistant.
export const WORKSET_SCHEMA = "lantai.client-workset/v1";
export const MAX_WORKSET_JSON = 8 * 1024 * 1024;
export const MAX_WORKSET_FILES = 10000;
export const MAX_WORKSET_BYTES = 2 ** 40;
export const MAX_BROWSER_WORKSET_BYTES = 64 * 1024 * 1024;
export const MAX_BROWSER_WORKSET_FILES = 256;
export const worksetID = /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/;
const hashPattern = /^[a-f0-9]{64}$/;
const digestPattern = /^sha256:[a-f0-9]{64}$/;
const encoder = new TextEncoder();

export interface WorksetEntry {
  asset_id: string;
  version_id: string;
  destination: string;
}
export interface WorksetDependency {
  from: string;
  requires: string[];
}
export interface WorksetInput {
  schema: typeof WORKSET_SCHEMA;
  entries: WorksetEntry[];
  entrypoints?: string[];
  dependencies?: WorksetDependency[];
}
export interface WorksetFile {
  source_path: string;
  path: string;
  sha256: string;
  size: number;
}
export interface VerifiedWorksetEntry extends WorksetEntry {
  instance_id: string;
  manifest_digest: string;
  files: WorksetFile[];
}
export interface BrowserWorksetPlan {
  schema: "lantai.browser-workset-check/v1";
  origin: string;
  input_digest: string;
  instance_id: string;
  checked_at: string;
  state: "remote_verified";
  local_directory_checked: false;
  file_count: number;
  total_bytes: number;
  entries: VerifiedWorksetEntry[];
  entrypoints: string[];
  dependencies: WorksetDependency[];
}
export class WorksetError extends Error {
  code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = "WorksetError";
    this.code = code;
  }
}
function fail(message: string, code = "SCHEMA_INVALID"): never {
  throw new WorksetError(code, message);
}
function validUnicode(value: string) {
  for (let i = 0; i < value.length; i++) {
    const point = value.charCodeAt(i);
    if (point >= 0xd800 && point <= 0xdbff) {
      const next = value.charCodeAt(++i);
      if (!(next >= 0xdc00 && next <= 0xdfff)) return false;
    } else if (point >= 0xdc00 && point <= 0xdfff) return false;
  }
  return true;
}

// Reject duplicate keys before JSON.parse can erase evidence. JSON.parse is
// used for each scalar only, retaining ECMAScript/RFC8785 number semantics.
export function parseWorksetJSON(text: string): unknown {
  if (encoder.encode(text).length > MAX_WORKSET_JSON)
    fail("JSON 超过 8 MiB，请显式拆分工作集。");
  let at = 0;
  const space = () => {
    while ([" ", "\t", "\n", "\r"].includes(text[at]) && at < text.length) at++;
  };
  const string = () => {
    const start = at++;
    let escaped = false;
    for (; at < text.length; at++) {
      if (!escaped && text[at] === '"') {
        at++;
        let result: unknown;
        try {
          result = JSON.parse(text.slice(start, at));
        } catch {
          fail("JSON 字符串无效。");
        }
        if (typeof result !== "string" || !validUnicode(result))
          fail("JSON 包含无效 Unicode。");
        return result;
      }
      if (!escaped && text[at] === "\\") escaped = true;
      else escaped = false;
    }
    return fail("JSON 字符串未结束。");
  };
  const value = (depth: number): unknown => {
    if (depth > 64) fail("JSON 嵌套超过 64 层。");
    space();
    const token = text[at];
    if (token === '"') return string();
    if (token === "{") {
      at++;
      const result: Record<string, unknown> = Object.create(null);
      const keys = new Set<string>();
      space();
      if (text[at] === "}") {
        at++;
        return result;
      }
      for (;;) {
        space();
        if (text[at] !== '"') fail("JSON 对象键无效。");
        const key = string();
        if (keys.has(key)) fail(`JSON 重复键：${key}`);
        keys.add(key);
        space();
        if (text[at++] !== ":") fail("JSON 对象缺少冒号。");
        result[key] = value(depth + 1);
        space();
        const end = text[at++];
        if (end === "}") return result;
        if (end !== ",") fail("JSON 对象未正确结束。");
      }
    }
    if (token === "[") {
      at++;
      const result: unknown[] = [];
      space();
      if (text[at] === "]") {
        at++;
        return result;
      }
      for (;;) {
        result.push(value(depth + 1));
        space();
        const end = text[at++];
        if (end === "]") return result;
        if (end !== ",") fail("JSON 数组未正确结束。");
      }
    }
    const match =
      /^(?:true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)/.exec(
        text.slice(at),
      );
    if (!match) return fail("JSON 值无效。");
    at += match[0].length;
    const result: unknown = JSON.parse(match[0]);
    if (
      typeof result === "number" &&
      (!Number.isFinite(result) ||
        (Number.isInteger(result) && !Number.isSafeInteger(result)))
    )
      fail("JSON 数字超出可安全表达范围。");
    return result;
  };
  const result = value(0);
  space();
  if (at !== text.length) fail("JSON 必须只包含一个完整文档。");
  return result;
}

export function canonicalWorksetJSON(value: unknown): string {
  if (value === null) return "null";
  if (typeof value === "string") {
    if (!validUnicode(value)) fail("内容包含无效 Unicode。");
    return JSON.stringify(value);
  }
  if (typeof value === "boolean") return String(value);
  if (typeof value === "number") {
    if (
      !Number.isFinite(value) ||
      (Number.isInteger(value) && !Number.isSafeInteger(value))
    )
      fail("内容包含不安全数字。");
    return JSON.stringify(value);
  }
  if (Array.isArray(value))
    return `[${value.map(canonicalWorksetJSON).join(",")}]`;
  if (typeof value === "object") {
    const object = value as Record<string, unknown>;
    return `{${Object.keys(object)
      .sort()
      .map(
        (key) =>
          `${canonicalWorksetJSON(key)}:${canonicalWorksetJSON(object[key])}`,
      )
      .join(",")}}`;
  }
  return fail("JSON 不得包含 undefined 或可执行值。");
}
export async function worksetSHA256(value: Uint8Array | string) {
  const bytes = typeof value === "string" ? encoder.encode(value) : value;
  const digest = await crypto.subtle.digest("SHA-256", new Uint8Array(bytes));
  return Array.from(new Uint8Array(digest), (n) =>
    n.toString(16).padStart(2, "0"),
  ).join("");
}
function object(value: unknown, name: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value))
    return fail(`${name} 必须是对象。`);
  return value as Record<string, unknown>;
}
function keys(value: Record<string, unknown>, allowed: string[], name: string) {
  if (Object.keys(value).some((key) => !allowed.includes(key)))
    fail(`${name} 包含不支持的字段。`);
}
export function worksetPath(value: unknown): string {
  if (
    typeof value !== "string" ||
    !value ||
    !validUnicode(value) ||
    value.normalize("NFC") !== value ||
    [...value].length > 200 ||
    /[\\<>:"|?*\p{Cc}\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/u.test(value)
  )
    return fail("路径须为 NFC、至多 200 字符的安全相对路径。");
  for (const part of value.split("/")) {
    const base = part.split(".")[0].replace(/ +$/, "").toLowerCase();
    if (
      !part ||
      part === "." ||
      part === ".." ||
      /[. ]$/.test(part) ||
      encoder.encode(part).length > 255 ||
      /^(?:con|prn|aux|nul|conin\$|conout\$|(?:com|lpt)[0-9¹²³])$/.test(base) ||
      /\.(?:lantai-part|lantai-download\.json|lantai-lock)$/i.test(part)
    )
      return fail(`不安全或保留的路径：${value}`);
  }
  return value;
}
// Conservatively catch cross-platform aliases, including full folds such as
// sharp-s/ss and final-sigma. The native client repeats authoritative checks.
function fold(value: string) {
  return value
    .toUpperCase()
    .toLowerCase()
    .toUpperCase()
    .toLowerCase()
    .normalize("NFC");
}
export function checkWorksetConflicts(paths: string[]) {
  const terminals = new Set<string>();
  const directories = new Set<string>();
  const spelling = new Map<string, string>();
  for (const path of paths) {
    worksetPath(path);
    const parts = path.split("/");
    for (let i = 0; i < parts.length; i++) {
      const prefix = parts.slice(0, i + 1).join("/");
      const key = fold(prefix);
      const previous = spelling.get(key);
      if (previous !== undefined && previous !== prefix)
        fail(`目录拼写或大小写冲突：${previous} / ${prefix}`, "PATH_CONFLICT");
      spelling.set(key, prefix);
      if (i === parts.length - 1) {
        if (terminals.has(key) || directories.has(key))
          fail(`目标路径重复或与目录重叠：${path}`, "PATH_CONFLICT");
        terminals.add(key);
      } else {
        if (terminals.has(key))
          fail(`文件与父目录冲突：${path}`, "PATH_CONFLICT");
        directories.add(key);
      }
    }
  }
}
export function validateWorkset(value: unknown): WorksetInput {
  const input = object(value, "工作集");
  keys(input, ["schema", "entries", "entrypoints", "dependencies"], "工作集");
  if (
    input.schema !== WORKSET_SCHEMA ||
    !Array.isArray(input.entries) ||
    input.entries.length === 0 ||
    input.entries.length > 1000
  )
    return fail("工作集需使用 v1 schema，并包含 1–1000 个固定版本。");
  const entries = input.entries.map((value) => {
    const e = object(value, "版本条目");
    keys(e, ["asset_id", "version_id", "destination"], "版本条目");
    if (
      typeof e.asset_id !== "string" ||
      !worksetID.test(e.asset_id) ||
      typeof e.version_id !== "string" ||
      !worksetID.test(e.version_id)
    )
      return fail("每个条目必须使用明确、规范的资产与版本 ID，不接受 latest。");
    return {
      asset_id: e.asset_id,
      version_id: e.version_id,
      destination: worksetPath(e.destination),
    };
  });
  checkWorksetConflicts(entries.map((e) => e.destination));
  const paths = (v: unknown, name: string): string[] => {
    if (!Array.isArray(v) || v.length > MAX_WORKSET_FILES)
      return fail(`${name} 需要有界路径数组。`);
    const result = v.map(worksetPath);
    if (new Set(result).size !== result.length) fail(`${name} 中有重复路径。`);
    return result;
  };
  const entrypoints =
    input.entrypoints === undefined
      ? undefined
      : paths(input.entrypoints, "入口");
  let dependencies: WorksetDependency[] | undefined;
  if (input.dependencies !== undefined) {
    if (
      !Array.isArray(input.dependencies) ||
      input.dependencies.length > MAX_WORKSET_FILES
    )
      return fail("依赖需要有界数组。");
    dependencies = input.dependencies.map((value) => {
      const d = object(value, "依赖");
      keys(d, ["from", "requires"], "依赖");
      const requires = paths(d.requires, "依赖目标");
      if (!requires.length) fail("每项依赖必须至少声明一个必需文件。");
      return { from: worksetPath(d.from), requires };
    });
  }
  const result: WorksetInput = { schema: WORKSET_SCHEMA, entries };
  // The Go WorksetInput contract omits empty optional slices before hashing.
  if (entrypoints?.length) result.entrypoints = entrypoints;
  if (dependencies?.length) result.dependencies = dependencies;
  if (encoder.encode(canonicalWorksetJSON(result)).length > MAX_WORKSET_JSON)
    fail("工作集超过 8 MiB。");
  return result;
}
export function checkDeclaredDependencies(
  input: WorksetInput,
  paths: string[],
) {
  const present = new Set(paths);
  const missing = new Set<string>();
  for (const p of input.entrypoints || []) if (!present.has(p)) missing.add(p);
  for (const d of input.dependencies || []) {
    if (!present.has(d.from)) missing.add(d.from);
    for (const p of d.requires) if (!present.has(p)) missing.add(p);
  }
  if (missing.size)
    fail(
      `固定清单缺少声明入口或依赖：${[...missing].join("、")}`,
      "DEPENDENCY_MISSING",
    );
}
export type WorksetRead = (
  path: string,
  signal: AbortSignal,
) => Promise<unknown>;
export function worksetOrigin(origin: string) {
  const u = new URL(origin);
  if (
    u.protocol !== "https:" ||
    u.origin !== origin ||
    u.username ||
    u.password
  )
    return fail("工作集 origin 必须是当前 HTTPS origin。");
  return origin;
}
export async function planBrowserWorkset(
  value: unknown,
  origin: string,
  read: WorksetRead,
  signal: AbortSignal,
): Promise<BrowserWorksetPlan> {
  const input = validateWorkset(value);
  worksetOrigin(origin);
  signal.throwIfAborted();
  const meta = object(await read("/api/v1/meta", signal), "实例 metadata");
  if (
    meta.api_version !== "v1" ||
    typeof meta.instance_id !== "string" ||
    !worksetID.test(meta.instance_id)
  )
    return fail("实例 metadata 缺少 v1 或规范 instance_id。", "REF_MISMATCH");
  const entries: VerifiedWorksetEntry[] = [];
  const allPaths: string[] = [];
  let total = 0;
  for (const entry of input.entries) {
    signal.throwIfAborted();
    const response = object(
      await read(
        `/api/v1/assets/${entry.asset_id}/versions/${entry.version_id}?view=full`,
        signal,
      ),
      "精确版本",
    );
    const version = object(response.version, "版本身份");
    const manifest = object(response.manifest, "固定清单");
    if (
      version.asset_id !== entry.asset_id ||
      version.version_id !== entry.version_id ||
      manifest.asset_id !== entry.asset_id ||
      manifest.version_id !== entry.version_id ||
      manifest.instance_id !== meta.instance_id ||
      typeof manifest.manifest_digest !== "string" ||
      !digestPattern.test(manifest.manifest_digest) ||
      version.manifest_digest !== manifest.manifest_digest ||
      typeof manifest.project_id !== "string" ||
      !worksetID.test(manifest.project_id) ||
      version.project_id !== manifest.project_id ||
      !Number.isSafeInteger(version.version_number) ||
      (version.version_number as number) < 1 ||
      version.version_number !== manifest.version_number ||
      version.uri !==
        `lantai://${meta.instance_id}/assets/${entry.asset_id}/versions/${entry.version_id}` ||
      manifest.contract !== "lantai.manifest/v1"
    )
      return fail("精确版本、清单或馆实例不匹配。", "REF_MISMATCH");
    const content = object(manifest.content, "冻结内容");
    if (
      `sha256:${await worksetSHA256(canonicalWorksetJSON(content))}` !==
      manifest.manifest_digest
    )
      return fail("固定清单内容摘要不匹配。", "HASH_MISMATCH");
    if (
      !Array.isArray(content.files) ||
      !content.files.length ||
      content.files.length > MAX_WORKSET_FILES
    )
      return fail("清单文件数无效。");
    const files = content.files.map((value) => {
      const file = object(value, "清单文件");
      const source = worksetPath(file.path);
      if (
        typeof file.sha256 !== "string" ||
        !hashPattern.test(file.sha256) ||
        typeof file.size !== "number" ||
        !Number.isSafeInteger(file.size) ||
        file.size < 0
      )
        return fail("清单文件大小或 SHA-256 无效。");
      const path = worksetPath(`${entry.destination}/${source}`);
      total += file.size;
      allPaths.push(path);
      if (allPaths.length > MAX_WORKSET_FILES || total > MAX_WORKSET_BYTES)
        return fail("工作集超过 10,000 文件或 1 TiB，请显式拆分。");
      return {
        source_path: source,
        path,
        size: file.size,
        sha256: file.sha256,
      };
    });
    checkWorksetConflicts(files.map((f) => f.source_path));
    entries.push({
      ...entry,
      instance_id: meta.instance_id,
      manifest_digest: manifest.manifest_digest,
      files,
    });
  }
  checkWorksetConflicts(allPaths);
  checkDeclaredDependencies(input, allPaths);
  signal.throwIfAborted();
  return {
    schema: "lantai.browser-workset-check/v1",
    origin,
    input_digest: `sha256:${await worksetSHA256(canonicalWorksetJSON(input))}`,
    instance_id: meta.instance_id,
    checked_at: new Date().toISOString(),
    state: "remote_verified",
    local_directory_checked: false,
    file_count: allPaths.length,
    total_bytes: total,
    entries,
    entrypoints: input.entrypoints || [],
    dependencies: input.dependencies || [],
  };
}

export function makeWorksetAssistantJob(
  input: WorksetInput,
  origin: string,
  action: "workset.plan" | "workset.pull",
  directory: string,
  output: string,
  purpose: string,
  jobID: string,
) {
  const normalized = validateWorkset(input);
  worksetOrigin(origin);
  worksetPath(directory);
  worksetPath(output);
  if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$/.test(jobID))
    fail("作业名称或工作目录无效。");
  checkWorksetConflicts([directory, output]);
  if (
    ![
      "archive_review",
      "reference",
      "production",
      "generative_input",
      "raw_export",
    ].includes(purpose)
  )
    fail("请选择明确合法的读取用途。");
  return {
    schema: "lantai.local-assistant-job/v1",
    job_id: jobID,
    action,
    origin,
    input: normalized,
    directory,
    output,
    purpose: action === "workset.plan" ? "archive_review" : purpose,
  };
}

export type WorksetAssistantJob = ReturnType<typeof makeWorksetAssistantJob>;
export interface WorksetInspection {
  job_id: string;
  action: string;
  job_digest: string;
  approval_digest: string;
  input_sha256: string;
  workspace_binding: string;
}
export async function checkWorksetInspection(
  value: unknown,
  job: WorksetAssistantJob,
  inputDigest: string,
): Promise<WorksetInspection> {
  const inspection = object(value, "助手检查回执");
  const paths = object(inspection.paths, "检查路径");
  const jobDigest = await worksetSHA256(canonicalWorksetJSON(job));
  if (
    !digestPattern.test(inputDigest) ||
    inspection.schema !== "lantai.local-assistant-inspection/v1" ||
    inspection.job_id !== job.job_id ||
    inspection.action !== job.action ||
    inspection.job_digest !== jobDigest ||
    inspection.origin !== job.origin ||
    inspection.input_sha256 !== inputDigest.slice(7) ||
    typeof inspection.approval_digest !== "string" ||
    !hashPattern.test(inspection.approval_digest) ||
    typeof inspection.workspace_binding !== "string" ||
    !hashPattern.test(inspection.workspace_binding) ||
    paths.directory !== job.directory ||
    paths.output !== job.output ||
    inspection.source_deleted !== false ||
    inspection.remote_writes !== false
  )
    return fail("检查回执与导出的同一作业、输入或路径不匹配。", "REF_MISMATCH");
  return {
    job_id: job.job_id,
    action: job.action,
    job_digest: jobDigest,
    approval_digest: inspection.approval_digest,
    input_sha256: inputDigest.slice(7),
    workspace_binding: inspection.workspace_binding,
  };
}
export interface WorksetAssistantObservation {
  job_id: string;
  action: string;
  job_digest: string;
  approval_digest: string;
  status: "completed" | "partial" | "cancelled" | "failed" | "unknown";
  exit_code: number;
  finished_at: string;
  input_digest: string;
  native_files_verified: boolean;
  local_receipt_state?: string;
  file_count?: number;
  total_bytes?: number;
}

// Only a fresh DeviceJobConsole observation can supply this context. A local
// result file alone remains an operator-supplied historical observation.
export function checkWorksetDeviceContext(
  value: unknown,
  inspection: WorksetInspection,
): void {
  const context = object(value, "设备核心结果关联");
  const fence = object(context.job_fence, "设备任务租约关联");
  if (
    context.state !== "succeeded" ||
    context.stopped !== true ||
    context.job_digest !== inspection.job_digest ||
    context.approval_digest !== inspection.approval_digest ||
    context.workspace_binding !== inspection.workspace_binding ||
    !hashPattern.test(inspection.job_digest) ||
    !hashPattern.test(inspection.workspace_binding) ||
    typeof context.job_id !== "string" ||
    !worksetID.test(context.job_id) ||
    typeof context.device_id !== "string" ||
    !worksetID.test(context.device_id) ||
    typeof context.operation_id !== "string" ||
    !worksetID.test(context.operation_id) ||
    fence.job_id !== context.job_id ||
    typeof fence.job_attempt_id !== "string" ||
    !worksetID.test(fence.job_attempt_id) ||
    !Number.isSafeInteger(fence.lease_fence) ||
    Number(fence.lease_fence) < 1 ||
    !Number.isSafeInteger(fence.recovery_epoch) ||
    Number(fence.recovery_epoch) < 0
  )
    fail(
      "设备核心尚未可信完成，或原作业、工作区与当前租约不匹配；保留原结果并按核心状态对账。",
      "REF_MISMATCH",
    );
}
export function checkWorksetAssistantResult(
  value: unknown,
  inspection: WorksetInspection,
  plan: BrowserWorksetPlan,
): WorksetAssistantObservation {
  const envelope = object(value, "助手执行回执");
  const statuses = ["completed", "partial", "cancelled", "failed", "unknown"];
  if (
    envelope.schema !== "lantai.local-assistant-result/v1" ||
    envelope.job_id !== inspection.job_id ||
    envelope.action !== inspection.action ||
    !hashPattern.test(inspection.job_digest) ||
    envelope.job_digest !== inspection.job_digest ||
    envelope.approval_digest !== inspection.approval_digest ||
    !statuses.includes(String(envelope.status)) ||
    typeof envelope.exit_code !== "number" ||
    !Number.isInteger(envelope.exit_code) ||
    typeof envelope.finished_at !== "string" ||
    !Number.isFinite(Date.parse(envelope.finished_at)) ||
    envelope.source_deleted !== false ||
    envelope.automatic_retry !== false ||
    envelope.credential_issued !== false ||
    inspection.input_sha256 !== plan.input_digest.slice(7)
  )
    return fail("助手执行回执未绑定原检查摘要或当前工作集。", "REF_MISMATCH");
  const observation: WorksetAssistantObservation = {
    job_id: inspection.job_id,
    action: inspection.action,
    job_digest: inspection.job_digest,
    approval_digest: inspection.approval_digest,
    status: envelope.status as WorksetAssistantObservation["status"],
    exit_code: envelope.exit_code,
    finished_at: envelope.finished_at,
    input_digest: plan.input_digest,
    native_files_verified: false,
  };
  if (envelope.status !== "completed") return observation;
  const result = object(envelope.result, "本机工作集结果");
  const pull = inspection.action === "workset.pull";
  if (
    envelope.exit_code !== 0 ||
    result.schema !==
      (pull
        ? "lantai.client-workset-receipt/v1"
        : "lantai.client-workset-plan/v1") ||
    result.state !== (pull ? "completed" : "planned") ||
    result.origin !== plan.origin ||
    result.input_digest !== plan.input_digest ||
    result.file_count !== plan.file_count ||
    result.total_bytes !== plan.total_bytes ||
    !Array.isArray(result.entries) ||
    result.entries.length !== plan.entries.length
  )
    return fail("助手完成标志与固定版本结果不一致。", "REF_MISMATCH");
  for (const [i, raw] of result.entries.entries()) {
    const entry = object(raw, "本机版本结果"),
      expected = plan.entries[i];
    if (
      entry.asset_id !== expected.asset_id ||
      entry.version_id !== expected.version_id ||
      entry.destination !== expected.destination ||
      entry.instance_id !== plan.instance_id ||
      entry.manifest_digest !== expected.manifest_digest ||
      !Array.isArray(entry.files) ||
      entry.files.length !== expected.files.length
    )
      return fail("助手结果版本或馆实例不匹配。", "REF_MISMATCH");
    for (const [j, rawFile] of entry.files.entries()) {
      const file = object(rawFile, "本机文件结果"),
        wanted = expected.files[j];
      if (
        file.path !== wanted.path ||
        file.source_path !== wanted.source_path ||
        file.sha256 !== wanted.sha256 ||
        file.size !== wanted.size ||
        (pull && file.state !== "verified")
      )
        return fail("助手结果的固定文件或摘要不匹配。", "HASH_MISMATCH");
    }
  }
  if (
    canonicalWorksetJSON(result.entrypoints || []) !==
      canonicalWorksetJSON(plan.entrypoints) ||
    canonicalWorksetJSON(result.dependencies || []) !==
      canonicalWorksetJSON(plan.dependencies)
  )
    return fail("助手结果的声明依赖不同。", "REF_MISMATCH");
  return {
    ...observation,
    native_files_verified: pull,
    local_receipt_state: String(result.state),
    file_count: plan.file_count,
    total_bytes: plan.total_bytes,
  };
}

function tarNames(path: string) {
  worksetPath(path);
  if (encoder.encode(path).length <= 100) return { name: path, prefix: "" };
  for (let i = path.lastIndexOf("/"); i > 0; i = path.lastIndexOf("/", i - 1)) {
    const prefix = path.slice(0, i),
      name = path.slice(i + 1);
    if (
      encoder.encode(prefix).length <= 155 &&
      encoder.encode(name).length <= 100
    )
      return { name, prefix };
  }
  return fail(
    "路径超过浏览器 TAR 字段预算，请使用本机助手。",
    "BROWSER_BUDGET",
  );
}
export function worksetTar(files: { path: string; bytes: Uint8Array }[]) {
  checkWorksetConflicts(files.map((f) => f.path));
  const parts: BlobPart[] = [];
  let count = 0;
  for (const file of files) {
    count += file.bytes.byteLength;
    if (
      count > MAX_BROWSER_WORKSET_BYTES ||
      files.length > MAX_BROWSER_WORKSET_FILES
    )
      fail(
        "浏览器导出上限为 256 文件、64 MiB；请使用本机助手。",
        "BROWSER_BUDGET",
      );
    const names = tarNames(file.path),
      header = new Uint8Array(512);
    const put = (value: string, at: number, length: number) => {
      const bytes = encoder.encode(value);
      if (bytes.length > length) fail("TAR 字段超限。");
      header.set(bytes, at);
    };
    const octal = (n: number, length: number) =>
      n.toString(8).padStart(length - 1, "0") + "\0";
    put(names.name, 0, 100);
    put(octal(0o600, 8), 100, 8);
    put(octal(0, 8), 108, 8);
    put(octal(0, 8), 116, 8);
    put(octal(file.bytes.byteLength, 12), 124, 12);
    put(octal(0, 12), 136, 12);
    header.fill(32, 148, 156);
    header[156] = 48;
    put("ustar\0", 257, 6);
    put("00", 263, 2);
    put(names.prefix, 345, 155);
    put(
      header
        .reduce((sum, n) => sum + n, 0)
        .toString(8)
        .padStart(6, "0") + "\0 ",
      148,
      8,
    );
    parts.push(header, new Uint8Array(file.bytes));
    if (file.bytes.byteLength % 512)
      parts.push(new Uint8Array(512 - (file.bytes.byteLength % 512)));
  }
  parts.push(new Uint8Array(1024));
  return new Blob(parts, { type: "application/x-tar" });
}
export type WorksetGrant = {
  path: string;
  size: number;
  sha256: string;
  url: string;
  expires_at: string;
};
export function checkWorksetGrant(
  grant: WorksetGrant,
  file: WorksetFile,
  origin: string,
) {
  worksetOrigin(origin);
  const url = new URL(grant.url, origin);
  if (
    grant.path !== file.source_path ||
    grant.size !== file.size ||
    grant.sha256 !== file.sha256 ||
    !Number.isFinite(Date.parse(grant.expires_at)) ||
    Date.parse(grant.expires_at) <= Date.now() ||
    url.origin !== origin ||
    url.username ||
    url.password ||
    url.hash ||
    !/^\/xfer\/v1\/reads\/[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(url.pathname)
  )
    return fail("读取授权未绑定固定文件或同源传输。", "REF_MISMATCH");
  return url.href;
}
export async function pullBrowserWorkset(options: {
  input: WorksetInput;
  origin: string;
  purpose: string;
  read: WorksetRead;
  grant: (
    entry: VerifiedWorksetEntry,
    file: WorksetFile,
    purpose: string,
    signal: AbortSignal,
  ) => Promise<WorksetGrant>;
  transfer: (url: string, signal: AbortSignal) => Promise<Response>;
  signal: AbortSignal;
  progress?: (done: number, total: number) => void;
}) {
  const plan = await planBrowserWorkset(
    options.input,
    options.origin,
    options.read,
    options.signal,
  );
  if (
    plan.total_bytes > MAX_BROWSER_WORKSET_BYTES ||
    plan.file_count > MAX_BROWSER_WORKSET_FILES
  )
    fail(
      "浏览器导出上限为 256 文件、64 MiB，请使用本机助手流式续传。",
      "BROWSER_BUDGET",
    );
  if (
    ![
      "archive_review",
      "reference",
      "production",
      "generative_input",
      "raw_export",
    ].includes(options.purpose)
  )
    fail("读取用途无效。");
  // Check archive path representability for every file before any read grant.
  for (const e of plan.entries) for (const f of e.files) tarNames(f.path);
  const files: { path: string; bytes: Uint8Array }[] = [];
  let done = 0;
  for (const entry of plan.entries)
    for (const file of entry.files) {
      options.signal.throwIfAborted();
      const grant = await options.grant(
        entry,
        file,
        options.purpose,
        options.signal,
      );
      const url = checkWorksetGrant(grant, file, options.origin);
      const response = await options.transfer(url, options.signal);
      if (response.status !== 200 || !response.body)
        fail("完整文件传输响应无效。", "INVALID_RESPONSE");
      const reader = response.body.getReader(),
        chunks: Uint8Array[] = [];
      let size = 0;
      try {
        for (;;) {
          options.signal.throwIfAborted();
          const next = await reader.read();
          if (next.done) break;
          size += next.value.byteLength;
          if (size > file.size) fail("传输超过固定文件大小。", "HASH_MISMATCH");
          chunks.push(next.value);
        }
      } finally {
        await reader.cancel().catch(() => {});
        reader.releaseLock();
      }
      const bytes = new Uint8Array(size);
      let at = 0;
      for (const chunk of chunks) {
        bytes.set(chunk, at);
        at += chunk.length;
      }
      if (size !== file.size || (await worksetSHA256(bytes)) !== file.sha256)
        fail("传输大小或完整 SHA-256 不匹配，未发布归档。", "HASH_MISMATCH");
      files.push({ path: file.path, bytes });
      options.progress?.(++done, plan.file_count);
    }
  // Recheck current access after the last byte; late revocation cannot turn a
  // cached download into a new successful current check. No new file grants.
  const final = await planBrowserWorkset(
    options.input,
    options.origin,
    options.read,
    options.signal,
  );
  if (
    final.input_digest !== plan.input_digest ||
    final.instance_id !== plan.instance_id ||
    final.entries.some(
      (e, i) => e.manifest_digest !== plan.entries[i].manifest_digest,
    )
  )
    fail("下载后固定清单绑定发生变化。", "REF_MISMATCH");
  options.signal.throwIfAborted();
  return {
    archive: worksetTar(files),
    plan: final,
    receipt: {
      schema: "lantai.browser-workset-export/v1",
      origin: plan.origin,
      instance_id: plan.instance_id,
      input_digest: plan.input_digest,
      checked_at: new Date().toISOString(),
      state: "archive_verified",
      local_directory_checked: false,
      file_count: plan.file_count,
      total_bytes: plan.total_bytes,
      entries: plan.entries,
      purpose: options.purpose,
    },
  };
}
