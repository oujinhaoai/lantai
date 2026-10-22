import { useEffect, useRef, useState, type FormEvent } from "react";
import { APIError, message } from "./api";
import {
  executeApproval,
  operationDiagnostic,
  prepareApproval,
  publicReceipt,
  type Approval,
  type CommandBody,
  type SensitiveResult,
  type Transport,
} from "./system-operations";

export function SystemError({ error }: { error: unknown }) {
  return (
    <div className="error" role="alert">
      <strong>
        {error instanceof APIError
          ? message(error)
          : error instanceof Error
            ? error.message
            : "操作未完成。"}
      </strong>
      {error instanceof APIError && (
        <small>
          {error.code}
          {error.requestID ? ` · 请求编号 ${error.requestID}` : ""}
        </small>
      )}
    </div>
  );
}
export function downloadDiagnostic(value: unknown, name: string) {
  const url = URL.createObjectURL(
    new Blob([JSON.stringify(value, null, 2) + "\n"], {
      type: "application/json",
    }),
  );
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
export function Receipt({ value }: { value: unknown }) {
  return (
    <details className="system-receipt" open>
      <summary>服务器操作回执</summary>
      <pre>{JSON.stringify(publicReceipt(value), null, 2)}</pre>
    </details>
  );
}
function useDialog() {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const previous = document.activeElement;
    ref.current?.focus();
    return () => {
      if (previous instanceof HTMLElement && previous.isConnected)
        previous.focus();
    };
  }, []);
  return ref;
}
export function SensitiveConfirmation({
  approval,
  request,
  retain,
  complete,
  dismiss,
}: {
  approval: Approval;
  request: Transport;
  retain: (value: Approval) => void;
  complete: (value: SensitiveResult) => void;
  dismiss: () => void;
}) {
  const dialog = useDialog();
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [lookup, setLookup] = useState<unknown>();
  const lock = useRef(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  const perform = async (action: () => Promise<void>) => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    try {
      await action();
    } catch (e) {
      if (alive.current) setError(e);
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    void perform(async () => {
      setLookup(undefined);
      const result = await executeApproval(approval, code, request, (next) => {
        setCode("");
        retain(next);
      });
      setCode("");
      if (alive.current) complete(result);
    });
  };
  const reprove = () =>
    void perform(async () => {
      const next = await prepareApproval(approval.intent, request, approval);
      if (alive.current) {
        setCode("");
        retain(next);
      }
    });
  const inspect = () =>
    void perform(async () => {
      const id = approval.childOperationID || approval.challenge.operation_id;
      const value = await request(
        `/api/v1/operations/${encodeURIComponent(id)}`,
      );
      if (alive.current) setLookup(value);
    });
  const unresolved = !!approval.executionAttempted;
  return (
    <div className="system-overlay">
      <div
        className="system-confirmation"
        ref={dialog}
        role="dialog"
        aria-modal="true"
        aria-labelledby="system-confirm-title"
        tabIndex={-1}
        onKeyDown={(e) => {
          if (e.key === "Escape" && !busy && !unresolved) {
            e.preventDefault();
            dismiss();
          }
          if (e.key === "Tab") {
            const items = Array.from(
              dialog.current?.querySelectorAll<HTMLElement>(
                "button:not(:disabled), input:not(:disabled), a[href]",
              ) || [],
            );
            const first = items[0];
            const last = items.at(-1);
            if (!first) {
              e.preventDefault();
              dialog.current?.focus();
            } else if (
              e.shiftKey &&
              (document.activeElement === first ||
                document.activeElement === dialog.current)
            ) {
              e.preventDefault();
              last?.focus();
            } else if (!e.shiftKey && document.activeElement === last) {
              e.preventDefault();
              first.focus();
            }
          }
        }}
      >
        <h2 id="system-confirm-title">{approval.intent.title}</h2>
        <p className="notice">
          请完整核对服务器固定的动作、摘要和所有目标，再输入新的动态码。权限会在最终接受时再次检查。
        </p>
        <p className="system-proof-summary">{approval.challenge.summary}</p>
        <dl>
          <dt>动作</dt>
          <dd>
            <code>{approval.challenge.action}</code>
          </dd>
          <dt>原操作</dt>
          <dd>
            <code>{approval.challenge.operation_id}</code>
          </dd>
          {approval.childOperationID && (
            <>
              <dt>子操作</dt>
              <dd>
                <code>{approval.childOperationID}</code>
              </dd>
            </>
          )}
          <dt>幂等键</dt>
          <dd>
            <code>{approval.key}</code>
          </dd>
          <dt>挑战到期</dt>
          <dd>{approval.challenge.expires_at}</dd>
        </dl>
        <table>
          <caption>服务器完整目标集合</caption>
          <thead>
            <tr>
              <th>类别</th>
              <th>目标</th>
              <th>预期修订</th>
            </tr>
          </thead>
          <tbody>
            {approval.challenge.targets.map((t, i) => (
              <tr key={`${t.kind}:${t.id}:${i}`}>
                <td>{t.kind}</td>
                <td>
                  <code>{t.id}</code>
                </td>
                <td>{t.expected_revision}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <details>
          <summary>冻结请求及领域目标</summary>
          <pre>
            {JSON.stringify(approval.actions || approval.intent, null, 2)}
          </pre>
        </details>
        {unresolved && (
          <p className="notice" role="status">
            已经发送执行请求。错误或断网可能发生在服务端接受之后；原授权和键保持不变。先核对原操作，不能另建操作。
          </p>
        )}
        {error !== undefined && <SystemError error={error} />}
        <form onSubmit={submit}>
          {!approval.grantID && (
            <label>
              确认操作的验证器动态码
              <input
                value={code}
                onChange={(e) =>
                  setCode(e.target.value.replace(/\D/g, "").slice(0, 6))
                }
                inputMode="numeric"
                autoComplete="one-time-code"
                pattern="[0-9]{6}"
                maxLength={6}
                required
                disabled={busy}
              />
            </label>
          )}
          <div className="system-actions">
            <button
              className="primary"
              disabled={busy || (!approval.grantID && code.length !== 6)}
            >
              {busy
                ? "正在处理…"
                : approval.grantID
                  ? "按原授权与键重试"
                  : "验证并执行"}
            </button>
            <button
              type="button"
              className="secondary"
              disabled={busy}
              onClick={reprove}
            >
              重新验证原操作
            </button>
            <button
              type="button"
              className="secondary"
              disabled={busy}
              onClick={inspect}
            >
              核对原操作
            </button>
            <button
              type="button"
              className="secondary"
              onClick={() =>
                downloadDiagnostic(
                  operationDiagnostic(approval, location.origin),
                  "lantai-admin-operation.json",
                )
              }
            >
              保存原操作诊断
            </button>
            <button
              type="button"
              className="text-button"
              disabled={busy || (unresolved && lookup === undefined)}
              onClick={dismiss}
            >
              {unresolved ? "已核对并关闭（不回滚）" : "关闭确认"}
            </button>
          </div>
        </form>
        {lookup !== undefined && (
          <>
            <p className="notice">
              以下仅是原操作的当前服务器观察；一次性秘密不会由对账返回。若凭据签发响应丢失，先按回执核对并吊销该凭据，再显式重新签发。
            </p>
            <Receipt value={lookup} />
          </>
        )}
      </div>
    </div>
  );
}

export interface DirectIntent {
  title: string;
  path: string;
  body: CommandBody;
  key: string;
  attempted: boolean;
}
export function DirectConfirmation({
  intent,
  request,
  retain,
  complete,
  dismiss,
}: {
  intent: DirectIntent;
  request: Transport;
  retain: (value: DirectIntent) => void;
  complete: (value: SensitiveResult) => void;
  dismiss: () => void;
}) {
  const dialog = useDialog();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [reconciled, setReconciled] = useState(false);
  const lock = useRef(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  const send = async () => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    setReconciled(false);
    retain({ ...intent, attempted: true });
    try {
      const result = await request<SensitiveResult>(intent.path, {
        method: "POST",
        headers: { "Idempotency-Key": intent.key },
        body: JSON.stringify(intent.body),
      });
      if (alive.current) complete(result);
    } catch (e) {
      if (alive.current) setError(e);
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  return (
    <div className="system-overlay">
      <div
        className="system-confirmation"
        role="dialog"
        aria-modal="true"
        aria-labelledby="system-direct-title"
        tabIndex={-1}
        ref={dialog}
        onKeyDown={(e) => {
          if (e.key === "Escape" && !busy && !intent.attempted) dismiss();
          if (e.key === "Tab") {
            const buttons = Array.from(
              dialog.current?.querySelectorAll<HTMLElement>(
                "button:not(:disabled), input:not(:disabled), a[href]",
              ) || [],
            );
            const first = buttons[0];
            const last = buttons.at(-1);
            if (
              e.shiftKey &&
              (document.activeElement === first ||
                document.activeElement === dialog.current)
            ) {
              e.preventDefault();
              last?.focus();
            } else if (!e.shiftKey && document.activeElement === last) {
              e.preventDefault();
              first?.focus();
            }
          }
        }}
      >
        <h2 id="system-direct-title">{intent.title}</h2>
        <p>核对固定请求。最终角色、scope、用途和修订由核心重新验证。</p>
        <pre>{JSON.stringify(intent.body, null, 2)}</pre>
        <p>
          幂等键：<code>{intent.key}</code>
        </p>
        {intent.attempted && (
          <p className="notice">
            已发送请求。失败可能是结果不明；继续使用相同请求与键，不新建替代操作。关闭不会回滚已接受结果。
          </p>
        )}
        {error !== undefined && <SystemError error={error} />}
        {intent.attempted && (
          <label className="system-check">
            <input
              type="checkbox"
              checked={reconciled}
              onChange={(e) => setReconciled(e.target.checked)}
              disabled={busy}
            />
            已通过当前项目或领域查询核对原请求结果；理解关闭不会回滚。如仍不明，应先保存诊断并按原键重试。
          </label>
        )}
        <div className="system-actions">
          <button
            className="primary"
            disabled={busy}
            onClick={() => void send()}
          >
            {busy
              ? "正在处理…"
              : intent.attempted
                ? "按原请求和键重试"
                : "确认执行"}
          </button>
          <button
            className="secondary"
            onClick={() =>
              downloadDiagnostic(
                {
                  schema: "lantai.admin-direct/v1",
                  origin: location.origin,
                  ...intent,
                },
                "lantai-admin-direct.json",
              )
            }
          >
            保存原请求诊断
          </button>
          <button
            className="text-button"
            disabled={busy || (intent.attempted && !reconciled)}
            onClick={dismiss}
          >
            {intent.attempted ? "已核对并关闭（不回滚）" : "关闭确认"}
          </button>
        </div>
      </div>
    </div>
  );
}
