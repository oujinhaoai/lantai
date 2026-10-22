import { api, type Asset } from "./api";
import {
  assertNoTransferSecrets,
  canonicalJSON,
  getExactManagedVersion,
  newPushState,
  parseManagedJSON,
  preparePushInput,
  validatePushState,
  type ManagedFile,
  type PushInput,
  type PushReceipt,
  type PushState,
  type Rights,
} from "./managed-transfer";
import {
  validateWorkset,
  type WorksetEntry,
  type WorksetInput,
} from "./workset-helpers";

export type WorksetReturn = {
  schema: "lantai.workset-return/v1";
  instance_id: string;
  entry: WorksetEntry;
  purpose: "reference" | "production";
  input: PushInput;
  state: PushState;
};

export function returnRights(value: unknown, usage: string): Rights {
  const rights = value as Rights | undefined;
  const ranges: Record<string, string[]> = {
    production: ["production", "reference", "restricted"],
    reference: ["reference", "restricted"],
    restricted: ["restricted"],
  };
  if (
    !rights ||
    !ranges[rights.usage]?.includes(usage) ||
    typeof rights.license !== "string" ||
    !rights.license.trim() ||
    typeof rights.redistribute_raw !== "boolean" ||
    typeof rights.noai !== "boolean" ||
    !["normal", "personal"].includes(rights.sensitivity)
  )
    throw new Error(
      "新版本权利必须在所选固定清单的许可范围内；许可变更需独立授权处理。",
    );
  return { ...rights, usage: usage as Rights["usage"] };
}

// Preparing is read-only. The original upload identity is created only after
// the user downloads this frozen input/state and explicitly confirms the write.
export async function prepareWorksetReturn(options: {
  entry: WorksetEntry;
  files: ManagedFile[];
  purpose: "reference" | "production";
  usage: Rights["usage"];
  note: string;
  signal?: AbortSignal;
  resume?: string;
}): Promise<WorksetReturn> {
  validateWorkset({
    schema: "lantai.client-workset/v1",
    entries: [options.entry],
  });
  if (!["reference", "production"].includes(options.purpose))
    throw new Error("请选择明确的回交用途。");
  const exact = await getExactManagedVersion({
    assetID: options.entry.asset_id,
    versionID: options.entry.version_id,
    signal: options.signal,
  });
  const rights = returnRights(exact.manifest.content.rights, options.usage);
  if (options.purpose === "production" && rights.usage !== "production")
    throw new Error(
      "制作回交必须具有 production 使用范围；不能将参考资料自动升级为制作许可。",
    );
  const input = await preparePushInput({
    input: {
      project_id: exact.manifest.project_id,
      asset_id: options.entry.asset_id,
      base_version_id: options.entry.version_id,
      content: {
        asset_type: exact.manifest.content.asset_type,
        version_note: options.note,
        files: [],
        rights,
        uses: [
          {
            asset_id: options.entry.asset_id,
            version_id: options.entry.version_id,
            relation:
              options.purpose === "production" ? "derived_from" : "reference",
          },
        ],
      },
    },
    files: options.files,
    signal: options.signal,
  });
  if (options.resume) {
    const value = parseManagedJSON(options.resume) as WorksetReturn;
    assertNoTransferSecrets(value);
    if (
      !value ||
      value.schema !== "lantai.workset-return/v1" ||
      Object.keys(value).some(
        (key) =>
          ![
            "schema",
            "instance_id",
            "entry",
            "purpose",
            "input",
            "state",
          ].includes(key),
      ) ||
      value.instance_id !== exact.manifest.instance_id ||
      value.purpose !== options.purpose ||
      canonicalJSON(value.entry) !== canonicalJSON(options.entry) ||
      canonicalJSON(value.input) !== canonicalJSON(input)
    )
      throw new Error(
        "原回交状态与实例、基线、用途或所选新文件不一致，不能换键继续。",
      );
    await validatePushState(input, value.state);
    return value;
  }
  const current = await api<Asset>(`/api/v1/assets/${options.entry.asset_id}`, {
    signal: options.signal,
  });
  if (
    current.asset_id !== options.entry.asset_id ||
    current.project_id !== input.project_id ||
    current.latest?.version_id !== options.entry.version_id
  )
    throw new Error(
      "所选固定基线已不是当前最新版本。请保留原工作集，明确选择新基线另建回交草稿。",
    );
  return {
    schema: "lantai.workset-return/v1",
    instance_id: exact.manifest.instance_id,
    entry: { ...options.entry },
    purpose: options.purpose,
    input,
    state: await newPushState({ input }),
  };
}

export function reviseReturnedWorkset(
  input: WorksetInput,
  original: WorksetEntry,
  receipt: PushReceipt,
): WorksetInput {
  const fixed = validateWorkset(input);
  const matches = fixed.entries.filter(
    (entry) => canonicalJSON(entry) === canonicalJSON(original),
  );
  if (
    matches.length !== 1 ||
    receipt.asset_id !== original.asset_id ||
    receipt.version_id === original.version_id ||
    !receipt.version_id ||
    receipt.description_pending
  )
    throw new Error("当前工作集已改变或回交未完整完成，请重新审阅原固定引用。");
  return validateWorkset({
    ...fixed,
    entries: fixed.entries.map((entry) =>
      canonicalJSON(entry) === canonicalJSON(original)
        ? { ...entry, version_id: receipt.version_id }
        : entry,
    ),
  });
}
