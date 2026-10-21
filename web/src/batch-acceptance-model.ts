import {
  assertNoTransferSecrets,
  canonicalJSON,
  identifierPattern,
  validateManagedPath,
} from "./managed-transfer";
import type { ManagedRecordRef } from "./managed-records";
import type { MigrationWorkspace } from "./migration-model";

export const batchPurposes = [
  "archive_review",
  "reference",
  "production",
  "generative_input",
  "raw_export",
] as const;
export const batchStages = [
  "inventory",
  "ingest",
  "verification",
  "workset",
  "references",
  "recovery",
  "acceptance",
  "in_use",
  "rollback",
] as const;
export type BatchStage = (typeof batchStages)[number];
export type BatchAsset = ManagedRecordRef & {
  key: string;
  manifest_digest: string;
  status:
    | "committed"
    | "description_pending"
    | "failed"
    | "unknown"
    | "not_attempted";
};
export const checklistLabels = {
  excluded_backed_up: "排除项已另行备份或明确无需保留，责任已确认",
  working_inputs_backed_up: "未入馆工作输入与切换后新成果有独立备份",
  entrypoints_opened: "固定工作集入口已在原生软件打开并完成续做检查",
  new_results_captured: "切换后新增成果均保留固定版本库存",
  fallback_ready: "原件和独立回退目标已保留，可按原计划回退",
  selected_notes_reviewed: "选中笔记的精确改链范围、diff 与跳过项已审阅",
} as const;
export type MigrationBatch = {
  schema: "lantai.migration-batch/v1";
  batch_id: string;
  revision: number;
  previous_ref?: ManagedRecordRef;
  project_id: string;
  instance_id: string;
  title: string;
  owner_id: string;
  last_action: { actor_id: string; at: string; action: string; note: string };
  stage: BatchStage;
  acceptance: "not_requested" | "requested";
  source_retained: true;
  intended_purpose: (typeof batchPurposes)[number];
  reference_scope: "none" | "selected_notes";
  import_summary: {
    planned: number;
    committed: number;
    description_pending: number;
    failed: number;
    unknown: number;
    not_attempted: number;
  };
  evidence: {
    assets: BatchAsset[];
    worksets: ManagedRecordRef[];
    verifications: ManagedRecordRef[];
    recovery: ManagedRecordRef[];
    native_continuations?: ManagedRecordRef[];
    reference_jobs: {
      job_id: string;
      action: "references.plan" | "references.copy" | "references.rollback";
    }[];
  };
  checklist: Record<keyof typeof checklistLabels, boolean>;
};
export type BatchCheckStatus =
  "passed" | "failed" | "missing" | "pending" | "unknown";
export type BatchCheck = {
  schema: "lantai.batch-check/v1";
  instance_id: string;
  record_ref: ManagedRecordRef;
  intended_purpose: MigrationBatch["intended_purpose"];
  batch_id: string;
  revision: number;
  checked_at: string;
  acceptance: "blocked" | "ready";
  checks: { key: string; status: BatchCheckStatus; code?: string }[];
  assets: (ManagedRecordRef & { manifest_digest: string })[];
  worksets: ManagedRecordRef[];
  recovery_refs: ManagedRecordRef[];
};
export const batchCheckKeys = [
  "record",
  "assets",
  "purpose",
  "worksets",
  "verifications",
  "native_continuation",
  "recovery",
  "references",
  "checklist",
] as const;
const object = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
const fail = (message: string): never => {
  throw new Error(message);
};
function keys(value: object, allowed: string[], label: string) {
  if (Object.keys(value).some((key) => !allowed.includes(key)))
    fail(`${label}含未知字段。`);
}
function id(value: unknown): value is string {
  return typeof value === "string" && identifierPattern.test(value);
}
function text(value: unknown, max: number) {
  return typeof value === "string" && !!value.trim() && value.length <= max;
}
export function validateBatchRef(value: unknown): ManagedRecordRef {
  if (!object(value) || !id(value.asset_id) || !id(value.version_id))
    return fail("档案必须使用固定 asset_id/version_id。");
  keys(value, ["asset_id", "version_id"], "固定引用");
  return value as ManagedRecordRef;
}
function refs(value: unknown): ManagedRecordRef[] {
  if (!Array.isArray(value) || value.length > 1000)
    return fail("证据引用必须为有界数组。");
  const seen = new Set<string>();
  return value.map((item) => {
    const ref = validateBatchRef(item);
    const key = `${ref.asset_id}/${ref.version_id}`;
    if (seen.has(key)) fail("同类证据不能重复引用。");
    seen.add(key);
    return ref;
  });
}
export function validateMigrationBatch(value: unknown): MigrationBatch {
  assertNoTransferSecrets(value);
  if (!object(value)) return fail("批次记录必须是对象。");
  keys(
    value,
    [
      "schema",
      "batch_id",
      "revision",
      "previous_ref",
      "project_id",
      "instance_id",
      "title",
      "owner_id",
      "last_action",
      "stage",
      "acceptance",
      "source_retained",
      "intended_purpose",
      "reference_scope",
      "import_summary",
      "evidence",
      "checklist",
    ],
    "批次记录",
  );
  if (
    value.schema !== "lantai.migration-batch/v1" ||
    typeof value.batch_id !== "string" ||
    !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value.batch_id) ||
    !Number.isSafeInteger(value.revision) ||
    Number(value.revision) < 1 ||
    !id(value.project_id) ||
    !id(value.instance_id) ||
    !id(value.owner_id) ||
    !text(value.title, 200) ||
    !batchStages.includes(value.stage as BatchStage) ||
    !["not_requested", "requested"].includes(String(value.acceptance)) ||
    value.source_retained !== true ||
    !batchPurposes.includes(
      value.intended_purpose as (typeof batchPurposes)[number],
    )
  )
    fail(
      "批次身份、责任人、阶段或原件保留声明无效；不能由客户端填写 accepted/complete。",
    );
  if (
    !["none", "selected_notes"].includes(String(value.reference_scope)) ||
    !object(value.import_summary)
  )
    fail("必须明确选择笔记切换范围与完整逐项导入统计。");
  keys(
    value.import_summary as object,
    [
      "planned",
      "committed",
      "description_pending",
      "failed",
      "unknown",
      "not_attempted",
    ],
    "导入统计",
  );
  const summary = value.import_summary as MigrationBatch["import_summary"];
  if (
    [
      "planned",
      "committed",
      "description_pending",
      "failed",
      "unknown",
      "not_attempted",
    ].some(
      (key) =>
        !Number.isSafeInteger(summary[key as keyof typeof summary]) ||
        summary[key as keyof typeof summary] < 0,
    ) ||
    summary.planned > 1000 ||
    summary.planned !==
      summary.committed +
        summary.description_pending +
        summary.failed +
        summary.unknown +
        summary.not_attempted
  )
    fail("导入统计缺项、溢出或未覆盖全部计划项。");
  if (
    (value.revision === 1 && value.previous_ref !== undefined) ||
    (Number(value.revision) > 1 && value.previous_ref === undefined)
  )
    fail("首修订不能带旧基线，后续修订必须绑定上个固定版本。");
  if (value.previous_ref !== undefined) validateBatchRef(value.previous_ref);
  if (
    !object(value.last_action) ||
    !id(value.last_action.actor_id) ||
    !Number.isFinite(Date.parse(String(value.last_action.at))) ||
    !text(value.last_action.action, 100) ||
    !text(value.last_action.note, 2000)
  )
    fail("最后动作必须保存实际操作者、时间、动作与说明。");
  keys(
    value.last_action as object,
    ["actor_id", "at", "action", "note"],
    "最后动作",
  );
  if (!object(value.evidence)) fail("批次证据无效。");
  const evidence = value.evidence as Record<string, unknown>;
  keys(
    evidence,
    [
      "assets",
      "worksets",
      "verifications",
      "recovery",
      "native_continuations",
      "reference_jobs",
    ],
    "批次证据",
  );
  if (!Array.isArray(evidence.assets) || evidence.assets.length > 1000)
    fail("资产证据超出预算。");
  const seen = new Set<string>();
  for (const asset of evidence.assets as unknown[]) {
    if (
      !object(asset) ||
      !text(asset.key, 100) ||
      !id(asset.asset_id) ||
      !id(asset.version_id) ||
      !/^sha256:[0-9a-f]{64}$/.test(String(asset.manifest_digest)) ||
      ![
        "committed",
        "description_pending",
        "failed",
        "unknown",
        "not_attempted",
      ].includes(String(asset.status))
    )
      fail("资产证据必须钉住固定版本、完整清单摘要和原逐项状态。");
    const item = asset as Record<string, unknown>;
    keys(
      item,
      ["key", "asset_id", "version_id", "manifest_digest", "status"],
      "资产证据",
    );
    if (seen.has(String(item.key))) fail("资产键重复。");
    seen.add(String(item.key));
  }
  refs(evidence.worksets);
  refs(evidence.verifications);
  refs(evidence.recovery);
  if (evidence.native_continuations !== undefined) {
    if (
      !Array.isArray(evidence.native_continuations) ||
      evidence.native_continuations.length > 64
    )
      fail("原生续做证据最多64项。");
    refs(evidence.native_continuations);
  }
  if (
    !Array.isArray(evidence.reference_jobs) ||
    evidence.reference_jobs.length > 100
  )
    fail("引用作业证据超出预算。");
  const jobs = new Set<string>();
  for (const job of evidence.reference_jobs as unknown[]) {
    if (
      !object(job) ||
      !id(job.job_id) ||
      !["references.plan", "references.copy", "references.rollback"].includes(
        String(job.action),
      ) ||
      jobs.has(String(job.job_id))
    )
      fail("引用作业必须使用实际设备通道的固定 job_id 与原动作。");
    keys(job as object, ["job_id", "action"], "引用作业证据");
    jobs.add(String((job as Record<string, unknown>).job_id));
  }
  if (
    !object(value.checklist) ||
    Object.keys(checklistLabels).some(
      (key) =>
        typeof (value.checklist as Record<string, unknown>)[key] !== "boolean",
    )
  )
    fail("统一切换清单必须逐项明确确认。");
  keys(value.checklist as object, Object.keys(checklistLabels), "切换清单");
  return structuredClone(value) as MigrationBatch;
}
export function batchReadiness(batch: MigrationBatch): string[] {
  const checked = validateMigrationBatch(batch);
  const blockers: string[] = [];
  if (!checked.evidence.assets.length) blockers.push("缺少固定资产版本");
  if (
    !checked.import_summary.planned ||
    checked.import_summary.committed !== checked.import_summary.planned ||
    checked.evidence.assets.length !== checked.import_summary.planned
  )
    blockers.push("导入统计未覆盖全部计划的已提交项");
  if (checked.evidence.assets.some((asset) => asset.status !== "committed"))
    blockers.push("资产存在 failed/pending/unknown/未开始项");
  for (const key of ["worksets", "verifications", "recovery"] as const)
    if (!checked.evidence[key].length) blockers.push(`缺少 ${key} 固定证据`);
  if (
    checked.reference_scope === "selected_notes" &&
    (!checked.evidence.reference_jobs.some(
      (job) => job.action === "references.plan",
    ) ||
      !checked.evidence.reference_jobs.some(
        (job) =>
          job.action ===
          (checked.stage === "rollback"
            ? "references.rollback"
            : "references.copy"),
      ))
  )
    blockers.push("缺少选中笔记计划与副本／回退作业");
  if (Object.values(checked.checklist).some((done) => !done))
    blockers.push("统一切换清单尚未逐项确认");
  if (checked.acceptance !== "requested") blockers.push("尚未明确请求切换接受");
  return blockers;
}
export function batchFromWorkspace({
  workspace,
  projectID,
  instanceID,
  actorID,
  ownerID = actorID,
  title = "迁移批次",
}: {
  workspace?: MigrationWorkspace;
  projectID: string;
  instanceID: string;
  actorID: string;
  ownerID?: string;
  title?: string;
}): MigrationBatch {
  if (
    workspace &&
    (workspace.plan.project_id !== projectID ||
      workspace.instance_id !== instanceID)
  )
    fail("原导入工作区不属于当前项目／实例。");
  const batch: MigrationBatch = {
    schema: "lantai.migration-batch/v1",
    batch_id: workspace?.batch_id ?? crypto.randomUUID(),
    revision: 1,
    project_id: projectID,
    instance_id: instanceID,
    title: workspace?.plan.name ?? title,
    owner_id: ownerID,
    last_action: {
      actor_id: actorID,
      at: new Date().toISOString(),
      action: "create",
      note: "建立批次台账，原件继续保留。",
    },
    stage: "inventory",
    acceptance: "not_requested",
    source_retained: true,
    intended_purpose: "production",
    reference_scope: "selected_notes",
    import_summary: {
      planned: workspace?.plan.assets.length ?? 0,
      committed: 0,
      description_pending: 0,
      failed: 0,
      unknown: 0,
      not_attempted: 0,
    },
    evidence: {
      assets: [],
      worksets: [],
      verifications: [],
      recovery: [],
      reference_jobs: [],
    },
    checklist: Object.fromEntries(
      Object.keys(checklistLabels).map((key) => [key, false]),
    ) as MigrationBatch["checklist"],
  };
  if (workspace)
    for (const asset of workspace.plan.assets) {
      const status = workspace.rows[asset.key].status;
      batch.import_summary[status === "running" ? "unknown" : status]++;
      const receipt = workspace.states[asset.key].result;
      if (!receipt) continue;
      batch.evidence.assets.push({
        key: asset.key,
        asset_id: receipt.asset_id,
        version_id: receipt.version_id,
        manifest_digest: receipt.manifest_digest,
        status: status === "running" ? "unknown" : status,
      });
    }
  return validateMigrationBatch(batch);
}
export function nextBatchRevision(
  batch: MigrationBatch,
  previous: ManagedRecordRef,
  actorID: string,
): MigrationBatch {
  const next = validateMigrationBatch(batch);
  validateBatchRef(previous);
  next.revision++;
  next.previous_ref = { ...previous };
  next.acceptance = "not_requested";
  next.last_action = {
    actor_id: actorID,
    at: new Date().toISOString(),
    action: "revise",
    note: "基于上个固定版本创建条件修订，接受状态须重新复核。",
  };
  return validateMigrationBatch(next);
}
export function validateBatchCheck(
  value: unknown,
  batch: MigrationBatch,
  recordRef: ManagedRecordRef,
): BatchCheck {
  if (
    !object(value) ||
    value.schema !== "lantai.batch-check/v1" ||
    value.instance_id !== batch.instance_id ||
    value.intended_purpose !== batch.intended_purpose ||
    value.batch_id !== batch.batch_id ||
    value.revision !== batch.revision ||
    canonicalJSON(validateBatchRef(value.record_ref)) !==
      canonicalJSON(recordRef) ||
    !Number.isFinite(Date.parse(String(value.checked_at))) ||
    !["ready", "blocked"].includes(String(value.acceptance)) ||
    !Array.isArray(value.checks) ||
    value.checks.length > 1000
  )
    fail("服务端接受观察未绑定当前批次修订与固定记录。");
  const checked = value as unknown as BatchCheck;
  const seen = new Set<string>();
  for (const check of checked.checks) {
    if (
      !object(check) ||
      !text(check.key, 150) ||
      seen.has(check.key) ||
      !["passed", "failed", "missing", "pending", "unknown"].includes(
        check.status,
      )
    )
      fail("服务端接受检查项无效或重复。");
    seen.add(check.key);
  }
  if (
    checked.acceptance === "ready" &&
    (batchReadiness(batch).length ||
      checked.checks.some((check) => check.status !== "passed") ||
      batchCheckKeys.some((key) => !seen.has(key)))
  )
    fail("缺失／失败／待办／不明证据不能成为接受完成。");
  if (
    !Array.isArray(checked.assets) ||
    checked.assets.length > batch.evidence.assets.length
  )
    fail("服务端资产检查集合无效。");
  const assetKeys = new Set<string>();
  for (const item of checked.assets) {
    const key = `${item.asset_id}/${item.version_id}`;
    if (
      assetKeys.has(key) ||
      !batch.evidence.assets.some(
        (asset) =>
          asset.asset_id === item.asset_id &&
          asset.version_id === item.version_id &&
          asset.manifest_digest === item.manifest_digest,
      )
    )
      fail("服务端检查混入其他资产或不同清单。");
    assetKeys.add(key);
  }
  const worksets = refs(checked.worksets),
    recovery = refs(checked.recovery_refs);
  for (const [actual, expected] of [
    [worksets, batch.evidence.worksets],
    [recovery, batch.evidence.recovery],
  ])
    for (const item of actual)
      if (
        !expected.some(
          (ref) =>
            ref.asset_id === item.asset_id &&
            ref.version_id === item.version_id,
        )
      )
        fail("服务端检查混入其他固定档案。");
  if (
    checked.acceptance === "ready" &&
    (checked.assets.length !== batch.evidence.assets.length ||
      worksets.length !== batch.evidence.worksets.length ||
      recovery.length !== batch.evidence.recovery.length)
  )
    fail("服务端ready检查未覆盖完整固定证据集合。");
  return structuredClone(checked);
}
export function parseBatchRefLines(text: string): ManagedRecordRef[] {
  return refs(
    text
      .split(/\r?\n/)
      .map((line) => line.trim())
      .filter(Boolean)
      .map((line) => {
        const parts = line.split("/");
        if (parts.length !== 2) fail("每行填写 asset_id/version_id 固定引用。");
        return { asset_id: parts[0], version_id: parts[1] };
      }),
  );
}
export function batchRefLines(refs: ManagedRecordRef[]) {
  return refs.map((ref) => `${ref.asset_id}/${ref.version_id}`).join("\n");
}
export function referenceJob({
  action,
  input,
  batchID,
  directory,
  destination,
  postCutoverRefs,
}: {
  action: "references.plan" | "references.copy" | "references.rollback";
  input: unknown;
  batchID: string;
  directory: string;
  destination: string;
  postCutoverRefs?: string[];
}) {
  assertNoTransferSecrets(input);
  validateManagedPath(directory);
  validateManagedPath(destination);
  if (
    directory === destination ||
    directory.startsWith(destination + "/") ||
    destination.startsWith(directory + "/")
  )
    fail("源和副本／回退目标必须为独立目录。");
  if (action === "references.rollback" && !Array.isArray(postCutoverRefs))
    fail("回退必须显式提供切换后新成果固定引用库存，空数组也须明确。");
  if (
    postCutoverRefs?.some(
      (ref) =>
        !/^lantai:\/\/[0-7][0-9A-HJKMNP-TV-Z]{25}\/assets\/[0-7][0-9A-HJKMNP-TV-Z]{25}\/versions\/[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(
          ref,
        ),
    )
  )
    fail("新增成果库存含非固定引用。");
  return {
    schema: "lantai.local-assistant-job/v1",
    job_id: `references-${batchID}-${action.split(".")[1]}`,
    action,
    input,
    directory,
    ...(action !== "references.plan" ? { destination } : {}),
    ...(action === "references.rollback"
      ? { post_cutover_refs: postCutoverRefs }
      : {}),
    output: `results/${batchID}-${action.replace(".", "-")}.json`,
  };
}

function referenceInputIdentity(value: unknown): unknown {
  if (
    !object(value) ||
    value.schema !== "lantai.reference-input/v1" ||
    !Array.isArray(value.notes) ||
    !value.notes.length ||
    !Array.isArray(value.mappings) ||
    !value.mappings.length ||
    (value.preserve !== undefined && !Array.isArray(value.preserve))
  )
    fail("原笔记输入缺少明确选择。");
  keys(
    value as object,
    ["schema", "notes", "mappings", "preserve"],
    "笔记输入",
  );
  const input = value as Record<string, unknown>;
  return { ...input, preserve: input.preserve ?? [] };
}
export async function bindReferenceResult({
  job,
  envelope,
  context,
}: {
  job: Record<string, unknown>;
  envelope: unknown;
  context: {
    job_id: string;
    job_digest: string;
    state?: string;
    stopped?: boolean;
    workspace_binding?: string;
  };
}): Promise<{
  result: Record<string, unknown>;
  completed: boolean;
  kind: "inline" | "local_ref";
}> {
  const { sha256 } = await import("./managed-transfer");
  assertNoTransferSecrets(envelope);
  if (
    !id(context.job_id) ||
    !object(envelope) ||
    envelope.schema !== "lantai.local-assistant-result/v1" ||
    envelope.job_id !== job.job_id ||
    envelope.action !== job.action ||
    envelope.job_digest !== (await sha256(canonicalJSON(job))) ||
    context.job_digest !== envelope.job_digest ||
    !object(envelope.result)
  )
    fail("设备结果未绑定本次原作业与服务端固定 job ID。");
  const result = (envelope as Record<string, unknown>).result as Record<
    string,
    unknown
  >;
  if (result.schema === "lantai.local-assistant-result-ref/v1") {
    keys(result, ["schema", "job_id", "result_sha256"], "本机原结果引用");
    if (
      result.job_id !== context.job_id ||
      !/^([0-9a-f]{64})$/.test(String(result.result_sha256))
    )
      fail("本机结果引用未绑定实际队列作业或完整原字节摘要。");
    const completed =
      (envelope as Record<string, unknown>).status === "completed" &&
      (envelope as Record<string, unknown>).exit_code === 0 &&
      context.state === "succeeded" &&
      context.stopped === true &&
      /^([0-9a-f]{64})$/.test(context.workspace_binding ?? "");
    return { result, completed, kind: "local_ref" };
  }
  if (result.scope !== "explicit_mappings_in_selected_notes_only")
    fail("设备引用结果范围不是本批精确映射。");
  const completed =
    (envelope as Record<string, unknown>).status === "completed" &&
    (envelope as Record<string, unknown>).exit_code === 0 &&
    result.complete === true;
  if (job.action === "references.plan") {
    if (
      result.schema !== "lantai.reference-plan/v1" ||
      canonicalJSON(referenceInputIdentity(result.input)) !==
        canonicalJSON(referenceInputIdentity(job.input)) ||
      !Array.isArray(result.unresolved) ||
      (completed && result.unresolved.length)
    )
      fail("原改链输入与计划／未解决项不匹配。");
    if (
      !Array.isArray(result.notes) ||
      !object(result.input) ||
      !Array.isArray(result.input.notes) ||
      result.notes.length !== result.input.notes.length ||
      result.notes.some(
        (note, index) =>
          !object(note) ||
          note.path !==
            (result.input as Record<string, unknown[]>).notes[index] ||
          !/^([0-9a-f]{64})$/.test(String(note.before_sha256)) ||
          !/^([0-9a-f]{64})$/.test(String(note.after_sha256)),
      )
    )
      fail("原计划未覆盖全部选中笔记与前后摘要。");
  } else {
    if (
      !object(job.input) ||
      result.schema !== "lantai.reference-receipt/v1" ||
      result.plan_digest !== job.input.digest ||
      result.mode !==
        (job.action === "references.copy" ? "copy" : "rollback") ||
      !Array.isArray(result.results) ||
      (completed &&
        result.results.some(
          (row) =>
            !object(row) || !["copied", "present"].includes(String(row.state)),
        ))
    )
      fail("原计划与副本／回退逐项回执不匹配。");
    const original = job.input as Record<string, unknown>;
    if (
      !Array.isArray(original.notes) ||
      !original.notes.length ||
      (result.results as unknown[]).length !== original.notes.length
    )
      fail("副本回执未覆盖全部原计划笔记。");
    const seenNotes = new Set<string>();
    for (const row of result.results as unknown[]) {
      if (
        !object(row) ||
        typeof row.path !== "string" ||
        seenNotes.has(row.path)
      )
        fail("副本回执含重复或无效路径。");
      const item = row as Record<string, unknown>;
      const note = (original.notes as unknown[]).find(
        (value) => object(value) && value.path === item.path,
      ) as Record<string, unknown> | undefined;
      if (
        !note ||
        note.before_sha256 !== item.before_sha256 ||
        note.after_sha256 !== item.after_sha256 ||
        (completed &&
          item.output_sha256 !==
            (job.action === "references.copy"
              ? note.after_sha256
              : note.before_sha256))
      )
        fail("副本回执前后或实际输出摘要不匹配原计划。");
      seenNotes.add(String(item.path));
    }
    if (
      job.action === "references.rollback" &&
      (canonicalJSON(result.post_cutover_refs) !==
        canonicalJSON(job.post_cutover_refs) ||
        result.inventory_scope !== "caller_declared_only_not_whole_library")
    )
      fail("回退丢失或改变了切换后新增成果库存。");
  }
  return { result, completed, kind: "inline" };
}

export function syncBatchImport(
  batch: MigrationBatch,
  workspace: MigrationWorkspace,
  actorID: string,
): MigrationBatch {
  if (batch.batch_id !== workspace.batch_id)
    fail("不能将其他批次原状态拼入本台账。");
  const current = batchFromWorkspace({
    workspace,
    projectID: batch.project_id,
    instanceID: batch.instance_id,
    actorID,
    ownerID: batch.owner_id,
  });
  return validateMigrationBatch({
    ...batch,
    import_summary: current.import_summary,
    evidence: { ...batch.evidence, assets: current.evidence.assets },
    acceptance: "not_requested",
    last_action: {
      actor_id: actorID,
      at: new Date().toISOString(),
      action: "refresh_import",
      note: "从同批原状态更新全部逐项导入事实；当前接受须重新复核。",
    },
  });
}
