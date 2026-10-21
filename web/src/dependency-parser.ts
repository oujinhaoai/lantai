import {
  parseWorksetJSON,
  checkWorksetConflicts,
  worksetPath,
  worksetSHA256,
  type WorksetDependency,
} from "./workset-helpers";

export const MAX_DEPENDENCY_FILE_BYTES = 1024 * 1024;
export const MAX_DEPENDENCY_TOTAL_BYTES = 4 * 1024 * 1024;
export const MAX_DEPENDENCY_FILES = 128;
export interface DependencySource {
  path: string;
  bytes: Uint8Array;
}
export interface DependencyFileReport {
  path: string;
  sha256: string;
  format: "obj" | "mtl" | "comfy-api" | "comfy-ui" | "unsupported";
  references: string[];
  notices: string[];
  complete: boolean;
}
export interface DependencyReport {
  schema: "lantai.selected-dependency-analysis/v1";
  scope: "explicit-selected-text-files";
  complete: boolean;
  files: DependencyFileReport[];
  dependencies: WorksetDependency[];
  missing_selected_files: { from: string; path: string }[];
  comfy_input_root?: string;
}

// This lexer reads names, never shell syntax. Backslashes remain literal and
// are later rejected by the cross-platform path validator.
function words(line: string): string[] {
  const out: string[] = [];
  let value = "",
    quote = "";
  for (const char of line.trim()) {
    if (quote) {
      if (char === quote) quote = "";
      else value += char;
    } else if (char === '"' || char === "'") quote = char;
    else if (/\s/u.test(char)) {
      if (value) {
        out.push(value);
        value = "";
      }
    } else if (char === "#" && !value) break;
    else value += char;
  }
  if (quote) throw new Error("带引号的文件名未结束");
  if (value) out.push(value);
  return out;
}
function reference(from: string, value: string): string {
  // Parent traversal is reported rather than silently rebasing. The user can
  // explicitly map a supported relative dependency in the workset editor.
  worksetPath(value);
  const parent = from.includes("/")
    ? from.slice(0, from.lastIndexOf("/")) + "/"
    : "";
  return worksetPath(parent + value);
}
const objGeometry = new Set([
  "v",
  "vt",
  "vn",
  "vp",
  "f",
  "l",
  "p",
  "o",
  "g",
  "s",
  "usemtl",
]);
const mtlProperties = new Set([
  "newmtl",
  "Ka",
  "Kd",
  "Ks",
  "Ke",
  "Ns",
  "Ni",
  "illum",
  "d",
  "Tr",
  "Tf",
]);
const textureProperties = new Set([
  "map_Ka",
  "map_Kd",
  "map_Ks",
  "map_Ke",
  "map_Ns",
  "map_d",
  "map_Bump",
  "bump",
  "norm",
  "disp",
  "decal",
  "refl",
]);
const numericOptions: Record<string, [number, number]> = {
  "-o": [1, 3],
  "-s": [1, 3],
  "-t": [1, 3],
  "-bm": [1, 1],
  "-boost": [1, 1],
  "-texres": [1, 1],
  "-mm": [2, 2],
};
const singleOptions = new Set([
  "-blendu",
  "-blendv",
  "-cc",
  "-clamp",
  "-imfchan",
  "-type",
]);
function textureFile(args: string[]) {
  let i = 0;
  while (i < args.length && args[i].startsWith("-")) {
    const option = args[i++],
      numeric = numericOptions[option];
    if (numeric) {
      let count = 0;
      while (
        count < numeric[1] &&
        i < args.length &&
        /^[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?$/.test(args[i])
      ) {
        count++;
        i++;
      }
      if (count < numeric[0]) throw new Error(`MTL ${option} 参数不完整`);
    } else if (singleOptions.has(option)) {
      if (i >= args.length || args[i].startsWith("-"))
        throw new Error(`MTL ${option} 参数不完整`);
      i++;
    } else throw new Error(`不支持的 MTL 贴图选项 ${option}，未猜测文件名`);
  }
  if (args.length - i !== 1)
    throw new Error("贴图语句需要一个明确文件名；含空格请加引号");
  return args[i];
}
function meshDependencies(path: string, text: string, format: "obj" | "mtl") {
  const references = new Set<string>(),
    notices: string[] = [];
  const lines = text.split(/\r?\n/);
  if (lines.length > 10000) throw new Error("文本超过 10,000 行");
  for (const [i, line] of lines.entries()) {
    try {
      const [command, ...args] = words(line);
      if (!command) continue;
      if (format === "obj" && command === "mtllib") {
        if (!args.length) throw new Error("mtllib 缺少文件名");
        for (const name of args) references.add(reference(path, name));
      } else if (format === "mtl" && textureProperties.has(command)) {
        references.add(reference(path, textureFile(args)));
      } else if (
        !(format === "obj" ? objGeometry : mtlProperties).has(command)
      ) {
        notices.push(
          `第 ${i + 1} 行语句 ${command} 未识别；未执行或推断其依赖。`,
        );
      }
    } catch (error) {
      notices.push(
        `第 ${i + 1} 行：${error instanceof Error ? error.message : "无法解析"}`,
      );
    }
    if (references.size > 10000 || notices.length > 1000)
      throw new Error("依赖或诊断数量超限");
  }
  return { references: [...references].sort(), notices };
}
function isObject(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value);
}
const noInputFileNodes = new Set([
  "ImageScale",
  "ImageScaleBy",
  "SaveImage",
  "PreviewImage",
  "EmptyImage",
  "ImageBatch",
  "InvertImage",
  "ImagePadForOutpaint",
  "ImageCrop",
]);
function comfyDependencies(text: string, inputRoot?: string) {
  const value = parseWorksetJSON(text);
  if (!isObject(value))
    throw new Error("仅支持 ComfyUI API 图或 nodes 数组 UI 图");
  const ui = Array.isArray(value.nodes);
  const nodes = ui
    ? (value.nodes as unknown[])
    : Object.entries(value)
        .filter(([key]) => /^\d+$/.test(key))
        .map(([, node]) => node);
  if (!nodes.length || nodes.length > 4096)
    throw new Error("图需包含 1–4096 个明确节点");
  if (!ui && Object.keys(value).some((key) => !/^\d+$/.test(key)))
    throw new Error("API 图含非节点顶层字段，未假定其格式");
  const references = new Set<string>(),
    notices: string[] = [];
  for (const [i, node] of nodes.entries()) {
    if (!isObject(node)) {
      notices.push(`节点 ${i + 1} 不是对象。`);
      continue;
    }
    const type = ui ? node.type : node.class_type;
    if (type === "LoadImage") {
      const image = ui
        ? Array.isArray(node.widgets_values)
          ? node.widgets_values[0]
          : undefined
        : isObject(node.inputs)
          ? node.inputs.image
          : undefined;
      if (typeof image !== "string") {
        notices.push(`LoadImage 节点 ${i + 1} 缺少明确图片路径。`);
        continue;
      }
      try {
        if (inputRoot === undefined || inputRoot === "")
          throw new Error(
            "需显式映射 ComfyUI input 根；不将 JSON 所在目录当作输入根",
          );
        if (/\s\[(?:input|output|temp)\]$/.test(image))
          throw new Error("目录注记未自动解释，请显式映射实际输入路径");
        worksetPath(image);
        references.add(
          inputRoot === "." ? image : worksetPath(`${inputRoot}/${image}`),
        );
      } catch (error) {
        notices.push(
          `LoadImage 节点 ${i + 1}：${error instanceof Error ? error.message : "路径无效"}`,
        );
      }
    } else if (typeof type !== "string" || !noInputFileNodes.has(type)) {
      notices.push(
        `节点 ${String(type ?? i + 1)} 不在已支持范围；模型、自定义节点和其他文件输入未解析。`,
      );
    }
  }
  return {
    format: ui ? ("comfy-ui" as const) : ("comfy-api" as const),
    references: [...references].sort(),
    notices,
  };
}
export async function analyzeSelectedDependencies(
  sources: DependencySource[],
  options: { comfyInputRoot?: string } = {},
): Promise<DependencyReport> {
  if (!sources.length || sources.length > MAX_DEPENDENCY_FILES)
    throw new Error("请选择 1–128 个文本文件。");
  if (options.comfyInputRoot && options.comfyInputRoot !== ".")
    worksetPath(options.comfyInputRoot);
  const selected = new Set<string>();
  let bytes = 0;
  for (const source of sources) {
    worksetPath(source.path);
    if (selected.has(source.path)) throw new Error("选定文件路径重复。");
    selected.add(source.path);
    bytes += source.bytes.byteLength;
    if (
      source.bytes.byteLength > MAX_DEPENDENCY_FILE_BYTES ||
      bytes > MAX_DEPENDENCY_TOTAL_BYTES
    )
      throw new Error("依赖解析限制：单文件 1 MiB、总计 4 MiB。");
  }
  checkWorksetConflicts(sources.map((source) => source.path));
  const files: DependencyFileReport[] = [];
  for (const source of sources) {
    const report: DependencyFileReport = {
      path: source.path,
      sha256: await worksetSHA256(source.bytes),
      format: "unsupported",
      references: [],
      notices: [],
      complete: false,
    };
    try {
      const ext = source.path.split(".").pop()?.toLowerCase();
      if (!["obj", "mtl", "json"].includes(ext || ""))
        throw new Error(
          "不支持该格式；Blender/GLB/FBX/UE 二进制、脚本、模型和自定义节点不自动解析。",
        );
      const text = new TextDecoder("utf-8", { fatal: true }).decode(
        source.bytes,
      );
      if (text.includes("\0"))
        throw new Error("包含二进制 NUL，不作为文本解析。");
      if (ext === "obj" || ext === "mtl") {
        report.format = ext;
        Object.assign(report, meshDependencies(source.path, text, ext));
      } else
        Object.assign(report, comfyDependencies(text, options.comfyInputRoot));
      report.complete = report.notices.length === 0;
    } catch (error) {
      report.notices.push(
        error instanceof Error ? error.message : "无法解析。",
      );
    }
    files.push(report);
  }
  const dependencies = files
    .filter((file) => file.references.length > 0)
    .map((file) => ({ from: file.path, requires: file.references }));
  const missing = dependencies.flatMap((dep) =>
    dep.requires
      .filter((path) => !selected.has(path))
      .map((path) => ({ from: dep.from, path })),
  );
  return {
    schema: "lantai.selected-dependency-analysis/v1",
    scope: "explicit-selected-text-files",
    complete: files.every((file) => file.complete),
    files,
    dependencies,
    missing_selected_files: missing,
    ...(options.comfyInputRoot
      ? { comfy_input_root: options.comfyInputRoot }
      : {}),
  };
}
