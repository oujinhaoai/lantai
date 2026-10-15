import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import {
  api,
  APIError,
  hasCSRF,
  isSessionError,
  message,
  type Identity,
} from "./api";
import "./backup-management.css";

interface Job {
  id: string;
  backup_id?: string;
  state: string;
  error?: string;
  created_at: string;
  updated_at: string;
  source?: string;
  scheduled_for?: string;
  schedule_timezone?: string;
  attempt?: number;
}
interface Schedule {
  enabled: boolean;
  timezone: string;
  time: string;
  drain_seconds?: number;
  timeout_seconds?: number;
  revision: number;
  updated_at: string;
  next_run_at: string | null;
  disabled_reason?: string;
}
interface Overview {
  availability: {
    configured: boolean;
    available: boolean;
    error?: string;
    checked_at?: string;
    check_deferred?: boolean;
  };
  current: Job | null;
  last_success: Job | null;
  schedule: Schedule;
  recent_schedule_events: {
    date: string;
    timezone: string;
    state: string;
    error?: string;
    at: string;
    job_id?: string;
  }[];
}
interface Challenge {
  challenge_id: string;
  summary: string;
  targets: { kind: string; id: string; expected_revision: number }[];
  expires_at: string;
}
interface Intent {
  title: string;
  action: string;
  command: Record<string, unknown>;
  path: string;
  method: string;
  body: Record<string, unknown>;
}
interface Pending extends Intent {
  challenge: Challenge;
  key: string;
  grantID?: string;
}
const root = "/api/v1/admin/backups";
const activeStates = new Set(["draining", "backing_up", "cancelling"]);
const resumableStates = new Set(["failed", "cancelled", "interrupted"]);
const stateNames: Record<string, string> = {
  draining: "等待在途操作完成",
  backing_up: "正在复制与验证",
  cancelling: "正在安全取消",
  complete: "已完成并验证",
  failed: "失败",
  cancelled: "已取消",
  interrupted: "因 core 重启而中断",
  started: "已启动",
  skipped_busy: "已有任务，已跳过",
  skipped_unavailable: "目录不可用，已跳过",
  unavailable: "备份不可用，已跳过",
  authorization_revoked: "管理员授权已失效，未运行",
  authorization_check_failed: "无法核验计划授权，未运行",
  missed: "错过的计划，已跳过",
  dst_missing: "夏令时跳过此时间，未运行",
};
const errorNames: Record<string, string> = {
  cancelled: "管理员取消了任务。",
  timeout: "任务超时。请检查存储和在途操作后续传。",
  backup_failed: "备份或验证失败。请检查 core 日志和备份存储。",
  journal_write_failed: "任务记录写入失败。请检查 core 数据存储。",
  process_interrupted: "core 在任务完成前停止。任务不会自动续传。",
  not_configured: "尚未配置备份目录。请由部署管理员一次性挂载专用目录。",
  backup_not_configured: "尚未配置备份目录。请由部署管理员一次性挂载专用目录。",
  root_missing: "备份目录不可用。请检查外部存储是否挂载。",
  root_identity_mismatch: "备份目录标识不符。请检查挂载位置和目录标识。",
  root_invalid: "备份目录配置不安全或不可用。请检查目录标识与权限。",
  permission_denied: "无法访问备份目录。请检查服务账户权限。",
  journal_invalid: "备份任务记录异常。请检查 core 日志后处理。",
  invalid_root_identity: "备份目录或标识格式无效。请检查部署配置。",
  root_identity_invalid:
    "备份目录标识不符或无法读取。请检查挂载位置和目录标识。",
  root_overlaps_data: "备份目录与数据或密钥目录重叠。请配置独立的备份目录。",
  insufficient_space: "备份空间不足。请释放空间后重试。",
  root_replaced: "备份目录被替换或重新挂载。请检查挂载与标识后重试。",
  root_unavailable: "无法访问备份目录。请检查外部存储、挂载和服务账户权限。",
  root_permissions_unsafe:
    "备份目录权限不符合要求。请由部署管理员检查服务账户属主和私有目录权限。",
  capacity_check_failed: "备份空间检查失败。请检查存储可用性后重试。",
  unsafe_backup_target: "任务目录不符合安全约束。请检查是否被替换或设为链接。",
  resume_point_invalid: "原备份检查点不可用或与当前数据不符，无法续传。",
  destination_permission_denied: "备份目标拒绝写入。请检查服务账户权限。",
  authorization_revoked: "原计划管理员权限已撤销。请由有效管理员重新确认计划。",
  authorization_check_failed:
    "暂时无法核验计划授权。core 将再次检查，通过前不会执行计划。",
  backup_start_failed: "计划未能启动备份。请检查目录与 core 日志。",
};
function errorText(code?: string) {
  return code
    ? errorNames[code] || `备份未能完成（${code}）。`
    : "暂无错误详情。";
}
function date(value?: string | null, timezone?: string) {
  if (!value || value.startsWith("0001-")) return "—";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "—";
  try {
    return new Intl.DateTimeFormat("zh-CN", {
      dateStyle: "medium",
      timeStyle: "medium",
      ...(timezone ? { timeZone: timezone } : {}),
    }).format(parsed);
  } catch {
    return parsed.toLocaleString("zh-CN");
  }
}
export function canManageBackups(who: Identity) {
  return (
    who.principal.kind === "human" &&
    !!who.system_roles?.includes("admin") &&
    who.session.scopes.includes("admin") &&
    who.session.session_kind === "normal" &&
    who.session.channel === "browser" &&
    !who.session.delegated_by
  );
}
function ErrorNotice({ error }: { error: unknown }) {
  return (
    <div className="error" role="alert">
      <strong>{message(error)}</strong>
      {error instanceof APIError && error.requestID && (
        <small>请求编号：{error.requestID}</small>
      )}
    </div>
  );
}
function JobFacts({ job }: { job: Job }) {
  return (
    <dl className="backup-facts">
      <dt>任务标识</dt>
      <dd>
        <code>{job.id}</code>
      </dd>
      <dt>发起方式</dt>
      <dd>{job.source === "schedule" ? "每日计划" : "手动"}</dd>
      <dt>发起时间</dt>
      <dd>{date(job.created_at)}</dd>
      <dt>最近更新</dt>
      <dd>{date(job.updated_at)}</dd>
      {job.attempt ? (
        <>
          <dt>尝试次数</dt>
          <dd>{job.attempt}</dd>
        </>
      ) : null}
      {job.backup_id && (
        <>
          <dt>备份标识</dt>
          <dd>
            <code>{job.backup_id}</code>
          </dd>
        </>
      )}
      {job.scheduled_for && (
        <>
          <dt>计划日期</dt>
          <dd>
            {job.scheduled_for} · {job.schedule_timezone}
          </dd>
        </>
      )}
    </dl>
  );
}

export function BackupManagement({
  expire,
}: {
  expire: (error: unknown) => void;
}) {
  const [overview, setOverview] = useState<Overview>();
  const [history, setHistory] = useState<Job[]>([]);
  const [error, setError] = useState<unknown>();
  const [historyError, setHistoryError] = useState<unknown>();
  const [actionError, setActionError] = useState<unknown>();
  const [denied, setDenied] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const [updated, setUpdated] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [pending, setPending] = useState<Pending>();
  const [proofError, setProofError] = useState<unknown>();
  const [code, setCode] = useState("");
  const [draft, setDraft] = useState({
    enabled: false,
    time: "03:00",
    timezone: "Etc/UTC",
    drain: "120",
    timeout: "7200",
  });
  const [dirty, setDirty] = useState(false);
  const [shown, setShown] = useState(10);
  const alive = useRef(true);
  const revoked = useRef(false);
  const controllers = useRef(new Set<AbortController>());
  const actionLock = useRef(false);
  const dirtyRef = useRef(false);
  const [draftRevision, setDraftRevision] = useState(0);
  const dialog = useRef<HTMLDialogElement>(null);
  const proofInput = useRef<HTMLInputElement>(null);

  useEffect(() => {
    alive.current = true;
    const activeControllers = controllers.current;
    return () => {
      alive.current = false;
      for (const controller of activeControllers) controller.abort();
      activeControllers.clear();
    };
  }, []);
  const request = useCallback(
    async <T,>(path: string, options: RequestInit = {}) => {
      const controller = new AbortController();
      controllers.current.add(controller);
      try {
        return await api<T>(path, { ...options, signal: controller.signal });
      } finally {
        controllers.current.delete(controller);
      }
    },
    [],
  );
  const fail = useCallback(
    (failure: unknown) => {
      if (!alive.current) return;
      if (isSessionError(failure)) expire(failure);
      else if (failure instanceof APIError && failure.status === 403) {
        revoked.current = true;
        for (const controller of controllers.current) controller.abort();
        setDenied(true);
        setOverview(undefined);
        setHistory([]);
        setPending(undefined);
        setCode("");
      }
    },
    [expire],
  );

  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    let failures = 0;
    async function read() {
      if (revoked.current) return;
      try {
        const value = await request<Overview>(root);
        if (stopped || revoked.current) return;
        setOverview(value);
        setError(undefined);
        setUpdated(new Date().toISOString());
        failures = 0;
        if (!dirtyRef.current) {
          setDraftRevision(value.schedule.revision);
          setDraft({
            enabled: value.schedule.enabled,
            time: value.schedule.time || "03:00",
            timezone: value.schedule.timezone || "Etc/UTC",
            drain: String(value.schedule.drain_seconds || 120),
            timeout: String(value.schedule.timeout_seconds || 7200),
          });
        }
        if (value.current && activeStates.has(value.current.state)) {
          timer = setTimeout(read, 2000);
        } else {
          timer = setTimeout(read, 15000);
        }
      } catch (failure) {
        if (stopped) return;
        fail(failure);
        setError(failure);
        if (
          isSessionError(failure) ||
          (failure instanceof APIError && failure.status === 403)
        )
          return;
        failures++;
        timer = setTimeout(read, Math.min(30000, 2000 * 2 ** failures));
      }
    }
    void read();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [refresh, request, fail]);

  useEffect(() => {
    let stopped = false;
    void request<{ items: Job[] | null }>(`${root}/history`)
      .then((value) => {
        if (!stopped && !revoked.current) {
          setHistory(value.items || []);
          setHistoryError(undefined);
        }
      })
      .catch((failure) => {
        if (!stopped) {
          fail(failure);
          setHistoryError(failure);
        }
      });
    return () => {
      stopped = true;
    };
  }, [refresh, overview?.current?.state, request, fail]);

  useEffect(() => {
    if (pending && dialog.current && !dialog.current.open) {
      dialog.current.showModal();
      proofInput.current?.focus();
    }
  }, [pending]);

  const reload = () => setRefresh((n) => n + 1);
  const lock = () => {
    if (actionLock.current) return false;
    actionLock.current = true;
    setBusy(true);
    return true;
  };
  const unlock = () => {
    actionLock.current = false;
    if (alive.current) setBusy(false);
  };
  const prepare = async (intent: Intent) => {
    if (!lock()) return;
    setActionError(undefined);
    setNotice("");
    try {
      const challenge = await request<Challenge>(
        "/api/v1/identity/challenges",
        {
          method: "POST",
          body: JSON.stringify({
            action: intent.action,
            command: intent.command,
          }),
        },
      );
      if (alive.current) {
        setPending({ ...intent, challenge, key: crypto.randomUUID() });
        setCode("");
        setProofError(undefined);
      }
    } catch (failure) {
      fail(failure);
      if (alive.current) setActionError(failure);
    } finally {
      unlock();
    }
  };
  const start = () =>
    prepare({
      title: "立即备份",
      action: "identity.backup_start",
      command: { kind: "start" },
      path: root,
      method: "POST",
      body: {},
    });
  const resume = (job: Job) =>
    prepare({
      title: "续传备份",
      action: "identity.backup_resume",
      command: { kind: "resume", job_id: job.id },
      path: `${root}/${encodeURIComponent(job.id)}/resume`,
      method: "POST",
      body: {},
    });
  const cancel = async (job: Job) => {
    if (!lock()) return;
    setActionError(undefined);
    try {
      await request(`${root}/${encodeURIComponent(job.id)}/cancel`, {
        method: "POST",
        body: "{}",
      });
      if (alive.current) {
        setNotice("取消请求已受理。请等待任务停止，期间业务仍暂停。");
        reload();
      }
    } catch (failure) {
      fail(failure);
      if (alive.current) setActionError(failure);
    } finally {
      unlock();
    }
  };
  const save = (event: FormEvent) => {
    event.preventDefault();
    if (!overview) return;
    const schedule = {
      enabled: draft.enabled,
      time: draft.time,
      timezone: draft.timezone.trim(),
      revision: draftRevision,
      drain_seconds: Number(draft.drain),
      timeout_seconds: Number(draft.timeout),
    };
    if (schedule.drain_seconds > schedule.timeout_seconds) {
      setActionError(new APIError("BACKUP_TIMEOUT_INVALID", 422));
      return;
    }
    void prepare({
      title: "保存每日计划",
      action: "identity.backup_schedule",
      command: { kind: "schedule", schedule },
      path: `${root}/schedule`,
      method: "PUT",
      body: schedule,
    });
  };
  const submitProof = async (event: FormEvent) => {
    event.preventDefault();
    if (!pending || !lock()) return;
    setProofError(undefined);
    const submitted = pending;
    try {
      let grantID = submitted.grantID;
      if (!grantID) {
        const grant = await request<{ grant_id: string }>(
          `/api/v1/identity/challenges/${encodeURIComponent(submitted.challenge.challenge_id)}/verify`,
          {
            method: "POST",
            body: JSON.stringify({ code }),
          },
        );
        grantID = grant.grant_id;
        if (alive.current) setPending({ ...submitted, grantID });
      }
      setCode("");
      await request(submitted.path, {
        method: submitted.method,
        headers: { "Idempotency-Key": submitted.key },
        body: JSON.stringify({ ...submitted.body, grant_id: grantID }),
      });
      if (alive.current) {
        setPending(undefined);
        if (submitted.action === "identity.backup_schedule") {
          dirtyRef.current = false;
          setDirty(false);
          setNotice("每日计划已保存。下次运行时间以服务器返回为准。");
        } else setNotice("任务已受理。请查看当前阶段与最终结果。");
        reload();
      }
    } catch (failure) {
      fail(failure);
      if (alive.current) {
        setProofError(failure);
        setCode("");
      }
    } finally {
      unlock();
    }
  };
  const changeDraft = (value: typeof draft) => {
    setDraft(value);
    dirtyRef.current = true;
    setDirty(true);
  };
  const resetDraft = () => {
    if (!overview) return;
    setDraft({
      enabled: overview.schedule.enabled,
      time: overview.schedule.time,
      timezone: overview.schedule.timezone,
      drain: String(overview.schedule.drain_seconds || 120),
      timeout: String(overview.schedule.timeout_seconds || 7200),
    });
    setDraftRevision(overview.schedule.revision);
    dirtyRef.current = false;
    setDirty(false);
  };
  const closeConfirmation = () => {
    setPending(undefined);
    setCode("");
    setProofError(undefined);
    reload();
  };
  const current = overview?.current;
  const active = !!current && activeStates.has(current.state);
  const mutable = hasCSRF() && !busy && !active;

  if (denied)
    return (
      <section>
        <h1 tabIndex={-1}>备份管理</h1>
        <div className="error" role="alert">
          当前会话没有备份管理权限。请使用管理员账户重新登录。
        </div>
      </section>
    );
  return (
    <div className="backup-page">
      <header className="page-heading backup-heading">
        <div>
          <p className="eyebrow">系统管理 / BACKUPS</p>
          <h1 tabIndex={-1}>备份管理</h1>
          <p>将档案库备份到已配置的专用目录。备份期间暂停全部业务读写。</p>
        </div>
        <button className="secondary" onClick={reload}>
          刷新状态
        </button>
      </header>
      {!hasCSRF() && (
        <p className="notice">
          此标签页缺少操作校验信息。可查看状态；执行操作请回到原管理员登录标签页，或重新登录。
        </p>
      )}
      {error ? <ErrorNotice error={error} /> : null}
      {actionError ? <ErrorNotice error={actionError} /> : null}
      {notice && (
        <p className="notice" role="status">
          {notice}
        </p>
      )}
      {!overview && !error && (
        <p className="loading" role="status">
          正在读取备份状态…
        </p>
      )}
      {overview && (
        <>
          <div className="backup-summary-grid">
            <section
              className="panel backup-status"
              aria-labelledby="backup-directory-title"
            >
              <h2 id="backup-directory-title">备份目录</h2>
              <p
                className={`backup-state ${overview.availability.available ? "success" : "warning"}`}
              >
                {overview.availability.available
                  ? overview.availability.check_deferred
                    ? "开始前检查可用"
                    : "可用"
                  : overview.availability.configured
                    ? "暂不可用"
                    : "尚未配置"}
              </p>
              <p>
                {overview.availability.check_deferred
                  ? `备份进行中，目录状态为开始前检查结果（${date(overview.availability.checked_at)}）；实时探测已延后。`
                  : overview.availability.available
                    ? "目录已通过当前可用性检查。实际任务仍会重新检查。"
                    : errorText(
                        overview.availability.error ||
                          (overview.availability.configured
                            ? "root_missing"
                            : "not_configured"),
                      )}
              </p>
              {!overview.availability.available && (
                <p className="subtle">
                  请检查部署配置、外部存储挂载、目录标识及服务账户权限。目录不可用不会阻止正常业务使用。
                </p>
              )}
            </section>
            <section
              className="panel backup-status"
              aria-labelledby="backup-success-title"
            >
              <h2 id="backup-success-title">上次成功</h2>
              <p className="backup-state">
                {overview.last_success
                  ? date(overview.last_success.updated_at)
                  : "尚无成功备份"}
              </p>
              {overview.last_success ? (
                <>
                  <p>已完成备份与验证。</p>
                  <code>{overview.last_success.id}</code>
                </>
              ) : (
                <p>首次备份完成后，这里会显示已验证的结果。</p>
              )}
            </section>
          </div>
          <section
            className="panel backup-current"
            aria-labelledby="backup-current-title"
          >
            <div className="backup-section-head">
              <h2 id="backup-current-title">当前任务</h2>
              <button
                className="primary"
                onClick={() => void start()}
                disabled={!mutable || !overview.availability.available}
              >
                立即备份
              </button>
            </div>
            {current ? (
              <>
                <p className="backup-state" role="status">
                  {stateNames[current.state] || current.state}
                </p>
                {active && (
                  <p>
                    {current.state === "cancelling"
                      ? "正在等待备份工作安全退出。完成前不会恢复业务读写。"
                      : "新的业务操作已暂停，管理状态与取消仍可使用。请保持此页或稍后返回查看。"}
                  </p>
                )}
                {current.error && (
                  <p className="backup-job-error">{errorText(current.error)}</p>
                )}
                <JobFacts job={current} />
                {active && (
                  <button
                    className="secondary"
                    onClick={() => void cancel(current)}
                    disabled={
                      !hasCSRF() || busy || current.state === "cancelling"
                    }
                  >
                    {current.state === "cancelling"
                      ? "等待安全停止"
                      : "取消当前任务"}
                  </button>
                )}
                {!active && resumableStates.has(current.state) && (
                  <button
                    className="secondary"
                    onClick={() => void resume(current)}
                    disabled={!mutable || !overview.availability.available}
                  >
                    续传此任务
                  </button>
                )}
              </>
            ) : (
              <p className="backup-empty">
                暂无正在执行的备份。立即备份会先排空在途操作，再复制并验证内容。
              </p>
            )}
            <p className="subtle">
              只显示真实阶段，不估算完成百分比。状态更新时间：{date(updated)}。
            </p>
          </section>
          <section className="panel" aria-labelledby="backup-schedule-title">
            <div className="backup-section-head">
              <h2 id="backup-schedule-title">每日计划</h2>
              <span className="badge">
                {overview.schedule.enabled ? "已开启" : "已关闭"}
              </span>
            </div>
            <p className="backup-next">
              下次运行：
              <strong>
                {overview.schedule.enabled
                  ? date(
                      overview.schedule.next_run_at,
                      overview.schedule.timezone,
                    )
                  : "计划已关闭"}
              </strong>
              {overview.schedule.enabled && (
                <>（{overview.schedule.timezone}）</>
              )}
            </p>
            {overview.schedule.disabled_reason && (
              <p className="notice" role="status">
                {errorText(overview.schedule.disabled_reason)}
              </p>
            )}
            <form className="backup-schedule-form" onSubmit={save}>
              {dirty && draftRevision !== overview.schedule.revision && (
                <p className="notice" role="status">
                  计划已在其他位置更新。当前草稿保留原修订号，请重置为已保存设置后重新编辑。
                </p>
              )}
              <label className="backup-check">
                <input
                  type="checkbox"
                  checked={draft.enabled}
                  onChange={(e) =>
                    changeDraft({ ...draft, enabled: e.target.checked })
                  }
                  disabled={!mutable}
                />
                开启每日备份
              </label>
              <div className="backup-form-row">
                <label>
                  每日时间
                  <input
                    type="time"
                    required
                    value={draft.time}
                    onChange={(e) =>
                      changeDraft({ ...draft, time: e.target.value })
                    }
                    disabled={!mutable}
                  />
                </label>
                <label>
                  IANA 时区
                  <input
                    required
                    maxLength={100}
                    autoCapitalize="none"
                    spellCheck={false}
                    list="backup-timezones"
                    placeholder="例如 Asia/Shanghai"
                    value={draft.timezone}
                    onChange={(e) =>
                      changeDraft({ ...draft, timezone: e.target.value })
                    }
                    disabled={!mutable}
                  />
                </label>
              </div>
              <div className="backup-form-row backup-timeouts">
                <label>
                  排空等待上限（秒）
                  <input
                    type="number"
                    required
                    min="1"
                    max="3600"
                    step="1"
                    value={draft.drain}
                    onChange={(e) =>
                      changeDraft({ ...draft, drain: e.target.value })
                    }
                    disabled={!mutable}
                  />
                </label>
                <label>
                  整个任务超时（秒）
                  <input
                    type="number"
                    required
                    min="1"
                    max="86400"
                    step="1"
                    value={draft.timeout}
                    onChange={(e) =>
                      changeDraft({ ...draft, timeout: e.target.value })
                    }
                    disabled={!mutable}
                  />
                </label>
              </div>
              <datalist id="backup-timezones">
                <option value="Asia/Shanghai" />
                <option value="Etc/UTC" />
                <option value="America/New_York" />
                <option value="Europe/London" />
              </datalist>
              <p className="subtle">
                时间按所选时区计算，不跟随当前浏览器时区。计划由 core
                保存并执行，无需保持登录或打开此页。
              </p>
              <p className="subtle">
                任务超时包含排空时间。超时或取消后仍会等待实际工作安全退出，再恢复业务。
              </p>
              <button
                className="secondary"
                type="submit"
                disabled={!mutable || !dirty}
              >
                验证并保存计划
              </button>
              {dirty && (
                <button
                  className="text-button"
                  type="button"
                  onClick={resetDraft}
                  disabled={busy}
                >
                  重置为已保存设置
                </button>
              )}
              {active && <p className="subtle">当前任务结束后可修改计划。</p>}
            </form>
            <details className="backup-policy">
              <summary>错过、夏令时与重启如何处理</summary>
              <p>
                每日同一当地日期最多自动触发一次。计划每 10 秒检查，到期 90
                秒内可启动；超过窗口的运行记为错过，不集中补跑。到点已有备份或目录不可用时记录原因并跳过，下一天再尝试。夏令时跳过的当地时间不运行，重复的当地时间只运行一次。
              </p>
              <p>
                重启后保留计划；未完成任务标为中断，不自动续传。管理员检查原因后可手动续传。取消或失败后，core
                等待实际工作退出再恢复业务。
              </p>
            </details>
            {overview.recent_schedule_events?.length > 0 && (
              <details className="backup-policy">
                <summary>最近 20 条计划执行记录</summary>
                <ul className="backup-events">
                  {overview.recent_schedule_events.map((event, index) => (
                    <li key={`${event.at}-${index}`}>
                      <strong>
                        {event.date} · {stateNames[event.state] || event.state}
                      </strong>
                      <span>
                        {event.timezone} · {date(event.at)}
                      </span>
                      {event.error && <span>{errorText(event.error)}</span>}
                      {event.job_id && <code>{event.job_id}</code>}
                    </li>
                  ))}
                </ul>
              </details>
            )}
          </section>
          <section className="panel" aria-labelledby="backup-history-title">
            <h2 id="backup-history-title">任务历史</h2>
            <p className="subtle">
              按创建时间从新到旧展示 core 保留的全部任务，每次显示 10
              条；此列表不代替备份目录的文件清单。
            </p>
            {historyError ? <ErrorNotice error={historyError} /> : null}
            {!historyError && history.length === 0 && (
              <p className="backup-empty">暂无备份记录。</p>
            )}
            <ul className="backup-history">
              {history.slice(0, shown).map((job) => (
                <li key={job.id}>
                  <div className="backup-section-head">
                    <strong>{stateNames[job.state] || job.state}</strong>
                    <span>{date(job.updated_at)}</span>
                  </div>
                  <code>{job.id}</code>
                  <p>
                    {job.source === "schedule" ? "每日计划" : "手动"}
                    {job.attempt ? ` · 第 ${job.attempt} 次尝试` : ""}
                  </p>
                  {job.error && (
                    <p className="backup-job-error">{errorText(job.error)}</p>
                  )}
                  <details>
                    <summary>任务详情</summary>
                    <JobFacts job={job} />
                  </details>
                  {resumableStates.has(job.state) && (
                    <button
                      className="secondary"
                      onClick={() => void resume(job)}
                      disabled={!mutable || !overview.availability.available}
                    >
                      续传此任务
                    </button>
                  )}
                </li>
              ))}
            </ul>
            {history.length > shown && (
              <button
                className="secondary more"
                onClick={() => setShown((n) => n + 10)}
              >
                显示更多历史（还有 {history.length - shown} 条）
              </button>
            )}
          </section>
        </>
      )}
      {pending && (
        <dialog
          ref={dialog}
          className="backup-dialog"
          aria-labelledby="backup-confirm-title"
          onCancel={(event) => {
            if (busy) event.preventDefault();
            else closeConfirmation();
          }}
        >
          <form onSubmit={(event) => void submitProof(event)}>
            <h2 id="backup-confirm-title">确认{pending.title}</h2>
            <p>请核对服务器给出的操作摘要和全部目标。</p>
            <p className="backup-server-summary">{pending.challenge.summary}</p>
            <ul className="backup-targets">
              {pending.challenge.targets.map((target, index) => (
                <li key={`${target.kind}-${target.id}-${index}`}>
                  <span>{target.kind}</span>
                  <code>{target.id}</code>
                  <span>预期修订：{target.expected_revision}</span>
                </li>
              ))}
            </ul>
            <p className="subtle">
              确认有效期至 {date(pending.challenge.expires_at)}
              。备份与续传期间暂停业务读写。
            </p>
            {proofError ? <ErrorNotice error={proofError} /> : null}
            {pending.grantID ? (
              <p className="notice">
                动态码已验证。若上次网络响应中断，重试会提交同一操作标识，不创建新的备份请求。关闭后请先刷新状态再发起其他操作。
              </p>
            ) : (
              <label>
                确认操作的验证器动态码
                <input
                  ref={proofInput}
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  pattern="[0-9]{6}"
                  maxLength={6}
                  required
                  value={code}
                  onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
                  disabled={busy}
                />
              </label>
            )}
            <div className="backup-dialog-actions">
              <button
                className="secondary"
                type="button"
                onClick={closeConfirmation}
                disabled={busy}
              >
                关闭确认
              </button>
              <button className="primary" type="submit" disabled={busy}>
                {busy
                  ? "正在提交…"
                  : pending.grantID
                    ? "重试同一请求"
                    : "验证并执行"}
              </button>
            </div>
          </form>
        </dialog>
      )}
    </div>
  );
}
