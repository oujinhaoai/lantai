import { useEffect, useRef, useState } from "react";
import {
  hasCSRF,
  isSessionError,
  message,
  APIError,
  type Identity,
} from "./api";
import {
  downloadManagedJSON,
  getExactManagedVersion,
  pushFiles,
  type ExactManagedVersion,
  type ManagedFile,
  type PushReceipt,
  type Rights,
} from "./managed-transfer";
import {
  prepareWorksetReturn,
  reviseReturnedWorkset,
  type WorksetReturn,
} from "./workset-return";
import { type WorksetInput } from "./workset-helpers";

export function WorksetReturnVersion({
  who,
  expire,
  input,
  onRevision,
  disabled = false,
}: {
  who: Identity;
  expire: (error: unknown) => void;
  input: WorksetInput;
  onRevision: (next: WorksetInput) => void;
  disabled?: boolean;
}) {
  const [index, setIndex] = useState(0);
  const [baseline, setBaseline] = useState<ExactManagedVersion>();
  const [files, setFiles] = useState<ManagedFile[]>([]);
  const [purpose, setPurpose] = useState<"reference" | "production">(
    "reference",
  );
  const [usage, setUsage] = useState<Rights["usage"]>("reference");
  const [note, setNote] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [saved, setSaved] = useState(false);
  const [prepared, setPrepared] = useState<WorksetReturn>();
  const [resume, setResume] = useState("");
  const [receipt, setReceipt] = useState<PushReceipt>();
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState<unknown>();
  const active = useRef<AbortController | null>(null);
  const owner = useRef(`workset-return:${crypto.randomUUID()}`);
  const pending = useRef(false);
  const markPending = (value: boolean) => {
    pending.current = value;
    window.dispatchEvent(
      new CustomEvent("lantai:unsaved-operation", {
        detail: { owner: owner.current, pending: value },
      }),
    );
  };
  useEffect(
    () => () => {
      active.current?.abort();
      if (pending.current)
        window.dispatchEvent(
          new CustomEvent("lantai:unsaved-operation", {
            detail: { owner: owner.current, pending: false },
          }),
        );
    },
    [],
  );
  const run = async (work: (signal: AbortSignal) => Promise<void>) => {
    if (busy) return;
    const c = new AbortController();
    active.current = c;
    setBusy(true);
    setError(undefined);
    setNotice("");
    try {
      await work(c.signal);
    } catch (cause) {
      if (!c.signal.aborted) {
        setError(cause);
        if (isSessionError(cause)) expire(cause);
      }
    } finally {
      if (!c.signal.aborted) setBusy(false);
    }
  };
  const entry = input.entries[index];
  const baselineMatches =
    baseline?.manifest.asset_id === entry?.asset_id &&
    baseline?.manifest.version_id === entry?.version_id;
  const rights = baseline?.manifest.content.rights as Rights | undefined;
  const allowed =
    !disabled && who.session.scopes.includes("ingest") && hasCSRF();
  return (
    <section className="panel" aria-label="工作集新版本回交">
      <h2>回交新的素材版本</h2>
      <p>
        选择本工作集的固定资产和基线，显式选取完整的新版本文件。原工作集固定引用保留，成功后另行选择工作集修订。
      </p>
      {!allowed && (
        <p className="notice">写入需要本标签页校验及目标项目 ingest 权限。</p>
      )}
      <label>
        固定资产 / 基线 / 工作目录
        <select
          value={index}
          disabled={busy || !!prepared}
          onChange={(e) => {
            setIndex(Number(e.target.value));
            setBaseline(undefined);
            setConfirmed(false);
          }}
        >
          {input.entries.map((item, i) => (
            <option
              key={`${item.asset_id}/${item.version_id}/${item.destination}`}
              value={i}
            >
              {item.destination} · {item.asset_id} / {item.version_id}
            </option>
          ))}
        </select>
      </label>
      <button
        type="button"
        className="secondary"
        disabled={!entry || busy || !!prepared}
        onClick={() =>
          void run(async (signal) => {
            const exact = await getExactManagedVersion({
              assetID: entry.asset_id,
              versionID: entry.version_id,
              signal,
            });
            if (!signal.aborted) {
              setBaseline(exact);
              setUsage((exact.manifest.content.rights as Rights).usage);
              setConfirmed(false);
              setFiles([]);
            }
          })
        }
      >
        读取该固定基线与许可范围
      </button>
      {baseline && baselineMatches && (
        <>
          <p>
            项目 {baseline.manifest.project_id} · 类型{" "}
            {baseline.manifest.content.asset_type} · 基线摘要{" "}
            <code>{baseline.manifest.manifest_digest}</code>
          </p>
          <p>
            固定清单权利快照：{rights?.usage} · {rights?.license} · 原件转发{" "}
            {rights?.redistribute_raw ? "允许" : "禁止"} · noai{" "}
            {String(rights?.noai)}
            。最终使用权限由服务器复验；许可纠正需独立权利授权。
          </p>
          <details>
            <summary>基线完整文件（新版本不会补回未选文件）</summary>
            <ul>
              {baseline.manifest.content.files.map((file) => (
                <li key={file.path}>
                  {file.path} · {file.size} B · {file.role}
                </li>
              ))}
            </ul>
          </details>
          <label>
            选择新版本完整文件
            <input
              type="file"
              multiple
              disabled={busy || !!prepared}
              onChange={(e) =>
                setFiles(
                  Array.from(e.target.files || []).map((file) => ({
                    path: file.name,
                    role: "primary",
                    file,
                  })),
                )
              }
            />
          </label>
          {files.map((file, i) => (
            <div className="workset-fields" key={i}>
              <label>
                版本内相对路径
                <input
                  value={file.path}
                  disabled={busy || !!prepared}
                  onChange={(e) =>
                    setFiles((old) =>
                      old.map((item, n) =>
                        n === i ? { ...item, path: e.target.value } : item,
                      ),
                    )
                  }
                />
              </label>
              <label>
                角色
                <select
                  value={file.role}
                  disabled={busy || !!prepared}
                  onChange={(e) =>
                    setFiles((old) =>
                      old.map((item, n) =>
                        n === i ? { ...item, role: e.target.value } : item,
                      ),
                    )
                  }
                >
                  {[
                    "primary",
                    "source",
                    "interchange",
                    "texture",
                    "recipe",
                    "record",
                    "preview",
                    "doc",
                  ].map((role) => (
                    <option key={role}>{role}</option>
                  ))}
                </select>
              </label>
              <span>{file.file.size.toLocaleString()} B</span>
            </div>
          ))}
          <label>
            回交用途
            <select
              value={purpose}
              disabled={busy || !!prepared}
              onChange={(e) => setPurpose(e.target.value as typeof purpose)}
            >
              <option value="reference">参考</option>
              <option value="production">制作（需要制作范围）</option>
            </select>
          </label>
          <label>
            新内容使用范围（仅可收窄）
            <select
              value={usage}
              disabled={busy || !!prepared}
              onChange={(e) => setUsage(e.target.value as Rights["usage"])}
            >
              {(rights?.usage === "production"
                ? ["production", "reference", "restricted"]
                : rights?.usage === "reference"
                  ? ["reference", "restricted"]
                  : ["restricted"]
              ).map((value) => (
                <option key={value}>{value}</option>
              ))}
            </select>
          </label>
          <label>
            版本说明
            <input
              value={note}
              disabled={busy || !!prepared}
              onChange={(e) => setNote(e.target.value)}
            />
          </label>
          <label>
            <input
              type="checkbox"
              checked={confirmed}
              disabled={busy || !!prepared}
              onChange={(e) => setConfirmed(e.target.checked)}
            />
            我有权回交这些新内容，已核对全部文件与许可；未选中的基线文件不会加入新版本
          </label>
          <details>
            <summary>恢复原回交操作</summary>
            <label>
              原状态 JSON
              <textarea
                value={resume}
                rows={3}
                disabled={busy || !!prepared}
                onChange={(e) => setResume(e.target.value)}
              />
            </label>
          </details>
          <button
            type="button"
            className="secondary"
            disabled={
              !allowed || !confirmed || !files.length || busy || !!prepared
            }
            onClick={() =>
              void run(async (signal) => {
                const next = await prepareWorksetReturn({
                  entry,
                  files,
                  purpose,
                  usage,
                  note,
                  signal,
                  resume: resume || undefined,
                });
                if (!signal.aborted) {
                  setPrepared(next);
                  setSaved(false);
                  setNotice("只读准备完成。先下载并保全原状态，再明确提交。");
                }
              })
            }
          >
            准备回交及原状态
          </button>
        </>
      )}
      {prepared && (
        <>
          <button
            type="button"
            className="secondary"
            onClick={() =>
              downloadManagedJSON(
                "workset-return-original-state.json",
                prepared,
              )
            }
          >
            下载最新原回交状态
          </button>
          <label>
            <input
              type="checkbox"
              checked={saved}
              disabled={busy || disabled}
              onChange={(e) => setSaved(e.target.checked)}
            />
            我已保存原输入及create/commit键；断线或结果不明时仅沿用原状态
          </label>
          <button
            type="button"
            className="primary"
            disabled={!allowed || !saved || busy || !!receipt}
            onClick={() =>
              void run(async (signal) => {
                markPending(true);
                try {
                  const result = await pushFiles({
                    input: prepared.input,
                    files,
                    state: prepared.state,
                    signal,
                    checkpoint: async (state) => {
                      prepared.state = structuredClone(state);
                      if (!signal.aborted) setPrepared({ ...prepared });
                    },
                  });
                  if (!signal.aborted) {
                    setReceipt(result);
                    markPending(false);
                    setNotice(
                      "新版本已提交。原工作集仍保留原固定版本；下载回执后可明确创建工作集修订。",
                    );
                  }
                } catch (cause) {
                  if (!signal.aborted)
                    setNotice(
                      "保留最新原状态并按原键对账。基线冲突不自动改基线；结果不明不表示未提交。",
                    );
                  throw cause;
                }
              })
            }
          >
            {busy
              ? "处理中…"
              : prepared.state.operation_id
                ? "按原操作继续回交"
                : "上传并提交新版本"}
          </button>
        </>
      )}
      {receipt && prepared && (
        <>
          <p>
            新版本 {receipt.asset_id} / {receipt.version_id} ·{" "}
            {receipt.manifest_digest}
          </p>
          <button
            type="button"
            className="secondary"
            onClick={() =>
              downloadManagedJSON("workset-return-receipt.json", receipt)
            }
          >
            下载回交回执
          </button>
          <button
            type="button"
            className="primary"
            disabled={busy || disabled}
            onClick={() => {
              try {
                onRevision(
                  reviseReturnedWorkset(input, prepared.entry, receipt),
                );
                setNotice(
                  "已创建工作集修订草稿。请重新核验依赖并显式保存；旧档案保留。",
                );
              } catch (cause) {
                setError(cause);
              }
            }}
          >
            选择新版本创建工作集修订草稿
          </button>
        </>
      )}
      {prepared && !busy && (
        <button
          type="button"
          className="secondary"
          disabled={!saved}
          onClick={() => {
            markPending(false);
            setPrepared(undefined);
            setReceipt(undefined);
            setSaved(false);
            setConfirmed(false);
            setResume("");
            setNotice(
              "原操作由你保全。另建草稿不会撤销旧操作；先对账结果不明的操作。",
            );
          }}
        >
          原状态已保全，另建回交草稿
        </button>
      )}
      {notice && (
        <p role="status" aria-live="polite">
          {notice}
        </p>
      )}
      {busy && (
        <button
          type="button"
          className="secondary"
          onClick={() => {
            active.current?.abort();
            setBusy(false);
            setNotice(
              "已请求停止。保留最新原状态；已发出的提交结果须按原操作对账。",
            );
          }}
        >
          停止并保留原状态
        </button>
      )}
      {error != null && (
        <p role="alert" className="error">
          {error instanceof APIError
            ? `${message(error)}${["BASE_VERSION_CONFLICT", "REVISION_CONFLICT"].includes(error.code) ? "：并发条件已改变；保留原状态，不覆盖、不自动选择新基线。" : ""}`
            : error instanceof Error
              ? error.message
              : String(error)}
        </p>
      )}
    </section>
  );
}
