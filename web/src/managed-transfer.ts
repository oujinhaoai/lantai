import { api, APIError, csrfForRequest } from "./api";

// These are browser transport budgets, not repository or server quotas. The
// existing native client remains the streaming path for larger material.
export const browserFileLimit = 64 * 1024 * 1024;
export const browserBatchLimit = 256 * 1024 * 1024;
export const browserFileCountLimit = 256;
export const identifierPattern = /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/;
const hashPattern = /^[0-9a-f]{64}$/;
const keyPattern = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
const roles = [
  "primary",
  "source",
  "interchange",
  "texture",
  "recipe",
  "record",
  "preview",
  "doc",
];
const assetTypes = [
  "image",
  "video",
  "audio",
  "doc",
  "config",
  "plugin",
  "model",
  "motion",
  "scene",
  "production",
];

export type ManagedFile = { path: string; role: string; file: File };
export type InputFile = {
  path: string;
  role: string;
  sha256: string;
  size: number;
};
export type Rights = {
  usage: "production" | "reference" | "restricted";
  license: string;
  redistribute_raw: boolean;
  noai: boolean;
  sensitivity: "normal" | "personal";
  digital_source_type?: string;
};
export type PushInput = {
  task?: unknown;
  project_id: string;
  asset_id?: string;
  slug?: string;
  base_version_id?: string;
  content: {
    asset_type: string;
    version_note?: string;
    files: InputFile[];
    uses?: unknown[];
    rights?: Rights;
    metadata?: Record<string, unknown>;
    producer?: Record<string, unknown>;
  };
  describe?: Record<string, unknown>;
};
export type PushReceipt = {
  operation_id: string;
  project_id: string;
  asset_id: string;
  version_id: string;
  version_number: number;
  manifest_digest: string;
  committed_at: string;
  uri: string;
  slug?: string;
  alias_generation?: number;
  ref?: { instance_id?: string; asset_id: string; version_id: string };
  description?: Record<string, unknown>;
  description_pending?: boolean;
  description_error?: string;
};
// Native field names remain for audit. Browser recovery supports browser JSON
// serialization only; lexical native RawMessage states must stay with the native client.
// No cookie, token, CSRF value or transfer URL belongs in this object.
export type PushState = {
  schema: "lantai.client-push/v1";
  origin: string;
  request_hash: string;
  create_key: string;
  commit_key: string;
  upload_id?: string;
  operation_id?: string;
  committing?: boolean;
  result?: PushReceipt;
};
export type TransferProgress = {
  phase:
    | "hashing"
    | "uploading"
    | "uploaded"
    | "committing"
    | "committed"
    | "verifying";
  path?: string;
  completed: number;
  total: number;
};
type UploadFile = {
  sha256: string;
  size: number;
  part_size: number;
  part_count: number;
  state: string;
  received_parts: number[];
  part_url_template: string;
};
type Upload = {
  upload_id: string;
  operation_id: string;
  project_id: string;
  files: UploadFile[];
};
export type ExactManagedVersion = {
  version: PushReceipt;
  manifest: {
    contract: string;
    instance_id: string;
    project_id: string;
    asset_id: string;
    version_id: string;
    version_number: number;
    manifest_digest: string;
    content: { asset_type: string; files: InputFile[]; [key: string]: unknown };
  };
};
export type VerificationReceipt = {
  schema: "lantai.browser-verification/v1";
  origin: string;
  instance_id: string;
  asset_id: string;
  version_id: string;
  manifest_digest: string;
  verified_at: string;
  completion_basis: "current_remote_and_downloaded_files";
  current_remote_verified: true;
  local_source_verified: boolean;
  files: InputFile[];
};

function invalid(message: string): never {
  throw new Error(message);
}
function object(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value);
}
function knownKeys(value: object, keys: string[], label: string) {
  if (Object.keys(value).some((key) => !keys.includes(key)))
    invalid(`${label} 含不支持的字段。`);
}
function originOf(value?: string) {
  const result = new URL(value ?? location.origin);
  if (
    result.protocol !== "https:" ||
    result.username ||
    result.password ||
    result.pathname !== "/" ||
    result.search ||
    result.hash
  )
    invalid("管理传输只接受无附加路径的 HTTPS origin。");
  return result.origin;
}
function abort(signal?: AbortSignal) {
  if (signal?.aborted)
    throw new DOMException("Operation aborted", "AbortError");
}

// Strict bounded JSON for imported plans/states: duplicate names and unsafe
// integers must not be silently changed by JSON.parse before review/hash.
export function parseManagedJSON(text: string): unknown {
  if (new TextEncoder().encode(text).length > 8 * 1024 * 1024)
    invalid("JSON 超过 8 MiB，请拆分明确批次。");
  let pos = 0;
  const whitespace = () => {
    while (/[ \t\r\n]/.test(text[pos] ?? "") && pos < text.length) pos++;
  };
  const string = (): string => {
    const start = pos++;
    let escaped = false;
    while (pos < text.length) {
      const ch = text[pos++];
      if (ch === '"' && !escaped) {
        const value = JSON.parse(text.slice(start, pos)) as string;
        assertUnicode(value);
        return value;
      }
      if (ch === "\\" && !escaped) escaped = true;
      else escaped = false;
    }
    return invalid("JSON 字符串未结束。");
  };
  const value = (depth: number): unknown => {
    if (depth > 64) invalid("JSON 嵌套过深。");
    whitespace();
    const ch = text[pos];
    if (ch === '"') return string();
    if (ch === "{") {
      pos++;
      const result: Record<string, unknown> = Object.create(null) as Record<
        string,
        unknown
      >;
      const names = new Set<string>();
      whitespace();
      if (text[pos] === "}") {
        pos++;
        return result;
      }
      while (pos < text.length) {
        whitespace();
        if (text[pos] !== '"') invalid("JSON 对象键无效。");
        const key = string();
        if (names.has(key)) invalid("JSON 包含重复字段。");
        names.add(key);
        whitespace();
        if (text[pos++] !== ":") invalid("JSON 缺少冒号。");
        result[key] = value(depth + 1);
        whitespace();
        const end = text[pos++];
        if (end === "}") return result;
        if (end !== ",") invalid("JSON 对象分隔符无效。");
      }
    }
    if (ch === "[") {
      pos++;
      const result: unknown[] = [];
      whitespace();
      if (text[pos] === "]") {
        pos++;
        return result;
      }
      while (pos < text.length) {
        result.push(value(depth + 1));
        whitespace();
        const end = text[pos++];
        if (end === "]") return result;
        if (end !== ",") invalid("JSON 数组分隔符无效。");
      }
    }
    for (const [literal, result] of [
      ["true", true],
      ["false", false],
      ["null", null],
    ] as const)
      if (text.startsWith(literal, pos)) {
        pos += literal.length;
        return result;
      }
    const number = text
      .slice(pos)
      .match(/^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/);
    if (number) {
      pos += number[0].length;
      const result = Number(number[0]);
      if (
        !Number.isFinite(result) ||
        (!/[.eE]/.test(number[0]) && !Number.isSafeInteger(result))
      )
        invalid("JSON 数值超出可靠范围。");
      return result;
    }
    return invalid("JSON 内容无效或未结束。");
  };
  const result = value(0);
  whitespace();
  if (pos !== text.length) invalid("JSON 含尾随内容。");
  return result;
}

function assertUnicode(value: string) {
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code >= 0xd800 && code <= 0xdbff) {
      const next = value.charCodeAt(++i);
      if (!(next >= 0xdc00 && next <= 0xdfff)) invalid("字符串含孤立代理项。");
    } else if (code >= 0xdc00 && code <= 0xdfff)
      invalid("字符串含孤立代理项。");
  }
}
export function canonicalJSON(value: unknown, depth = 0): string {
  if (depth > 64) return invalid("JSON 嵌套过深。");
  if (value === null || typeof value === "boolean")
    return JSON.stringify(value);
  if (typeof value === "string") {
    assertUnicode(value);
    return JSON.stringify(value);
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) return invalid("JSON 数字无效。");
    return JSON.stringify(value);
  }
  if (Array.isArray(value))
    return (
      "[" + value.map((item) => canonicalJSON(item, depth + 1)).join(",") + "]"
    );
  if (object(value))
    return (
      "{" +
      Object.keys(value)
        .sort()
        .map((key) => {
          assertUnicode(key);
          return (
            JSON.stringify(key) + ":" + canonicalJSON(value[key], depth + 1)
          );
        })
        .join(",") +
      "}"
    );
  return invalid("JSON 含无法保存的值。");
}
export async function sha256(
  value: BufferSource | Blob | string,
): Promise<string> {
  const bytes =
    typeof value === "string"
      ? new TextEncoder().encode(value)
      : value instanceof Blob
        ? await value.arrayBuffer()
        : value;
  const hash = await crypto.subtle.digest("SHA-256", bytes as BufferSource);
  return Array.from(new Uint8Array(hash), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
}

export function validateManagedPath(path: string) {
  assertUnicode(path);
  if (
    !path ||
    path !== path.normalize("NFC") ||
    [...path].length > 200 ||
    // eslint-disable-next-line no-control-regex -- reject controls in cross-platform paths
    /[\\\u0000-\u001f\u007f<>:"|?*\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/.test(
      path,
    )
  )
    invalid("路径不是安全的 NFC 相对路径。");
  for (const part of path.split("/")) {
    if (
      !part ||
      part === "." ||
      part === ".." ||
      /[. ]$/.test(part) ||
      new TextEncoder().encode(part).length > 255 ||
      /^(?:con|prn|aux|nul|conin\$|conout\$|com[0-9¹²³]|lpt[0-9¹²³])(?:\.|$)/i.test(
        part,
      )
    )
      invalid("路径包含空段、保留名称或不安全组件。");
  }
  if (/\.(?:lantai-part|lantai-download\.json|lantai-lock)$/i.test(path))
    invalid("路径占用了客户端恢复文件后缀。");
}
export function validateManagedFiles(files: InputFile[]) {
  if (
    !Array.isArray(files) ||
    !files.length ||
    files.length > browserFileCountLimit
  )
    invalid(`浏览器每项必须为 1–${browserFileCountLimit} 个明确文件。`);
  let bytes = 0;
  const paths: string[] = [];
  const directories = new Map<string, string>();
  for (const file of files) {
    if (!object(file)) invalid("文件描述无效。");
    knownKeys(file, ["path", "role", "size", "sha256"], "文件描述");
    if (
      typeof file.path !== "string" ||
      !roles.includes(file.role) ||
      !Number.isSafeInteger(file.size) ||
      file.size < 0 ||
      !hashPattern.test(file.sha256)
    )
      invalid("文件路径、角色、大小或 SHA-256 无效。");
    validateManagedPath(file.path);
    if (file.size > browserFileLimit)
      invalid("文件超过浏览器 64 MiB 上限，请使用本机受控助手。");
    bytes += file.size;
    if (bytes > browserBatchLimit)
      invalid("浏览器操作超过 256 MiB，请拆批或使用本机受控助手。");
    const fold = (path: string) =>
      path
        .normalize("NFKC")
        .toLowerCase()
        .replace(/ß/g, "ss")
        .replace(/ς/g, "σ")
        .normalize("NFC");
    const folded = fold(file.path);
    if (
      paths.some(
        (p) =>
          p === folded ||
          p.startsWith(folded + "/") ||
          folded.startsWith(p + "/"),
      )
    )
      invalid("文件名存在大小写等价或文件/目录冲突。");
    paths.push(folded);
    const parts = file.path.split("/");
    for (let i = 1; i < parts.length; i++) {
      const prefix = parts.slice(0, i).join("/");
      const foldedPrefix = fold(prefix);
      if (
        directories.has(foldedPrefix) &&
        directories.get(foldedPrefix) !== prefix
      )
        invalid("中间目录存在大小写或 Unicode 等价拼写冲突。");
      directories.set(foldedPrefix, prefix);
    }
  }
}
const forbiddenTransferField =
  /^(?:password|password_hash|secret_hash|totp_secret|totp_uri|recovery_codes|master_key|private_key|upload_url|download_url|csrf_token|csrf|session_token|access_token|refresh_token|authorization|part_url_template|read_grant_url|download_token|grant_id|token)$/i;
export function assertNoTransferSecrets(value: unknown, depth = 0): void {
  if (depth > 64) invalid("输入嵌套超过限制。");
  if (typeof value === "string") {
    if (/^bearer\s/i.test(value.trim()))
      invalid("输入不能保存 Bearer 凭据字符串。");
    let url: URL | undefined;
    try {
      url = new URL(value);
    } catch {
      /* Ordinary prose is not a URL. */
    }
    if (
      url &&
      ["https:", "http:"].includes(url.protocol) &&
      (url.username ||
        url.password ||
        url.pathname.startsWith("/xfer/") ||
        [...url.searchParams.keys()].some((key) =>
          /^(?:token|access_token|sig|signature|credential|csrf_token)$/i.test(
            key,
          ),
        ))
    )
      invalid("输入不能保存含凭据或签名的短时传输 URL。");
  } else if (Array.isArray(value)) {
    for (const item of value) assertNoTransferSecrets(item, depth + 1);
  } else if (object(value)) {
    for (const [key, item] of Object.entries(value)) {
      if (forbiddenTransferField.test(key))
        invalid(`输入不能保存凭据或短时传输授权字段：${key}`);
      assertNoTransferSecrets(item, depth + 1);
    }
  }
}
export function validatePushInput(input: PushInput) {
  assertNoTransferSecrets(input);
  if (!object(input) || !object(input.content)) invalid("入藏请求必须为对象。");
  knownKeys(
    input,
    [
      "task",
      "project_id",
      "asset_id",
      "slug",
      "base_version_id",
      "content",
      "describe",
    ],
    "入藏请求",
  );
  knownKeys(
    input.content,
    [
      "asset_type",
      "version_note",
      "files",
      "uses",
      "rights",
      "metadata",
      "producer",
    ],
    "版本内容",
  );
  if (
    !identifierPattern.test(input.project_id) ||
    !assetTypes.includes(input.content.asset_type)
  )
    invalid("项目 ID 或素材类型无效。");
  if (input.asset_id) {
    if (
      !identifierPattern.test(input.asset_id) ||
      !identifierPattern.test(input.base_version_id ?? "") ||
      input.slug ||
      input.describe
    )
      invalid(
        "追加版本须指定原资产与 base_version_id，不能附带 slug 或当前著录。",
      );
  } else {
    if (!input.slug || input.base_version_id)
      invalid("新建资产必须指定路径名，不能指定旧基线。");
    validateManagedPath(input.slug);
    const rights = input.content.rights;
    if (
      !rights ||
      !["production", "reference", "restricted"].includes(rights.usage) ||
      typeof rights.license !== "string" ||
      !rights.license.trim() ||
      rights.license.length > 256 ||
      typeof rights.redistribute_raw !== "boolean" ||
      typeof rights.noai !== "boolean" ||
      !["normal", "personal"].includes(rights.sensitivity)
    )
      invalid("新资产必须显式确认完整 rights；来源或业务标签不能替代许可。");
  }
  validateManagedFiles(input.content.files);
  if (new TextEncoder().encode(canonicalJSON(input)).length > 8 * 1024 * 1024)
    invalid("请求超过 8 MiB。");
}
export async function preparePushInput({
  input,
  files,
  signal,
  onProgress,
}: {
  input: PushInput;
  files: ManagedFile[];
  signal?: AbortSignal;
  onProgress?: (progress: TransferProgress) => void;
}): Promise<PushInput> {
  abort(signal);
  if (
    !object(input) ||
    !object(input.content) ||
    !Array.isArray(input.content.files)
  )
    invalid("入藏输入缺少版本文件清单。");
  if (
    !files.length ||
    files.length > browserFileCountLimit ||
    files.some((f) => f.file.size > browserFileLimit) ||
    files.reduce((n, f) => n + f.file.size, 0) > browserBatchLimit
  )
    invalid("所选文件超过浏览器预算；使用明确小批次或受控本机助手。");
  const prepared = structuredClone(input);
  if (
    prepared.content.files.length &&
    (prepared.content.files.length !== files.length ||
      prepared.content.files.some(
        (item) =>
          !files.some(
            (file) => file.path === item.path && file.role === item.role,
          ),
      ))
  )
    invalid("所选本机文件与冻结清单不一致。");
  const declared = new Map(prepared.content.files.map((f) => [f.path, f]));
  const hashed: InputFile[] = [];
  for (const file of files) {
    abort(signal);
    validateManagedPath(file.path);
    if (
      /(?:^|\/)(?:\.env(?:\..*)?|credentials\.json|token\.json|session\.json|\.ssh|\.aws)(?:\/|$)|\.(?:key|pem)$/i.test(
        file.path,
      )
    )
      invalid("选中路径疑似凭据或密钥；请显式排除并保留独立保护责任。");
    const digest = await sha256(file.file);
    abort(signal);
    const old = declared.get(file.path);
    if (
      old &&
      ((old.sha256 && old.sha256 !== digest) || old.size !== file.file.size)
    )
      invalid("本机源文件已变化，不能沿用冻结请求或原键。");
    hashed.push({
      path: file.path,
      role: file.role,
      sha256: digest,
      size: file.file.size,
    });
    onProgress?.({
      phase: "hashing",
      path: file.path,
      completed: hashed.length,
      total: files.length,
    });
  }
  // Preserve declared browser plan order; native lexical states stay native.
  prepared.content.files = prepared.content.files.length
    ? prepared.content.files.map((f) => hashed.find((h) => h.path === f.path)!)
    : hashed;
  validatePushInput(prepared);
  return prepared;
}

// Match Go's ordered PushInput/ContentInput structs and encoding/json escaping.
// Raw object member order is retained in the exported browser-generated input.
function pushJSON(input: PushInput) {
  const content = input.content;
  const ordered = {
    ...(input.task !== undefined ? { task: input.task } : {}),
    project_id: input.project_id,
    ...(input.asset_id ? { asset_id: input.asset_id } : {}),
    ...(input.slug ? { slug: input.slug } : {}),
    ...(input.base_version_id
      ? { base_version_id: input.base_version_id }
      : {}),
    content: {
      asset_type: content.asset_type,
      ...(content.version_note ? { version_note: content.version_note } : {}),
      files: content.files.map((f) => ({
        path: f.path,
        role: f.role,
        sha256: f.sha256,
        size: f.size,
      })),
      ...(content.uses !== undefined ? { uses: content.uses } : {}),
      ...(content.rights !== undefined ? { rights: content.rights } : {}),
      ...(content.metadata !== undefined ? { metadata: content.metadata } : {}),
      ...(content.producer !== undefined ? { producer: content.producer } : {}),
    },
    ...(input.describe !== undefined ? { describe: input.describe } : {}),
  };
  return JSON.stringify(ordered).replace(
    /[<>&\u2028\u2029]/g,
    (ch) => "\\u" + ch.charCodeAt(0).toString(16).padStart(4, "0"),
  );
}
export async function newPushState({
  input,
  origin,
}: {
  input: PushInput;
  origin?: string;
}): Promise<PushState> {
  validatePushInput(input);
  return {
    schema: "lantai.client-push/v1",
    origin: originOf(origin),
    request_hash: await sha256(pushJSON(input)),
    create_key: crypto.randomUUID(),
    commit_key: crypto.randomUUID(),
  };
}
export async function validatePushState(
  input: PushInput,
  state: PushState,
  origin?: string,
) {
  if (!object(state)) invalid("缺少原始恢复状态。");
  knownKeys(
    state,
    [
      "schema",
      "origin",
      "request_hash",
      "create_key",
      "commit_key",
      "upload_id",
      "operation_id",
      "committing",
      "result",
    ],
    "原始恢复状态",
  );
  validatePushInput(input);
  if (
    state.schema !== "lantai.client-push/v1" ||
    state.origin !== originOf(origin) ||
    !keyPattern.test(state.create_key) ||
    !keyPattern.test(state.commit_key) ||
    state.create_key === state.commit_key
  )
    invalid("恢复状态的服务器、schema 或原键不匹配。不要新建替代状态。");
  if (state.request_hash !== (await sha256(pushJSON(input))))
    invalid(
      "输入与原状态摘要不匹配；本页仅续办本浏览器生成的状态。原生 RawMessage 词法状态请交受控助手，不能改 hash 或换键重试。",
    );
  if (
    (state.upload_id && !identifierPattern.test(state.upload_id)) ||
    (state.operation_id && !identifierPattern.test(state.operation_id)) ||
    !!state.upload_id !== !!state.operation_id ||
    (state.committing && !state.upload_id)
  )
    invalid("原状态缺少完整 upload/operation 身份。");
  if (state.result) validateReceipt(state.result, input, state);
}
function validateReceipt(
  receipt: PushReceipt,
  input: PushInput,
  state: PushState,
) {
  if (!object(receipt)) invalid("提交回执必须为对象。");
  knownKeys(
    receipt,
    [
      "operation_id",
      "project_id",
      "asset_id",
      "version_id",
      "version_number",
      "manifest_digest",
      "committed_at",
      "uri",
      "description_pending",
      "description_error",
      "slug",
      "alias_generation",
      "ref",
      "description",
    ],
    "提交回执",
  );
  assertNoTransferSecrets(receipt);
  if (receipt.ref !== undefined) {
    if (!object(receipt.ref)) invalid("提交回执永久引用无效。");
    knownKeys(
      receipt.ref,
      ["instance_id", "asset_id", "version_id"],
      "提交永久引用",
    );
    if (
      receipt.ref.asset_id !== receipt.asset_id ||
      receipt.ref.version_id !== receipt.version_id ||
      (receipt.ref.instance_id !== undefined &&
        !identifierPattern.test(receipt.ref.instance_id)) ||
      (receipt.ref.instance_id &&
        receipt.uri !==
          `lantai://${receipt.ref.instance_id}/assets/${receipt.asset_id}/versions/${receipt.version_id}`)
    )
      invalid("提交回执永久引用与固定身份不匹配。");
  }
  if (receipt.slug !== undefined) {
    if (typeof receipt.slug !== "string") invalid("提交回执路径名无效。");
    validateManagedPath(receipt.slug);
    if (input.slug && receipt.slug !== input.slug)
      invalid("提交回执路径名与原请求不匹配。");
  }
  if (
    receipt.alias_generation !== undefined &&
    (!Number.isSafeInteger(receipt.alias_generation) ||
      receipt.alias_generation < 1)
  )
    invalid("提交回执别名代数无效。");
  if (receipt.description !== undefined) {
    if (!object(receipt.description)) invalid("提交著录回执无效。");
    knownKeys(
      receipt.description,
      [
        "asset_id",
        "project_id",
        "asset_type",
        "slug",
        "title",
        "summary",
        "tags",
        "subjects",
        "sensitivity",
        "defaults",
        "revision",
        "updated_by",
        "extra",
      ],
      "提交著录回执",
    );
    if (
      receipt.description.asset_id !== receipt.asset_id ||
      receipt.description.project_id !== receipt.project_id
    )
      invalid("提交著录回执与固定身份不匹配。");
  }
  if (
    (receipt.description_pending !== undefined &&
      typeof receipt.description_pending !== "boolean") ||
    (receipt.description_error !== undefined &&
      (typeof receipt.description_error !== "string" ||
        !/^[A-Z][A-Z0-9_]{0,79}$/.test(receipt.description_error)))
  )
    invalid("提交回执的著录状态或公开错误码无效。");
  if (
    !receipt ||
    !identifierPattern.test(receipt.asset_id) ||
    !identifierPattern.test(receipt.version_id) ||
    receipt.operation_id !== state.operation_id ||
    receipt.project_id !== input.project_id ||
    (input.asset_id && receipt.asset_id !== input.asset_id) ||
    !Number.isSafeInteger(receipt.version_number) ||
    receipt.version_number < 1 ||
    !/^sha256:[0-9a-f]{64}$/.test(receipt.manifest_digest) ||
    !Number.isFinite(Date.parse(receipt.committed_at)) ||
    !new RegExp(
      `^lantai://[0-7][0-9A-HJKMNP-TV-Z]{25}/assets/${receipt.asset_id}/versions/${receipt.version_id}$`,
    ).test(receipt.uri)
  )
    invalid("服务端提交回执与原操作/固定身份不匹配；保留原状态并对账。");
}
function request<T>(path: string, options: RequestInit = {}) {
  return api<T>(path, { ...options, redirect: "error" });
}
export function managedTransferURL(
  value: string,
  kind: "upload" | "read",
  origin?: string,
) {
  const base = originOf(origin);
  const url = new URL(value, base);
  const pattern =
    kind === "upload"
      ? /^\/xfer\/v1\/uploads\/[0-7][0-9A-HJKMNP-TV-Z]{25}\/files\/[0-9a-f]{64}\/parts\/[1-9][0-9]*$/
      : /^\/xfer\/v1\/reads\/[0-7][0-9A-HJKMNP-TV-Z]{25}$/;
  if (
    url.origin !== base ||
    url.username ||
    url.password ||
    url.hash ||
    !pattern.test(url.pathname) ||
    (kind === "upload" && url.search)
  )
    invalid("传输地址不是当前服务端的安全同源路径。");
  if (
    kind === "read" &&
    (Array.from(url.searchParams.keys()).some((key) => key !== "sig") ||
      url.searchParams.getAll("sig").length !== 1 ||
      !/^[A-Za-z0-9_-]{16,128}$/.test(url.searchParams.get("sig") ?? ""))
  )
    invalid("读取地址缺少有效签名或含未知参数。");
  return url.href;
}
async function transfer(url: string, options: RequestInit) {
  const headers = new Headers(options.headers);
  if (options.method && options.method !== "GET" && options.method !== "HEAD") {
    const csrf = csrfForRequest();
    if (!csrf) invalid("此标签页缺少 CSRF，请重新验证身份。");
    headers.set("X-CSRF-Token", csrf);
  }
  let response: Response;
  try {
    response = await fetch(url, {
      ...options,
      headers,
      credentials: "same-origin",
      cache: "no-store",
      redirect: "error",
    });
  } catch (error) {
    if (options.signal?.aborted) throw error;
    throw new APIError("NETWORK_ERROR", 0);
  }
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new APIError(body.error?.code || "SERVER_ERROR", response.status);
  }
  return response;
}
const activePushes = new Set<string>();
export async function pushFiles({
  input,
  files,
  state,
  checkpoint,
  signal,
  onProgress,
}: {
  input: PushInput;
  files: ManagedFile[];
  state: PushState;
  checkpoint: (state: PushState) => Promise<void>;
  signal?: AbortSignal;
  onProgress?: (progress: TransferProgress) => void;
}): Promise<PushReceipt> {
  if (typeof checkpoint !== "function") invalid("写入前必须保留原始恢复状态。");
  const prepared = await preparePushInput({ input, files, signal, onProgress });
  await validatePushState(prepared, state);
  const lock = state.origin + "/" + state.create_key;
  if (activePushes.has(lock)) invalid("此原操作已在当前标签页执行。");
  activePushes.add(lock);
  const save = async () => {
    await checkpoint(structuredClone(state));
    abort(signal);
  };
  try {
    await save();
    const body = { ...prepared };
    delete (body as Partial<PushInput>).project_id;
    if (state.result && !state.result.description_pending) return state.result;
    if (!state.committing) {
      const unique = Array.from(
        new Map(
          prepared.content.files.map((f) => [
            f.sha256,
            { sha256: f.sha256, size: f.size },
          ]),
        ).values(),
      );
      let upload: Upload;
      if (!state.upload_id) {
        upload = await request<Upload>("/api/v1/uploads", {
          method: "POST",
          headers: { "Idempotency-Key": state.create_key },
          body: JSON.stringify({
            project_id: prepared.project_id,
            files: unique,
          }),
          signal,
        });
        if (
          !identifierPattern.test(upload.upload_id) ||
          !identifierPattern.test(upload.operation_id) ||
          upload.project_id !== prepared.project_id
        )
          invalid("服务端缺少上传身份，保留原 create_key 后对账。");
        state.upload_id = upload.upload_id;
        state.operation_id = upload.operation_id;
        await save();
      } else
        upload = await request<Upload>(`/api/v1/uploads/${state.upload_id}`, {
          signal,
        });
      if (
        upload.upload_id !== state.upload_id ||
        upload.operation_id !== state.operation_id ||
        upload.project_id !== prepared.project_id ||
        !Array.isArray(upload.files) ||
        upload.files.length !== unique.length
      )
        invalid("上传状态不再匹配原请求。");
      const seen = new Set<string>();
      let completed = 0;
      const total = unique.reduce((n, f) => n + f.size, 0);
      for (const remote of upload.files) {
        abort(signal);
        const declared = prepared.content.files.find(
          (f) => f.sha256 === remote.sha256,
        );
        if (
          !declared ||
          seen.has(remote.sha256) ||
          remote.size !== declared.size
        )
          invalid("上传文件与冻结清单不匹配。");
        seen.add(remote.sha256);
        if (remote.state === "verified") {
          completed += remote.size;
          continue;
        }
        if (
          !Number.isSafeInteger(remote.part_size) ||
          remote.part_size < 1 ||
          !Number.isSafeInteger(remote.part_count) ||
          remote.part_count < 1 ||
          remote.part_count !==
            Math.ceil(Math.max(1, remote.size) / remote.part_size) ||
          !Array.isArray(remote.received_parts) ||
          remote.received_parts.some(
            (p) => !Number.isInteger(p) || p < 1 || p > remote.part_count,
          ) ||
          new Set(remote.received_parts).size !== remote.received_parts.length
        )
          invalid("服务端分片范围无效。");
        const local = files.find((f) => f.path === declared.path)!;
        for (let part = 1; part <= remote.part_count; part++) {
          abort(signal);
          const offset = (part - 1) * remote.part_size;
          const bytes = local.file.slice(
            offset,
            Math.min(offset + remote.part_size, remote.size),
          );
          if (!remote.received_parts.includes(part)) {
            if (remote.part_url_template.split("{part_number}").length !== 2)
              invalid("服务端上传模板无效。");
            const url = managedTransferURL(
              remote.part_url_template.replace("{part_number}", String(part)),
              "upload",
            );
            const parsed = new URL(url);
            if (
              !parsed.pathname.startsWith(
                `/xfer/v1/uploads/${state.upload_id}/files/${remote.sha256}/parts/`,
              )
            )
              invalid("分片地址属于其他上传。");
            await transfer(url, {
              method: "PUT",
              body: bytes,
              signal,
              headers: {
                "Content-Type": "application/octet-stream",
                "Lantai-Part-Sha256": await sha256(bytes),
              },
            });
          }
          completed += bytes.size;
          onProgress?.({
            phase: "uploading",
            path: declared.path,
            completed,
            total,
          });
        }
        await request(
          `/api/v1/uploads/${state.upload_id}/files/${remote.sha256}/complete`,
          { method: "POST", body: "{}", signal },
        );
      }
      onProgress?.({ phase: "uploaded", completed: total, total });
      state.committing = true;
      await save();
    } else if (state.operation_id) {
      const op = await request<{ operation_id: string }>(
        `/api/v1/operations/${state.operation_id}`,
        { signal },
      );
      if (op.operation_id !== state.operation_id) invalid("原操作身份不匹配。");
    }
    onProgress?.({ phase: "committing", completed: 0, total: 1 });
    const receipt = await request<PushReceipt>(
      `/api/v1/uploads/${state.upload_id}/commit`,
      {
        method: "POST",
        headers: { "Idempotency-Key": state.commit_key },
        body: JSON.stringify(body),
        signal,
      },
    );
    validateReceipt(receipt, prepared, state);
    if (
      state.result &&
      (state.result.asset_id !== receipt.asset_id ||
        state.result.version_id !== receipt.version_id)
    )
      invalid("重放改变了固定版本，保留状态并停止。");
    state.result = receipt;
    await save();
    onProgress?.({ phase: "committed", completed: 1, total: 1 });
    return receipt;
  } finally {
    activePushes.delete(lock);
  }
}

export async function getExactManagedVersion({
  assetID,
  versionID,
  signal,
}: {
  assetID: string;
  versionID: string;
  signal?: AbortSignal;
}): Promise<ExactManagedVersion> {
  if (!identifierPattern.test(assetID) || !identifierPattern.test(versionID))
    invalid("必须指定规范的固定资产与版本 ID。");
  const meta = await request<{ api_version: string; instance_id: string }>(
    "/api/v1/meta",
    { signal },
  );
  if (meta.api_version !== "v1" || !identifierPattern.test(meta.instance_id))
    invalid("当前实例 metadata 无效。");
  const exact = await request<ExactManagedVersion>(
    `/api/v1/assets/${assetID}/versions/${versionID}?view=full`,
    { signal },
  );
  const m = exact?.manifest;
  const v = exact?.version;
  if (
    !m ||
    !v ||
    m.contract !== "lantai.manifest/v1" ||
    m.instance_id !== meta.instance_id ||
    m.asset_id !== assetID ||
    m.version_id !== versionID ||
    v.asset_id !== assetID ||
    v.version_id !== versionID ||
    m.project_id !== v.project_id ||
    !identifierPattern.test(v.project_id) ||
    m.version_number !== v.version_number ||
    !Number.isSafeInteger(v.version_number) ||
    v.version_number < 1 ||
    m.manifest_digest !== v.manifest_digest ||
    v.uri !==
      `lantai://${meta.instance_id}/assets/${assetID}/versions/${versionID}` ||
    !Number.isFinite(Date.parse(v.committed_at)) ||
    !object(m.content)
  )
    invalid("固定清单与当前实例、版本或永久引用不匹配。");
  validateManagedFiles(m.content.files);
  if (`sha256:${await sha256(canonicalJSON(m.content))}` !== m.manifest_digest)
    invalid("冻结清单摘要不一致。");
  return exact;
}
export async function readManagedFile({
  assetID,
  versionID,
  file,
  purpose = "archive_review",
  signal,
}: {
  assetID: string;
  versionID: string;
  file: InputFile;
  purpose?: string;
  signal?: AbortSignal;
}): Promise<Uint8Array> {
  if (!identifierPattern.test(assetID) || !identifierPattern.test(versionID))
    invalid("读取必须绑定规范的固定资产与版本 ID。");
  validateManagedFiles([file]);
  if (
    ![
      "archive_review",
      "reference",
      "production",
      "generative_input",
      "raw_export",
    ].includes(purpose)
  )
    invalid("读取用途无效。");
  const grant = await request<{
    grant_id: string;
    asset_id: string;
    version_id: string;
    path: string;
    sha256: string;
    size: number;
    purpose: string;
    method: string;
    expires_at: string;
    url: string;
  }>(`/api/v1/assets/${assetID}/versions/${versionID}/read-grants`, {
    method: "POST",
    body: JSON.stringify({ path: file.path, purpose }),
    signal,
  });
  if (
    !identifierPattern.test(grant.grant_id) ||
    grant.asset_id !== assetID ||
    grant.version_id !== versionID ||
    grant.path !== file.path ||
    grant.sha256 !== file.sha256 ||
    grant.size !== file.size ||
    grant.purpose !== purpose ||
    grant.method !== "GET" ||
    !Number.isFinite(Date.parse(grant.expires_at)) ||
    Date.parse(grant.expires_at) <= Date.now()
  )
    invalid("读取授权不匹配固定文件。");
  const url = managedTransferURL(grant.url, "read");
  if (new URL(url).pathname !== `/xfer/v1/reads/${grant.grant_id}`)
    invalid("读取地址与授权 ID 不符。");
  const response = await transfer(url, { method: "GET", signal });
  if (
    response.status !== 200 ||
    (response.headers.get("Content-Length") &&
      Number(response.headers.get("Content-Length")) !== file.size)
  )
    invalid("下载长度或响应范围不匹配。");
  if (!response.body) invalid("文件响应缺少内容流。");
  const output = new Uint8Array(file.size);
  const reader = response.body.getReader();
  let offset = 0;
  try {
    for (;;) {
      abort(signal);
      const part = await reader.read();
      if (part.done) break;
      if (offset + part.value.length > file.size)
        invalid("下载超过冻结文件大小，已停止。");
      output.set(part.value, offset);
      offset += part.value.length;
    }
  } finally {
    await reader.cancel();
    reader.releaseLock();
  }
  if (offset !== file.size || (await sha256(output)) !== file.sha256)
    invalid("下载文件大小或完整 SHA-256 不一致。");
  return output;
}
export type ExactVerificationBinding = {
  instance_id: string;
  project_id: string;
  manifest_digest: string;
  asset_id?: string;
  version_id?: string;
  version_number?: number;
  uri?: string;
};
export async function verifyExactFiles({
  assetID,
  versionID,
  expectedFiles,
  expectedIdentity,
  localFiles,
  purpose = "archive_review",
  signal,
  onProgress,
}: {
  assetID: string;
  versionID: string;
  expectedFiles?: InputFile[];
  expectedIdentity?: ExactVerificationBinding;
  localFiles?: ManagedFile[];
  purpose?: string;
  signal?: AbortSignal;
  onProgress?: (progress: TransferProgress) => void;
}): Promise<VerificationReceipt> {
  const exact = await getExactManagedVersion({ assetID, versionID, signal });
  if (
    expectedIdentity &&
    (exact.manifest.instance_id !== expectedIdentity.instance_id ||
      exact.version.project_id !== expectedIdentity.project_id ||
      exact.manifest.manifest_digest !== expectedIdentity.manifest_digest ||
      (expectedIdentity.asset_id !== undefined &&
        exact.version.asset_id !== expectedIdentity.asset_id) ||
      (expectedIdentity.version_id !== undefined &&
        exact.version.version_id !== expectedIdentity.version_id) ||
      (expectedIdentity.version_number !== undefined &&
        exact.version.version_number !== expectedIdentity.version_number) ||
      (expectedIdentity.uri !== undefined &&
        exact.version.uri !== expectedIdentity.uri))
  )
    invalid(
      "当前固定版本与原工作区实例、项目或提交清单身份不匹配；不能报告当前通过。",
    );
  const files = exact.manifest.content.files;
  if (
    expectedFiles &&
    canonicalJSON(
      [...expectedFiles].sort((a, b) => a.path.localeCompare(b.path)),
    ) !== canonicalJSON([...files].sort((a, b) => a.path.localeCompare(b.path)))
  )
    invalid("固定版本文件集合与冻结计划不同。");
  if (
    localFiles &&
    (localFiles.length !== files.length ||
      localFiles.some(
        (f) =>
          !files.some((item) => item.path === f.path && item.role === f.role),
      ))
  )
    invalid("本机源文件集合与固定版本不同。");
  for (const [index, file] of files.entries()) {
    await readManagedFile({ assetID, versionID, file, purpose, signal });
    const local = localFiles?.find((f) => f.path === file.path);
    if (
      local &&
      (local.file.size !== file.size ||
        (await sha256(local.file)) !== file.sha256)
    )
      invalid("当前本机源与固定版本字节不同。");
    onProgress?.({
      phase: "verifying",
      path: file.path,
      completed: index + 1,
      total: files.length,
    });
  }
  abort(signal);
  return {
    schema: "lantai.browser-verification/v1",
    origin: originOf(),
    instance_id: exact.manifest.instance_id,
    asset_id: assetID,
    version_id: versionID,
    manifest_digest: exact.manifest.manifest_digest,
    verified_at: new Date().toISOString(),
    completion_basis: "current_remote_and_downloaded_files",
    current_remote_verified: true,
    local_source_verified: !!localFiles,
    files,
  };
}

export function downloadManagedJSON(name: string, value: unknown) {
  const blob = new Blob([JSON.stringify(value, null, 2) + "\n"], {
    type: "application/json",
  });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
