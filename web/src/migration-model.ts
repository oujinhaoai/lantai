import {
  assertNoTransferSecrets,
  browserBatchLimit,
  browserFileCountLimit,
  browserFileLimit,
  canonicalJSON,
  identifierPattern,
  newPushState,
  preparePushInput,
  sha256,
  validateManagedPath,
  validatePushInput,
  validatePushState,
  type ManagedFile,
  type PushInput,
  type PushState,
  type TransferProgress,
} from "./managed-transfer";

export type InventoryFile = { path: string; size: number };
export type MigrationAsset = {
  key: string;
  directory: string;
  classification: "final" | "intermediate" | "unknown";
  push: PushInput;
};
export type SourceDisposition = { path: string; reason: string };
export type MigrationDraft = {
  schema: "lantai.browser-migration-plan/v1";
  name: string;
  source_alias: string;
  project_id: string;
  assets: MigrationAsset[];
  excluded: SourceDisposition[];
  working_inputs: SourceDisposition[];
  capacity?: {
    existing_library_bytes?: number;
    free_main_bytes?: number;
    free_backup_bytes?: number;
    free_restore_bytes?: number;
    free_verification_bytes?: number;
  };
};
export type MigrationRow = {
  status:
    | "not_attempted"
    | "running"
    | "committed"
    | "description_pending"
    | "failed"
    | "unknown";
  error_code?: string;
};
export type MigrationWorkspace = {
  schema: "lantai.browser-migration-state/v1";
  state_format: "lantai.browser-push-json/v1";
  origin: string;
  instance_id: string;
  batch_id: string;
  created_at: string;
  plan_digest: string;
  plan: MigrationDraft;
  inventory: InventoryFile[];
  states: Record<string, PushState>;
  rows: Record<string, MigrationRow>;
};
export type MigrationPreflight = {
  selected_files: number;
  logical_bytes: number;
  uncovered: InventoryFile[];
  excluded: InventoryFile[];
  working_inputs: InventoryFile[];
  browser_eligible: boolean;
  budgets: {
    main: number;
    backup: number;
    restore: number;
    verification: number;
  };
  capacity_failures: string[];
  capacity_unknown: string[];
};
function fail(message: string): never {
  throw new Error(message);
}
function record(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value);
}
function keys(value: object, allowed: string[], label: string) {
  if (Object.keys(value).some((key) => !allowed.includes(key)))
    fail(`${label} 含未知字段。`);
}
function directory(path: string) {
  if (path !== ".") validateManagedPath(path);
}
function fullPath(asset: MigrationAsset, path: string) {
  return asset.directory === "." ? path : `${asset.directory}/${path}`;
}
const covers = (prefix: string, path: string) =>
  path === prefix || path.startsWith(prefix + "/");
export function blankMigrationDraft(projectID = ""): MigrationDraft {
  return {
    schema: "lantai.browser-migration-plan/v1",
    name: "新迁移批次",
    source_alias: "明确选中的本机来源",
    project_id: projectID,
    assets: [],
    excluded: [],
    working_inputs: [],
  };
}

export function validateMigrationDraft(value: unknown): MigrationDraft {
  assertNoTransferSecrets(value);
  if (!record(value)) fail("迁移计划不是对象。");
  keys(
    value,
    [
      "schema",
      "name",
      "source_alias",
      "project_id",
      "assets",
      "excluded",
      "working_inputs",
      "capacity",
    ],
    "迁移计划",
  );
  if (
    value.schema !== "lantai.browser-migration-plan/v1" ||
    typeof value.name !== "string" ||
    !value.name.trim() ||
    value.name.length > 200 ||
    typeof value.source_alias !== "string" ||
    !value.source_alias.trim() ||
    value.source_alias.length > 200 ||
    typeof value.project_id !== "string" ||
    !identifierPattern.test(value.project_id) ||
    !Array.isArray(value.assets) ||
    !value.assets.length ||
    value.assets.length > 1000
  )
    fail("迁移计划缺少有效名称、来源别名、项目 ID 或明确素材分组。");
  const plan = structuredClone(value) as unknown as MigrationDraft;
  const groupNames = new Set<string>();
  let count = 0;
  for (const asset of plan.assets) {
    if (!record(asset)) fail("素材分组无效。");
    keys(asset, ["key", "directory", "classification", "push"], "素材分组");
    if (
      typeof asset.key !== "string" ||
      typeof asset.directory !== "string" ||
      !["final", "intermediate", "unknown"].includes(asset.classification)
    )
      fail("素材分组缺少 key、相对目录或明确分类。");
    validateManagedPath(asset.key);
    if (
      ["__proto__", "constructor", "prototype"].includes(
        asset.key.toLowerCase(),
      )
    )
      fail("分组 key 不能使用 JavaScript 保留属性名。");
    if (
      asset.key.includes("/") ||
      asset.key.startsWith(".") ||
      asset.key.length > 100 ||
      groupNames.has(asset.key.toLowerCase())
    )
      fail("分组 key 必须为唯一安全名称。");
    groupNames.add(asset.key.toLowerCase());
    directory(asset.directory);
    if (
      !record(asset.push) ||
      !record(asset.push.content) ||
      asset.push.project_id !== plan.project_id ||
      !Array.isArray(asset.push.content.files) ||
      !asset.push.content.files.length ||
      asset.push.content.files.length > 10000
    )
      fail("分组必须映射同一明确项目及有界文件清单。");
    count += asset.push.content.files.length;
    if (count > 30000) fail("批次超过 30,000 选中文件，请显式拆批。");
    // Native execution may support larger files than the browser. Validate the
    // public fields using bounded placeholder sizes, never truncate a selection.
    const sample = structuredClone(asset.push);
    sample.content.files = sample.content.files.map((file) => {
      if (
        !record(file) ||
        typeof file.path !== "string" ||
        !Number.isSafeInteger(file.size) ||
        file.size < 0 ||
        typeof file.sha256 !== "string" ||
        (file.sha256 !== "" && !/^[0-9a-f]{64}$/.test(file.sha256))
      )
        fail("清单文件字段无效。");
      return { ...file, size: 0, sha256: file.sha256 || "0".repeat(64) };
    });
    // The transfer helper's 256-file UI cap is enforced by preflight below.
    // Validate every window here so native jobs preserve their full selection.
    for (
      let start = 0;
      start < sample.content.files.length;
      start += browserFileCountLimit
    )
      validatePushInput({
        ...sample,
        content: {
          ...sample.content,
          files: sample.content.files.slice(
            start,
            start + browserFileCountLimit,
          ),
        },
      });
    for (const file of asset.push.content.files)
      validateManagedPath(fullPath(asset, file.path));
  }
  for (const [name, values] of [
    ["excluded", plan.excluded],
    ["working_inputs", plan.working_inputs],
  ] as const) {
    if (!Array.isArray(values) || values.length > 30000)
      fail(`${name} 必须为有界的路径与原因数组。`);
    for (const item of values) {
      if (!record(item)) fail(`${name} 项无效。`);
      keys(item, ["path", "reason"], name);
      if (
        typeof item.path !== "string" ||
        typeof item.reason !== "string" ||
        !item.reason.trim() ||
        item.reason.length > 1000
      )
        fail(`${name} 必须有明确相对路径与原因。`);
      validateManagedPath(item.path);
    }
  }
  if (plan.capacity) {
    if (!record(plan.capacity)) fail("容量预算无效。");
    keys(
      plan.capacity,
      [
        "existing_library_bytes",
        "free_main_bytes",
        "free_backup_bytes",
        "free_restore_bytes",
        "free_verification_bytes",
      ],
      "容量预算",
    );
    if (
      Object.values(plan.capacity).some(
        (n) => !Number.isSafeInteger(n) || n < 0,
      )
    )
      fail("容量必须为非负安全整数字节；未知请省略。");
  }
  canonicalJSON(plan);
  return plan;
}

export function preflightMigration(
  plan: MigrationDraft,
  inventory: InventoryFile[],
): MigrationPreflight {
  validateMigrationDraft(plan);
  if (!Array.isArray(inventory) || inventory.length > 30000)
    fail("本次来源清单超出 30,000 项预算。");
  const seen = new Set<string>();
  const selected = new Map<string, number>();
  for (const asset of plan.assets)
    for (const file of asset.push.content.files) {
      const path = fullPath(asset, file.path);
      if (selected.has(path))
        fail("同一源文件被多个分组重复选择，请明确唯一归属。");
      selected.set(path, file.size);
    }
  for (const item of inventory) {
    if (
      !record(item) ||
      typeof item.path !== "string" ||
      !Number.isSafeInteger(item.size) ||
      item.size < 0
    )
      fail("来源清单条目无效。");
    validateManagedPath(item.path);
    const folded = item.path
      .normalize("NFKC")
      .toLowerCase()
      .replace(/ß/g, "ss");
    if (seen.has(folded)) fail("来源清单有同名或大小写/Unicode 等价项。");
    seen.add(folded);
    if (selected.has(item.path) && selected.get(item.path) !== item.size)
      fail("选中文件大小已变化，需重建计划。");
  }
  if (
    Array.from(selected.keys()).some(
      (path) => !inventory.some((item) => item.path === path),
    )
  )
    fail("本次所选来源缺少计划文件；不会从其他目录猜测补齐。");
  const dispositions = [...plan.excluded, ...plan.working_inputs];
  for (const [index, item] of dispositions.entries()) {
    if (
      dispositions.some(
        (other, i) =>
          i !== index &&
          (covers(item.path, other.path) || covers(other.path, item.path)),
      )
    )
      fail("排除与工作输入路径重复或互相嵌套。");
    if (
      Array.from(selected.keys()).some(
        (path) => covers(item.path, path) || covers(path, item.path),
      )
    )
      fail("排除/工作输入与选中素材重叠。");
  }
  const logical = Array.from(selected.values()).reduce((a, b) => a + b, 0);
  if (!Number.isSafeInteger(logical)) fail("总字节数超过可靠整数范围。");
  const library = plan.capacity?.existing_library_bytes;
  const fullLibrary = (library ?? 0) + logical;
  const budgets = {
    main: logical * 2,
    backup: fullLibrary * 2,
    restore: fullLibrary * 3,
    verification: logical * 2,
  };
  if (
    !Number.isSafeInteger(fullLibrary) ||
    Object.values(budgets).some((value) => !Number.isSafeInteger(value))
  )
    fail("容量预算超过可靠整数范围，请拆批或提供准确容量。 ");
  const failures: string[] = [];
  const unknown: string[] = [];
  const check = (
    label: string,
    available: number | undefined,
    required: number,
  ) => {
    if (available === undefined) unknown.push(label);
    else if (available < required) failures.push(label);
  };
  check("主库新增原件＋上传暂存", plan.capacity?.free_main_bytes, budgets.main);
  check("快照＋完整备份包", plan.capacity?.free_backup_bytes, budgets.backup);
  check(
    "新目标包＋解包＋恢复",
    plan.capacity?.free_restore_bytes,
    budgets.restore,
  );
  check(
    "独立校验副本＋工作副本",
    plan.capacity?.free_verification_bytes,
    budgets.verification,
  );
  if (library === undefined)
    unknown.push("现有馆藏容量（备份/恢复预算只是所选增量下界）");
  return {
    selected_files: selected.size,
    logical_bytes: logical,
    uncovered: inventory.filter(
      (item) =>
        !selected.has(item.path) &&
        !dispositions.some((d) => covers(d.path, item.path)),
    ),
    excluded: inventory.filter((item) =>
      plan.excluded.some((d) => covers(d.path, item.path)),
    ),
    working_inputs: inventory.filter((item) =>
      plan.working_inputs.some((d) => covers(d.path, item.path)),
    ),
    browser_eligible:
      selected.size <= browserFileCountLimit &&
      logical <= browserBatchLimit &&
      Array.from(selected.values()).every((size) => size <= browserFileLimit),
    budgets,
    capacity_failures: failures,
    capacity_unknown: unknown,
  };
}

export function filesForAsset(
  asset: MigrationAsset,
  files: ManagedFile[],
): ManagedFile[] {
  return asset.push.content.files.map((file) => {
    const path = fullPath(asset, file.path);
    const local = files.find((item) => item.path === path);
    if (!local) return fail(`缺少明确选中的源文件：${path}`);
    return { path: file.path, role: file.role, file: local.file };
  });
}
export async function freezeMigration({
  plan,
  files,
  instanceID,
  origin,
  signal,
  onProgress,
}: {
  plan: MigrationDraft;
  files: ManagedFile[];
  instanceID: string;
  origin: string;
  signal?: AbortSignal;
  onProgress?: (progress: TransferProgress) => void;
}): Promise<MigrationWorkspace> {
  const frozen = validateMigrationDraft(plan);
  if (!identifierPattern.test(instanceID)) fail("当前实例 ID 无效。");
  const inventory = files.map((file) => ({
    path: file.path,
    size: file.file.size,
  }));
  const preflight = preflightMigration(frozen, inventory);
  if (preflight.capacity_failures.length)
    fail("已声明的可用空间不足；先修正容量或缩小批次。");
  if (!preflight.browser_eligible)
    fail("所选批次超过浏览器明确预算，请导出本机助手作业。");
  const states: Record<string, PushState> = Object.create(null);
  const rows: Record<string, MigrationRow> = Object.create(null);
  for (const asset of frozen.assets) {
    asset.push = await preparePushInput({
      input: asset.push,
      files: filesForAsset(asset, files),
      signal,
      onProgress,
    });
    states[asset.key] = await newPushState({ input: asset.push, origin });
    rows[asset.key] = { status: "not_attempted" };
  }
  return {
    schema: "lantai.browser-migration-state/v1",
    state_format: "lantai.browser-push-json/v1",
    origin: new URL(origin).origin,
    instance_id: instanceID,
    batch_id: crypto.randomUUID(),
    created_at: new Date().toISOString(),
    plan_digest: `sha256:${await sha256(canonicalJSON(frozen))}`,
    plan: frozen,
    inventory,
    states,
    rows,
  };
}
export async function importMigrationWorkspace(
  value: unknown,
  origin: string,
): Promise<MigrationWorkspace> {
  assertNoTransferSecrets(value);
  if (!record(value))
    fail("原状态包无效。本页仅导入浏览器原状态；原生 state 请沿助手继续。");
  keys(
    value,
    [
      "schema",
      "state_format",
      "origin",
      "instance_id",
      "batch_id",
      "created_at",
      "plan_digest",
      "plan",
      "inventory",
      "states",
      "rows",
    ],
    "批次原状态包",
  );
  const workspace = structuredClone(value) as unknown as MigrationWorkspace;
  if (
    workspace.schema !== "lantai.browser-migration-state/v1" ||
    workspace.state_format !== "lantai.browser-push-json/v1" ||
    workspace.origin !== new URL(origin).origin ||
    !identifierPattern.test(workspace.instance_id) ||
    typeof workspace.batch_id !== "string" ||
    !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(workspace.batch_id) ||
    !Number.isFinite(Date.parse(workspace.created_at)) ||
    !record(workspace.states) ||
    !record(workspace.rows)
  )
    fail("原状态包的 schema、服务器或固定身份无效。禁止换 origin 续办。");
  workspace.plan = validateMigrationDraft(workspace.plan);
  if (
    workspace.plan_digest !==
    `sha256:${await sha256(canonicalJSON(workspace.plan))}`
  )
    fail("原计划摘要已变化，不能改键/改字段继续。");
  preflightMigration(workspace.plan, workspace.inventory);
  const names = workspace.plan.assets.map((asset) => asset.key);
  if (
    Object.keys(workspace.states).length !== names.length ||
    Object.keys(workspace.rows).length !== names.length ||
    names.some(
      (name) =>
        !Object.hasOwn(workspace.states, name) ||
        !Object.hasOwn(workspace.rows, name),
    )
  )
    fail("原状态缺少分组或夹带其他状态；不要创建替代键。");
  for (const asset of workspace.plan.assets) {
    await validatePushState(asset.push, workspace.states[asset.key], origin);
    const row = workspace.rows[asset.key];
    if (!record(row)) fail("逐项回执状态无效。");
    keys(row, ["status", "error_code"], "逐项回执");
    if (
      ![
        "not_attempted",
        "running",
        "committed",
        "description_pending",
        "failed",
        "unknown",
      ].includes(row.status)
    )
      fail("逐项回执状态无效。");
    if (row.status === "running") row.status = "unknown";
    const result = workspace.states[asset.key].result;
    if (
      (row.status === "committed" && result?.description_pending) ||
      (row.status === "description_pending" && !result?.description_pending)
    )
      fail("逐项状态与原提交回执的著录状态不符。");
    if (row.error_code && !/^[A-Z][A-Z0-9_]{0,79}$/.test(row.error_code))
      fail("逐项错误码无效。");
    if (
      ["committed", "description_pending"].includes(row.status) &&
      !workspace.states[asset.key].result
    )
      fail("完成记录缺少原固定版本回执。");
  }
  return workspace;
}

export function nativeImportInput(plan: MigrationDraft) {
  const checked = validateMigrationDraft(plan);
  return {
    schema: "lantai.asset-import-input/v1",
    assets: checked.assets,
    excluded: [
      ...checked.excluded,
      ...checked.working_inputs.map((item) => ({
        path: item.path,
        reason: `保留工作输入灾备：${item.reason}`,
      })),
    ],
  };
}
export function makeAssistantJob({
  action,
  input,
  report,
  source = "source",
  state = "state/migration",
  destination = "verified-copy",
  output = "results/migration.json",
  origin,
  jobID,
}: {
  action: "import.plan" | "import.apply" | "import.verify";
  input: unknown;
  report?: unknown;
  source?: string;
  state?: string;
  destination?: string;
  output?: string;
  origin: string;
  jobID?: string;
}) {
  for (const path of [source, state, destination, output])
    validateManagedPath(path);
  return {
    schema: "lantai.local-assistant-job/v1",
    job_id: jobID ?? `migration-${crypto.randomUUID()}`,
    action,
    ...(action !== "import.plan" ? { origin: new URL(origin).origin } : {}),
    input,
    directory: source,
    ...(action !== "import.plan" ? { state } : {}),
    ...(action === "import.verify"
      ? { destination, report, purpose: "archive_review" }
      : {}),
    output,
  };
}

// Native plans contain Go RawMessage values whose lexical bytes participate in
// the native digest. Without preserving raw spans, only a lossless JSON
// round-trip is safe to hand back to the native client. This conservative check
// permits ordinary pretty-print whitespace and never repairs an original digest.
export function assertNativeJSONRoundTrip(raw: string, parsed: unknown): void {
  let compact = "";
  let inString = false;
  let escaped = false;
  for (const char of raw) {
    if (inString) {
      compact += char;
      if (escaped) escaped = false;
      else if (char === "\\") escaped = true;
      else if (char === '"') inString = false;
    } else if (char === '"') {
      compact += char;
      inString = true;
    } else if (!/[ \t\r\n]/.test(char)) compact += char;
  }
  if (compact !== JSON.stringify(parsed))
    throw new Error(
      "原生计划含 JSON 词法差异（例如 1.0、转义或键序），本页不能无损续办。请保留原计划文件字节，通过本机助手 input_file 使用原文件；不要重算 digest 或更换 state/键。",
    );
}

export async function bindImportDeviceResult({
  job,
  envelope,
  context,
}: {
  job: Record<string, unknown>;
  envelope: unknown;
  context: {
    job_id: string;
    job_digest: string;
    state: string;
    stopped: boolean;
    workspace_binding: string;
  };
}): Promise<{ reference: Record<string, unknown>; completed: boolean }> {
  assertNoTransferSecrets(envelope);
  if (
    !record(envelope) ||
    envelope.schema !== "lantai.local-assistant-result/v1" ||
    !["import.plan", "import.apply", "import.verify"].includes(
      String(job.action),
    ) ||
    envelope.job_id !== job.job_id ||
    envelope.action !== job.action ||
    envelope.job_digest !== (await sha256(canonicalJSON(job))) ||
    context.job_digest !== envelope.job_digest ||
    !identifierPattern.test(context.job_id) ||
    !record(envelope.result)
  )
    fail("设备导入结果未绑定本次原作业与实际队列身份。");
  const value = envelope as Record<string, unknown>;
  const reference = value.result as Record<string, unknown>;
  keys(reference, ["schema", "job_id", "result_sha256"], "设备私有原结果引用");
  if (
    reference.schema !== "lantai.local-assistant-result-ref/v1" ||
    reference.job_id !== context.job_id ||
    !/^([0-9a-f]{64})$/.test(String(reference.result_sha256))
  )
    fail("原导入计划／报告引用或完整字节摘要不匹配。");
  const completed =
    value.status === "completed" &&
    value.exit_code === 0 &&
    context.state === "succeeded" &&
    context.stopped === true &&
    /^([0-9a-f]{64})$/.test(context.workspace_binding);
  return { reference: structuredClone(reference), completed };
}
