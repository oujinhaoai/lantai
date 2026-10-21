import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  APIError,
  hasCSRF,
  isSessionError,
  message,
  type Identity,
} from "./api";
import {
  loadManagedRecord,
  listManagedRecords,
  type ManagedRecordRef,
} from "./managed-records";
import { ManagedRecordSaver } from "./managed-record-saver";
import { WorksetReturnVersion } from "./workset-return-version";
import {
  DeviceJobConsole,
  type DeviceAssistantResult,
  type DeviceResultContext,
} from "./device-management";
import {
  analyzeSelectedDependencies,
  MAX_DEPENDENCY_FILES,
  MAX_DEPENDENCY_FILE_BYTES,
  MAX_DEPENDENCY_TOTAL_BYTES,
  type DependencyReport,
} from "./dependency-parser";
import {
  WORKSET_SCHEMA,
  MAX_WORKSET_JSON,
  MAX_BROWSER_WORKSET_BYTES,
  MAX_BROWSER_WORKSET_FILES,
  canonicalWorksetJSON,
  parseWorksetJSON,
  validateWorkset,
  worksetID,
  planBrowserWorkset,
  pullBrowserWorkset,
  makeWorksetAssistantJob,
  checkWorksetInspection,
  checkWorksetAssistantResult,
  checkWorksetDeviceContext,
  checkDeclaredDependencies,
  type WorksetInput,
  type WorksetEntry,
  type BrowserWorksetPlan,
  type WorksetGrant,
  type WorksetAssistantJob,
  type WorksetInspection,
  type WorksetAssistantObservation,
} from "./workset-helpers";
import "./workset-management.css";

type Props = { who: Identity; expire: (error: unknown) => void; route: string };
type SavedItem = ManagedRecordRef & { title?: string };
type SelectedText = { file: File; path: string };
type StoredWorkset = {
  schema: "lantai.managed-workset/v1";
  title: string;
  input: WorksetInput;
  remote_check?: BrowserWorksetPlan;
  assistant_observation?: WorksetAssistantObservation;
};
const purposes = [
  ["archive_review", "档案查看"],
  ["reference", "参考"],
  ["production", "制作"],
  ["generative_input", "生成式输入"],
  ["raw_export", "原件导出"],
];
const initialInput = (route: string): WorksetInput => {
  const query = new URLSearchParams(route.split("?")[1]);
  const asset = query.get("asset"),
    version = query.get("version");
  return {
    schema: WORKSET_SCHEMA,
    entries:
      asset && version && worksetID.test(asset) && worksetID.test(version)
        ? [{ asset_id: asset, version_id: version, destination: "project" }]
        : [],
  };
};
const bytes = (n: number) => `${n.toLocaleString("zh-CN")} B`;
function download(name: string, value: Blob | string) {
  const blob =
    typeof value === "string"
      ? new Blob([value], { type: "application/json;charset=utf-8" })
      : value;
  const url = URL.createObjectURL(blob),
    anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = name;
  anchor.rel = "noreferrer";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
async function fileJSON(file?: File) {
  if (!file) throw new Error("请选择 JSON 文件。");
  if (file.size > MAX_WORKSET_JSON) throw new Error("JSON 文件超过 8 MiB。");
  return parseWorksetJSON(await file.text());
}
async function strictRead(path: string, signal: AbortSignal): Promise<unknown> {
  if (!path.startsWith("/api/v1/") || path.startsWith("//"))
    throw new Error("无效的同源 API 路径。");
  let response: Response;
  try {
    response = await fetch(path, {
      credentials: "same-origin",
      cache: "no-store",
      redirect: "error",
      signal,
    });
  } catch (error) {
    if (signal.aborted) throw error;
    throw new APIError("NETWORK_ERROR", 0);
  }
  if (!response.ok) {
    const value = await response.json().catch(() => ({}));
    throw new APIError(
      value.error?.code || "SERVER_ERROR",
      response.status,
      response.headers.get("X-Request-Id") || undefined,
    );
  }
  if (!response.body) throw new APIError("INVALID_RESPONSE", 0);
  const reader = response.body.getReader(),
    chunks: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const next = await reader.read();
      if (next.done) break;
      total += next.value.length;
      if (total > MAX_WORKSET_JSON)
        throw new Error("服务端 JSON 超过工作集 8 MiB 预算。");
      chunks.push(next.value);
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
  const value = new Uint8Array(total);
  let at = 0;
  for (const chunk of chunks) {
    value.set(chunk, at);
    at += chunk.length;
  }
  return parseWorksetJSON(
    new TextDecoder("utf-8", { fatal: true }).decode(value),
  );
}

export function WorksetManagement({ who, expire, route }: Props) {
  const [input, setInput] = useState<WorksetInput>(() => initialInput(route));
  const [title, setTitle] = useState("新的固定版本工作集");
  const [entrypoints, setEntrypoints] = useState("");
  const [dependencies, setDependencies] = useState("[]");
  const [json, setJSON] = useState("");
  const [plan, setPlan] = useState<BrowserWorksetPlan>();
  const [visibleFiles, setVisibleFiles] = useState(100);
  const [projectID, setProjectID] = useState("");
  const [saved, setSaved] = useState<SavedItem[]>([]);
  const [nextRecords, setNextRecords] = useState<string>();
  const [savedRef, setSavedRef] = useState<ManagedRecordRef>();
  const [openAsset, setOpenAsset] = useState("");
  const [openVersion, setOpenVersion] = useState("");
  const [busy, setBusy] = useState("");
  const [error, setError] = useState<unknown>();
  const [notice, setNotice] = useState("");
  const [purpose, setPurpose] = useState("archive_review");
  const [progress, setProgress] = useState("");
  const [browserReceipt, setBrowserReceipt] = useState<unknown>();
  const [selected, setSelected] = useState<SelectedText[]>([]);
  const [analysis, setAnalysis] = useState<DependencyReport>();
  const [comfyInputRoot, setComfyInputRoot] = useState("");
  const [directory, setDirectory] = useState("working-copy");
  const [output, setOutput] = useState("workset-result.json");
  const [job, setJob] = useState<WorksetAssistantJob>();
  const [inspection, setInspection] = useState<WorksetInspection>();
  const [observation, setObservation] = useState<WorksetAssistantObservation>();
  const [devicePending, setDevicePending] = useState(false);
  const [deviceObservation, setDeviceObservation] = useState<{
    result: DeviceAssistantResult;
    context: DeviceResultContext;
  }>();
  const deviceRequest = useCallback(
    async <T,>(path: string, options?: RequestInit): Promise<T> => {
      try {
        return await api<T>(path, options);
      } catch (cause) {
        if (isSessionError(cause)) expire(cause);
        throw cause;
      }
    },
    [expire],
  );
  const request = useRef<AbortController | null>(null);
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      request.current?.abort();
    };
  }, []);
  const report = (value: unknown) => {
    if (isSessionError(value)) expire(value);
    if (value instanceof APIError && [401, 403, 404].includes(value.status)) {
      setPlan(undefined);
      setBrowserReceipt(undefined);
      setObservation(undefined);
      setSaved([]);
      setNextRecords(undefined);
    }
    setError(value);
  };
  const invalidate = () => {
    request.current?.abort();
    request.current = null;
    setBusy("");
    setPlan(undefined);
    setVisibleFiles(100);
    setJob(undefined);
    setInspection(undefined);
    setObservation(undefined);
    setDeviceObservation(undefined);
    setBrowserReceipt(undefined);
    setProgress("");
    setNotice("");
    setError(undefined);
  };
  const assemble = () =>
    validateWorkset({
      ...input,
      entrypoints: entrypoints
        .split(/\r?\n/)
        .map((s) => s.trim())
        .filter(Boolean),
      dependencies: parseWorksetJSON(dependencies),
    });
  const action = async (
    name: string,
    work: (signal: AbortSignal) => Promise<void>,
  ) => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setBusy(name);
    setProgress("");
    setError(undefined);
    setNotice("");
    try {
      await work(controller.signal);
    } catch (value) {
      if (!controller.signal.aborted && mounted.current) report(value);
    } finally {
      if (mounted.current && request.current === controller) {
        setBusy("");
        request.current = null;
      }
    }
  };
  const replace = (value: unknown) => {
    const next = validateWorkset(value);
    invalidate();
    setInput(next);
    setEntrypoints((next.entrypoints || []).join("\n"));
    setDependencies(JSON.stringify(next.dependencies || [], null, 2));
  };
  const check = () =>
    void action("检查固定清单", async (signal) => {
      const current = assemble();
      setPlan(undefined);
      setBrowserReceipt(undefined);
      setObservation(undefined);
      const next = await planBrowserWorkset(
        current,
        location.origin,
        strictRead,
        signal,
      );
      if (!signal.aborted) {
        setPlan(next);
        setInput(current);
        setNotice(
          "当前远端固定清单与声明依赖已核对。本机目标目录尚未检查，也未声明工程可运行。",
        );
      }
    });
  const updateEntry = (
    index: number,
    key: keyof WorksetEntry,
    value: string,
  ) => {
    invalidate();
    setInput((old) => ({
      ...old,
      entries: old.entries.map((e, i) =>
        i === index ? { ...e, [key]: value } : e,
      ),
    }));
  };
  const load = (ref: ManagedRecordRef) =>
    void action("打开工作集记录", async (signal) => {
      setPlan(undefined);
      setSavedRef(undefined);
      const stored = await loadManagedRecord<StoredWorkset>(ref, signal, {
        kind: "workset",
        projectID: projectID || undefined,
      });
      if (
        stored.schema !== "lantai.managed-workset/v1" ||
        typeof stored.title !== "string"
      )
        throw new Error("该固定记录不是工作集档案。");
      const next = validateWorkset(stored.input);
      const current = await planBrowserWorkset(
        next,
        location.origin,
        strictRead,
        signal,
      );
      if (signal.aborted) return;
      setTitle(stored.title);
      setInput(next);
      setEntrypoints((next.entrypoints || []).join("\n"));
      setDependencies(JSON.stringify(next.dependencies || [], null, 2));
      setPlan(current);
      setSavedRef(ref);
      setJob(undefined);
      setInspection(undefined);
      setObservation(undefined);
      setBrowserReceipt(undefined);
      setNotice("已重新核对当前远端；档案内旧验证或本机结果未作为新的验收。");
    });
  const pull = () =>
    void action("拉取并校验归档", async (signal) => {
      setPlan(undefined);
      setBrowserReceipt(undefined);
      setProgress("重新核对全部固定清单…");
      const result = await pullBrowserWorkset({
        input: assemble(),
        origin: location.origin,
        purpose,
        signal,
        read: strictRead,
        grant: (entry, file, use, signal) =>
          api<WorksetGrant>(
            `/api/v1/assets/${entry.asset_id}/versions/${entry.version_id}/read-grants`,
            {
              method: "POST",
              body: JSON.stringify({ path: file.source_path, purpose: use }),
              signal,
            },
          ),
        transfer: async (url, signal) => {
          const response = await fetch(url, {
            credentials: "same-origin",
            cache: "no-store",
            redirect: "error",
            signal,
          });
          if (!response.ok) {
            const body = await response.json().catch(() => ({}));
            throw new APIError(
              body.error?.code || "SERVER_ERROR",
              response.status,
            );
          }
          return response;
        },
        progress: (done, total) => {
          if (!signal.aborted)
            setProgress(`${done} / ${total} 文件已核对完整 SHA-256`);
        },
      });
      if (signal.aborted) return;
      setPlan(result.plan);
      setBrowserReceipt(result.receipt);
      download("lantai-fixed-workset.tar", result.archive);
      setNotice(
        "全部固定文件已校验，已交给浏览器下载 TAR。未检查或写入本机工作目录，不代表软件已打开或工程已续做。",
      );
    });
  const listRecords = (cursor?: string) =>
    void action("列出工作集记录", async (signal) => {
      setSaved([]);
      setNextRecords(undefined);
      const result = await listManagedRecords({
        projectID,
        kind: "workset",
        cursor,
        signal,
      });
      if (!signal.aborted) {
        setSaved(
          result.items.map((item) => ({
            asset_id: item.asset_id,
            version_id: item.version_id,
            title: item.title,
          })),
        );
        setNextRecords(result.next_cursor);
        setNotice(
          result.next_cursor
            ? "仅显示本页记录，可继续读取下一页；所有结果仍经当前权限过滤。"
            : "已读取当前有权查看的工作集记录。",
        );
      }
    });
  const selectText = (files: FileList | null) => {
    if (!files) return;
    setAnalysis(undefined);
    setError(undefined);
    const next = Array.from(files);
    if (
      !next.length ||
      next.length > MAX_DEPENDENCY_FILES ||
      next.some((f) => f.size > MAX_DEPENDENCY_FILE_BYTES) ||
      next.reduce((sum, f) => sum + f.size, 0) > MAX_DEPENDENCY_TOTAL_BYTES
    ) {
      report(
        new Error(
          "依赖解析仅接受 1–128 个选定文本，单个 ≤1 MiB、合计 ≤4 MiB。",
        ),
      );
      return;
    }
    setSelected(
      next.map((file) => ({
        file,
        path: file.webkitRelativePath || file.name,
      })),
    );
  };
  const parseDependencies = () =>
    void action("解析选定文本依赖", async (signal) => {
      const sources = [];
      for (const item of selected) {
        signal.throwIfAborted();
        sources.push({
          path: item.path,
          bytes: new Uint8Array(await item.file.arrayBuffer()),
        });
      }
      const result = await analyzeSelectedDependencies(sources, {
        comfyInputRoot,
      });
      if (!signal.aborted) setAnalysis(result);
    });
  const applyDependencies = () => {
    try {
      if (!analysis?.complete || !plan)
        throw new Error("先核对固定清单，并处理所有未识别或无效语句。");
      const files = new Map(
        plan.entries.flatMap((e) => e.files).map((f) => [f.path, f]),
      );
      if (analysis.files.some((f) => files.get(f.path)?.sha256 !== f.sha256))
        throw new Error(
          "选定解析文本必须逐项匹配刚核验的固定工作集路径与 SHA-256。",
        );
      const current = assemble(),
        map = new Map<string, Set<string>>();
      for (const dep of [
        ...(current.dependencies || []),
        ...analysis.dependencies,
      ]) {
        const values = map.get(dep.from) || new Set<string>();
        for (const p of dep.requires) values.add(p);
        map.set(dep.from, values);
      }
      const next = {
        ...current,
        dependencies: [...map].map(([from, requires]) => ({
          from,
          requires: [...requires].sort(),
        })),
      };
      checkDeclaredDependencies(next, [...files.keys()]);
      invalidate();
      setInput(next);
      setDependencies(JSON.stringify(next.dependencies, null, 2));
      setNotice("已加入显式依赖草稿，请重新检查固定清单。没有改写或执行工程。");
    } catch (error) {
      report(error);
    }
  };
  const exportJob = (type: "workset.plan" | "workset.pull") => {
    try {
      if (!plan) throw new Error("先核对当前固定清单。");
      const next = makeWorksetAssistantJob(
        assemble(),
        location.origin,
        type,
        directory,
        output,
        purpose,
        `workset-${crypto.randomUUID()}`,
      );
      setJob(next);
      setInspection(undefined);
      setObservation(undefined);
      // Compact JCS also makes the assistant's raw input_sha256 equal the
      // current workset input_digest; whitespace edits require new inspection.
      download("workset-job.json", canonicalWorksetJSON(next));
      setNotice(
        "仅导出显式作业，没有联系或认领设备。请在私有本机工作区先 inspect，再批准其完整摘要执行。",
      );
    } catch (error) {
      report(error);
    }
  };
  let payload: StoredWorkset | undefined;
  try {
    payload = {
      schema: "lantai.managed-workset/v1",
      title,
      input: assemble(),
      ...(plan ? { remote_check: plan } : {}),
      ...(observation ? { assistant_observation: observation } : {}),
    };
  } catch {
    /* incomplete draft is not silently archived */
  }

  return (
    <div className="workset-management">
      <header className="workset-heading">
        <p className="eyebrow">工作台 / WORKSETS</p>
        <h1 tabIndex={-1}>工程工作集</h1>
        <p>
          固定多份素材版本，还原明确的目录、入口和依赖。源件不移动，工程不执行；新版本发布不会改变本工作集。
        </p>
      </header>
      {error !== undefined && (
        <div className="error" role="alert">
          <strong>
            {error instanceof APIError
              ? message(error)
              : error instanceof Error
                ? error.message
                : "操作未完成，请检查输入后重试。"}
          </strong>
          {error instanceof APIError && error.requestID && (
            <small>请求编号：{error.requestID}</small>
          )}
        </div>
      )}
      <div
        role="status"
        aria-live="polite"
        aria-atomic="true"
        className="workset-status"
      >
        {busy ? `${busy}… ${progress}` : notice}
      </div>
      {busy && (
        <button
          type="button"
          className="secondary"
          onClick={() => {
            request.current?.abort();
            setNotice(
              "已请求停止本页读取；已完成的本机文件不被回滚。等待当前操作退出。",
            );
          }}
        >
          取消当前操作
        </button>
      )}

      <section className="panel" aria-labelledby="workset-definition-title">
        <h2 id="workset-definition-title">1. 定义固定版本</h2>
        <label>
          工作集名称
          <input
            value={title}
            disabled={devicePending || !!busy}
            maxLength={200}
            onChange={(e) => setTitle(e.target.value)}
          />
        </label>
        {input.entries.map((entry, index) => (
          <fieldset
            key={index}
            className="workset-entry"
            disabled={!!busy || devicePending}
          >
            <legend>版本 {index + 1}</legend>
            <label>
              资产 ID
              <input
                value={entry.asset_id}
                spellCheck={false}
                autoComplete="off"
                onChange={(e) =>
                  updateEntry(index, "asset_id", e.target.value.trim())
                }
              />
            </label>
            <label>
              固定版本 ID
              <input
                value={entry.version_id}
                spellCheck={false}
                autoComplete="off"
                onChange={(e) =>
                  updateEntry(index, "version_id", e.target.value.trim())
                }
              />
            </label>
            <label>
              独占相对目录
              <input
                value={entry.destination}
                spellCheck={false}
                onChange={(e) =>
                  updateEntry(index, "destination", e.target.value)
                }
                placeholder="例如 project 或 textures"
              />
            </label>
            <button
              type="button"
              className="text-button"
              onClick={() => {
                invalidate();
                setInput((old) => ({
                  ...old,
                  entries: old.entries.filter((_, i) => i !== index),
                }));
              }}
            >
              移除此条
            </button>
          </fieldset>
        ))}
        <button
          type="button"
          className="secondary"
          disabled={!!busy || devicePending || input.entries.length >= 1000}
          onClick={() => {
            invalidate();
            setInput((old) => ({
              ...old,
              entries: [
                ...old.entries,
                { asset_id: "", version_id: "", destination: "" },
              ],
            }));
          }}
        >
          添加固定版本
        </button>
        <label>
          入口文件（每行一条完整工作集相对路径）
          <textarea
            rows={3}
            value={entrypoints}
            disabled={devicePending || !!busy}
            onChange={(e) => {
              invalidate();
              setEntrypoints(e.target.value);
            }}
            placeholder="project/scene.blend"
          />
        </label>
        <label>
          显式依赖 JSON
          <textarea
            rows={5}
            className="workset-code"
            value={dependencies}
            disabled={devicePending || !!busy}
            onChange={(e) => {
              invalidate();
              setDependencies(e.target.value);
            }}
            spellCheck={false}
          />
        </label>
        <p className="subtle">
          不接受
          latest、路径穿越、链接别名或大小写等价目标。声明闭合只检查固定清单，不自动证明
          Blender、UE、插件或模型可运行。
        </p>
        <button
          type="button"
          className="primary"
          disabled={!!busy || devicePending}
          onClick={check}
        >
          检查当前固定清单与依赖
        </button>
        <details>
          <summary>导入／导出原工作集 JSON</summary>
          <label>
            导入工作集文件
            <input
              type="file"
              accept="application/json,.json"
              disabled={!!busy || devicePending}
              onChange={(e) => {
                const file = e.target.files?.[0];
                void action("读取工作集 JSON", async (signal) => {
                  const value = await fileJSON(file);
                  if (!signal.aborted) replace(value);
                });
                e.target.value = "";
              }}
            />
          </label>
          <label>
            或粘贴工作集 JSON
            <textarea
              rows={8}
              className="workset-code"
              value={json}
              disabled={devicePending || !!busy}
              onChange={(e) => setJSON(e.target.value)}
              spellCheck={false}
            />
          </label>
          <div className="workset-actions">
            <button
              type="button"
              className="secondary"
              disabled={!!busy || devicePending}
              onClick={() => {
                try {
                  replace(parseWorksetJSON(json));
                } catch (error) {
                  report(error);
                }
              }}
            >
              应用 JSON 到草稿
            </button>
            <button
              type="button"
              className="secondary"
              disabled={!!busy || devicePending}
              onClick={() => {
                try {
                  download("workset.json", JSON.stringify(assemble(), null, 2));
                } catch (error) {
                  report(error);
                }
              }}
            >
              导出原工作集
            </button>
          </div>
        </details>
      </section>

      {plan && (
        <section className="panel" aria-labelledby="workset-plan-title">
          <h2 id="workset-plan-title">2. 本次远端核验</h2>
          <p className="notice">
            固定清单与声明依赖已核对；本机目录未检查。以下记录仅代表{" "}
            {new Date(plan.checked_at).toLocaleString("zh-CN")}{" "}
            的观察，后续读取仍重新授权。
          </p>
          <dl className="workset-facts">
            <dt>馆实例</dt>
            <dd>
              <code>{plan.instance_id}</code>
            </dd>
            <dt>工作集摘要</dt>
            <dd>
              <code>{plan.input_digest}</code>
            </dd>
            <dt>逻辑量</dt>
            <dd>
              {plan.file_count} 文件 · {bytes(plan.total_bytes)}
            </dd>
            <dt>本机目录／容量</dt>
            <dd>
              未知；浏览器不探测本机磁盘。原生冲突、空间及续传仍由显式本机作业核对。
            </dd>
          </dl>
          <div className="workset-table-scroll">
            <table>
              <caption>
                固定版本的目标文件：已核对全部 {plan.file_count} 项，当前显示前{" "}
                {Math.min(visibleFiles, plan.file_count)} 项
              </caption>
              <thead>
                <tr>
                  <th scope="col">路径</th>
                  <th scope="col">字节</th>
                  <th scope="col">SHA-256</th>
                </tr>
              </thead>
              <tbody>
                {plan.entries
                  .flatMap((e) => e.files)
                  .slice(0, visibleFiles)
                  .map((file) => (
                    <tr key={file.path}>
                      <th scope="row">{file.path}</th>
                      <td>{file.size.toLocaleString("zh-CN")}</td>
                      <td>
                        <code>{file.sha256}</code>
                      </td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
          {visibleFiles < plan.file_count && (
            <button
              type="button"
              className="secondary"
              onClick={() => setVisibleFiles((n) => n + 100)}
            >
              再显示 100 项已核对文件
            </button>
          )}
          <div className="workset-actions">
            <button
              type="button"
              className="secondary"
              onClick={() =>
                download(
                  "workset-remote-check.json",
                  JSON.stringify(plan, null, 2),
                )
              }
            >
              导出本次远端核验
            </button>
          </div>
        </section>
      )}

      <section className="panel" aria-labelledby="workset-pull-title">
        <h2 id="workset-pull-title">3. 拉取固定工作副本</h2>
        <label>
          读取用途
          <select
            value={purpose}
            disabled={devicePending || !!busy}
            onChange={(e) => {
              setPurpose(e.target.value);
              setJob(undefined);
              setInspection(undefined);
              setObservation(undefined);
            }}
          >
            {purposes.map(([value, name]) => (
              <option value={value} key={value}>
                {name}
              </option>
            ))}
          </select>
        </label>
        <p>
          用途由服务器裁决，拒绝时不会自动换用途或扩权。普通标签不代表正式生产许可。
        </p>
        <h3>小型浏览器导出</h3>
        <p>
          上限 {MAX_BROWSER_WORKSET_FILES} 文件、
          {bytes(MAX_BROWSER_WORKSET_BYTES)}
          。每次重新检查固定清单与当前权限、逐文件散列后才生成保留相对目录的
          TAR；取消或错误不发布半包。浏览器没有本机目录句柄，不声称已检查本机冲突、续传或软件可打开。
        </p>
        {!hasCSRF() && (
          <p className="notice">
            本标签页没有签发读取授权所需的 CSRF，请回原登录标签页或重新验证。
          </p>
        )}
        <button
          type="button"
          className="primary"
          disabled={!plan || !!busy || devicePending || !hasCSRF()}
          onClick={pull}
        >
          拉取并校验 TAR
        </button>
        {browserReceipt !== undefined && (
          <button
            type="button"
            className="secondary"
            onClick={() =>
              download(
                "workset-browser-export-receipt.json",
                JSON.stringify(browserReceipt, null, 2),
              )
            }
          >
            导出归档校验回执
          </button>
        )}
        <h3>原生目录检查、流式拉取与断点续传</h3>
        <p>
          选择已授权设备与工作区，取得实际本机检查后批准完整摘要，再派发工作集作业。
          也可导出一次性作业手动运行。原生客户端先检查全部目标，再读取授权并下载；不同内容不覆盖。
        </p>
        <div className="workset-grid">
          <label>
            工作区内相对目录
            <input
              value={directory}
              disabled={devicePending || !!busy}
              onChange={(e) => {
                setDirectory(e.target.value);
                setJob(undefined);
                setInspection(undefined);
                setObservation(undefined);
              }}
            />
          </label>
          <label>
            独立结果 JSON 相对路径
            <input
              value={output}
              disabled={devicePending || !!busy}
              onChange={(e) => {
                setOutput(e.target.value);
                setJob(undefined);
                setInspection(undefined);
                setObservation(undefined);
              }}
            />
          </label>
        </div>
        <div className="workset-actions">
          <button
            type="button"
            className="secondary"
            disabled={!plan || !!busy || devicePending}
            onClick={() => exportJob("workset.plan")}
          >
            导出本机预检作业
          </button>
          <button
            type="button"
            className="secondary"
            disabled={!plan || !!busy || devicePending}
            onClick={() => exportJob("workset.pull")}
          >
            导出本机拉取作业
          </button>
        </div>
        {job && (
          <div className="workset-assistant">
            <label>
              设备作业归属项目（显式选择）
              <input
                value={projectID}
                list="workset-projects"
                disabled={devicePending || !!busy}
                onChange={(e) => setProjectID(e.target.value.trim())}
              />
            </label>
            {worksetID.test(projectID) && (
              <DeviceJobConsole
                request={deviceRequest}
                projectID={projectID}
                job={job}
                disabled={!!busy}
                onPendingChange={setDevicePending}
                onInspection={(value) => {
                  void action("核对设备实际检查", async (signal) => {
                    setInspection(undefined);
                    setObservation(undefined);
                    if (!plan) throw new Error("先核验当前工作集。");
                    const checked = await checkWorksetInspection(
                      value,
                      job,
                      plan.input_digest,
                    );
                    if (!signal.aborted) {
                      setInspection(checked);
                      setObservation(undefined);
                    }
                  });
                }}
                onResult={(value, context) =>
                  action("核对设备执行结果", async (signal) => {
                    setDeviceObservation({ result: value, context });
                    setObservation(undefined);
                    if (!inspection)
                      throw new Error("先核对同一工作集的原本机检查。");
                    checkWorksetDeviceContext(context, inspection);
                    const current = await planBrowserWorkset(
                      assemble(),
                      location.origin,
                      strictRead,
                      signal,
                    );
                    try {
                      const checked = checkWorksetAssistantResult(
                        value,
                        inspection,
                        current,
                      );
                      if (!signal.aborted) {
                        setPlan(current);
                        setObservation(checked);
                        setNotice(
                          checked.native_files_verified
                            ? "设备回执逐项绑定固定版本，当前远端已复核。原生软件续做仍需独立证据。"
                            : "设备结果已保留；预检或未完成结果不计作文件拉取完成。",
                        );
                      }
                    } catch (cause) {
                      setObservation(undefined);
                      setNotice(
                        "设备结果或私有结果引用已保留，尚未通过固定文件核验。若是result-ref，请在本机打开后显式导入完整执行结果；不把引用当作计划或拉取完成。",
                      );
                      throw cause;
                    }
                  })
                }
              />
            )}
            {deviceObservation && (
              <button
                type="button"
                className="secondary"
                onClick={() =>
                  download(
                    "workset-device-result-observation.json",
                    JSON.stringify(deviceObservation, null, 2),
                  )
                }
              >
                下载原设备结果／引用及作业关联
              </button>
            )}
            <p>
              将作业保存为私有工作区内的私有普通文件，先检查；检查摘要绑定该工作区和原始作业字节。改动任何输入后必须重新检查，不删改旧回执来续办。
            </p>
            <pre>
              lantai assistant inspect --job workset-job.json --workspace
              /absolute/private-workspace
            </pre>
            <label>
              导入该作业 inspect 回执
              <input
                type="file"
                accept="application/json,.json"
                disabled={!!busy || devicePending}
                onChange={(e) => {
                  const file = e.target.files?.[0];
                  void action("核对本机检查回执", async (signal) => {
                    if (!plan) throw new Error("先核验工作集。");
                    const result = await checkWorksetInspection(
                      await fileJSON(file),
                      job,
                      plan.input_digest,
                    );
                    if (!signal.aborted) {
                      setInspection(result);
                      setObservation(undefined);
                      setNotice(
                        "检查回执与同一作业绑定。此文件是本机观察，不是设备认证或当前文件存在证明。",
                      );
                    }
                  });
                  e.target.value = "";
                }}
              />
            </label>
            {inspection && (
              <>
                <p>
                  完整批准摘要：<code>{inspection.approval_digest}</code>
                </p>
                <pre>{`lantai assistant run --job workset-job.json --workspace /absolute/private-workspace --approve ${inspection.approval_digest} --session-file /absolute/private-session.json`}</pre>
                <p className="subtle">
                  私有 CA 如有需要通过已有 --ca-file 明确指定；没有跳过 TLS
                  的选项。会话文件留在本机，不上传到网页。
                </p>
                <label>
                  导入本机执行结果
                  <input
                    type="file"
                    accept="application/json,.json"
                    disabled={!!busy || devicePending}
                    onChange={(e) => {
                      const file = e.target.files?.[0];
                      void action("核对本机执行结果", async (signal) => {
                        if (deviceObservation)
                          checkWorksetDeviceContext(
                            deviceObservation.context,
                            inspection,
                          );
                        const current = await planBrowserWorkset(
                          assemble(),
                          location.origin,
                          strictRead,
                          signal,
                        );
                        const result = checkWorksetAssistantResult(
                          await fileJSON(file),
                          inspection,
                          current,
                        );
                        if (!signal.aborted) {
                          setPlan(current);
                          setObservation(result);
                          setNotice(
                            result.native_files_verified
                              ? "导入文件仅作为操作者提供的本机历史观察：报告原固定文件已核验，当前远端已另行复核；它本身不证明设备核心可信完成，也不证明软件续做通过。"
                              : "已保留操作者提供的本机预检或未完成历史观察，没有记作文件拉取完成或核心可信完成。",
                          );
                        }
                      });
                      e.target.value = "";
                    }}
                  />
                </label>
              </>
            )}
          </div>
        )}
        {observation && (
          <div className="notice">
            <strong>
              {observation.native_files_verified
                ? "本机逐文件完成（历史观察）"
                : "本机结果，尚未证明拉取完成"}
            </strong>
            <p>
              {observation.status} · 退出码 {observation.exit_code} ·{" "}
              {new Date(observation.finished_at).toLocaleString("zh-CN")}
            </p>
            <p>
              源件删除、自动换键重试及新凭据签发均未执行。取消或 partial
              不等于回滚或完成。
            </p>
          </div>
        )}
      </section>

      <section className="panel" aria-labelledby="workset-dependency-title">
        <h2 id="workset-dependency-title">4. 解析选定工程文本依赖</h2>
        <p>
          支持 OBJ 的 mtllib、MTL 贴图语句及常见显式选项、ComfyUI API/UI 图中的
          LoadImage 和列出的基础图像节点。只读选择的 UTF-8 文本；单文件 ≤1
          MiB，总计 ≤4 MiB，最多 128
          文件。未知语句、自定义节点、模型输入、绝对或父级路径会留下诊断，不猜测或执行。Blender/FBX/GLB/UE
          二进制不支持自动解析。
        </p>
        <label>
          明确选择 OBJ／MTL／工作流 JSON
          <input
            type="file"
            multiple
            accept=".obj,.mtl,.json"
            disabled={!!busy || devicePending}
            onChange={(e) => selectText(e.target.files)}
          />
        </label>
        <label>
          ComfyUI input 根在工作集中的相对目录
          <input
            value={comfyInputRoot}
            placeholder="例如 project/input；仅明确的工作集根可填 ."
            onChange={(e) => {
              setAnalysis(undefined);
              setComfyInputRoot(e.target.value);
            }}
          />
        </label>
        <p className="subtle">
          LoadImage 相对于 ComfyUI input 根，不默认相对于工作流
          JSON。未明确映射时保留未解决项；不查找真实软件目录。
        </p>
        {selected.map((item, i) => (
          <label key={`${i}-${item.file.name}`}>
            {item.file.name} 在工作集中的完整相对路径
            <input
              value={item.path}
              onChange={(e) => {
                setAnalysis(undefined);
                setSelected((old) =>
                  old.map((entry, index) =>
                    index === i ? { ...entry, path: e.target.value } : entry,
                  ),
                );
              }}
            />
          </label>
        ))}
        <button
          type="button"
          className="secondary"
          disabled={!!busy || !selected.length}
          onClick={parseDependencies}
        >
          只读解析选定文本
        </button>
        {analysis && (
          <>
            <p className="notice">
              {analysis.complete
                ? "选定文本的已支持语句已解析。"
                : "存在未识别或无效内容，不能宣称解析完整。"}{" "}
              这不代表整个工程依赖闭合或可运行；最终以固定清单逐项核对。
            </p>
            <ul>
              {analysis.files.map((file) => (
                <li key={file.path}>
                  <strong>{file.path}</strong> · {file.format} ·{" "}
                  {file.references.length} 项引用
                  {file.notices.length > 0 && (
                    <ul>
                      {file.notices.map((n, i) => (
                        <li key={i}>{n}</li>
                      ))}
                    </ul>
                  )}
                </li>
              ))}
            </ul>
            <div className="workset-table-scroll">
              <table>
                <caption>发现的声明候选，尚未自动写入工作集</caption>
                <thead>
                  <tr>
                    <th scope="col">来源</th>
                    <th scope="col">必需文件</th>
                  </tr>
                </thead>
                <tbody>
                  {analysis.dependencies.flatMap((d) =>
                    d.requires.map((p) => (
                      <tr key={`${d.from}-${p}`}>
                        <td>{d.from}</td>
                        <td>{p}</td>
                      </tr>
                    )),
                  )}
                </tbody>
              </table>
            </div>
            {analysis.missing_selected_files.length > 0 && (
              <p className="subtle">
                部分引用不在本次所选解析文本中，可能已在固定清单，也可能缺失；不会据此自动上传或查找外部文件。
              </p>
            )}
            <button
              type="button"
              className="secondary"
              disabled={!plan || !analysis.complete || !!busy || devicePending}
              onClick={applyDependencies}
            >
              核对文本摘要后加入依赖草稿
            </button>
            <button
              type="button"
              className="secondary"
              onClick={() =>
                download(
                  "selected-dependency-analysis.json",
                  JSON.stringify(analysis, null, 2),
                )
              }
            >
              导出解析报告
            </button>
          </>
        )}
      </section>

      {payload && (
        <WorksetReturnVersion
          who={who}
          expire={expire}
          input={payload.input}
          disabled={devicePending}
          onRevision={(next) => {
            replace(next);
            setSavedRef(undefined);
            setTitle((old) => `${old} · 新版本修订`);
            setNotice(
              "已明确选择新的固定版本。请重新核验目录和依赖，另存工作集修订；旧记录未覆盖。",
            );
          }}
        />
      )}
      <section className="panel" aria-labelledby="workset-save-title">
        <h2 id="workset-save-title">5. 保存／打开馆内工作集记录</h2>
        <p>
          保存的是新生成的私人工作集元数据记录，采用明确的
          LicenseRef-Lantai-Private-Record／reference、禁止原件导出和 noai
          条款；这不会给所引用素材重新赋许可。原计划、固定引用和观察范围进入普通
          config 资产及完整备份。记录内旧回执不是新授权。
        </p>
        <label>
          归档项目 ID
          <input
            value={projectID}
            disabled={devicePending || !!busy}
            list="workset-projects"
            onChange={(e) => {
              setProjectID(e.target.value.trim());
              setSaved([]);
              setNextRecords(undefined);
            }}
            spellCheck={false}
          />
        </label>
        <datalist id="workset-projects">
          {Object.keys(who.project_roles || {}).map((id) => (
            <option value={id} key={id} />
          ))}
        </datalist>
        {payload && (
          <ManagedRecordSaver
            who={who}
            expire={expire}
            projectID={projectID}
            title={title}
            kind="workset"
            payload={payload}
            onSaved={(ref) => setSavedRef(ref)}
          />
        )}
        {!payload && (
          <p className="notice">
            完成有效工作集草稿后才可保存；不会把空白或无效输入归档。
          </p>
        )}
        {savedRef && (
          <p>
            固定记录：
            <a
              href={`/#/assets/${savedRef.asset_id}?version=${savedRef.version_id}`}
            >
              {savedRef.asset_id} / {savedRef.version_id}
            </a>
          </p>
        )}
        <button
          type="button"
          className="secondary"
          disabled={!!busy || !worksetID.test(projectID)}
          onClick={() => listRecords()}
        >
          读取项目工作集记录
        </button>
        {nextRecords && (
          <button
            type="button"
            className="secondary"
            disabled={!!busy || devicePending}
            onClick={() => listRecords(nextRecords)}
          >
            读取下一页工作集记录
          </button>
        )}
        {saved.length > 0 && (
          <ul>
            {saved.map((item) => (
              <li key={`${item.asset_id}-${item.version_id}`}>
                <button
                  type="button"
                  className="text-button"
                  disabled={!!busy || devicePending}
                  onClick={() => load(item)}
                >
                  {item.title || item.asset_id}
                </button>
                <small> {item.version_id}</small>
              </li>
            ))}
          </ul>
        )}
        <details>
          <summary>按固定记录 ID 打开</summary>
          <div className="workset-grid">
            <label>
              记录资产 ID
              <input
                value={openAsset}
                onChange={(e) => setOpenAsset(e.target.value.trim())}
                spellCheck={false}
              />
            </label>
            <label>
              记录版本 ID
              <input
                value={openVersion}
                onChange={(e) => setOpenVersion(e.target.value.trim())}
                spellCheck={false}
              />
            </label>
          </div>
          <button
            type="button"
            className="secondary"
            disabled={
              !!busy ||
              devicePending ||
              !worksetID.test(openAsset) ||
              !worksetID.test(openVersion)
            }
            onClick={() =>
              load({ asset_id: openAsset, version_id: openVersion })
            }
          >
            打开并重新核验当前远端
          </button>
        </details>
      </section>
    </div>
  );
}
