import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import { api, APIError, isSessionError, message } from "./api";
import { BackupManagement, type BackupTransport } from "./backup-management";

interface ControlSession {
  principal: { id: string; name: string; display_name?: string };
  expires_at: string;
  csrf_token: string;
  scope: { job_id: string; attempt: number };
  capabilities: string[];
}
const endpoint = "/api/v1/admin/backups/control-session";
function checked(value: ControlSession) {
  if (
    !value?.principal?.name ||
    !value.csrf_token ||
    !value.scope?.job_id ||
    !Number.isFinite(new Date(value.expires_at).getTime()) ||
    !value.capabilities?.includes("backup.read") ||
    !value.capabilities.includes("backup.cancel")
  ) {
    throw new APIError("INVALID_RESPONSE", 200);
  }
  return value;
}

// This session is intentionally independent from the ordinary browser session.
// Its CSRF value is never persisted or passed to rememberCSRF/ordinary APIs.
export function BackupMaintenanceAccess({ onExit }: { onExit: () => void }) {
  const [session, setSession] = useState<ControlSession>();
  const [checking, setChecking] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [notice, setNotice] = useState("");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const lock = useRef(false);
  const alive = useRef(true);
  const requests = useRef(new Set<AbortController>());
  const nameInput = useRef<HTMLInputElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    alive.current = true;
    const active = requests.current;
    return () => {
      alive.current = false;
      for (const c of active) c.abort();
      active.clear();
    };
  }, []);
  const call = useCallback(
    async <T,>(
      path: string,
      options: RequestInit = {},
      csrf: string | null = null,
    ) => {
      const controller = new AbortController();
      requests.current.add(controller);
      const abort = () => controller.abort();
      options.signal?.addEventListener("abort", abort, { once: true });
      if (options.signal?.aborted) controller.abort();
      try {
        return await api<T>(
          path,
          { ...options, signal: controller.signal },
          csrf,
        );
      } finally {
        requests.current.delete(controller);
        options.signal?.removeEventListener("abort", abort);
      }
    },
    [],
  );
  const expire = useCallback((failure: unknown) => {
    if (!alive.current) return;
    setSession(undefined);
    setPassword("");
    setCode("");
    setNotice(
      isSessionError(failure)
        ? "维护身份已过期、任务已结束或授权已失效。若备份仍在执行可重新验证；否则请返回普通登录。"
        : "维护身份无法继续使用，请重新验证管理员身份。",
    );
  }, []);
  useEffect(() => {
    let stopped = false;
    void call<ControlSession>(endpoint)
      .then((value) => {
        if (!stopped) setSession(checked(value));
      })
      .catch((failure) => {
        if (!stopped && !isSessionError(failure)) setError(failure);
      })
      .finally(() => {
        if (!stopped) setChecking(false);
      });
    return () => {
      stopped = true;
    };
  }, [call]);
  useEffect(() => {
    if (checking) return;
    if (!session) nameInput.current?.focus();
    else heading.current?.focus();
  }, [checking, session]);
  useEffect(() => {
    if (!session) return;
    const timeout = setTimeout(
      () => expire(new APIError("TOKEN_EXPIRED", 401)),
      Math.max(0, new Date(session.expires_at).getTime() - Date.now()),
    );
    return () => clearTimeout(timeout);
  }, [session, expire]);
  const request = useCallback<BackupTransport>(
    async <T,>(path: string, options: RequestInit = {}) => {
      if (!session) throw new APIError("AUTH_REQUIRED", 401);
      const read = !options.method || options.method === "GET";
      const cancel =
        options.method === "POST" &&
        /^\/api\/v1\/admin\/backups\/[A-Za-z0-9]+\/cancel$/.test(path);
      const allowedRead =
        /^\/api\/v1\/admin\/backups(?:\/(?:history|schedule|[A-Z0-9]+))?$/.test(
          path,
        );
      if (!(read && allowedRead) && !cancel)
        throw new APIError("FORBIDDEN", 403);
      return call<T>(path, options, session.csrf_token);
    },
    [call, session],
  );
  const login = async (event: FormEvent) => {
    event.preventDefault();
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    setNotice("");
    const body = JSON.stringify({ name: name.trim(), password, code });
    setPassword("");
    setCode("");
    try {
      const value = checked(
        await call<ControlSession>(endpoint, { method: "POST", body }),
      );
      if (alive.current) setSession(value);
    } catch (failure) {
      if (alive.current) setError(failure);
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  const logout = async () => {
    if (!session || lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    try {
      await call(endpoint, { method: "DELETE" }, session.csrf_token);
      if (alive.current) {
        setSession(undefined);
        onExit();
      }
    } catch (failure) {
      if (alive.current) {
        if (isSessionError(failure)) {
          setSession(undefined);
          onExit();
        } else setError(failure);
      }
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  const errorView = error ? (
    <div className="error" role="alert">
      <strong>{message(error)}</strong>
      {error instanceof APIError && error.requestID && (
        <small>请求编号：{error.requestID}</small>
      )}
    </div>
  ) : null;
  if (checking)
    return (
      <main className="maintenance-access">
        <p role="status">正在核验维护身份…</p>
      </main>
    );
  if (!session)
    return (
      <main className="maintenance-access">
        <form className="login-form" onSubmit={(event) => void login(event)}>
          <h1>备份维护身份验证</h1>
          <p>
            普通会话过期时，可重新验证管理员身份以查看或取消当前备份。业务仍保持暂停，原会话不会延长。
          </p>
          {notice && (
            <p className="notice" role="status">
              {notice}
            </p>
          )}
          {errorView}
          <label>
            维护管理账户
            <input
              ref={nameInput}
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
              disabled={busy}
            />
          </label>
          <label>
            维护管理口令
            <input
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
              disabled={busy}
            />
          </label>
          <label>
            维护验证器动态码
            <input
              inputMode="numeric"
              autoComplete="one-time-code"
              pattern="[0-9]{6}"
              maxLength={6}
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
              required
              disabled={busy}
            />
          </label>
          <button
            className="primary login-submit"
            type="submit"
            disabled={busy}
          >
            {busy ? "正在验证维护身份…" : "验证并查看当前备份"}
          </button>
          <p className="subtle">
            临时身份最多有效 10 分钟，并绑定当前任务与尝试次数；任务结束或 core
            重启后失效。只允许查看备份和取消当前任务。
          </p>
          <button
            className="text-button"
            type="button"
            onClick={onExit}
            disabled={busy}
          >
            返回普通登录
          </button>
        </form>
      </main>
    );
  return (
    <div className="maintenance-shell">
      <header className="maintenance-access-header">
        <h2 ref={heading} tabIndex={-1}>
          备份维护管理
        </h2>
        <p>
          {session.principal.display_name || session.principal.name} ·
          仅查看与取消 · 有效期至{" "}
          {new Date(session.expires_at).toLocaleString("zh-CN")}
        </p>
        <p>
          <code>{session.scope.job_id}</code> · 第 {session.scope.attempt}{" "}
          次尝试
        </p>
        <button
          className="secondary"
          onClick={() => void logout()}
          disabled={busy}
        >
          退出维护身份
        </button>
      </header>
      {errorView}
      <main>
        <BackupManagement expire={expire} maintenanceRequest={request} />
      </main>
    </div>
  );
}
