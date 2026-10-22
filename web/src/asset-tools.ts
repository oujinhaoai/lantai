import {
  profileFromDraft,
  profileTags,
  type ProfileDraft,
  type AssetProfile,
} from "./asset-profile";
// Bounded data adapters only. No file execution, URL resolution or authority.
export const assetIdentifier = /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/;
export const imageByteLimit = 16 * 1024 * 1024;
export const imagePixelLimit = 16 * 1024 * 1024;
export const objLimits = {
  bytes: 2 * 1024 * 1024,
  vertices: 20000,
  edges: 60000,
  faces: 30000,
  faceVertices: 64,
  lines: 100000,
};
export function mediaToolFormat(
  path: string,
): "image" | "obj" | "unsupported-3d" | "none" {
  const ext = path.split(".").pop()?.toLowerCase() || "";
  if (["png", "jpg", "jpeg", "webp"].includes(ext)) return "image";
  if (ext === "obj") return "obj";
  return ["glb", "gltf", "fbx", "blend", "usd", "usdz", "mtl"].includes(ext)
    ? "unsupported-3d"
    : "none";
}

export type ManifestFile = {
  path: string;
  role: string;
  size: number;
  sha256: string;
};
export type FileDifference = {
  path: string;
  kind: "added" | "removed" | "changed" | "unchanged";
  before?: ManifestFile;
  after?: ManifestFile;
};

function filesByPath(files: ManifestFile[]) {
  if (!Array.isArray(files) || files.length > 10000)
    throw new Error("清单超过 10,000 文件预算。");
  const out = new Map<string, ManifestFile>();
  for (const file of files) {
    if (
      !file ||
      typeof file.path !== "string" ||
      !file.path ||
      file.path.startsWith("/") ||
      file.path
        .split("/")
        .some((part) => !part || part === "." || part === "..") ||
      file.path.includes("\\") ||
      out.has(file.path) ||
      typeof file.role !== "string" ||
      !Number.isSafeInteger(file.size) ||
      file.size < 0 ||
      !/^[a-f0-9]{64}$/.test(file.sha256)
    )
      throw new Error("清单文件的身份、路径、大小或摘要无效。");
    out.set(file.path, file);
  }
  return out;
}

export function compareFiles(
  before: ManifestFile[],
  after: ManifestFile[],
): FileDifference[] {
  const left = filesByPath(before);
  const right = filesByPath(after);
  return [...new Set([...left.keys(), ...right.keys()])].sort().map((path) => {
    const a = left.get(path);
    const b = right.get(path);
    const kind = !a
      ? "added"
      : !b
        ? "removed"
        : a.sha256 !== b.sha256 || a.size !== b.size || a.role !== b.role
          ? "changed"
          : "unchanged";
    return { path, kind, before: a, after: b };
  });
}

// A bounded textual view, never a full historical-metadata equality claim when
// truncated. This keeps deeply nested or huge arbitrary extra fields inert.
export function frozenValueText(value: unknown): {
  text: string;
  truncated: boolean;
} {
  let visited = 0,
    truncated = false;
  const clip = (input: unknown, depth: number): unknown => {
    if (++visited > 1000 || depth > 8) {
      truncated = true;
      return "…（结构已截断）";
    }
    if (typeof input === "string") {
      if (input.length > 512) {
        truncated = true;
        return input.slice(0, 512) + "…（已截断）";
      }
      return input;
    }
    if (Array.isArray(input)) {
      if (input.length > 64) truncated = true;
      return input.slice(0, 64).map((item) => clip(item, depth + 1));
    }
    if (input && typeof input === "object") {
      const result: Record<string, unknown> = Object.create(null);
      const keys = Object.keys(input).sort();
      if (keys.length > 64) truncated = true;
      for (const key of keys.slice(0, 64))
        result[key] = clip((input as Record<string, unknown>)[key], depth + 1);
      return result;
    }
    return input ?? null;
  };
  let text = JSON.stringify(clip(value, 0));
  if (text.length > 4096) {
    text = text.slice(0, 4096) + "…（已截断）";
    truncated = true;
  }
  return { text, truncated };
}

export function exactTerms(input: string) {
  const terms = input
    .split(/[\n,，]/u)
    .map((term) => term.normalize("NFC").trim())
    .filter(Boolean);
  const unique = [...new Set(terms)].sort();
  if (unique.length > 64 || unique.some((term) => [...term].length > 64))
    throw new Error("每类最多 64 项，每项最多 64 个字符。");
  return unique;
}

export type MetadataDraft = {
  title: string;
  summary: string;
  tags: string;
  subjects: string;
  profile?: ProfileDraft;
};
export type MetadataPatch = {
  title: string;
  summary: string;
  tags: string[];
  subjects: string[];
  profile?: AssetProfile;
};
export const metadataDraftLimit = 8 * 1024 * 1024;
export function metadataPatch(draft: MetadataDraft): MetadataPatch {
  const title = draft.title.normalize("NFC").trim();
  const summary = draft.summary.normalize("NFC").trim();
  if (!title || [...title].length > 200 || [...summary].length > 4000)
    throw new Error("标题为 1–200 字，摘要最多 4,000 字。");
  let tags = exactTerms(draft.tags);
  let subjects = exactTerms(draft.subjects);
  let profile: AssetProfile | undefined;
  if (draft.profile) {
    const normalized = profileFromDraft(draft.profile);
    tags = exactTerms(
      [
        ...tags.filter(
          (tag) =>
            !["character:", "state:", "source:", "legacy-version:"].some(
              (prefix) => tag.startsWith(prefix),
            ),
        ),
        ...profileTags(normalized),
      ].join("\n"),
    );
    subjects = exactTerms([...subjects, ...normalized.characters].join("\n"));
    profile = normalized;
  }
  return { title, summary, tags, subjects, ...(profile ? { profile } : {}) };
}

export type MetadataDraftRecord = {
  schema: "lantai.web-metadata-draft/v1";
  origin: string;
  asset_id: string;
  draft: MetadataDraft;
  attempt?: {
    key: string;
    revision: number;
    body: ReturnType<typeof metadataPatch>;
  };
  uncertain: boolean;
};

export function restoreMetadataDraft(
  value: unknown,
  origin: string,
  assetID: string,
): MetadataDraftRecord {
  const object = (input: unknown): input is Record<string, unknown> =>
    !!input && typeof input === "object" && !Array.isArray(input);
  if (
    !object(value) ||
    Object.keys(value).some(
      (key) =>
        ![
          "schema",
          "origin",
          "asset_id",
          "draft",
          "attempt",
          "uncertain",
        ].includes(key),
    ) ||
    value.schema !== "lantai.web-metadata-draft/v1" ||
    value.origin !== origin ||
    value.asset_id !== assetID ||
    typeof value.uncertain !== "boolean" ||
    !object(value.draft)
  )
    throw new Error("草稿不属于当前实例/素材，或格式不完整。");
  const draft = value.draft;
  if (
    Object.keys(draft).some(
      (key) =>
        !["title", "summary", "tags", "subjects", "profile"].includes(key),
    ) ||
    ["title", "summary", "tags", "subjects"].some(
      (key) => typeof draft[key] !== "string",
    )
  )
    throw new Error("草稿著录字段无效。");
  const typed = draft as MetadataDraftRecord["draft"];
  const body = metadataPatch(typed);
  let attempt: MetadataDraftRecord["attempt"];
  if (value.attempt !== undefined) {
    const saved = value.attempt;
    if (
      !object(saved) ||
      Object.keys(saved).some(
        (key) => !["key", "revision", "body"].includes(key),
      ) ||
      typeof saved.key !== "string" ||
      !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/u.test(saved.key) ||
      !Number.isSafeInteger(saved.revision) ||
      (saved.revision as number) < 0 ||
      !object(saved.body) ||
      Object.keys(saved.body).some(
        (key) =>
          !["title", "summary", "tags", "subjects", "profile"].includes(key),
      ) ||
      saved.body.title !== body.title ||
      saved.body.summary !== body.summary ||
      JSON.stringify(saved.body.tags) !== JSON.stringify(body.tags) ||
      JSON.stringify(saved.body.subjects) !== JSON.stringify(body.subjects) ||
      JSON.stringify(saved.body.profile) !== JSON.stringify(body.profile)
    )
      throw new Error("原提交键、修订或冻结内容不匹配；未产生替代操作。");
    attempt = { key: saved.key, revision: saved.revision as number, body };
  } else if (value.uncertain)
    throw new Error("结果不明的草稿缺少原提交键，不能自动恢复。");
  return {
    schema: "lantai.web-metadata-draft/v1",
    origin,
    asset_id: assetID,
    draft: typed,
    attempt,
    uncertain: !!attempt,
  };
}

export function assetSearchQuery(route: string) {
  const input = new URLSearchParams(route.split("?")[1]);
  const query = new URLSearchParams({ limit: "24" });
  const text = input.get("q")?.trim() || "";
  if (text.length > 512) throw new Error("检索文字最多 512 字符。");
  if (text) query.set("q", text);
  const project = input.get("project") || "";
  if (project && !assetIdentifier.test(project))
    throw new Error("项目标识必须是规范的 26 位大写标识。");
  if (project) query.set("project_id", project);
  const kind = input.get("type") || "";
  if (kind) query.set("asset_type", kind);
  const values = new Set<string>();
  for (const [key, prefix] of [
    ["character", "character:"],
    ["source", "source:"],
    ["state", "state:"],
    ["legacy", "legacy-version:"],
    ["tags", ""],
  ]) {
    for (const term of exactTerms(input.get(key) || "")) {
      const tag = prefix + term;
      if ([...tag].length > 64) throw new Error("组合筛选标签最多 64 字符。");
      values.add(tag);
    }
  }
  if (values.size > 64) throw new Error("组合筛选最多 64 个精确标签。");
  for (const tag of [...values].sort()) query.append("tag", tag);
  return query;
}

export type Wireframe = {
  vertices: [number, number, number][];
  edges: [number, number][];
  faces: number;
  materialLibraries: string[];
};

export function parseOBJ(source: string): Wireframe {
  if (new TextEncoder().encode(source).length > objLimits.bytes)
    throw new Error("OBJ 超过 2 MiB 线框预算。");
  for (const character of source) {
    const code = character.charCodeAt(0);
    if ((code < 32 && ![9, 10, 13].includes(code)) || code === 127)
      throw new Error("OBJ 不是可安全解析的文本。");
  }
  const lines = source.split(/\r?\n/u);
  if (lines.length > objLimits.lines) throw new Error("OBJ 行数超过预算。");
  const vertices: Wireframe["vertices"] = [];
  const edges: Wireframe["edges"] = [];
  const seen = new Set<string>();
  const materialLibraries: string[] = [];
  let faces = 0;
  for (const line of lines) {
    const tokens = line.split("#", 1)[0].trim().split(/\s+/u);
    const kind = tokens.shift();
    if (kind === "v") {
      if (tokens.length !== 3) throw new Error("线框只支持三坐标 OBJ 顶点。");
      const xyz = tokens.map(Number);
      if (xyz.some((v) => !Number.isFinite(v) || Math.abs(v) > 1e9))
        throw new Error("OBJ 顶点不是有界有限数值。");
      if (vertices.length >= objLimits.vertices)
        throw new Error("OBJ 超过 20,000 顶点预算。");
      vertices.push(xyz as [number, number, number]);
    } else if (kind === "f") {
      if (tokens.length < 3 || tokens.length > objLimits.faceVertices)
        throw new Error("OBJ 面需要 3–64 个顶点。");
      if (++faces > objLimits.faces) throw new Error("OBJ 面数超过预算。");
      const indices = tokens.map((token) => {
        if (!/^-?\d+(?:\/(?:-?\d+)?(?:\/-?\d+)?)?$/u.test(token))
          throw new Error("OBJ 面索引格式无效。");
        const raw = Number(token.split("/")[0]);
        const index = raw > 0 ? raw - 1 : vertices.length + raw;
        if (
          !Number.isSafeInteger(raw) ||
          raw === 0 ||
          index < 0 ||
          index >= vertices.length
        )
          throw new Error("OBJ 面引用了不存在的顶点。");
        return index;
      });
      for (let i = 0; i < indices.length; i++) {
        const a = indices[i];
        const b = indices[(i + 1) % indices.length];
        if (a === b) continue;
        const key = `${Math.min(a, b)}/${Math.max(a, b)}`;
        if (!seen.has(key)) {
          if (edges.length >= objLimits.edges)
            throw new Error("OBJ 超过 60,000 边预算。");
          seen.add(key);
          edges.push([a, b]);
        }
      }
    } else if (kind === "mtllib") {
      if (materialLibraries.length < 64)
        materialLibraries.push(tokens.join(" ").slice(0, 512));
      // Record only. Never fetch libraries, URLs or texture paths.
    } else if (kind === "curv" || kind === "surf" || kind === "l") {
      throw new Error("此 OBJ 包含未支持的曲线、曲面或线元素。");
    }
  }
  if (!vertices.length || !edges.length)
    throw new Error("OBJ 没有可显示的多边形线框。");
  return { vertices, edges, faces, materialLibraries };
}

export type ImageSize = { width: number; height: number; mime: string };
export function imageSize(bytes: Uint8Array): ImageSize {
  if (bytes.length > imageByteLimit) throw new Error("图像超过 16 MiB 预算。");
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const ascii = (start: number, count: number) =>
    String.fromCharCode(...bytes.subarray(start, start + count));
  let result: ImageSize | undefined;
  if (
    bytes.length >= 33 &&
    [137, 80, 78, 71, 13, 10, 26, 10].every(
      (value, index) => bytes[index] === value,
    ) &&
    view.getUint32(8) === 13 &&
    ascii(12, 4) === "IHDR"
  ) {
    result = {
      width: view.getUint32(16),
      height: view.getUint32(20),
      mime: "image/png",
    };
    for (let offset = 8; offset + 12 <= bytes.length;) {
      const length = view.getUint32(offset);
      if (length > bytes.length - offset - 12)
        throw new Error("PNG 块被截断。");
      if (ascii(offset + 4, 4) === "acTL")
        throw new Error("代理工具不处理动画 PNG。");
      offset += length + 12;
    }
  } else if (bytes.length >= 4 && bytes[0] === 255 && bytes[1] === 216) {
    for (let offset = 2; offset + 4 <= bytes.length;) {
      if (bytes[offset++] !== 255) throw new Error("JPEG 标记格式无效。");
      while (bytes[offset] === 255) offset++;
      const marker = bytes[offset++];
      if (marker === 217 || marker === 218) break;
      if (marker === 1 || (marker >= 208 && marker <= 215)) continue;
      if (offset + 2 > bytes.length) throw new Error("JPEG 被截断。");
      const length = view.getUint16(offset);
      if (length < 2 || offset + length > bytes.length)
        throw new Error("JPEG 段被截断。");
      if (
        [
          192, 193, 194, 195, 197, 198, 199, 201, 202, 203, 205, 206, 207,
        ].includes(marker)
      ) {
        if (length < 8) throw new Error("JPEG 尺寸段无效。");
        result = {
          width: view.getUint16(offset + 5),
          height: view.getUint16(offset + 3),
          mime: "image/jpeg",
        };
        break;
      }
      offset += length;
    }
  } else if (
    bytes.length >= 30 &&
    ascii(0, 4) === "RIFF" &&
    ascii(8, 4) === "WEBP"
  ) {
    const kind = ascii(12, 4);
    if (kind === "VP8X") {
      if (bytes[20] & 2) throw new Error("代理工具不处理动画 WebP。");
      result = {
        width: 1 + bytes[24] + (bytes[25] << 8) + (bytes[26] << 16),
        height: 1 + bytes[27] + (bytes[28] << 8) + (bytes[29] << 16),
        mime: "image/webp",
      };
    } else if (kind === "VP8L" && bytes[20] === 47) {
      const packed = view.getUint32(21, true);
      result = {
        width: 1 + (packed & 16383),
        height: 1 + ((packed >>> 14) & 16383),
        mime: "image/webp",
      };
    } else if (
      kind === "VP8 " &&
      bytes[23] === 157 &&
      bytes[24] === 1 &&
      bytes[25] === 42
    ) {
      result = {
        width: view.getUint16(26, true) & 16383,
        height: view.getUint16(28, true) & 16383,
        mime: "image/webp",
      };
    }
  }
  if (!result) throw new Error("代理仅支持有效的静态 PNG、JPEG 和 WebP。");
  if (
    !result.width ||
    !result.height ||
    result.width > 16384 ||
    result.height > 16384 ||
    result.width * result.height > imagePixelLimit
  )
    throw new Error("图像超过 16,777,216 像素或 16,384 边长预算。");
  return result;
}

export function fitImage(width: number, height: number, edge: number) {
  if (![320, 512, 1024, 2048].includes(edge) || width < 1 || height < 1)
    throw new Error("输出尺寸无效。");
  const factor = Math.min(1, edge / Math.max(width, height));
  return {
    width: Math.max(1, Math.round(width * factor)),
    height: Math.max(1, Math.round(height * factor)),
  };
}
