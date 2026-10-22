import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import { hasCSRF } from "./api";
import { identifier, type Transport } from "./system-operations";
import {
  downloadDiagnostic,
  Receipt,
  SystemError,
} from "./system-confirmation";
import {
  checkedDeviceResult,
  checkedInspection,
  deviceDigest,
  deviceJobDigest,
  deviceJobUnresolved,
  frozenJob,
  newDeviceRequest,
  restoreDeviceRequest,
  sendDeviceRequest,
  validDevice,
  validDeviceJob,
  workspaceAlias,
  type Device,
  type DeviceAssistantResult,
  type DeviceInspection,
  type DeviceJob,
  type DeviceJobInput,
  type DeviceRequest,
  type DeviceResultContext,
} from "./device-protocol";
import "./device-management.css";
export type {
  DeviceJobInput,
  DeviceInspection,
  DeviceAssistantResult,
  DeviceResultContext,
} from "./device-protocol";

const deviceBase = (id: string) =>
  `/api/v1/devices/${encodeURIComponent(identifier(id))}`;
const jobBase = (device: string, job: string) =>
  `${deviceBase(device)}/jobs/${encodeURIComponent(identifier(job))}`;
const stateNames: Record<string, string> = {
  pending: "等待认领",
  active: "已认领",
  revoked: "已撤权",
  expired: "已过期，保留原作业对账",
  queued: "等待本机检查",
  awaiting_approval: "本机检查完成，等待网页批准",
  ready: "已批准，等待设备领取",
  running: "设备执行中",
  cancelling: "已请求取消，等待对账",
  succeeded: "已收到本次动作结果",
  failed: "本次动作失败",
  cancelled: "已取消",
  needs_reconciliation: "结果不明，保留原作业对账",
};
function useAlive() {
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  return alive;
}
const unresolvedDevices = new Set<symbol>();
function useUnsavedDevice(active: boolean) {
  const id = useRef(Symbol("device-operation"));
  useEffect(() => {
    const key = id.current;
    if (active) unresolvedDevices.add(key);
    else unresolvedDevices.delete(key);
    window.dispatchEvent(
      new CustomEvent("lantai:unsaved-operation", {
        detail: {
          owner: "first-party-devices",
          pending: unresolvedDevices.size > 0,
        },
      }),
    );
    const guard = (event: BeforeUnloadEvent) => {
      if (unresolvedDevices.size > 0) {
        event.preventDefault();
        event.returnValue = "";
      }
    };
    window.addEventListener("beforeunload", guard);
    return () => {
      unresolvedDevices.delete(key);
      window.removeEventListener("beforeunload", guard);
      window.dispatchEvent(
        new CustomEvent("lantai:unsaved-operation", {
          detail: {
            owner: "first-party-devices",
            pending: unresolvedDevices.size > 0,
          },
        }),
      );
    };
  }, [active]);
}
function useDeviceMutation(projectID: string, request: Transport) {
  const [pending, setPending] = useState<{
    title: string;
    intent: DeviceRequest;
    accept: (value: unknown) => void | Promise<void>;
  }>();
  const lock = useRef(false);
  const alive = useAlive();
  const start = (
    title: string,
    deviceID: string,
    path: string,
    body: Record<string, unknown>,
    accept: (value: unknown) => void | Promise<void>,
  ) => {
    if (lock.current) throw new Error("先处理当前冻结的设备请求。");
    if (!hasCSRF()) throw new Error("此标签页缺少操作校验信息，请重新验证。");
    const intent = newDeviceRequest(
      location.origin,
      projectID,
      deviceID,
      path,
      body,
    );
    lock.current = true;
    setPending({ title, intent, accept });
  };
  const restore = (
    value: unknown,
    accept: (value: unknown) => void | Promise<void>,
  ) => {
    if (lock.current) throw new Error("先处理当前冻结的设备请求。");
    const intent = restoreDeviceRequest(value, location.origin, projectID);
    if ("claim_code" in intent.body)
      throw new Error("认领代码不可从诊断恢复；请按设备原状态核对。");
    lock.current = true;
    setPending({ title: "续办原设备请求", intent, accept });
  };
  const close = () => {
    lock.current = false;
    setPending(undefined);
  };
  const unsaved = !!pending?.intent.attempted;
  useUnsavedDevice(unsaved);
  const modal = pending ? (
    <DeviceRequestConfirmation
      title={pending.title}
      intent={pending.intent}
      request={request}
      retain={(intent) => {
        if (alive.current)
          setPending((old) => (old ? { ...old, intent } : old));
      }}
      finish={async (value) => {
        if (alive.current) {
          await pending.accept(value);
          if (alive.current) close();
        }
      }}
      close={close}
    />
  ) : null;
  return { start, restore, modal, pending: !!pending };
}
function DeviceRequestConfirmation({
  title,
  intent,
  request,
  retain,
  finish,
  close,
}: {
  title: string;
  intent: DeviceRequest;
  request: Transport;
  retain: (value: DeviceRequest) => void;
  finish: (value: unknown) => void | Promise<void>;
  close: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [checked, setChecked] = useState(false);
  const [lookup, setLookup] = useState<unknown>();
  const alive = useAlive();
  const lock = useRef(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const previous = document.activeElement;
    ref.current?.focus();
    return () => {
      if (previous instanceof HTMLElement && previous.isConnected)
        previous.focus();
    };
  }, []);
  const send = async () => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setChecked(false);
    setLookup(undefined);
    setError(undefined);
    try {
      const result = await sendDeviceRequest(intent, request, retain);
      if (alive.current) await finish(result);
    } catch (err) {
      if (alive.current) setError(err);
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  const inspect = async () => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    try {
      const originalJob = intent.path.match(
        /\/jobs\/([0-7][0-9A-HJKMNP-TV-Z]{25})\/(approve|cancel)$/,
      )?.[1];
      let value: unknown;
      if (originalJob)
        value = validDeviceJob(
          await request<DeviceJob>(jobBase(intent.device_id, originalJob)),
          intent.project_id,
          intent.device_id,
        );
      else if (intent.path.endsWith("/jobs")) {
        const page = await request<{
          items: DeviceJob[];
          next_cursor?: string;
        }>(`${deviceBase(intent.device_id)}/jobs?limit=100`);
        const matches = page.items
          .map((item) =>
            validDeviceJob(item, intent.project_id, intent.device_id),
          )
          .filter(
            (item) =>
              item.job_digest === intent.body.job_digest &&
              item.job.job_id === (intent.body.job as DeviceJobInput).job_id,
          );
        value = {
          matching_original_jobs: matches,
          more_pages_exist: !!page.next_cursor,
          observation_only:
            "这里只显示当前受权范围的匹配观察；未找到不表示服务端未接受。原请求与键保持不变。",
        };
      } else
        value = validDevice(
          await request<Device>(deviceBase(intent.device_id)),
          intent.project_id,
        );
      if (alive.current) setLookup(value);
    } catch (err) {
      if (alive.current) setError(err);
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  const pairing = "claim_code" in intent.body;
  const shown = pairing
    ? { ...intent.body, claim_code: "（仅在此确认框内存中保留，不导出）" }
    : intent.body;
  return (
    <div className="system-overlay">
      <div
        className="system-confirmation device-confirmation"
        role="dialog"
        aria-modal="true"
        aria-labelledby="device-confirm-title"
        ref={ref}
        tabIndex={-1}
        onKeyDown={(event) => {
          if (event.key === "Escape" && !busy && !intent.attempted) close();
          if (event.key === "Tab") {
            const nodes = [
              ...(ref.current?.querySelectorAll<HTMLElement>(
                "button:not(:disabled),input:not(:disabled),summary",
              ) || []),
            ];
            const first = nodes[0],
              last = nodes.at(-1);
            if (
              event.shiftKey &&
              (document.activeElement === first ||
                document.activeElement === ref.current)
            ) {
              event.preventDefault();
              last?.focus();
            } else if (!event.shiftKey && document.activeElement === last) {
              event.preventDefault();
              first?.focus();
            }
          }
        }}
      >
        <h2 id="device-confirm-title">{title}</h2>
        <p>
          核对当前项目、设备、目录别名、摘要和修订。服务器在最终接受和设备领取时重新检查身份、期限、授权和
          fence。
        </p>
        <dl>
          <dt>项目</dt>
          <dd>
            <code>{intent.project_id}</code>
          </dd>
          <dt>设备</dt>
          <dd>
            <code>{intent.device_id}</code>
          </dd>
          <dt>原幂等键</dt>
          <dd>
            <code>{intent.key}</code>
          </dd>
        </dl>
        <pre>{JSON.stringify(shown, null, 2)}</pre>
        {intent.attempted && (
          <p className="notice">
            请求已发出；断网不表示服务端未接受。只能用原请求与原键重试，不能重新派发替代作业。
          </p>
        )}
        {error !== undefined && <SystemError error={error} />}
        {intent.attempted && (
          <label className="system-check">
            <input
              type="checkbox"
              checked={checked}
              onChange={(e) => setChecked(e.target.checked)}
              disabled={busy}
            />
            已按原设备或原作业查询核对；理解关闭不会撤销已接受的认领、派发或取消。
          </label>
        )}
        <div className="system-actions">
          <button
            className="primary"
            disabled={busy}
            onClick={() => void send()}
          >
            {busy
              ? "提交中…"
              : intent.attempted
                ? "按原请求与键重试"
                : "确认此固定请求"}
          </button>
          <button
            className="secondary"
            disabled={busy}
            onClick={() => void inspect()}
          >
            读取原设备或作业观察
          </button>
          <button
            className="secondary"
            onClick={() =>
              downloadDiagnostic(
                pairing
                  ? {
                      schema: "lantai.device-pairing-observation/v1",
                      origin: intent.origin,
                      project_id: intent.project_id,
                      device_id: intent.device_id,
                      key: intent.key,
                      attempted: intent.attempted,
                    }
                  : intent,
                "lantai-device-operation.json",
              )
            }
          >
            保存原请求诊断{pairing ? "（不含认领码）" : ""}
          </button>
          <button
            className="text-button"
            disabled={
              busy || (intent.attempted && (!checked || lookup === undefined))
            }
            onClick={close}
          >
            {intent.attempted ? "已核对并关闭" : "关闭确认"}
          </button>
        </div>
        {lookup !== undefined && (
          <>
            <p className="notice">
              以下仅为当前服务器观察，不证明外部副作用已回滚或请求未接受。继续按原键取得回执。
            </p>
            <Receipt value={lookup} />
          </>
        )}
      </div>
    </div>
  );
}
function DeviceFacts({ device }: { device: Device }) {
  return (
    <dl className="device-facts">
      <dt>设备</dt>
      <dd>
        {device.alias} <code>{device.device_id}</code>
      </dd>
      <dt>项目</dt>
      <dd>
        <code>{device.project_id}</code>
      </dd>
      <dt>状态 / 修订</dt>
      <dd>
        {stateNames[device.state]} / {device.revision}
      </dd>
      <dt>截止</dt>
      <dd>{device.expires_at}</dd>
      <dt>工作区目录别名</dt>
      <dd>{device.workspace_alias || "待明确批准"}</dd>
      <dt>本机工作区绑定摘要</dt>
      <dd>
        <code>{device.workspace_binding || "待本机登记"}</code>
      </dd>
      <dt>允许动作</dt>
      <dd>{device.actions.join("、")}</dd>
    </dl>
  );
}
function DeviceJobFacts({ job }: { job: DeviceJob }) {
  return (
    <div className="device-job-facts">
      <p role="status">
        {stateNames[job.state]} · 修订 {job.revision}
        {job.cancel_requested ? " · 取消已请求" : ""}
      </p>
      <dl>
        <dt>原队列作业</dt>
        <dd>
          <code>{job.job_id}</code>
        </dd>
        <dt>原操作</dt>
        <dd>
          <code>{job.operation_id}</code>
        </dd>
        <dt>原助手作业名 / 动作</dt>
        <dd>
          {job.job.job_id} / {job.job.action}
        </dd>
        <dt>工作区目录别名</dt>
        <dd>{job.workspace_alias}</dd>
        <dt>完整作业摘要</dt>
        <dd>
          <code>{job.job_digest}</code>
        </dd>
        <dt>批准摘要</dt>
        <dd>
          <code>
            {job.approval_digest ||
              job.inspection?.approval_digest ||
              "等待本机检查"}
          </code>
        </dd>
        <dt>T06 fence / 租约</dt>
        <dd>
          <pre>{JSON.stringify(job.job_fence || {}, null, 2)}</pre>
          {job.lease_expires_at || "尚无执行租约"}
        </dd>
        <dt>本次执行停止回执</dt>
        <dd>{job.stopped ? "已记录停止" : "尚未确认停止，保留原作业"}</dd>
      </dl>
      {job.failure_code && (
        <p className="notice">公开失败码：{job.failure_code}</p>
      )}
      {job.state === "needs_reconciliation" && (
        <p className="notice">
          取消、失联、撤权、过期或重启可能留下外部副作用。保留原作业、状态目录和键，不自动创建替代动作，不把
          unknown 视为成功或回滚。
        </p>
      )}
    </div>
  );
}

export interface DeviceJobConsoleProps {
  request: Transport;
  projectID: string;
  job: DeviceJobInput;
  disabled?: boolean;
  onInspection?: (inspection: DeviceInspection) => void;
  onResult?: (
    result: DeviceAssistantResult,
    context: DeviceResultContext,
  ) => void | Promise<void>;
  onPendingChange?: (pending: boolean) => void;
  onOriginalJob?: (job: DeviceJobInput) => void;
}
export function DeviceJobConsole({
  request,
  projectID,
  job,
  disabled = false,
  onInspection,
  onResult,
  onPendingChange,
  onOriginalJob,
}: DeviceJobConsoleProps) {
  const [devices, setDevices] = useState<Device[]>([]);
  const [cursor, setCursor] = useState("");
  const [selected, setSelected] = useState("");
  const [original, setOriginal] = useState<DeviceJob>();
  const [inspection, setInspection] = useState<DeviceInspection>();
  const [result, setResult] = useState<DeviceAssistantResult>();
  const [reviewed, setReviewed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [lookupDevice, setLookupDevice] = useState("");
  const [lookupJob, setLookupJob] = useState("");
  const alive = useAlive();
  const lock = useRef(false);
  const mutation = useDeviceMutation(projectID, request);
  const operationPending =
    mutation.pending || (!!original && deviceJobUnresolved(original));
  useUnsavedDevice(operationPending);
  const pendingCallback = useRef(onPendingChange);
  useEffect(() => {
    pendingCallback.current = onPendingChange;
  }, [onPendingChange]);
  useEffect(() => {
    pendingCallback.current?.(operationPending);
  }, [operationPending]);
  useEffect(
    () => () => {
      pendingCallback.current?.(false);
    },
    [],
  );
  const work = async (action: () => Promise<void>) => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    try {
      await action();
    } catch (err) {
      if (alive.current) {
        setInspection(undefined);
        setResult(undefined);
        setError(err);
      }
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  const loadDevices = useCallback(
    async (after = "") => {
      const q = new URLSearchParams({
        project_id: identifier(projectID),
        limit: "100",
      });
      if (after) q.set("after", after);
      const page = await request<{ items: Device[]; next_cursor?: string }>(
        `/api/v1/devices?${q}`,
      );
      const rows = page.items.map((value) => validDevice(value, projectID));
      if (page.next_cursor && page.next_cursor === after)
        throw new Error("设备列表游标未推进。");
      if (alive.current) {
        setDevices((old) => (after ? [...old, ...rows] : rows));
        setCursor(page.next_cursor || "");
      }
    },
    [request, projectID, alive],
  );
  useEffect(() => {
    let active = true;
    queueMicrotask(() => {
      if (active)
        void loadDevices().catch((err) => {
          if (active) {
            setDevices([]);
            setError(err);
          }
        });
    });
    return () => {
      active = false;
    };
  }, [loadDevices]);
  const observe = async (value: DeviceJob) => {
    validDeviceJob(value, projectID, value.device_id);
    if (!alive.current) return;
    // Retain the original queue/operation handles before any additional read;
    // a lost follow-up observation must never lose an accepted dispatch.
    setOriginal(value);
    setLookupDevice(value.device_id);
    setLookupJob(value.job_id);
    setReviewed(false);
    setInspection(undefined);
    setResult(undefined);
    const device = validDevice(
      await request<Device>(deviceBase(value.device_id)),
      projectID,
    );
    if (
      value.job_digest !== (await deviceJobDigest(value.job, location.origin))
    )
      throw new Error("原作业 JSON 与服务端固定摘要不符，未续办。");
    if (!alive.current) return;
    onOriginalJob?.(value.job);
    if (value.inspection) {
      const checked = await checkedInspection(value, device, location.origin);
      if (alive.current) {
        setInspection(checked);
        onInspection?.(checked);
      }
    }
    if (value.result) {
      const checked = await checkedDeviceResult(value, device, location.origin);
      if (alive.current) setResult(checked);
    }
  };
  const readOriginal = (deviceID = lookupDevice, jobID = lookupJob) =>
    void work(async () => {
      setInspection(undefined);
      setResult(undefined);
      setReviewed(false);
      const value = validDeviceJob(
        await request<DeviceJob>(jobBase(deviceID, jobID)),
        projectID,
        deviceID,
      );
      if (value.job_id !== jobID) throw new Error("服务器返回了不同的原作业。");
      await observe(value);
    });
  const stage = (fn: () => void) => {
    setError(undefined);
    try {
      fn();
    } catch (err) {
      setError(err);
    }
  };
  const create = () =>
    void work(async () => {
      if (original && deviceJobUnresolved(original))
        throw new Error("原作业尚未确定结束；先按原作业对账，不能重复派发。");
      const device = validDevice(
        await request<Device>(deviceBase(selected)),
        projectID,
      );
      if (
        device.state !== "active" ||
        Date.parse(device.expires_at) <= Date.now() ||
        !device.actions.includes(job.action)
      )
        throw new Error("设备未激活、已过期或未批准此动作。");
      const frozen = frozenJob(job, location.origin);
      const digest = await deviceJobDigest(frozen, location.origin);
      if (alive.current)
        mutation.start(
          "提交原作业供设备只读检查",
          device.device_id,
          `${deviceBase(device.device_id)}/jobs`,
          {
            expected_device_revision: device.revision,
            project_id: projectID,
            workspace_alias: workspaceAlias(device.workspace_alias),
            job: frozen,
            job_digest: digest,
          },
          (value) =>
            observe(
              validDeviceJob(value as DeviceJob, projectID, device.device_id),
            ),
        );
    });
  const importDiagnostic = (file?: File) =>
    void work(async () => {
      if (!file) return;
      if (file.size > 8 * 1024 * 1024) throw new Error("诊断文件过大。");
      const value: unknown = JSON.parse(await file.text());
      if (alive.current)
        mutation.restore(value, (out) => observe(out as DeviceJob));
    });
  return (
    <section className="device-console panel system-section">
      <h2>在已认领设备执行此固定作业</h2>
      <p>
        先提交给设备检查，再在此页批准设备返回的完整摘要。批准前不会执行；作业结果不代替服务端当前远端核验或批次接受。
      </p>
      {error !== undefined && <SystemError error={error} />}
      <details>
        <summary>本次固定作业与相对目录</summary>
        <pre>
          {JSON.stringify(
            operationPending && original ? original.job : job,
            null,
            2,
          )}
        </pre>
      </details>
      <div className="system-inline">
        <label>
          同一主体、当前项目的设备
          <select
            value={selected}
            onChange={(e) => setSelected(e.target.value)}
            disabled={busy || mutation.pending || disabled}
          >
            <option value="">请选择已认领设备</option>
            {devices.map((d) => (
              <option
                key={d.device_id}
                value={d.device_id}
                disabled={d.state !== "active"}
              >
                {d.alias} · {d.workspace_alias} · {stateNames[d.state]} ·{" "}
                {d.expires_at}
              </option>
            ))}
          </select>
        </label>
        <button
          className="secondary"
          disabled={busy || mutation.pending}
          onClick={() => void work(() => loadDevices())}
        >
          刷新可见设备
        </button>
        {cursor && (
          <button
            className="secondary"
            disabled={busy || mutation.pending}
            onClick={() => void work(() => loadDevices(cursor))}
          >
            下一页设备
          </button>
        )}
        <button
          className="primary"
          disabled={
            disabled ||
            busy ||
            mutation.pending ||
            !selected ||
            (!!original && deviceJobUnresolved(original))
          }
          onClick={create}
        >
          提交给设备检查
        </button>
      </div>
      {original && (
        <>
          <DeviceJobFacts job={original} />
          <div className="system-actions">
            <button
              className="secondary"
              disabled={busy || mutation.pending}
              onClick={() => readOriginal(original.device_id, original.job_id)}
            >
              读取原作业当前状态
            </button>
            {deviceJobUnresolved(original) && (
              <button
                className="secondary"
                disabled={disabled || busy || mutation.pending}
                onClick={() =>
                  stage(() =>
                    mutation.start(
                      "请求取消原设备作业",
                      original.device_id,
                      `${jobBase(original.device_id, original.job_id)}/cancel`,
                      { expected_revision: original.revision },
                      (value) =>
                        observe(
                          validDeviceJob(
                            value as DeviceJob,
                            projectID,
                            original.device_id,
                          ),
                        ),
                    ),
                  )
                }
              >
                请求取消原作业
              </button>
            )}
            <button
              className="secondary"
              onClick={() =>
                downloadDiagnostic(
                  {
                    schema: "lantai.device-job-observation/v1",
                    origin: location.origin,
                    project_id: projectID,
                    device_id: original.device_id,
                    job_id: original.job_id,
                    operation_id: original.operation_id,
                    job_digest: original.job_digest,
                    approval_digest: original.approval_digest,
                    job_fence: original.job_fence,
                  },
                  "lantai-device-job-observation.json",
                )
              }
            >
              保存原作业标识
            </button>
          </div>
          {inspection && (
            <div className="device-inspection">
              <h3>设备返回的精确本机检查</h3>
              <Receipt value={inspection} />
              <label className="system-check">
                <input
                  type="checkbox"
                  checked={reviewed}
                  onChange={(e) => setReviewed(e.target.checked)}
                  disabled={busy || mutation.pending || disabled}
                />
                已核对动作、输入
                SHA-256、工作区绑定、全部相对路径、网络和写入范围，并批准以上完整摘要。
              </label>
              <button
                className="primary"
                disabled={
                  disabled ||
                  busy ||
                  mutation.pending ||
                  !reviewed ||
                  original.state !== "awaiting_approval"
                }
                onClick={() =>
                  stage(() =>
                    mutation.start(
                      "批准这一次本机检查摘要",
                      original.device_id,
                      `${jobBase(original.device_id, original.job_id)}/approve`,
                      {
                        expected_revision: original.revision,
                        job_digest: inspection.job_digest,
                        approval_digest: inspection.approval_digest,
                        workspace_binding: inspection.workspace_binding,
                      },
                      (value) =>
                        observe(
                          validDeviceJob(
                            value as DeviceJob,
                            projectID,
                            original.device_id,
                          ),
                        ),
                    ),
                  )
                }
              >
                批准该摘要并允许设备领取
              </button>
            </div>
          )}
          {result && (
            <>
              <Receipt value={result} />
              {onResult && (
                <button
                  className="secondary"
                  disabled={busy || mutation.pending}
                  onClick={() =>
                    void work(async () => {
                      await onResult(result, {
                        job_id: original.job_id,
                        device_id: original.device_id,
                        workspace_alias: original.workspace_alias,
                        workspace_binding: original.workspace_binding,
                        state: original.state,
                        stopped: original.stopped,
                        job_digest: original.job_digest,
                        approval_digest: inspection!.approval_digest,
                        operation_id: original.operation_id,
                        job_fence: original.job_fence,
                      });
                    })
                  }
                >
                  将本次回执交给当前业务核对
                </button>
              )}
            </>
          )}
        </>
      )}
      <details>
        <summary>重开页面后按原队列作业对账</summary>
        <div className="system-inline">
          <label>
            原设备标识
            <input
              value={lookupDevice}
              onChange={(e) => setLookupDevice(e.target.value.trim())}
              maxLength={26}
              disabled={busy || mutation.pending}
            />
          </label>
          <label>
            原队列作业标识
            <input
              value={lookupJob}
              onChange={(e) => setLookupJob(e.target.value.trim())}
              maxLength={26}
              disabled={busy || mutation.pending}
            />
          </label>
          <button
            className="secondary"
            disabled={busy || mutation.pending || !lookupDevice || !lookupJob}
            onClick={() => readOriginal()}
          >
            按原作业读取与对账
          </button>
        </div>
        <label>
          续办原请求诊断
          <input
            type="file"
            accept="application/json,.json"
            disabled={busy || mutation.pending || disabled}
            onChange={(e) => {
              const file = e.target.files?.[0];
              e.currentTarget.value = "";
              importDiagnostic(file);
            }}
          />
        </label>
      </details>
      {mutation.modal}
    </section>
  );
}

export function DeviceManagement({
  projectID,
  request,
  disabled = false,
}: {
  projectID: string;
  request: Transport;
  disabled?: boolean;
}) {
  const [devices, setDevices] = useState<Device[]>([]);
  const [cursor, setCursor] = useState("");
  const [deviceID, setDeviceID] = useState("");
  const [device, setDevice] = useState<Device>();
  const [jobs, setJobs] = useState<DeviceJob[]>([]);
  const [jobCursor, setJobCursor] = useState("");
  const [currentJob, setCurrentJob] = useState<DeviceJob>();
  const [currentInspection, setCurrentInspection] =
    useState<DeviceInspection>();
  const [currentResult, setCurrentResult] = useState<DeviceAssistantResult>();
  const [inspectionReviewed, setInspectionReviewed] = useState(false);
  const [code, setCode] = useState("");
  const [alias, setAlias] = useState("");
  const [binding, setBinding] = useState("");
  const [approved, setApproved] = useState(false);
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const alive = useAlive();
  const lock = useRef(false);
  const mutation = useDeviceMutation(projectID, request);
  useUnsavedDevice(!!currentJob && deviceJobUnresolved(currentJob));
  const work = async (fn: () => Promise<void>) => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    try {
      await fn();
    } catch (err) {
      if (alive.current) {
        setDevice(undefined);
        setJobs([]);
        setCurrentJob(undefined);
        setCurrentInspection(undefined);
        setCurrentResult(undefined);
        setDevices([]);
        setError(err);
      }
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  const list = useCallback(
    async (after = "") => {
      const q = new URLSearchParams({
        project_id: identifier(projectID),
        limit: "100",
      });
      if (after) q.set("after", after);
      const page = await request<{ items: Device[]; next_cursor?: string }>(
        `/api/v1/devices?${q}`,
      );
      const rows = page.items.map((value) => validDevice(value, projectID));
      if (page.next_cursor && page.next_cursor === after)
        throw new Error("设备游标没有推进。");
      if (alive.current) {
        setDevices((old) => (after ? [...old, ...rows] : rows));
        setCursor(page.next_cursor || "");
      }
    },
    [request, projectID, alive],
  );
  useEffect(() => {
    let active = true;
    queueMicrotask(() => {
      if (active)
        void list().catch((err) => {
          if (active) {
            setDevices([]);
            setError(err);
          }
        });
    });
    return () => {
      active = false;
    };
  }, [list]);
  const readJobs = async (id: string, after = "") => {
    const q = new URLSearchParams({ limit: "100" });
    if (after) q.set("after", after);
    const page = await request<{ items: DeviceJob[]; next_cursor?: string }>(
      `${deviceBase(id)}/jobs?${q}`,
    );
    const rows = page.items.map((value) =>
      validDeviceJob(value, projectID, id),
    );
    if (page.next_cursor && page.next_cursor === after)
      throw new Error("设备作业游标没有推进。");
    if (alive.current) {
      setJobs((old) => (after ? [...old, ...rows] : rows));
      setJobCursor(page.next_cursor || "");
    }
  };
  const read = (id = deviceID) =>
    void work(async () => {
      setDevice(undefined);
      setJobs([]);
      setCurrentJob(undefined);
      setCurrentInspection(undefined);
      setCurrentResult(undefined);
      setCode("");
      setApproved(false);
      const value = validDevice(
        await request<Device>(deviceBase(id)),
        projectID,
      );
      if (value.device_id !== id) throw new Error("服务器返回了不同的设备。");
      if (alive.current) {
        setDevice(value);
        setDeviceID(id);
        setAlias(value.workspace_alias);
        setBinding(value.workspace_binding);
      }
      await readJobs(id);
    });
  const stage = (fn: () => void) => {
    setError(undefined);
    try {
      fn();
    } catch (err) {
      setError(err);
    }
  };
  const observeCurrentJob = async (value: DeviceJob, target: Device) => {
    validDeviceJob(value, projectID, target.device_id);
    if (!alive.current) return;
    setCurrentJob(value);
    setCurrentInspection(undefined);
    setCurrentResult(undefined);
    setInspectionReviewed(false);
    if (value.inspection) {
      const checked = await checkedInspection(value, target, location.origin);
      if (alive.current) setCurrentInspection(checked);
    }
    if (value.result) {
      const checked = await checkedDeviceResult(value, target, location.origin);
      if (alive.current) setCurrentResult(checked);
    }
  };
  const received = (value: unknown) => {
    const out = validDevice(value as Device, projectID);
    setCode("");
    setApproved(false);
    setDevice(out);
    void list().catch((err) => {
      if (alive.current) {
        setDevices([]);
        setError(err);
      }
    });
  };
  const pair = (event: FormEvent) => {
    event.preventDefault();
    stage(() => {
      if (!device || !approved || !code.trim())
        throw new Error("先读取本机显示的设备，并明确核对认领范围。");
      mutation.start(
        "认领此设备、项目和工作区目录别名",
        device.device_id,
        `${deviceBase(device.device_id)}/approve`,
        {
          expected_revision: device.revision,
          claim_code: code.trim(),
          workspace_alias: workspaceAlias(alias),
          workspace_binding: deviceDigest(binding),
        },
        received,
      );
    });
  };
  return (
    <div className="device-management system-stack">
      <section className="panel system-section">
        <h2>本人设备与受控本机执行</h2>
        <p>
          设备必须由操作者在本机明确启动，只发起出站
          HTTPS。此页不接收设备凭据、任意 shell、签名 URL
          或本机绝对路径；认领不授予新的项目角色。写操作需要已明确申请的
          organize scope，权限每次由服务端检查。
        </p>
        {error !== undefined && <SystemError error={error} />}
        <div className="system-actions">
          <button
            className="secondary"
            disabled={busy || mutation.pending}
            onClick={() => void work(() => list())}
          >
            刷新本人设备
          </button>
          {cursor && (
            <button
              className="secondary"
              disabled={busy || mutation.pending}
              onClick={() => void work(() => list(cursor))}
            >
              下一页本人设备
            </button>
          )}
        </div>
        <div className="system-table-scroll">
          <table>
            <thead>
              <tr>
                <th>设备与目录别名</th>
                <th>状态与期限</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {devices.map((d) => (
                <tr key={d.device_id}>
                  <td>
                    {d.alias}
                    <br />
                    <code>{d.device_id}</code>
                    <br />
                    {d.workspace_alias}
                  </td>
                  <td>
                    {stateNames[d.state]}
                    <br />
                    {d.expires_at}
                  </td>
                  <td>
                    <button
                      className="secondary"
                      disabled={busy || mutation.pending}
                      onClick={() => read(d.device_id)}
                    >
                      读取设备与原作业
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="system-inline">
          <label>
            本机显示的设备标识
            <input
              value={deviceID}
              onChange={(e) => {
                setDeviceID(e.target.value.trim());
                setDevice(undefined);
                setJobs([]);
              }}
              maxLength={26}
              disabled={busy || mutation.pending}
            />
          </label>
          <button
            className="secondary"
            disabled={busy || mutation.pending || !deviceID}
            onClick={() => read()}
          >
            读取明确设备
          </button>
        </div>
        {device && (
          <>
            <DeviceFacts device={device} />
            {device.state === "pending" && (
              <form className="system-form" onSubmit={pair}>
                <label>
                  本机一次认领代码
                  <input
                    value={code}
                    onChange={(e) => setCode(e.target.value)}
                    type="password"
                    autoComplete="off"
                    required
                    disabled={disabled || mutation.pending || busy}
                  />
                </label>
                <label>
                  明确批准的工作区目录别名
                  <input
                    value={alias}
                    onChange={(e) => {
                      setAlias(e.target.value);
                      setApproved(false);
                    }}
                    required
                    disabled={disabled || mutation.pending || busy}
                  />
                </label>
                <label>
                  本机工作区完整绑定摘要
                  <input
                    value={binding}
                    onChange={(e) => {
                      setBinding(e.target.value.trim());
                      setApproved(false);
                    }}
                    minLength={64}
                    maxLength={64}
                    required
                    disabled={disabled || mutation.pending || busy}
                  />
                </label>
                <label className="system-check">
                  <input
                    type="checkbox"
                    checked={approved}
                    onChange={(e) => setApproved(e.target.checked)}
                    disabled={disabled || mutation.pending || busy}
                  />
                  已核对本机显示的同一主体、项目、设备、目录别名、工作区绑定、期限与全部动作；不批准别的设备或工作区。
                </label>
                <button
                  className="primary"
                  disabled={disabled || mutation.pending || busy || !approved}
                >
                  核对并认领此设备
                </button>
              </form>
            )}
            {device.state === "active" && (
              <button
                className="secondary"
                disabled={disabled || busy || mutation.pending}
                onClick={() =>
                  stage(() =>
                    mutation.start(
                      "撤销此设备的新执行与现有租约",
                      device.device_id,
                      `${deviceBase(device.device_id)}/revoke`,
                      { expected_revision: device.revision },
                      received,
                    ),
                  )
                }
              >
                请求撤销此设备授权
              </button>
            )}
            <h3>该设备原队列作业</h3>
            <p>
              真实派发入口位于迁移和工作集的固定作业面板。此处读取、取消与对账，不复制或重新创建原作业。
            </p>
            <div className="system-table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>原作业 / 动作</th>
                    <th>状态 / 修订</th>
                    <th>读取</th>
                  </tr>
                </thead>
                <tbody>
                  {jobs.map((j) => (
                    <tr key={j.job_id}>
                      <td>
                        <code>{j.job_id}</code>
                        <br />
                        {j.job.action}
                      </td>
                      <td>
                        {stateNames[j.state]} / {j.revision}
                      </td>
                      <td>
                        <button
                          className="secondary"
                          disabled={busy || mutation.pending}
                          onClick={() =>
                            void work(async () => {
                              const value = validDeviceJob(
                                await request<DeviceJob>(
                                  jobBase(device.device_id, j.job_id),
                                ),
                                projectID,
                                device.device_id,
                              );
                              await observeCurrentJob(value, device);
                            })
                          }
                        >
                          读取原作业
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {jobCursor && (
              <button
                className="secondary"
                disabled={busy || mutation.pending}
                onClick={() =>
                  void work(() => readJobs(device.device_id, jobCursor))
                }
              >
                下一页原作业
              </button>
            )}
            {currentJob && (
              <>
                <DeviceJobFacts job={currentJob} />
                <div className="system-actions">
                  <button
                    className="secondary"
                    disabled={busy || mutation.pending}
                    onClick={() =>
                      void work(async () => {
                        const value = validDeviceJob(
                          await request<DeviceJob>(
                            jobBase(device.device_id, currentJob.job_id),
                          ),
                          projectID,
                          device.device_id,
                        );
                        await observeCurrentJob(value, device);
                      })
                    }
                  >
                    刷新原作业观察
                  </button>
                  {deviceJobUnresolved(currentJob) && (
                    <button
                      className="secondary"
                      disabled={disabled || busy || mutation.pending}
                      onClick={() =>
                        stage(() =>
                          mutation.start(
                            "请求取消原队列作业",
                            device.device_id,
                            `${jobBase(device.device_id, currentJob.job_id)}/cancel`,
                            { expected_revision: currentJob.revision },
                            (value) =>
                              observeCurrentJob(value as DeviceJob, device),
                          ),
                        )
                      }
                    >
                      取消原队列作业
                    </button>
                  )}
                </div>
                {currentInspection && (
                  <div className="device-inspection">
                    <h3>原作业的设备检查与摘要批准</h3>
                    <Receipt value={currentInspection} />
                    <label className="system-check">
                      <input
                        type="checkbox"
                        checked={inspectionReviewed}
                        onChange={(e) =>
                          setInspectionReviewed(e.target.checked)
                        }
                        disabled={disabled || busy || mutation.pending}
                      />
                      已核对原作业、输入摘要、工作区绑定、全部相对路径和写入范围，批准以上本机检查摘要。
                    </label>
                    <button
                      className="primary"
                      disabled={
                        disabled ||
                        busy ||
                        mutation.pending ||
                        !inspectionReviewed ||
                        currentJob.state !== "awaiting_approval"
                      }
                      onClick={() =>
                        stage(() =>
                          mutation.start(
                            "批准原作业的精确本机检查",
                            device.device_id,
                            `${jobBase(device.device_id, currentJob.job_id)}/approve`,
                            {
                              expected_revision: currentJob.revision,
                              job_digest: currentInspection.job_digest,
                              approval_digest:
                                currentInspection.approval_digest,
                              workspace_binding:
                                currentInspection.workspace_binding,
                            },
                            (value) =>
                              observeCurrentJob(value as DeviceJob, device),
                          ),
                        )
                      }
                    >
                      批准原作业检查并允许领取
                    </button>
                  </div>
                )}
                {currentResult && <Receipt value={currentResult} />}
              </>
            )}
          </>
        )}
      </section>
      {mutation.modal}
    </div>
  );
}
