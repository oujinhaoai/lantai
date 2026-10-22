import { api, APIError, type Version } from "./api";
import { assetIdentifier } from "./asset-tools";
import { canonicalJSON, parseManagedJSON } from "./managed-transfer";
import { prepareApproval, type Transport } from "./system-operations";
export const rightsPurposes = [
  "archive_review",
  "reference",
  "production",
  "generative_input",
  "raw_export",
] as const;
export type RightsSnapshot = {
  subject: { instance_id: string; asset_id: string; version_id: string };
  manifest_digest: string;
  revision: number;
  last_assertion_id?: string;
  rights: {
    usage: string;
    license: string;
    sensitivity: string;
    redistribute_raw: boolean;
    noai: boolean;
  };
  restricted_source: boolean;
  deny_purposes: string[];
  evidence_revision: number;
  evidence: { record_id: string; revision: number }[];
  purposes: {
    purpose: string;
    allowed: boolean;
    code?: string;
    rights_epoch: number;
  }[];
};
export function rightsTarget(version: Version) {
  const match =
    /^lantai:\/\/([0-7][0-9A-HJKMNP-TV-Z]{25})\/assets\/([0-7][0-9A-HJKMNP-TV-Z]{25})\/versions\/([0-7][0-9A-HJKMNP-TV-Z]{25})$/u.exec(
      version.uri,
    );
  if (
    !match ||
    match[2] !== version.asset_id ||
    match[3] !== version.version_id ||
    !/^sha256:[a-f0-9]{64}$/u.test(version.manifest_digest)
  )
    throw new Error("权利操作必须绑定完整固定版本与摘要。");
  return { instance_id: match[1], asset_id: match[2], version_id: match[3] };
}
export async function getVersionRights(
  version: Version,
  signal?: AbortSignal,
): Promise<RightsSnapshot> {
  const target = rightsTarget(version);
  const value = await api<RightsSnapshot>(
    `/api/v1/assets/${target.asset_id}/versions/${target.version_id}/rights`,
    { signal },
  );
  if (
    !value ||
    !value.subject ||
    value.subject.instance_id !== target.instance_id ||
    value.subject.asset_id !== target.asset_id ||
    value.subject.version_id !== target.version_id ||
    value.manifest_digest !== version.manifest_digest ||
    !Number.isSafeInteger(value.revision) ||
    value.revision < 1 ||
    !Number.isSafeInteger(value.evidence_revision) ||
    value.evidence_revision < 0 ||
    !Array.isArray(value.evidence) ||
    value.evidence.length > 1000 ||
    value.evidence.length !== value.evidence_revision ||
    value.evidence.some(
      (e, i) => !assetIdentifier.test(e.record_id) || e.revision !== i + 1,
    ) ||
    !value.rights ||
    !["production", "reference", "restricted"].includes(value.rights.usage) ||
    !["normal", "personal"].includes(value.rights.sensitivity) ||
    typeof value.rights.license !== "string" ||
    typeof value.rights.noai !== "boolean" ||
    typeof value.rights.redistribute_raw !== "boolean" ||
    typeof value.restricted_source !== "boolean" ||
    !Array.isArray(value.deny_purposes) ||
    value.deny_purposes.some(
      (p) => !(rightsPurposes as readonly string[]).includes(p),
    ) ||
    (value.last_assertion_id &&
      !assetIdentifier.test(value.last_assertion_id)) ||
    !Array.isArray(value.purposes) ||
    value.purposes.length !== 5 ||
    new Set(value.purposes.map((p) => p.purpose)).size !== 5 ||
    value.purposes.some(
      (p) =>
        !(rightsPurposes as readonly string[]).includes(p.purpose) ||
        typeof p.allowed !== "boolean" ||
        !Number.isSafeInteger(p.rights_epoch) ||
        p.rights_epoch < 1,
    )
  )
    throw new APIError("INVALID_RESPONSE", 0);
  return value;
}
export type RightsDraft = {
  kind: "restrict" | "correct" | "release" | "evidence";
  usage: string;
  license_expression: string;
  sensitivity: string;
  redistribute_raw: string;
  noai: string;
  restricted_source: string;
  replace_deny: boolean;
  deny_purposes: string[];
  confirmed_evidence_ids: string;
  uses: string;
  evidence_ids: string;
  reason: string;
};
export const emptyRightsDraft = (): RightsDraft => ({
  kind: "restrict",
  usage: "",
  license_expression: "",
  sensitivity: "",
  redistribute_raw: "",
  noai: "",
  restricted_source: "",
  replace_deny: false,
  deny_purposes: [],
  confirmed_evidence_ids: "",
  uses: "",
  evidence_ids: "",
  reason: "",
});
function ids(input: string) {
  const result = [...new Set(input.split(/[\s,，]+/u).filter(Boolean))];
  if (result.length > 1000 || result.some((id) => !assetIdentifier.test(id)))
    throw new Error("证据标识必须是完整固定 ID，最多1,000项。");
  return result;
}
export function rightsAssertion(
  snapshot: RightsSnapshot,
  draft: RightsDraft,
): Record<string, unknown> {
  const fields: Record<string, unknown> = {};
  for (const key of ["usage", "license_expression", "sensitivity"] as const)
    if (draft[key]) fields[key] = draft[key].trim();
  if (
    fields.usage &&
    !["production", "reference", "restricted"].includes(fields.usage as string)
  )
    throw new Error("用途声明无效。");
  if (
    fields.sensitivity &&
    !["normal", "personal"].includes(fields.sensitivity as string)
  )
    throw new Error("敏感级别无效。");
  if (
    fields.license_expression &&
    [...String(fields.license_expression)].length > 256
  )
    throw new Error("许可表达式最多256字；核心仍核验其格式。");
  for (const key of ["noai", "redistribute_raw", "restricted_source"] as const)
    if (draft[key]) {
      if (!["true", "false"].includes(draft[key]))
        throw new Error("权利布尔字段无效。");
      fields[key] = draft[key] === "true";
    }
  if (draft.replace_deny) {
    if (
      draft.deny_purposes.some(
        (p) => !(rightsPurposes as readonly string[]).includes(p),
      )
    )
      throw new Error("用途不支持。");
    fields.deny_purposes = [...new Set(draft.deny_purposes)].sort();
  }
  if (draft.confirmed_evidence_ids.trim())
    fields.confirmed_evidence_ids = ids(draft.confirmed_evidence_ids);
  if (draft.uses.trim()) {
    const uses = parseManagedJSON(draft.uses);
    if (!Array.isArray(uses) || uses.length > 1000)
      throw new Error("来源声明须为最多1,000项的完整数组。");
    for (const row of uses) {
      if (
        !row ||
        typeof row !== "object" ||
        Array.isArray(row) ||
        Object.keys(row).some(
          (k) =>
            ![
              "instance_id",
              "asset_id",
              "version_id",
              "relation",
              "declared",
            ].includes(k),
        ) ||
        ["instance_id", "asset_id", "version_id"].some(
          (k) => !assetIdentifier.test((row as Record<string, string>)[k]),
        ) ||
        !["uses", "derived_from", "reference"].includes(
          (row as Record<string, string>).relation,
        ) ||
        ((row as Record<string, unknown>).declared !== undefined &&
          (typeof (row as Record<string, unknown>).declared !== "string" ||
            [...String((row as Record<string, unknown>).declared)].length >
              4000))
      )
        throw new Error("来源声明必须是完整固定引用；不能放脚本或路径。");
    }
    fields.uses = uses;
  }
  if (draft.kind === "evidence" && Object.keys(fields).length)
    throw new Error("evidence 断言不能更改任何限制字段。");
  if (!["restrict", "correct", "release", "evidence"].includes(draft.kind))
    throw new Error("权利动作不支持。");
  const evidence = ids(draft.evidence_ids);
  if (
    !evidence.length ||
    evidence.some((id) => !snapshot.evidence.some((e) => e.record_id === id))
  )
    throw new Error(
      "请选择当前固定版本已接受的证据；更正／解除需要比原断言更新的证据。",
    );
  const reason = draft.reason.trim();
  if (!reason || [...reason].length > 4096)
    throw new Error("请填写1–4,096字理由。");
  const result = {
    subject: snapshot.subject,
    manifest_digest: snapshot.manifest_digest,
    expected_revision: snapshot.revision,
    kind: draft.kind,
    ...(snapshot.last_assertion_id
      ? { replaces_assertion_id: snapshot.last_assertion_id }
      : {}),
    fields,
    evidence_ids: evidence,
    reason,
  };
  if (new TextEncoder().encode(JSON.stringify(result)).length > 64 * 1024)
    throw new Error("权利请求超过64 KiB人审绑定预算。");
  return result;
}
export function rightsEvidence(
  snapshot: RightsSnapshot,
  note: string,
  unknownURLs: string,
): Record<string, unknown> {
  if (!note.trim() || [...note].length > 4000)
    throw new Error("请填写1–4,000字的来源证据说明。");
  const urls = unknownURLs
    .split(/\r?\n/u)
    .map((s) => s.trim())
    .filter(Boolean);
  if (urls.length > 1000 || urls.some((s) => [...s].length > 2048))
    throw new Error("未知外部来源最多1,000行，每行2,048字。");
  return {
    subject: snapshot.subject,
    manifest_digest: snapshot.manifest_digest,
    expected_revision: snapshot.evidence_revision,
    evidence: {
      note: note.trim(),
      external_inputs: urls.map((source_url) => ({
        source_url,
        rights_status: "unknown",
      })),
    },
  };
}

export async function prepareRightsApproval(
  snapshot: RightsSnapshot,
  draft: RightsDraft,
  projectID: string,
  request: Transport,
) {
  if (
    !["correct", "release"].includes(draft.kind) ||
    !assetIdentifier.test(projectID)
  )
    throw new Error("权利更正需要精确项目与人审动作。");
  const body = rightsAssertion(snapshot, draft);
  const approval = await prepareApproval(
    {
      mode: "domain",
      title: "人审更正／解除此固定版本的权利限制",
      kind: "rights",
      request: body,
    },
    request,
  );
  const action = approval.actions?.[0],
    proof = approval.challenge;
  if (
    Date.parse(proof.expires_at) <= Date.now() ||
    !action ||
    action.action !== "provenance.release_restriction" ||
    action.project_id !== projectID ||
    action.resource_id !== snapshot.subject.version_id ||
    action.resource_revision !== snapshot.revision ||
    action.manifest_digest !== snapshot.manifest_digest ||
    canonicalJSON(action.request) !== canonicalJSON(body) ||
    proof.action !== action.action ||
    proof.targets.length !== 1 ||
    proof.targets[0].kind !== "domain_target" ||
    proof.targets[0].id !== snapshot.subject.version_id ||
    proof.targets[0].expected_revision !== snapshot.revision
  )
    throw new Error(
      "服务器人审目标与本次固定版本、修订或完整权利请求不符，未请求动态码。",
    );
  return approval;
}
