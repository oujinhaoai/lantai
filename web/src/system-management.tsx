import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import {
  api,
  hasCSRF,
  isSessionError,
  type Identity,
  type Page,
  type Project,
} from "./api";
import { BusinessControls, type StageDirect } from "./business-controls";
import { DeviceManagement } from "./device-management";
import {
  credentialIntent,
  identifier,
  kindScopes,
  prepareApproval,
  principalIntent,
  publicReceipt,
  restoreOperationDiagnostic,
  revokeCredentialIntent,
  roleIntent,
  roles,
  type Approval,
  type SensitiveIntent,
  type SensitiveResult,
  type Transport,
} from "./system-operations";
import {
  DirectConfirmation,
  Receipt,
  SensitiveConfirmation,
  SystemError,
  type DirectIntent,
} from "./system-confirmation";
import "./system-management.css";

interface Member {
  principal_id: string;
  name: string;
  kind: string;
  roles: string[];
}
interface Principal {
  principal_id: string;
  name: string;
  display_name?: string;
  kind: string;
  state: string;
  revision: number;
  profile: Record<string, unknown>;
}
interface Credential {
  credential_id: string;
  principal_id: string;
  prefix: string;
  scopes: string[];
  projects: string[];
  label: string;
  created_at: string;
  expires_at: string;
  revoked_at?: string;
}
type Section =
  "projects" | "principals" | "collaboration" | "audit" | "devices";
const sectionTitles: Record<Section, string> = {
  projects: "项目与成员权限",
  principals: "主体与凭据",
  collaboration: "协作、审定与回收",
  audit: "项目审计",
  devices: "设备与本机执行",
};
const human = (who: Identity) =>
  who.principal.kind === "human" &&
  who.session.channel === "browser" &&
  who.session.session_kind === "normal" &&
  !who.session.delegated_by;
export function canManageSystem(who: Identity) {
  return (
    human(who) &&
    !!who.system_roles?.includes("admin") &&
    who.session.scopes.includes("admin")
  );
}

function OneTimeSecret({ value, clear }: { value: string; clear: () => void }) {
  const [visible, setVisible] = useState(false);
  return (
    <section className="panel system-secret">
      <h2>仅此响应返回的一次性秘密</h2>
      <p>
        请在可信环境中按私密凭据保管。它不进入回执、审计导出或浏览器存储；服务器重放和操作查询不会再次返回。若响应丢失，先核对并吊销已生成凭据，再另行明确签发。
      </p>
      {visible ? (
        <label>
          一次性秘密
          <textarea
            readOnly
            value={value}
            autoComplete="off"
            spellCheck={false}
          />
        </label>
      ) : (
        <button className="secondary" onClick={() => setVisible(true)}>
          显示一次性秘密
        </button>
      )}
      <button className="secondary" onClick={clear}>
        已保管，从页面清除
      </button>
    </section>
  );
}
function InstanceFacts({ request }: { request: Transport }) {
  const [value, setValue] = useState<{
    instance_id: string;
    recovery_epoch: number;
    state: string;
    observed_at: string;
    free_bytes?: number;
    min_free_bytes?: number;
    capacity_error?: string;
  }>();
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const alive = useRef(true);
  const controller = useRef<AbortController | undefined>(undefined);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      controller.current?.abort();
    };
  }, []);
  const read = async () => {
    controller.current?.abort();
    const current = new AbortController();
    controller.current = current;
    setBusy(true);
    setValue(undefined);
    setError(undefined);
    try {
      const out = await request<NonNullable<typeof value>>(
        "/api/v1/admin/instance",
        { signal: current.signal },
      );
      if (alive.current && !current.signal.aborted) setValue(out);
    } catch (e) {
      if (alive.current && !current.signal.aborted) setError(e);
    } finally {
      if (alive.current && !current.signal.aborted) setBusy(false);
    }
  };
  return (
    <section className="panel system-section">
      <h2>实例与存储观察</h2>
      <button className="secondary" disabled={busy} onClick={() => void read()}>
        {busy ? "读取中…" : "读取当前实例信息"}
      </button>
      {error !== undefined && <SystemError error={error} />}
      {value && (
        <dl>
          <dt>实例</dt>
          <dd>
            <code>{value.instance_id}</code>
          </dd>
          <dt>恢复代际</dt>
          <dd>{value.recovery_epoch}</dd>
          <dt>状态</dt>
          <dd>{value.state}</dd>
          <dt>本次观察时间</dt>
          <dd>{value.observed_at}</dd>
          <dt>当前可用字节</dt>
          <dd>
            {value.capacity_error || value.free_bytes === undefined
              ? "未能核实当前容量"
              : value.free_bytes.toLocaleString()}
          </dd>
          <dt>配置最低剩余字节</dt>
          <dd>{value.min_free_bytes?.toLocaleString() ?? "未返回"}</dd>
        </dl>
      )}
      <p className="subtle">
        这里是本次实例读观察，不代表 NAS
        介质、备份盘、峰值容量或独立恢复已经验收；不显示主机路径或密钥。
      </p>
    </section>
  );
}

export function SystemManagement({
  who,
  expire,
  route,
}: {
  who: Identity;
  expire: (error: unknown) => void;
  route: string;
}) {
  const initial = route.split("?")[1]
    ? new URLSearchParams(route.split("?")[1]).get("section")
    : null;
  const [section, setSection] = useState<Section>(
    initial && initial in sectionTitles ? (initial as Section) : "projects",
  );
  const [projects, setProjects] = useState<Project[]>([]);
  const [nextProject, setNextProject] = useState("");
  const [projectID, setProjectID] = useState("");
  const [memberProjectDraft, setMemberProjectDraft] = useState("");
  const [memberProjectID, setMemberProjectID] = useState("");
  const [projectsBusy, setProjectsBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [notice, setNotice] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [approval, setApproval] = useState<Approval>();
  const [direct, setDirect] = useState<DirectIntent>();
  const [preparing, setPreparing] = useState(false);
  const [result, setResult] = useState<SensitiveResult>();
  const [secret, setSecret] = useState<string>();
  const alive = useRef(true);
  const lock = useRef(false);
  const pending = useRef(false);
  const abort = useRef(new AbortController());
  const projectAbort = useRef<AbortController | undefined>(undefined);
  const request: Transport = useCallback(
    async <T,>(path: string, options?: RequestInit): Promise<T> => {
      try {
        return await api<T>(path, {
          ...options,
          signal: options?.signal ?? abort.current.signal,
        });
      } catch (e) {
        if (isSessionError(e)) expire(e);
        throw e;
      }
    },
    [expire],
  );
  useEffect(() => {
    alive.current = true;
    const scope = new AbortController();
    abort.current = scope;
    return () => {
      alive.current = false;
      scope.abort();
      projectAbort.current?.abort();
      window.dispatchEvent(
        new CustomEvent("lantai:unsaved-operation", { detail: false }),
      );
    };
  }, []);
  const unsaved = !!approval?.executionAttempted || !!direct?.attempted;
  useEffect(() => {
    window.dispatchEvent(
      new CustomEvent("lantai:unsaved-operation", { detail: unsaved }),
    );
    const guard = (e: BeforeUnloadEvent) => {
      if (unsaved) {
        e.preventDefault();
        e.returnValue = "";
      }
    };
    window.addEventListener("beforeunload", guard);
    return () => window.removeEventListener("beforeunload", guard);
  }, [unsaved]);
  const loadProjects = useCallback(
    async (cursor = "") => {
      projectAbort.current?.abort();
      const controller = new AbortController();
      projectAbort.current = controller;
      setProjectsBusy(true);
      setError(undefined);
      if (!cursor) {
        setProjects([]);
        setNextProject("");
      }
      try {
        const q = new URLSearchParams({ limit: "100", view: "full" });
        if (cursor) q.set("cursor", cursor);
        const out = await request<Page<Project>>(`/api/v1/projects?${q}`, {
          signal: controller.signal,
        });
        if (!alive.current || controller.signal.aborted) return;
        const rows = out.items || [];
        if (out.next_cursor && out.next_cursor === cursor)
          throw new Error("项目游标未推进，已停止读取。");
        setProjects((old) => (cursor ? [...old, ...rows] : rows));
        setNextProject(out.next_cursor || "");
        if (!cursor)
          setProjectID((id) =>
            rows.some((p) => p.project_id === id)
              ? id
              : rows[0]?.project_id || "",
          );
      } catch (e) {
        if (alive.current && !controller.signal.aborted) {
          setProjects([]);
          setNextProject("");
          setProjectID("");
          setError(e);
        }
      } finally {
        if (alive.current && !controller.signal.aborted) setProjectsBusy(false);
      }
    },
    [request],
  );
  useEffect(() => {
    let active = true;
    queueMicrotask(() => {
      if (active) void loadProjects();
    });
    return () => {
      active = false;
    };
  }, [loadProjects, refresh]);
  const stageSensitive = (intent: SensitiveIntent) => {
    if (pending.current || lock.current) {
      setNotice("请先处理已冻结的操作。");
      return;
    }
    lock.current = true;
    setPreparing(true);
    setError(undefined);
    setNotice("");
    setSecret(undefined);
    void prepareApproval(intent, request)
      .then((value) => {
        if (alive.current) {
          pending.current = true;
          setApproval(value);
        }
      })
      .catch((e) => {
        if (alive.current) setError(e);
      })
      .finally(() => {
        lock.current = false;
        if (alive.current) setPreparing(false);
      });
  };
  const stageDirect: StageDirect = (title, path, body) => {
    if (pending.current || lock.current) {
      setNotice("请先处理已冻结的操作。");
      return;
    }
    if (!hasCSRF()) {
      setError(new Error("此标签页缺少操作校验信息，请重新验证后操作。"));
      return;
    }
    pending.current = true;
    setError(undefined);
    setNotice("");
    setSecret(undefined);
    setDirect({
      title,
      path,
      body: JSON.parse(JSON.stringify(body)),
      key: crypto.randomUUID(),
      attempted: false,
    });
  };
  const finish = (out: SensitiveResult) => {
    if (!alive.current) return;
    pending.current = false;
    setApproval(undefined);
    setDirect(undefined);
    setResult(publicReceipt(out) as SensitiveResult);
    setSecret(typeof out.secret === "string" ? out.secret : undefined);
    setNotice("已取得服务器回执；是否完成以回执和最新领域状态为准。");
    setRefresh((n) => n + 1);
  };
  const dismiss = () => {
    pending.current = false;
    setApproval(undefined);
    setDirect(undefined);
    setNotice(
      "确认框已关闭，不会撤销服务器已接受的结果。原操作诊断可用于对账。",
    );
  };
  const importDiagnostic = async (file?: File) => {
    if (!file) return;
    setError(undefined);
    try {
      if (pending.current || lock.current)
        throw new Error("先处理当前冻结操作，再载入诊断。");
      if (file.size > 256 * 1024)
        throw new Error("诊断文件超过 256 KiB 上限。");
      const value = JSON.parse(await file.text()) as Record<string, unknown>;
      if (!alive.current) return;
      if (pending.current || lock.current)
        throw new Error("读取文件期间已产生另一冻结操作，请先处理它。");
      if (value.schema === "lantai.admin-direct/v1") {
        const paths = [
          "/api/v1/projects",
          "/api/v1/review-targets",
          "/api/v1/publications",
          "/api/v1/trash/own",
          "/api/v1/trash/restore",
          "/api/v1/inbox/read",
        ];
        if (
          value.origin !== location.origin ||
          typeof value.path !== "string" ||
          !paths.includes(value.path) ||
          typeof value.key !== "string" ||
          !/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value.key) ||
          typeof value.title !== "string" ||
          value.title.length > 256 ||
          !value.body ||
          typeof value.body !== "object" ||
          Array.isArray(value.body) ||
          typeof value.attempted !== "boolean"
        )
          throw new Error("直接请求诊断不属于当前服务或格式不合法。");
        pending.current = true;
        setDirect({
          title: value.title,
          path: value.path,
          key: value.key,
          body: value.body as Record<string, unknown>,
          attempted: value.attempted,
        });
      } else {
        const restored = restoreOperationDiagnostic(value, location.origin);
        pending.current = true;
        setApproval(restored);
      }
      setNotice(
        "已载入原请求诊断；没有恢复 HumanGrant 或凭据。先核对原操作，再按同一操作重新验证。",
      );
    } catch (e) {
      if (alive.current)
        setError(
          e instanceof SyntaxError ? new Error("诊断文件不是合法 JSON。") : e,
        );
    }
  };
  const project = projects.find((p) => p.project_id === projectID);
  const disabled = preparing || !!approval || !!direct || !hasCSRF();
  const systemAdmin = canManageSystem(who);
  return (
    <div className="system-management">
      <header className="system-header">
        <p className="eyebrow">系统管理 / SYSTEM</p>
        <h1>系统与协作管理</h1>
        <p>
          项目角色、会话
          scope、操作用途及新鲜人类授权分别核验。系统管理员身份不会自动取得所有项目业务角色。
        </p>
      </header>
      {!human(who) && (
        <p className="notice">
          当前不是人的普通、非委托浏览器会话。系统敏感操作不可用。
        </p>
      )}
      {!hasCSRF() && (
        <p className="notice">此标签页只读；请重新验证后进行写操作。</p>
      )}
      <nav className="system-tabs" aria-label="系统管理分区">
        {(Object.keys(sectionTitles) as Section[]).map((name) => (
          <button
            key={name}
            className={section === name ? "primary" : "secondary"}
            aria-pressed={section === name}
            disabled={preparing || !!approval || !!direct}
            onClick={() => {
              setSection(name);
              setError(undefined);
              setResult(undefined);
              setSecret(undefined);
            }}
          >
            {sectionTitles[name]}
          </button>
        ))}
      </nav>
      {error !== undefined && <SystemError error={error} />}
      {notice && (
        <p className="notice" role="status" aria-live="polite">
          {notice}
        </p>
      )}
      {result !== undefined && <Receipt value={result} />}
      {secret && (
        <OneTimeSecret value={secret} clear={() => setSecret(undefined)} />
      )}
      <section className="panel system-section">
        <h2>当前项目范围</h2>
        <div className="system-inline">
          <label>
            已授权项目
            <select
              value={projectID}
              onChange={(e) => {
                setProjectID(e.target.value);
                setMemberProjectID("");
                setMemberProjectDraft("");
              }}
              disabled={projectsBusy || !!approval || !!direct}
            >
              <option value="">请选择项目</option>
              {projects.map((p) => (
                <option key={p.project_id} value={p.project_id}>
                  {p.description.name || p.key} · {p.key}
                </option>
              ))}
            </select>
          </label>
          <button
            className="secondary"
            disabled={projectsBusy || !!approval || !!direct}
            onClick={() => void loadProjects()}
          >
            刷新项目
          </button>
          {nextProject && (
            <button
              className="secondary"
              disabled={projectsBusy || !!approval || !!direct}
              onClick={() => void loadProjects(nextProject)}
            >
              读取下一页项目
            </button>
          )}
        </div>
        {project && (
          <p>
            项目 <code>{project.project_id}</code> · {project.state} ·{" "}
            {project.description.project_type}
          </p>
        )}
        {!projectsBusy && !projects.length && (
          <p>
            当前范围没有可见项目；可读取下一页时请继续，不推断全馆没有项目。
          </p>
        )}
      </section>
      {section === "projects" && (
        <>
          <ProjectCreation
            direct={stageDirect}
            disabled={disabled || !systemAdmin}
          />
          <section className="panel system-section">
            <h2>按明确项目标识管理成员</h2>
            <p>
              新建项目不会自动授予读取角色。可将创建回执中的项目标识填在这里，再指定负责人；
              这里只查询成员访问控制，不将该项目加入素材读取范围。服务器仍检查本次成员查询和每次角色变更的权限。
            </p>
            <form
              className="system-inline"
              onSubmit={(e) => {
                e.preventDefault();
                setError(undefined);
                try {
                  setMemberProjectID(identifier(memberProjectDraft.trim()));
                } catch (err) {
                  setError(err);
                }
              }}
            >
              <label>
                成员管理的明确项目标识
                <input
                  value={memberProjectDraft}
                  onChange={(e) => setMemberProjectDraft(e.target.value.trim())}
                  maxLength={26}
                  required
                  disabled={disabled}
                />
              </label>
              <button className="secondary" disabled={disabled}>
                读取此项目成员
              </button>
              {memberProjectID && (
                <button
                  type="button"
                  className="text-button"
                  disabled={disabled}
                  onClick={() => {
                    setMemberProjectID("");
                    setMemberProjectDraft("");
                  }}
                >
                  返回所选可读项目
                </button>
              )}
            </form>
          </section>
          {memberProjectID || project ? (
            <MemberManagement
              key={memberProjectID || project!.project_id}
              projectID={memberProjectID || project!.project_id}
              request={request}
              sensitive={stageSensitive}
              disabled={
                disabled || !human(who) || !who.session.scopes.includes("admin")
              }
              refresh={refresh}
            />
          ) : (
            <p className="notice">选择已授权项目后查看成员及修订。</p>
          )}
          {systemAdmin && <InstanceFacts request={request} />}
        </>
      )}
      {section === "principals" &&
        (systemAdmin ? (
          <PrincipalManagement
            key={`${who.session.session_id}:${refresh}`}
            request={request}
            sensitive={stageSensitive}
            disabled={disabled}
            refresh={refresh}
            projects={projects}
          />
        ) : (
          <p className="notice">
            主体和凭据管理需要人的系统管理员角色及 admin
            scope。项目成员管理仍按项目权限读取。
          </p>
        ))}
      {(section === "collaboration" || section === "audit") &&
        (project ? (
          <BusinessControls
            key={`${project.project_id}:${section}`}
            project={project}
            request={request}
            sensitive={stageSensitive}
            direct={stageDirect}
            disabled={disabled || !human(who)}
            refresh={refresh}
            view={section === "audit" ? "audit" : "collaboration"}
          />
        ) : (
          <p className="notice">先选择明确的项目范围。</p>
        ))}
      {section === "devices" &&
        (project ? (
          <DeviceManagement
            key={project.project_id}
            projectID={project.project_id}
            request={request}
            disabled={
              disabled ||
              !human(who) ||
              !who.session.scopes.includes("organize")
            }
          />
        ) : (
          <p className="notice">
            先选择当前有权访问的明确项目，再认领或核对该项目的本人设备。
          </p>
        ))}
      <section className="panel system-section">
        <h2>原操作诊断续办</h2>
        <p>
          只载入当前服务的明确原请求。诊断不含
          HumanGrant、动态码、口令或一次性秘密；载入后不会自动执行。
        </p>
        <label>
          导入原操作诊断 JSON
          <input
            type="file"
            accept="application/json,.json"
            disabled={disabled}
            onChange={(e) => {
              const file = e.target.files?.[0];
              e.currentTarget.value = "";
              void importDiagnostic(file);
            }}
          />
        </label>
      </section>
      {approval && (
        <SensitiveConfirmation
          key={approval.key}
          approval={approval}
          request={request}
          retain={setApproval}
          complete={finish}
          dismiss={dismiss}
        />
      )}
      {direct && (
        <DirectConfirmation
          key={direct.key}
          intent={direct}
          request={request}
          retain={setDirect}
          complete={finish}
          dismiss={dismiss}
        />
      )}
    </div>
  );
}

function ProjectCreation({
  direct,
  disabled,
}: {
  direct: StageDirect;
  disabled: boolean;
}) {
  const [key, setKey] = useState("");
  const [name, setName] = useState("");
  const [summary, setSummary] = useState("");
  const [kind, setKind] = useState("library");
  return (
    <section className="panel system-section">
      <h2>创建项目</h2>
      <p>
        只创建登记项目，不自动授予所有成员角色。需要当前人类系统管理员和相应
        scope。
      </p>
      <form
        className="system-form"
        onSubmit={(e) => {
          e.preventDefault();
          direct("创建项目", "/api/v1/projects", {
            key: key.trim(),
            name: name.trim(),
            summary: summary.trim(),
            project_type: kind,
          });
        }}
      >
        <label>
          项目 key
          <input
            value={key}
            onChange={(e) => setKey(e.target.value)}
            pattern="[a-z][a-z0-9-]{0,62}"
            maxLength={63}
            required
            disabled={disabled}
          />
        </label>
        <label>
          名称
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            maxLength={256}
            disabled={disabled}
          />
        </label>
        <label>
          项目类别
          <select
            value={kind}
            onChange={(e) => setKind(e.target.value)}
            disabled={disabled}
          >
            <option value="library">素材库</option>
            <option value="production">制作</option>
            <option value="reference">参考</option>
            <option value="sandbox">合成试验</option>
          </select>
        </label>
        <label>
          说明
          <textarea
            value={summary}
            onChange={(e) => setSummary(e.target.value)}
            maxLength={4096}
            disabled={disabled}
          />
        </label>
        <button className="primary" disabled={disabled}>
          核对创建请求
        </button>
      </form>
    </section>
  );
}

function MemberManagement({
  projectID,
  request,
  sensitive,
  disabled,
  refresh,
}: {
  projectID: string;
  request: Transport;
  sensitive: (intent: SensitiveIntent) => void;
  disabled: boolean;
  refresh: number;
}) {
  const [data, setData] = useState<{ items: Member[]; revision: number }>();
  const [principalID, setPrincipalID] = useState("");
  const [role, setRole] = useState<string>("viewer");
  const [grant, setGrant] = useState(true);
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const [reload, setReload] = useState(0);
  useEffect(() => {
    const abort = new AbortController();
    queueMicrotask(() => {
      if (abort.signal.aborted) return;
      setData(undefined);
      setError(undefined);
      setBusy(true);
      request<{ items: Member[]; revision: number }>(
        `/api/v1/identity/projects/${encodeURIComponent(projectID)}/members`,
        { signal: abort.signal },
      )
        .then((out) => {
          if (!abort.signal.aborted) setData(out);
        })
        .catch((e) => {
          if (!abort.signal.aborted) setError(e);
        })
        .finally(() => {
          if (!abort.signal.aborted) setBusy(false);
        });
    });
    return () => abort.abort();
  }, [projectID, request, refresh, reload]);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setError(undefined);
    try {
      if (!data) throw new Error("先读取最新成员修订。");
      sensitive(roleIntent(projectID, data.revision, principalID, role, grant));
    } catch (err) {
      setError(err);
    }
  };
  return (
    <section className="panel system-section">
      <h2>成员与项目角色</h2>
      <p>
        成员管理项目：<code>{projectID}</code>
      </p>
      <button
        className="secondary"
        disabled={busy || disabled}
        onClick={() => setReload((n) => n + 1)}
      >
        读取当前成员修订
      </button>
      {error !== undefined && <SystemError error={error} />}
      {busy && <p role="status">正在读取成员…</p>}
      {data && (
        <>
          <p>访问控制修订：{data.revision}。确认时和最终接受时均绑定该修订。</p>
          <div className="system-table-scroll">
            <table>
              <thead>
                <tr>
                  <th>主体</th>
                  <th>类别</th>
                  <th>角色</th>
                  <th>选择</th>
                </tr>
              </thead>
              <tbody>
                {data.items.map((m) => (
                  <tr key={m.principal_id}>
                    <td>
                      {m.name}
                      <br />
                      <code>{m.principal_id}</code>
                    </td>
                    <td>{m.kind}</td>
                    <td>{m.roles.join("、")}</td>
                    <td>
                      <button
                        className="secondary"
                        disabled={disabled}
                        onClick={() => setPrincipalID(m.principal_id)}
                      >
                        选择此主体
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <form className="system-form" onSubmit={submit}>
            <label>
              主体标识
              <input
                value={principalID}
                onChange={(e) => setPrincipalID(e.target.value.trim())}
                maxLength={26}
                required
                disabled={disabled}
              />
            </label>
            <label>
              项目角色
              <select
                value={role}
                onChange={(e) => setRole(e.target.value)}
                disabled={disabled}
              >
                {roles.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
            </label>
            <label>
              动作
              <select
                value={grant ? "grant" : "revoke"}
                onChange={(e) => setGrant(e.target.value === "grant")}
                disabled={disabled}
              >
                <option value="grant">授予</option>
                <option value="revoke">撤销</option>
              </select>
            </label>
            <button className="primary" disabled={disabled}>
              请求动作绑定的人类确认
            </button>
          </form>
        </>
      )}
    </section>
  );
}

function PrincipalManagement({
  request,
  sensitive,
  disabled,
  refresh,
  projects,
}: {
  request: Transport;
  sensitive: (intent: SensitiveIntent) => void;
  disabled: boolean;
  refresh: number;
  projects: Project[];
}) {
  const [rows, setRows] = useState<Principal[]>([]);
  const [cursor, setCursor] = useState("");
  const [lookupID, setLookupID] = useState("");
  const [selected, setSelected] = useState<Principal>();
  const [credentials, setCredentials] = useState<Credential[]>([]);
  const [credentialCursor, setCredentialCursor] = useState("");
  const [credentialID, setCredentialID] = useState("");
  const [revokeReason, setRevokeReason] = useState("");
  const [kind, setKind] = useState("human");
  const [name, setName] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [agentType, setAgentType] = useState("");
  const [node, setNode] = useState("");
  const [model, setModel] = useState("");
  const [capabilities, setCapabilities] = useState("");
  const [scopes, setScopes] = useState<string[]>(["self", "read"]);
  const [scopeProjects, setScopeProjects] = useState<string[]>([]);
  const [ttl, setTTL] = useState("86400");
  const [label, setLabel] = useState("");
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const alive = useRef(true);
  const lock = useRef(false);
  const abort = useRef(new AbortController());
  useEffect(() => {
    alive.current = true;
    const scope = new AbortController();
    abort.current = scope;
    return () => {
      alive.current = false;
      scope.abort();
    };
  }, []);
  const work = async (fn: () => Promise<void>) => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    try {
      await fn();
    } catch (e) {
      if (alive.current) {
        setSelected(undefined);
        setCredentials([]);
        setCredentialCursor("");
        setError(e);
      }
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  const list = useCallback(
    async (next = "") => {
      if (lock.current) return;
      lock.current = true;
      setError(undefined);
      setBusy(true);
      if (!next) {
        setRows([]);
        setCursor("");
        setSelected(undefined);
        setCredentials([]);
        setCredentialCursor("");
      }
      try {
        const q = new URLSearchParams({ limit: "100" });
        if (next) q.set("cursor", next);
        const out = await request<Page<Principal>>(
          `/api/v1/identity/principals?${q}`,
          { signal: abort.current.signal },
        );
        if (alive.current) {
          if (out.next_cursor && out.next_cursor === next)
            throw new Error("主体游标未推进。");
          setRows((old) =>
            next ? [...old, ...(out.items || [])] : out.items || [],
          );
          setCursor(out.next_cursor || "");
        }
      } catch (e) {
        if (alive.current) {
          setRows([]);
          setCursor("");
          setSelected(undefined);
          setCredentials([]);
          setError(e);
        }
      } finally {
        lock.current = false;
        if (alive.current) setBusy(false);
      }
    },
    [request],
  );
  useEffect(() => {
    let active = true;
    queueMicrotask(() => {
      if (active) void list();
    });
    return () => {
      active = false;
    };
  }, [list, refresh]);
  const readCredentials = async (principal: Principal, next = "") => {
    const q = new URLSearchParams({ limit: "100" });
    if (next) q.set("cursor", next);
    const out = await request<Page<Credential>>(
      `/api/v1/identity/principals/${encodeURIComponent(principal.principal_id)}/credentials?${q}`,
      { signal: abort.current.signal },
    );
    if (alive.current) {
      if (out.next_cursor && out.next_cursor === next)
        throw new Error("凭据游标未推进。");
      setCredentials((old) =>
        next ? [...old, ...(out.items || [])] : out.items || [],
      );
      setCredentialCursor(out.next_cursor || "");
    }
  };
  const lookup = (id = lookupID) =>
    void work(async () => {
      setSelected(undefined);
      setCredentials([]);
      setCredentialCursor("");
      const p = await request<Principal>(
        `/api/v1/identity/principals/${encodeURIComponent(identifier(id))}`,
        { signal: abort.current.signal },
      );
      if (p.principal_id !== id || !kindScopes[p.kind])
        throw new Error("主体响应标识或类别不符。");
      if (alive.current) {
        setLookupID(id);
        setSelected(p);
        setScopes(
          kindScopes[p.kind].includes("read") ? ["self", "read"] : ["self"],
        );
      }
      await readCredentials(p);
    });
  const stage = (fn: () => SensitiveIntent) => {
    setError(undefined);
    try {
      sensitive(fn());
    } catch (e) {
      setError(e);
    }
  };
  return (
    <div className="system-stack">
      <section className="panel system-section">
        <h2>当前登记主体</h2>
        <p>
          只有人的非委托管理员会话可读此列表；展示身份元数据，不返回凭据秘密。
        </p>
        <div className="system-actions">
          <button
            className="secondary"
            disabled={busy || disabled}
            onClick={() => void list()}
          >
            刷新主体
          </button>
          {cursor && (
            <button
              className="secondary"
              disabled={busy || disabled}
              onClick={() => void list(cursor)}
            >
              下一页主体
            </button>
          )}
        </div>
        {error !== undefined && <SystemError error={error} />}
        <div className="system-table-scroll">
          <table>
            <thead>
              <tr>
                <th>主体</th>
                <th>类别／状态</th>
                <th>修订</th>
                <th>查看</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((p) => (
                <tr key={p.principal_id}>
                  <td>
                    {p.display_name || p.name}
                    <br />
                    {p.name}
                    <br />
                    <code>{p.principal_id}</code>
                  </td>
                  <td>
                    {p.kind} / {p.state}
                  </td>
                  <td>{p.revision}</td>
                  <td>
                    <button
                      className="secondary"
                      disabled={busy || disabled}
                      onClick={() => lookup(p.principal_id)}
                    >
                      读取及管理凭据
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="system-inline">
          <label>
            按已知主体标识查询
            <input
              value={lookupID}
              onChange={(e) => {
                setLookupID(e.target.value.trim());
                setSelected(undefined);
                setCredentials([]);
              }}
              maxLength={26}
              disabled={busy}
            />
          </label>
          <button
            className="secondary"
            disabled={busy || !lookupID || disabled}
            onClick={() => lookup()}
          >
            核对当前主体
          </button>
        </div>
      </section>
      <section className="panel system-section">
        <h2>登记新主体</h2>
        <p>
          登记 human
          返回一次性设置码；不会设置默认口令、自动分配项目或系统管理员角色。
        </p>
        <form
          className="system-form"
          onSubmit={(e) => {
            e.preventDefault();
            stage(() =>
              principalIntent(
                kind,
                name.trim(),
                displayName.trim(),
                kind === "human"
                  ? {}
                  : {
                      ...(agentType ? { agent_type: agentType.trim() } : {}),
                      ...(node ? { node: node.trim() } : {}),
                      ...(model ? { default_model: model.trim() } : {}),
                      ...(capabilities.trim()
                        ? {
                            capabilities: capabilities
                              .split(/[\s,]+/)
                              .filter(Boolean),
                          }
                        : {}),
                    },
              ),
            );
          }}
        >
          <label>
            主体类别
            <select
              value={kind}
              onChange={(e) => setKind(e.target.value)}
              disabled={disabled}
            >
              {Object.keys(kindScopes).map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
          </label>
          <label>
            规范名称
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
              maxLength={128}
              disabled={disabled}
              placeholder={
                kind === "human"
                  ? "ada"
                  : kind === "agent"
                    ? "editor@node-a"
                    : `${kind}:name`
              }
            />
          </label>
          <label>
            显示名称
            <input
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
              maxLength={128}
              disabled={disabled}
            />
          </label>
          {kind !== "human" && (
            <>
              <label>
                Agent 类型（可选）
                <input
                  value={agentType}
                  onChange={(e) => setAgentType(e.target.value)}
                  maxLength={128}
                  disabled={disabled}
                />
              </label>
              <label>
                节点描述（可选）
                <input
                  value={node}
                  onChange={(e) => setNode(e.target.value)}
                  maxLength={128}
                  disabled={disabled}
                />
              </label>
              <label>
                默认模型描述（可选）
                <input
                  value={model}
                  onChange={(e) => setModel(e.target.value)}
                  maxLength={128}
                  disabled={disabled}
                />
              </label>
              <label>
                能力标签（小写，逗号分隔；不是权限）
                <input
                  value={capabilities}
                  onChange={(e) => setCapabilities(e.target.value)}
                  maxLength={2048}
                  disabled={disabled}
                />
              </label>
            </>
          )}
          <button className="primary" disabled={disabled}>
            请求登记确认
          </button>
        </form>
      </section>
      {selected && (
        <section className="panel system-section">
          <h2>{selected.name} 的凭据</h2>
          <p>
            主体当前修订 {selected.revision}，状态 {selected.state}。scope
            只收窄会话，不授予项目角色。长期凭据仅第一次直接响应返回；此页面不自动复制或持久保存。
          </p>
          <form
            className="system-form"
            onSubmit={(e) => {
              e.preventDefault();
              stage(() =>
                credentialIntent({
                  principalID: selected.principal_id,
                  expectedRevision: selected.revision,
                  kind: selected.kind,
                  scopes,
                  projects: scopeProjects,
                  ttl: Number(ttl),
                  label: label.trim(),
                }),
              );
            }}
          >
            <fieldset disabled={disabled || selected.state !== "active"}>
              <legend>明确的 scope 范围</legend>
              {kindScopes[selected.kind].map((scope) => (
                <label className="system-check" key={scope}>
                  <input
                    type="checkbox"
                    checked={scopes.includes(scope)}
                    onChange={(e) =>
                      setScopes((old) =>
                        e.target.checked
                          ? [...old, scope]
                          : old.filter((s) => s !== scope),
                      )
                    }
                  />
                  {scope}
                </label>
              ))}
            </fieldset>
            <fieldset disabled={disabled}>
              <legend>
                限制到已选项目（不选表示不添加此范围限制，仍受实际角色约束）
              </legend>
              {projects.map((p) => (
                <label className="system-check" key={p.project_id}>
                  <input
                    type="checkbox"
                    checked={scopeProjects.includes(p.project_id)}
                    onChange={(e) =>
                      setScopeProjects((old) =>
                        e.target.checked
                          ? [...old, p.project_id]
                          : old.filter((id) => id !== p.project_id),
                      )
                    }
                  />
                  {p.description.name || p.key}
                </label>
              ))}
            </fieldset>
            <label>
              有效秒数（0 表示使用核心默认）
              <input
                type="number"
                min="0"
                step="1"
                value={ttl}
                onChange={(e) => setTTL(e.target.value)}
                required
                disabled={disabled}
              />
            </label>
            <label>
              凭据标签
              <input
                value={label}
                onChange={(e) => setLabel(e.target.value)}
                maxLength={128}
                disabled={disabled}
              />
            </label>
            <button
              className="primary"
              disabled={disabled || selected.state !== "active"}
            >
              请求签发确认
            </button>
          </form>
          <div className="system-table-scroll">
            <table>
              <thead>
                <tr>
                  <th>凭据前缀／标识</th>
                  <th>范围</th>
                  <th>状态／期限</th>
                  <th>选择</th>
                </tr>
              </thead>
              <tbody>
                {credentials.map((c) => (
                  <tr key={c.credential_id}>
                    <td>
                      {c.prefix}…<br />
                      <code>{c.credential_id}</code>
                      <br />
                      {c.label}
                    </td>
                    <td>
                      {c.scopes.join("、")}
                      <br />
                      {c.projects.join("、") || "未附加项目范围"}
                    </td>
                    <td>
                      {c.revoked_at
                        ? `已吊销 ${c.revoked_at}`
                        : `有效期 ${c.expires_at}`}
                    </td>
                    <td>
                      <button
                        className="secondary"
                        disabled={disabled || !!c.revoked_at}
                        onClick={() => setCredentialID(c.credential_id)}
                      >
                        选择吊销
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {credentialCursor && (
            <button
              className="secondary"
              disabled={busy || disabled}
              onClick={() =>
                void work(() => readCredentials(selected, credentialCursor))
              }
            >
              下一页凭据
            </button>
          )}
        </section>
      )}
      <section className="panel system-section">
        <h2>吊销明确的凭据</h2>
        <form
          className="system-form"
          onSubmit={(e) => {
            e.preventDefault();
            stage(() => revokeCredentialIntent(credentialID, revokeReason));
          }}
        >
          <label>
            凭据标识
            <input
              value={credentialID}
              onChange={(e) => setCredentialID(e.target.value.trim())}
              maxLength={26}
              required
              disabled={disabled}
            />
          </label>
          <label>
            吊销理由
            <input
              value={revokeReason}
              onChange={(e) => setRevokeReason(e.target.value)}
              maxLength={128}
              required
              disabled={disabled}
            />
          </label>
          <button className="secondary" disabled={disabled}>
            请求吊销确认
          </button>
        </form>
        <p className="subtle">
          主体停用、系统角色/策略和会话强制撤销尚未在当前封闭公共身份命令中开放，页面不会伪造这些动作。
        </p>
      </section>
    </div>
  );
}
