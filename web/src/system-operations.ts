// Thin browser orchestration of existing core commands. This module has no
// storage, clock renewal, role grants of its own, or replacement operation keys.
export type Transport = <T>(path: string, options?: RequestInit) => Promise<T>;
export type CommandBody = Record<string, unknown>;
export type IdentityAction =
  | "identity.register_principal"
  | "identity.issue_credential"
  | "identity.revoke_credential"
  | "identity.grant_project_role"
  | "identity.revoke_project_role";
export interface IdentityIntent {
  mode: "identity";
  title: string;
  action: IdentityAction;
  command: CommandBody;
}
export interface DomainIntent {
  mode: "domain";
  title: string;
  kind:
    | "review"
    | "trash"
    | "force_trash"
    | "trash_mutation"
    | "control"
    | "rights";
  request: CommandBody;
}
export type SensitiveIntent = IdentityIntent | DomainIntent;
export interface Challenge {
  challenge_id: string;
  operation_id: string;
  action: string;
  summary: string;
  targets: { kind: string; id: string; expected_revision: number }[];
  expires_at: string;
  request_hash?: string;
}
export interface HumanAction {
  action: string;
  project_id?: string;
  resource_id: string;
  resource_revision: number;
  request: CommandBody;
  manifest_digest?: string;
}
export interface Approval {
  intent: SensitiveIntent;
  challenge: Challenge;
  key: string;
  actions?: HumanAction[];
  grantID?: string;
  childOperationID?: string;
  executionAttempted?: boolean;
}
export interface SensitiveResult {
  operation_id?: string;
  replayed?: boolean;
  result?: unknown;
  secret?: string;
  [key: string]: unknown;
}

export const roles = [
  "owner",
  "coordinator",
  "contributor",
  "curator",
  "checker",
  "reviewer",
  "viewer",
] as const;
export const kindScopes: Record<string, string[]> = {
  human: ["self", "read", "ingest", "organize", "task", "admin"],
  agent: ["self", "read", "ingest", "organize", "task"],
  node: ["self", "node"],
  worker: ["self", "worker"],
  runner: ["self", "runner"],
  service: ["self", "service", "read"],
};
const namePatterns: Record<string, RegExp> = {
  human: /^[a-z][a-z0-9_-]{0,31}$/,
  agent: /^[a-z][a-z0-9-]{0,31}@[a-z0-9][a-z0-9-]{0,62}$/,
  node: /^node:[a-z0-9][a-z0-9-]{0,62}$/,
  worker: /^worker:[a-z][a-z0-9-]{0,31}@[a-z0-9][a-z0-9-]{0,62}$/,
  runner: /^runner:[a-z][a-z0-9-]{0,31}@[a-z0-9][a-z0-9-]{0,62}$/,
  service: /^service:[a-z][a-z0-9-]{0,62}$/,
};
export function identifier(value: string): string {
  if (!/^[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(value))
    throw new Error("请填写完整、规范的对象标识。");
  return value;
}
export function revision(value: string | number, minimum = 1): number {
  if (typeof value === "string" && !/^(0|[1-9]\d*)$/.test(value))
    throw new Error("修订号必须是整数。");
  const n = Number(value);
  if (!Number.isSafeInteger(n) || n < minimum)
    throw new Error(`修订号必须不小于 ${minimum}。`);
  return n;
}
export function reason(value: string): string {
  const out = value.trim();
  if (!out || out.length > 4096)
    throw new Error("请填写不超过 4096 字符的操作理由。");
  return out;
}
function freeze<T>(value: T): T {
  const copied = JSON.parse(JSON.stringify(value)) as T;
  function lock(v: unknown) {
    if (v && typeof v === "object") {
      Object.values(v).forEach(lock);
      Object.freeze(v);
    }
  }
  lock(copied);
  return copied;
}
function stable(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stable).join(",")}]`;
  if (value && typeof value === "object")
    return `{${Object.entries(value)
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([k, v]) => `${JSON.stringify(k)}:${stable(v)}`)
      .join(",")}}`;
  return JSON.stringify(value);
}
export function roleIntent(
  projectID: string,
  expectedRevision: number,
  principalID: string,
  role: string,
  grant: boolean,
): IdentityIntent {
  if (!roles.some((r) => r === role))
    throw new Error("请选择已登记的项目角色。");
  return freeze({
    mode: "identity",
    title: grant ? "授予项目角色" : "撤销项目角色",
    action: grant
      ? "identity.grant_project_role"
      : "identity.revoke_project_role",
    command: {
      project_id: identifier(projectID),
      expected_revision: revision(expectedRevision, 0),
      principal_id: identifier(principalID),
      role,
      grant,
    },
  });
}
export function principalIntent(
  kind: string,
  name: string,
  displayName: string,
  profile: CommandBody = {},
): IdentityIntent {
  if (!namePatterns[kind]?.test(name))
    throw new Error("主体名称不符合所选类别的命名规则。");
  if (displayName.length > 128) throw new Error("显示名称不能超过 128 字符。");
  if (kind === "human" && Object.keys(profile).length)
    throw new Error("人的账户不能使用 Agent 档案。");
  return freeze({
    mode: "identity",
    title: "登记主体",
    action: "identity.register_principal",
    command: { kind, name, display_name: displayName, profile },
  });
}
export function credentialIntent(input: {
  principalID: string;
  expectedRevision: number;
  kind: string;
  scopes: string[];
  projects: string[];
  ttl: number;
  label: string;
}): IdentityIntent {
  const scopes = [...new Set(input.scopes)].sort();
  if (
    !scopes.length ||
    scopes.some((s) => !kindScopes[input.kind]?.includes(s))
  )
    throw new Error("所选 scope 不适用于该主体类别。");
  if (!Number.isSafeInteger(input.ttl) || input.ttl < 0)
    throw new Error("凭据有效秒数必须是非负整数。");
  if (input.label.length > 128) throw new Error("标签过长。");
  return freeze({
    mode: "identity",
    title: "签发长期凭据",
    action: "identity.issue_credential",
    command: {
      principal_id: identifier(input.principalID),
      expected_revision: revision(input.expectedRevision),
      scopes,
      projects: [...new Set(input.projects.map(identifier))].sort(),
      ttl_seconds: input.ttl,
      label: input.label,
    },
  });
}
export function revokeCredentialIntent(
  credentialID: string,
  note: string,
): IdentityIntent {
  return freeze({
    mode: "identity",
    title: "吊销凭据",
    action: "identity.revoke_credential",
    command: { credential_id: identifier(credentialID), reason: reason(note) },
  });
}
function challenge(value: Challenge): Challenge {
  identifier(value.challenge_id);
  identifier(value.operation_id);
  if (
    typeof value.summary !== "string" ||
    !value.summary ||
    typeof value.action !== "string" ||
    !Array.isArray(value.targets) ||
    value.targets.length < 1 ||
    value.targets.length > 1000 ||
    !Number.isFinite(Date.parse(value.expires_at))
  )
    throw new Error("服务器挑战不完整，未请求动态码。");
  for (const t of value.targets)
    if (
      typeof t.kind !== "string" ||
      typeof t.id !== "string" ||
      !t.id ||
      !Number.isSafeInteger(t.expected_revision) ||
      t.expected_revision < 0
    )
      throw new Error("服务器目标集合不完整，未请求动态码。");
  return freeze(value);
}
export async function prepareApproval(
  intent: SensitiveIntent,
  request: Transport,
  previous?: Approval,
): Promise<Approval> {
  const frozen = freeze(intent);
  if (previous && stable(previous.intent) !== stable(frozen))
    throw new Error("原操作结果未明确，不能替换已冻结的请求。");
  let proof: Challenge;
  let actions: HumanAction[] | undefined;
  if (frozen.mode === "identity") {
    proof = challenge(
      await request<Challenge>("/api/v1/identity/challenges", {
        method: "POST",
        body: JSON.stringify({
          action: frozen.action,
          command: frozen.command,
          ...(previous
            ? { operation_id: previous.challenge.operation_id }
            : {}),
        }),
      }),
    );
    if (proof.action !== frozen.action)
      throw new Error("服务器挑战动作不符，未请求动态码。");
  } else if (previous) {
    proof = challenge(
      await request<Challenge>("/api/v1/human/rechallenge", {
        method: "POST",
        body: JSON.stringify({ operation_id: previous.challenge.operation_id }),
      }),
    );
    actions = previous.actions;
  } else {
    const response = await request<{
      challenge: Challenge;
      items: HumanAction[];
    }>("/api/v1/human/prepare", {
      method: "POST",
      body: JSON.stringify({
        items: [{ kind: frozen.kind, request: frozen.request }],
      }),
    });
    proof = challenge(response.challenge);
    if (!Array.isArray(response.items) || response.items.length !== 1)
      throw new Error("服务器未返回精确的单项操作，未请求动态码。");
    actions = freeze(response.items);
  }
  if (previous && proof.operation_id !== previous.challenge.operation_id)
    throw new Error("重新验证更换了原操作，已停止。");
  return {
    intent: frozen,
    challenge: proof,
    actions,
    key: previous?.key ?? crypto.randomUUID(),
    childOperationID: previous?.childOperationID,
    executionAttempted: previous?.executionAttempted,
  };
}
export async function executeApproval(
  approval: Approval,
  code: string,
  request: Transport,
  retain: (approval: Approval) => void,
): Promise<SensitiveResult> {
  let next = approval;
  if (!next.grantID) {
    if (!/^\d{6}$/.test(code))
      throw new Error("请输入验证器当前的六位动态码。");
    const grant = await request<{
      grant_id: string;
      operation_id: string;
      action: string;
    }>(
      `/api/v1/identity/challenges/${encodeURIComponent(next.challenge.challenge_id)}/verify`,
      { method: "POST", body: JSON.stringify({ code }) },
    );
    identifier(grant.grant_id);
    if (
      grant.operation_id !== next.challenge.operation_id ||
      grant.action !== next.challenge.action
    )
      throw new Error("动态码证明与冻结操作不符，已停止。");
    next = { ...next, grantID: grant.grant_id };
    retain(next);
  }
  if (next.intent.mode === "domain") {
    const items = await request<
      { operation_id: string; action: HumanAction }[]
    >(`/api/v1/human/grants/${encodeURIComponent(next.grantID!)}`);
    if (
      !Array.isArray(items) ||
      items.length !== 1 ||
      stable(items[0].action) !== stable(next.actions?.[0])
    )
      throw new Error("授权中的冻结目标已变化，未执行。");
    identifier(items[0].operation_id);
    if (
      next.childOperationID &&
      next.childOperationID !== items[0].operation_id
    )
      throw new Error("原子操作不符，已停止。");
    next = { ...next, childOperationID: items[0].operation_id };
    retain(next);
  }
  next = { ...next, executionAttempted: true };
  retain(next); // Keep the same grant/key/child BEFORE sending any mutation.
  return request<SensitiveResult>(
    next.intent.mode === "identity"
      ? "/api/v1/identity/commands"
      : "/api/v1/human/execute",
    {
      method: "POST",
      headers: { "Idempotency-Key": next.key },
      body: JSON.stringify(
        next.intent.mode === "identity"
          ? {
              action: next.intent.action,
              command: next.intent.command,
              grant_id: next.grantID,
            }
          : { grant_id: next.grantID, operation_id: next.childOperationID },
      ),
    },
  );
}

// Results can include a one-time secret. Never put it in a generic receipt,
// export, console message, error dump, or business record.
export function publicReceipt(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(publicReceipt);
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value)
        .filter(
          ([k]) =>
            !/^(secret|token|password|code|csrf_token|uri|totp_secret)$/i.test(
              k,
            ),
        )
        .map(([k, v]) => [k, publicReceipt(v)]),
    );
  return value;
}

export function operationDiagnostic(approval: Approval, origin: string) {
  return {
    schema: "lantai.admin-operation/v1",
    origin,
    intent: approval.intent,
    challenge: approval.challenge,
    key: approval.key,
    actions: approval.actions,
    child_operation_id: approval.childOperationID,
    execution_attempted: !!approval.executionAttempted,
  };
}
export function restoreOperationDiagnostic(
  value: unknown,
  origin: string,
): Approval {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("诊断文件必须是对象。");
  const v = value as Record<string, unknown>;
  const allowed = [
    "schema",
    "origin",
    "intent",
    "challenge",
    "key",
    "actions",
    "child_operation_id",
    "execution_attempted",
  ];
  if (
    Object.keys(v).some((k) => !allowed.includes(k)) ||
    v.schema !== "lantai.admin-operation/v1" ||
    v.origin !== origin ||
    typeof v.key !== "string" ||
    !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(v.key) ||
    typeof v.execution_attempted !== "boolean"
  )
    throw new Error("诊断文件不属于当前服务或不符合原操作格式。");
  const intent = v.intent as SensitiveIntent;
  if (!intent || typeof intent.title !== "string" || intent.title.length > 256)
    throw new Error("诊断缺少冻结操作。");
  if (intent.mode === "identity") {
    if (
      ![
        "identity.register_principal",
        "identity.issue_credential",
        "identity.revoke_credential",
        "identity.grant_project_role",
        "identity.revoke_project_role",
      ].includes(intent.action) ||
      !intent.command ||
      typeof intent.command !== "object"
    )
      throw new Error("诊断不是支持的身份操作。");
  } else if (intent.mode === "domain") {
    if (
      !["review", "trash", "force_trash", "trash_mutation", "control"].includes(
        intent.kind,
      ) ||
      !intent.request ||
      typeof intent.request !== "object" ||
      !Array.isArray(v.actions) ||
      v.actions.length !== 1
    )
      throw new Error("诊断不是支持的人审操作。");
  } else throw new Error("诊断操作类别不受支持。");
  const proof = challenge(v.challenge as Challenge);
  if (intent.mode === "identity" && proof.action !== intent.action)
    throw new Error("诊断动作与挑战不一致。");
  if (v.child_operation_id !== undefined)
    identifier(String(v.child_operation_id));
  return {
    intent: freeze(intent),
    challenge: proof,
    key: v.key,
    actions: v.actions ? freeze(v.actions as HumanAction[]) : undefined,
    childOperationID: v.child_operation_id as string | undefined,
    executionAttempted: v.execution_attempted,
  }; // A diagnostic never restores a HumanGrant; reprove the original operation.
}

export interface CurrentObject {
  ref: { project_id: string; kind: string; id: string };
  revision: number;
  title: string;
  state: string;
}
export async function currentObjects(
  projectID: string,
  request: Transport,
  signal?: AbortSignal,
): Promise<{ objects: CurrentObject[]; replayAfter: number }> {
  identifier(projectID);
  const objects: CurrentObject[] = [];
  const seen = new Set<string>();
  const cursors = new Set<string>();
  let cursor = "";
  let replayAfter: number | undefined;
  for (let pages = 0; pages < 100; pages++) {
    const q = new URLSearchParams({ project_id: projectID, limit: "100" });
    if (cursor) q.set("cursor", cursor);
    const page = await request<{
      objects: CurrentObject[];
      replay_after: number;
      next_cursor?: string;
      replace: boolean;
    }>(`/api/v1/resync?${q}`, { signal });
    if (
      !page.replace ||
      !Array.isArray(page.objects) ||
      !Number.isSafeInteger(page.replay_after) ||
      page.replay_after < 0 ||
      (replayAfter !== undefined && replayAfter !== page.replay_after)
    )
      throw new Error("当前对象列表协议不一致，请重新读取。");
    replayAfter = page.replay_after;
    for (const object of page.objects) {
      identifier(object.ref.id);
      if (
        object.ref.project_id !== projectID ||
        !Number.isSafeInteger(object.revision)
      )
        throw new Error("当前对象列表混入其他项目，已丢弃读取结果。");
      const key = `${object.ref.kind}:${object.ref.id}`;
      if (seen.has(key)) throw new Error("当前对象重复，已丢弃读取结果。");
      seen.add(key);
      objects.push(object);
    }
    if (!page.next_cursor) return { objects, replayAfter };
    if (cursors.has(page.next_cursor))
      throw new Error("列表游标循环，已停止读取。");
    cursor = page.next_cursor;
    cursors.add(cursor);
  }
  throw new Error("对象清单超过 100 页预算，未返回不完整范围。");
}
