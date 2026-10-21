// Mirrors internal/client/asset_profile.go. Descriptive input only; no authority.
export const profileSchema = "lantai.asset-profile/v1";
export const profileStates = [
  "active",
  "reference",
  "rejected",
  "superseded",
  "archived",
  "pending_review",
  "unknown",
] as const;
export type ProfileDraft = {
  characters: string;
  source: string;
  business_state: string;
  legacy_state: string;
  legacy_version: string;
  source_paths: string;
  notes: string;
};
export type AssetProfile = {
  schema: string;
  characters: string[];
  source: string;
  business_state: string;
  legacy_state: string;
  legacy_version: string;
  source_paths: string[];
  notes: string[];
};
export const emptyProfile = (): ProfileDraft => ({
  characters: "",
  source: "",
  business_state: "unknown",
  legacy_state: "",
  legacy_version: "",
  source_paths: "",
  notes: "",
});
const fields = [
  "schema",
  "characters",
  "source",
  "business_state",
  "legacy_state",
  "legacy_version",
  "source_paths",
  "notes",
];
function text(value: string, max: number) {
  const normalized = value.normalize("NFC").trim();
  if (
    [...normalized].length > max ||
    /[\p{Cc}\u202a-\u202e\u2066-\u2069]/u.test(normalized) ||
    /[\ud800-\udfff]/u.test(normalized)
  )
    throw new Error(`画像字段超过 ${max} 字或含控制字符。`);
  return normalized;
}
function set(value: string[], count: number, max: number) {
  const items = [...new Set(value.map((v) => text(v, max)))];
  if (items.length > count || items.some((v) => !v))
    throw new Error(`画像列表最多 ${count} 个非空值。`);
  return items.sort((a, b) => {
    const x = [...a],
      y = [...b];
    for (let i = 0; i < Math.min(x.length, y.length); i++) {
      const n = x[i].codePointAt(0)! - y[i].codePointAt(0)!;
      if (n) return n;
    }
    return x.length - y.length;
  });
}
export function normalizeProfile(value: unknown): AssetProfile {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("画像必须是对象。");
  const p = value as Record<string, unknown>;
  if (
    Object.keys(p).some((k) => !fields.includes(k)) ||
    Object.values(p).some((v) => v === null)
  )
    throw new Error("画像包含未知或安全字段；未改写现有画像。");
  for (const k of ["characters", "source_paths", "notes"])
    if (
      p[k] !== undefined &&
      (!Array.isArray(p[k]) ||
        (p[k] as unknown[]).some((v) => typeof v !== "string"))
    )
      throw new Error("画像列表字段必须是文字数组。");
  for (const k of [
    "schema",
    "source",
    "business_state",
    "legacy_state",
    "legacy_version",
  ])
    if (p[k] !== undefined && typeof p[k] !== "string")
      throw new Error("画像文字字段无效。");
  const schema = (p.schema as string | undefined) || profileSchema,
    state = (p.business_state as string | undefined) || "unknown";
  if (
    schema !== profileSchema ||
    !(profileStates as readonly string[]).includes(state)
  )
    throw new Error("画像版本或业务状态不支持；旧拼写须保存在旧版状态。");
  return {
    schema,
    characters: set((p.characters as string[]) || [], 64, 54),
    source: text((p.source as string) || "", 57),
    business_state: state,
    legacy_state: text((p.legacy_state as string) || "", 200),
    legacy_version: text((p.legacy_version as string) || "", 49),
    source_paths: set((p.source_paths as string[]) || [], 256, 2048),
    notes: set((p.notes as string[]) || [], 64, 4000),
  };
}
export function profileFromDraft(draft: ProfileDraft) {
  const lines = (s: string) => s.split(/\r?\n/u).filter((v) => v.trim());
  const profile = normalizeProfile({
    ...draft,
    schema: profileSchema,
    characters: lines(draft.characters),
    source_paths: lines(draft.source_paths),
    notes: lines(draft.notes),
  });
  if (new TextEncoder().encode(JSON.stringify(profile)).length > 1024 * 1024)
    throw new Error("完整画像超过 1 MiB；未提交或覆盖未知 extra。");
  return profile;
}
export function profileDraft(value: unknown): ProfileDraft {
  const p = normalizeProfile(value);
  return {
    ...p,
    characters: p.characters.join("\n"),
    source_paths: p.source_paths.join("\n"),
    notes: p.notes.join("\n"),
  };
}
export function profileTags(p: AssetProfile) {
  return [
    ...p.characters.map((v) => `character:${v}`),
    `state:${p.business_state}`,
    ...(p.source ? [`source:${p.source}`] : []),
    ...(p.legacy_version ? [`legacy-version:${p.legacy_version}`] : []),
  ];
}
