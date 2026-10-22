// Views of the existing REST contract in api/openapi.yaml. No secret/token
// field is accepted as application authentication state.
export interface Session {
  session_id: string;
  channel: string;
  session_kind: string;
  expires_at: string;
  scopes: string[];
  delegated_by?: string;
}
export interface Identity {
  principal: {
    principal_id?: string;
    name: string;
    display_name?: string;
    kind?: string;
  };
  session: Session;
  system_roles?: string[];
  project_roles?: Record<string, string[]>;
}
export interface Project {
  project_id: string;
  key: string;
  state: string;
  description: {
    name: string;
    summary?: string;
    project_type: string;
    revision: number;
  };
  description_pending?: boolean;
}
export interface Item {
  asset_id: string;
  project_id: string;
  version_id: string;
  version_number: number;
  title: string;
  slug: string;
  summary?: string;
  asset_type: string;
  tags: string[];
  subjects: string[];
}
export interface Version {
  version_id: string;
  version_number: number;
  asset_id: string;
  project_id: string;
  committed_at: string;
  uri: string;
  manifest_digest: string;
}
export interface Asset {
  asset_id: string;
  project_id: string;
  asset_type: string;
  description: {
    title: string;
    slug: string;
    summary?: string;
    tags: string[];
    subjects: string[];
    revision: number;
    sensitivity?: "normal" | "personal";
    defaults?: { usage: string; license: string };
    extra?: Record<string, unknown>;
  };
  latest: Version;
}
export interface ExactVersion {
  version: Version;
  manifest?: {
    content: {
      version_note?: string;
      files: { path: string; size: number; role: string; sha256: string }[];
    };
  };
}
export interface Page<T> {
  items: T[] | null;
  next_cursor?: string;
}

export class APIError extends Error {
  constructor(
    public code: string,
    public status: number,
    public requestID?: string,
    public reason?: string,
  ) {
    super(code);
  }
}

// This is a CSRF value, never the HttpOnly session credential. It is scoped to
// this tab and bound to the server's session ID. No password, TOTP, bearer token,
// principal data, or response cache is persisted by the application.
const csrfKey = "lantai.csrf.v1";
let csrf: { session: string; value: string } | null = null;
export function restoreCSRF(sessionID: string) {
  if (csrf?.session === sessionID) return;
  try {
    const saved = JSON.parse(sessionStorage.getItem(csrfKey) || "null");
    csrf =
      saved?.session === sessionID && typeof saved.value === "string"
        ? saved
        : null;
    if (!csrf) sessionStorage.removeItem(csrfKey);
  } catch {
    csrf = null;
  }
}
export function rememberCSRF(sessionID: string, value: string) {
  csrf = { session: sessionID, value };
  try {
    sessionStorage.setItem(csrfKey, JSON.stringify(csrf));
  } catch {
    /* in-memory mode */
  }
}
export function clearCSRF() {
  csrf = null;
  try {
    sessionStorage.removeItem(csrfKey);
  } catch {
    /* disabled storage */
  }
}
export function hasCSRF() {
  return !!csrf;
}
// Used only after the transfer adapter verifies the exact same-origin /xfer
// endpoint. This is the existing per-tab CSRF proof, never a session token.
export function csrfForRequest(): string | null {
  return csrf?.value ?? null;
}
export const isSessionError = (e: unknown) =>
  e instanceof APIError &&
  ["AUTH_REQUIRED", "TOKEN_EXPIRED", "TOKEN_REVOKED"].includes(e.code);

export async function api<T>(
  path: string,
  options: RequestInit = {},
  csrfOverride?: string | null,
): Promise<T> {
  // Only the fixed same-origin API can receive this tab's cookies/CSRF value.
  if (!path.startsWith("/api/v1/")) throw new Error("Invalid API path");
  const headers = new Headers(options.headers);
  if (options.body) headers.set("Content-Type", "application/json");
  if (options.method && !["GET", "HEAD"].includes(options.method)) {
    const value = csrfOverride === undefined ? csrf?.value : csrfOverride;
    if (value) headers.set("X-CSRF-Token", value);
    else headers.delete("X-CSRF-Token");
  }
  let response: Response;
  try {
    response = await fetch(path, {
      ...options,
      headers,
      credentials: "same-origin",
      cache: "no-store",
      redirect: "error",
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw new APIError("NETWORK_ERROR", 0);
  }
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new APIError(
      body.error?.code || "SERVER_ERROR",
      response.status,
      response.headers.get("X-Request-Id") || undefined,
      typeof body.error?.details?.[0]?.reason === "string"
        ? body.error.details[0].reason
        : undefined,
    );
  }
  if (response.status === 204) return undefined as T;
  try {
    return (await response.json()) as T;
  } catch {
    throw new APIError("INVALID_RESPONSE", response.status);
  }
}

export const typeNames: Record<string, string> = {
  image: "图像",
  video: "视频",
  audio: "音频",
  doc: "文档",
  config: "配置",
  model: "模型",
  motion: "动作",
  plugin: "插件",
  scene: "场景",
  production: "制作成果",
};
export function message(e: unknown) {
  const code = e instanceof APIError ? e.code : "UNKNOWN";
  const messages: Record<string, string> = {
    AUTH_REQUIRED: "请重新登录后继续。",
    TOKEN_EXPIRED: "会话已过期，请重新登录。",
    TOKEN_REVOKED: "会话已失效，请重新登录。",
    FORBIDDEN: "当前账号没有访问权限。",
    USE_RESTRICTED: "此文件不允许用于当前读取用途。",
    RIGHTS_PENDING: "此文件的权利核验尚未完成，暂时不能读取。",
    NOT_FOUND: "内容不存在，或你没有查看权限。",
    CURSOR_EXPIRED: "列表已变化，请刷新后从第一页浏览。",
    RATE_LIMITED: "尝试过于频繁，请稍后再试。",
    TOTP_REPLAY: "这个动态码已使用，请等待验证器显示下一个动态码。",
    NETWORK_ERROR: "无法连接服务，请检查网络后重试。",
    SERVER_ERROR: "服务暂时不可用，请稍后重试。",
    INVALID_RESPONSE: "服务返回了无法识别的内容，请稍后重试。",
    HUMAN_PROOF_REQUIRED: "此操作需要重新验证，请关闭确认框后重新发起。",
    HUMAN_GRANT_MISMATCH: "验证内容与操作不一致，请重新发起操作。",
    HUMAN_GRANT_EXPIRED: "此次操作验证已过期，请重新发起。",
    INVALID_STATE_TRANSITION: "当前状态不允许此操作，请刷新查看最新任务。",
    PRECONDITION_FAILED: "设置已被其他管理员更新，请刷新后重试。",
    IDEMPOTENCY_CONFLICT: "请求内容发生变化，请刷新并重新确认操作。",
    STORAGE_UNAVAILABLE: "备份存储暂不可用，请检查目录配置和权限后重试。",
    SCHEMA_INVALID: "输入格式不正确，请检查时间、时区和其他设置。",
    BACKUP_TIMEOUT_INVALID: "排空等待上限不能大于整个任务超时。",
    MAINTENANCE: "备份维护中，业务操作暂时暂停。备份管理仍可查看和取消。",
    MAINTENANCE_MODE: "当前无法执行此操作，请检查维护状态后重试。",
  };
  return messages[code] || "请求未完成，请检查输入或稍后重试。";
}
