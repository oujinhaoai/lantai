import { useEffect, useRef, useState } from "react";
import { hasCSRF, isSessionError, message, type Identity } from "./api";
import { downloadManagedJSON, type PushState } from "./managed-transfer";
import {
  managedRecordResume,
  prepareManagedRecord,
  restoreManagedRecordResume,
  saveManagedRecord,
  type ManagedRecordKind,
  type ManagedRecordRef,
  type PreparedManagedRecord,
} from "./managed-records";

export function ManagedRecordSaver({
  who,
  expire,
  projectID,
  title,
  kind,
  payload,
  assetID,
  baseVersionID,
  onSaved,
}: {
  who: Identity;
  expire: (error: unknown) => void;
  projectID: string;
  title: string;
  kind: ManagedRecordKind;
  payload: unknown;
  assetID?: string;
  baseVersionID?: string;
  onSaved?: (
    ref: ManagedRecordRef & {
      operation_id: string;
      description_pending?: boolean;
    },
  ) => void;
}) {
  const [agreed, setAgreed] = useState(false);
  const [saved, setSaved] = useState(false);
  const [prepared, setPrepared] = useState<PreparedManagedRecord>();
  const [resumeText, setResumeText] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState<unknown>();
  const controller = useRef<AbortController | null>(null);
  const pending = useRef(false);
  const owner = useRef(`managed-record:${crypto.randomUUID()}`);
  const setPending = (value: boolean) => {
    pending.current = value;
    window.dispatchEvent(
      new CustomEvent("lantai:unsaved-operation", {
        detail: { owner: owner.current, pending: value },
      }),
    );
  };
  useEffect(
    () => () => {
      controller.current?.abort();
      if (pending.current)
        window.dispatchEvent(
          new CustomEvent("lantai:unsaved-operation", {
            detail: { owner: owner.current, pending: false },
          }),
        );
    },
    [],
  );
  const allowed = who.session.scopes.includes("ingest") && hasCSRF();
  const fail = (cause: unknown) => {
    if (isSessionError(cause)) expire(cause);
    else setError(cause);
  };
  const prepare = async () => {
    if (busy) return;
    setBusy(true);
    setError(undefined);
    setNotice("");
    const c = new AbortController();
    controller.current = c;
    try {
      const options = {
        projectID,
        title,
        kind,
        payload,
        assetID,
        baseVersionID,
        signal: c.signal,
      };
      const next = resumeText
        ? await restoreManagedRecordResume(resumeText, options)
        : await prepareManagedRecord(options);
      if (!c.signal.aborted) {
        setPrepared(next);
        setSaved(false);
        setNotice("原操作已准备，尚未发起写入。请先下载并保存原状态。");
      }
    } catch (cause) {
      if (!c.signal.aborted) fail(cause);
    } finally {
      if (!c.signal.aborted) setBusy(false);
    }
  };
  const run = async () => {
    if (busy || !prepared || !saved || !agreed) return;
    const c = new AbortController();
    controller.current = c;
    setBusy(true);
    setError(undefined);
    setPending(true);
    try {
      const result = await saveManagedRecord({
        projectID,
        title,
        kind,
        payload,
        assetID,
        baseVersionID,
        prepared,
        signal: c.signal,
        checkpoint: async (state: PushState) => {
          prepared.state = structuredClone(state);
          if (!c.signal.aborted) setPrepared({ ...prepared });
        },
      });
      if (!c.signal.aborted) {
        setPending(false);
        setNotice(
          result.description_pending
            ? "版本已提交，初始著录待完成；保留原状态，按原操作补办。"
            : `记录已提交：${result.asset_id} / ${result.version_id}`,
        );
        onSaved?.(result);
      }
    } catch (cause) {
      if (!c.signal.aborted) {
        fail(cause);
        setNotice(
          "结果可能部分完成。保留并导出原状态，仅使用原操作重试；不能据此当作未提交。",
        );
      }
    } finally {
      if (!c.signal.aborted) setBusy(false);
    }
  };
  return (
    <section className="panel managed-record-saver" aria-label="保存管理档案">
      <h3>保存馆内管理档案</h3>
      <p>
        仅保存本次生成的计划／回执；使用
        reference、LicenseRef-Lantai-Private-Record、禁止原件转发和 noai
        条款。这些条款不改变被引用素材的权利。档案与该项目资料一并纳入完整备份。
      </p>
      {!allowed && (
        <p className="notice">
          需要当前标签页校验及目标项目的 ingest 权限；完善初始著录另需
          organize。请明确选择写入范围重新登录。
        </p>
      )}
      <label>
        <input
          type="checkbox"
          checked={agreed}
          onChange={(event) => setAgreed(event.target.checked)}
          disabled={busy}
        />
        确认有权保存本次记录并采用上述私人记录条款
      </label>
      <details>
        <summary>沿用已保存的原操作状态</summary>
        <label>
          原状态 JSON
          <textarea
            rows={3}
            value={resumeText}
            onChange={(event) => setResumeText(event.target.value)}
            disabled={busy || !!prepared}
          />
        </label>
      </details>
      <button
        type="button"
        className="secondary"
        disabled={!allowed || !agreed || busy || !!prepared}
        onClick={() => void prepare()}
      >
        准备档案及原状态
      </button>
      {prepared && (
        <>
          <button
            type="button"
            className="secondary"
            onClick={() =>
              downloadManagedJSON(
                `lantai-${kind}-original-state.json`,
                managedRecordResume(prepared),
              )
            }
          >
            下载最新原操作状态
          </button>
          <label>
            <input
              type="checkbox"
              checked={saved}
              onChange={(event) => setSaved(event.target.checked)}
              disabled={busy}
            />
            我已保存原状态，断线后仍使用相同键和输入
          </label>
          <button
            type="button"
            className="primary"
            disabled={!allowed || !agreed || !saved || busy}
            onClick={() => void run()}
          >
            {busy
              ? "正在保存…"
              : prepared.state.operation_id
                ? "按原操作继续保存"
                : "保存记录"}
          </button>
        </>
      )}
      {notice && (
        <p role="status" aria-live="polite">
          {notice}
        </p>
      )}
      {error != null && (
        <p className="error" role="alert">
          {error instanceof Error && !("code" in error)
            ? error.message
            : message(error)}
        </p>
      )}
    </section>
  );
}
