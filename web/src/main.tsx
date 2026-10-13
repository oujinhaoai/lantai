import { createRoot } from "react-dom/client";
import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
import {
  api,
  APIError,
  clearCSRF,
  hasCSRF,
  isSessionError,
  message,
  rememberCSRF,
  restoreCSRF,
  typeNames,
  type Asset,
  type ExactVersion,
  type Identity,
  type Item,
  type Page,
  type Project,
  type Session,
} from "./api";
import "./style.css";
import { FilePreview } from "./file-preview";

type Expire = (error: unknown) => void;
const href = (path: string) => `#${path}`;
const helpAvailable = import.meta.env.LANTAI_HELP_AVAILABLE === true;
const decodeKey = (key: string) => {
  try {
    return decodeURIComponent(key);
  } catch {
    return "";
  }
};
function useRoute() {
  const [route, setRoute] = useState(location.hash.slice(1) || "/");
  useEffect(() => {
    const change = () => setRoute(location.hash.slice(1) || "/");
    addEventListener("hashchange", change);
    return () => removeEventListener("hashchange", change);
  }, []);
  return route;
}
function Icon({ kind, size = 20 }: { kind: string; size?: number }) {
  const paths: Record<string, ReactNode> = {
    home: (
      <>
        <path d="m3 10 9-7 9 7v10H3Z" />
        <path d="M9 20v-7h6v7" />
      </>
    ),
    folder: <path d="M3 7V4h6l3 3h9v13H3Z" />,
    grid: (
      <>
        <rect x="3" y="3" width="7" height="7" rx="1" />
        <rect x="14" y="3" width="7" height="7" rx="1" />
        <rect x="3" y="14" width="7" height="7" rx="1" />
        <rect x="14" y="14" width="7" height="7" rx="1" />
      </>
    ),
    search: (
      <>
        <circle cx="10" cy="10" r="6" />
        <path d="m15 15 6 6" />
      </>
    ),
    doc: (
      <>
        <path d="M5 3h9l5 5v13H5Z" />
        <path d="M14 3v6h5M8 13h8M8 17h6" />
      </>
    ),
    image: (
      <>
        <rect x="3" y="3" width="18" height="18" rx="2" />
        <circle cx="8" cy="8" r="1.5" />
        <path d="m3 18 6-6 4 4 3-6 5 8" />
      </>
    ),
    lock: (
      <>
        <rect x="5" y="10" width="14" height="11" rx="2" />
        <path d="M8 10V7a4 4 0 0 1 8 0v3M12 14v3" />
      </>
    ),
    arrow: <path d="M5 12h14m-6-6 6 6-6 6" />,
    back: <path d="M19 12H5m6-6-6 6 6 6" />,
    exit: (
      <>
        <path d="M10 4H4v16h6m4-13 5 5-5 5m-6-5h13" />
      </>
    ),
    refresh: (
      <>
        <path d="M20 8a8 8 0 1 0 0 8M20 3v6h-6" />
      </>
    ),
    check: <path d="m5 12 4 4L19 6" />,
  };
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {paths[kind] || paths.doc}
    </svg>
  );
}
function ErrorBox({ error, retry }: { error: unknown; retry?: () => void }) {
  return (
    <div className="error" role="alert">
      <strong>{message(error)}</strong>
      {error instanceof APIError && error.requestID && (
        <small>请求编号：{error.requestID}</small>
      )}
      {retry && (
        <button className="secondary" onClick={retry}>
          重试
        </button>
      )}
    </div>
  );
}
function Loading() {
  return (
    <div className="loading" role="status">
      <span className="spinner" />
      正在读取…
    </div>
  );
}
function Empty({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="empty">
      <span className="empty-icon">
        <Icon kind="folder" size={28} />
      </span>
      <h3>{title}</h3>
      <p>{children}</p>
    </div>
  );
}
function useRead<T>(path: string, expire: Expire) {
  const [retry, setRetry] = useState(0);
  const [state, setState] = useState<{
    key: string;
    data?: T;
    error?: unknown;
  }>({ key: "" });
  const key = `${path}|${retry}`;
  useEffect(() => {
    const controller = new AbortController();
    api<T>(path, { signal: controller.signal })
      .then((data) => {
        if (!controller.signal.aborted) setState({ key, data });
      })
      .catch((error) => {
        if (!controller.signal.aborted) {
          if (isSessionError(error)) expire(error);
          else setState({ key, error });
        }
      });
    return () => controller.abort();
  }, [path, retry, key, expire]);
  return {
    data: state.key === key ? state.data : undefined,
    error: state.key === key ? state.error : undefined,
    reload: () => setRetry((n) => n + 1),
  };
}
function Resource<T>({
  path,
  expire,
  children,
}: {
  path: string;
  expire: Expire;
  children: (data: T) => ReactNode;
}) {
  const { data, error, reload } = useRead<T>(path, expire);
  return error ? (
    <ErrorBox error={error} retry={reload} />
  ) : data === undefined ? (
    <Loading />
  ) : (
    children(data)
  );
}
function Heading({
  eyebrow,
  title,
  children,
}: {
  eyebrow: string;
  title: string;
  children?: ReactNode;
}) {
  return (
    <header className="page-heading">
      <p className="eyebrow">{eyebrow}</p>
      <h1 tabIndex={-1}>{title}</h1>
      {children && <p>{children}</p>}
    </header>
  );
}
function ProjectCard({ project }: { project: Project }) {
  const labels: Record<string, string> = {
    production: "制作项目",
    library: "素材库",
    reference: "参考资料",
    sandbox: "试验项目",
  };
  return (
    <a
      className="project-card"
      href={href(`/projects/${encodeURIComponent(project.key)}`)}
    >
      <span className="project-icon">
        <Icon kind="folder" size={25} />
      </span>
      <span className="badge">
        {project.state === "archived"
          ? "已归档"
          : labels[project.description.project_type] || "项目"}
      </span>
      <h3>{project.description.name || project.key}</h3>
      <p>{project.description.summary || "这个项目还没有填写简介。"}</p>
      <div className="card-foot">
        <code>{project.key}</code>
        <Icon kind="arrow" size={17} />
      </div>
    </a>
  );
}
function AssetRows({ items, from }: { items: Item[]; from: string }) {
  return (
    <ul className="asset-list">
      {items.map((item) => (
        <li key={item.asset_id}>
          <a
            href={href(
              `/assets/${item.asset_id}?from=${encodeURIComponent(from)}`,
            )}
          >
            <span className={`asset-icon ${item.asset_type}`}>
              <Icon
                kind={item.asset_type === "image" ? "image" : "doc"}
                size={23}
              />
            </span>
            <span className="asset-copy">
              <strong>{item.title || item.slug}</strong>
              <small>{item.summary || item.slug}</small>
            </span>
            <span className="type-label">
              {typeNames[item.asset_type] || item.asset_type}
            </span>
            <span className="version">v{item.version_number}</span>
            <Icon kind="arrow" size={17} />
          </a>
        </li>
      ))}
    </ul>
  );
}
function Home({ who, expire }: { who: Identity; expire: Expire }) {
  return (
    <>
      <Heading
        eyebrow="工作台 / OVERVIEW"
        title={`你好，${who.principal.display_name || who.principal.name}`}
      >
        从项目出发，找到下一步需要的素材。
      </Heading>
      <div className="welcome">
        <div>
          <span className="eyebrow">每一份素材，都有来处</span>
          <h2>
            你的项目与资源，
            <br />
            现在触手可及。
          </h2>
          <p>浏览已归档的内容，查看著录与固定版本。</p>
          <a className="primary" href={href("/assets")}>
            浏览资源 <Icon kind="arrow" size={17} />
          </a>
        </div>
        <div className="archive-art" aria-hidden="true">
          <span />
          <span />
          <span />
          <b>
            兰<br />台
          </b>
        </div>
      </div>
      <section>
        <div className="section-head">
          <h2>我的项目</h2>
          <a href={href("/projects")}>
            查看全部 <Icon kind="arrow" size={16} />
          </a>
        </div>
        <Resource<Page<Project>>
          path="/api/v1/projects?limit=6"
          expire={expire}
        >
          {(page) =>
            page.items?.length ? (
              <div className="project-grid">
                {page.items.map((project) => (
                  <ProjectCard key={project.project_id} project={project} />
                ))}
              </div>
            ) : (
              <Empty title="还没有可查看的项目">
                项目成员授权后，内容会出现在这里。管理员身份不会自动获得项目读取权限。
              </Empty>
            )
          }
        </Resource>
      </section>
      <section>
        <div className="section-head">
          <h2>资源速览</h2>
          <a href={href("/assets")}>
            进入资源库 <Icon kind="arrow" size={16} />
          </a>
        </div>
        <Resource<Page<Item>> path="/api/v1/assets?limit=6" expire={expire}>
          {(page) =>
            page.items?.length ? (
              <AssetRows items={page.items} from="/" />
            ) : (
              <Empty title="暂无可见资源">
                资源入库并获得读取权限后，即可在这里浏览。
                {page.next_cursor && (
                  <a href={href("/assets")}>继续查看后续页面</a>
                )}
              </Empty>
            )
          }
        </Resource>
      </section>
    </>
  );
}
function Paged<T extends Project | Item>({
  path,
  expire,
  children,
  empty,
}: {
  path: string;
  expire: Expire;
  children: (items: T[]) => ReactNode;
  empty: ReactNode;
}) {
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<{
    items?: T[];
    cursor?: string;
    error?: unknown;
  }>({});
  const [busy, setBusy] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const inFlight = useRef(false);
  const load = useCallback(
    (cursor?: string) => {
      if (inFlight.current) return;
      inFlight.current = true;
      const request = new AbortController();
      controller.current = request;
      setBusy(true);
      if (!cursor) setState({});
      api<Page<T>>(
        `${path}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`,
        { signal: request.signal },
      )
        .then((page) => {
          if (request.signal.aborted) return;
          setState((previous) => ({
            items: cursor
              ? [...(previous.items || []), ...(page.items || [])]
              : page.items || [],
            cursor: page.next_cursor,
          }));
        })
        .catch((error) => {
          if (request.signal.aborted) return;
          if (isSessionError(error)) expire(error);
          else setState((previous) => ({ ...previous, error }));
        })
        .finally(() => {
          if (!request.signal.aborted) {
            inFlight.current = false;
            setBusy(false);
          }
        });
    },
    [path, expire],
  );
  useEffect(() => {
    let active = true;
    queueMicrotask(() => {
      if (active) load();
    });
    return () => {
      active = false;
      controller.current?.abort();
      inFlight.current = false;
    };
  }, [load, revision]);
  return (
    <>
      <div className="list-meta">
        <span>
          {state.items
            ? `已显示 ${state.items.length} 项 · 仅含当前可见内容`
            : "按当前项目权限读取"}
        </span>
        <button
          className="text-button"
          disabled={busy}
          onClick={() => setRevision((n) => n + 1)}
        >
          <Icon kind="refresh" size={16} />
          刷新
        </button>
      </div>
      {state.items?.length
        ? children(state.items)
        : state.items && !state.error
          ? empty
          : null}
      {state.error ? (
        <ErrorBox
          error={state.error}
          retry={() =>
            state.cursor &&
            !(
              state.error instanceof APIError &&
              state.error.code === "CURSOR_EXPIRED"
            )
              ? load(state.cursor)
              : setRevision((n) => n + 1)
          }
        />
      ) : null}
      {busy && <Loading />}
      {state.cursor && !busy && !state.error && (
        <button className="secondary more" onClick={() => load(state.cursor)}>
          加载下一页
        </button>
      )}
    </>
  );
}
function Projects({ expire }: { expire: Expire }) {
  return (
    <>
      <Heading eyebrow="工作台 / PROJECTS" title="我的项目">
        这里仅展示当前会话有权查看的项目。
      </Heading>
      <Paged<Project>
        path="/api/v1/projects?limit=24"
        expire={expire}
        empty={
          <Empty title="还没有可查看的项目">
            请联系项目管理员确认成员角色与读取权限。
          </Empty>
        }
      >
        {(items) => (
          <div className="project-grid">
            {items.map((project) => (
              <ProjectCard key={project.project_id} project={project} />
            ))}
          </div>
        )}
      </Paged>
    </>
  );
}
function Search({
  project,
  route,
  expire,
}: {
  project?: Project;
  route: string;
  expire: Expire;
}) {
  const params = new URLSearchParams(route.split("?")[1]);
  const text = params.get("q") || "";
  const type = params.get("type") || "";
  const [draft, setDraft] = useState(text);
  const [draftType, setDraftType] = useState(type);
  const search = (event: FormEvent) => {
    event.preventDefault();
    const next = new URLSearchParams();
    if (draft.trim()) next.set("q", draft.trim());
    if (draftType) next.set("type", draftType);
    location.hash = href(
      `${route.split("?")[0]}${next.size ? `?${next}` : ""}`,
    );
  };
  const query = new URLSearchParams({ limit: "24" });
  if (text) query.set("q", text);
  if (type) query.set("asset_type", type);
  if (project) query.set("project_id", project.project_id);
  return (
    <>
      <form className="search-bar" onSubmit={search} role="search">
        <label className="search-field">
          <Icon kind="search" />
          <span className="sr-only">搜索资源</span>
          <input
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            maxLength={512}
            placeholder="搜索标题、摘要、标签…"
          />
        </label>
        <label>
          <span className="sr-only">资源类型</span>
          <select
            value={draftType}
            onChange={(e) => setDraftType(e.target.value)}
          >
            <option value="">全部类型</option>
            {Object.entries(typeNames).map(([key, label]) => (
              <option value={key} key={key}>
                {label}
              </option>
            ))}
          </select>
        </label>
        <button className="primary" type="submit">
          搜索
        </button>
      </form>
      {(text || type) && (
        <p className="filter-note">
          当前筛选：{text && `“${text}”`}
          {text && type && " · "}
          {type && (typeNames[type] || type)}{" "}
          <a href={href(route.split("?")[0])}>清除筛选</a>
        </p>
      )}
      <Paged<Item>
        key={query.toString()}
        path={`/api/v1/assets?${query}`}
        expire={expire}
        empty={
          <Empty title={text || type ? "没有匹配的资源" : "暂无可见资源"}>
            {text || type
              ? "试试其他关键词或清除类型筛选。"
              : "当前范围还没有可读取的资源。资源入库或权限变更后可刷新。"}
          </Empty>
        }
      >
        {(items) => <AssetRows items={items} from={route} />}
      </Paged>
    </>
  );
}
function ProjectDetail({
  projectKey,
  route,
  expire,
}: {
  projectKey: string;
  route: string;
  expire: Expire;
}) {
  return (
    <Resource<Project>
      path={`/api/v1/projects/${encodeURIComponent(projectKey)}?view=full`}
      expire={expire}
    >
      {(project) => (
        <>
          <a className="back" href={href("/projects")}>
            <Icon kind="back" size={17} />
            返回项目
          </a>
          <Heading
            eyebrow={`项目 / ${project.key}`}
            title={project.description.name || project.key}
          >
            {project.description.summary || "这个项目还没有填写简介。"}
          </Heading>
          <div className="project-facts">
            <span>
              项目标识 <code>{project.project_id}</code>
            </span>
            <span>
              状态 <b>{project.state === "archived" ? "已归档" : "进行中"}</b>
            </span>
            {project.description_pending && <span>部分著录待完成</span>}
          </div>
          <section>
            <div className="section-head">
              <h2>项目资源</h2>
              <span>按当前读取权限展示</span>
            </div>
            <Search project={project} route={route} expire={expire} />
          </section>
        </>
      )}
    </Resource>
  );
}
const date = (value: string) =>
  new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
const bytes = (size: number) =>
  size < 1024
    ? `${size} B`
    : size < 1048576
      ? `${(size / 1024).toFixed(1)} KB`
      : `${(size / 1048576).toFixed(1)} MB`;
function AssetDetail({
  assetID,
  route,
  expire,
}: {
  assetID: string;
  route: string;
  expire: Expire;
}) {
  const from =
    new URLSearchParams(route.split("?")[1]).get("from") || "/assets";
  const safeFrom = /^\/(?:assets(?:\?|$)|projects(?:\/|$)|$)/.test(from)
    ? from
    : "/assets";
  return (
    <div className="asset-detail">
      <a className="back" href={href(safeFrom)}>
        <Icon kind="back" size={17} />
        返回列表
      </a>
      <Resource<Asset>
        path={`/api/v1/assets/${encodeURIComponent(assetID)}?view=full`}
        expire={expire}
      >
        {(asset) => (
          <>
            <Heading
              eyebrow={`资源 / ${typeNames[asset.asset_type] || asset.asset_type}`}
              title={asset.description.title || asset.description.slug}
            >
              {asset.description.summary}
            </Heading>
            <section>
              <div className="section-head">
                <h2>
                  当前固定版本{" "}
                  <span className="badge">v{asset.latest.version_number}</span>
                </h2>
                <span>{date(asset.latest.committed_at)}</span>
              </div>
              <Resource<ExactVersion>
                path={`/api/v1/assets/${encodeURIComponent(assetID)}/versions/${asset.latest.version_id}?view=full`}
                expire={expire}
              >
                {(exact) => (
                  <>
                    {exact.manifest?.content.version_note && (
                      <p>{exact.manifest.content.version_note}</p>
                    )}
                    {!!exact.manifest?.content.files.length && (
                      <FilePreview
                        files={exact.manifest.content.files}
                        assetType={asset.asset_type}
                        assetID={assetID}
                        versionID={asset.latest.version_id}
                        expire={expire}
                      />
                    )}
                    {exact.manifest?.content.files.length ? (
                      <details className="file-manifest">
                        <summary>查看文件清单与 SHA-256</summary>
                        <div className="table-scroll">
                          <table>
                            <thead>
                              <tr>
                                <th scope="col">文件路径</th>
                                <th scope="col">用途</th>
                                <th scope="col">大小</th>
                                <th scope="col">SHA-256</th>
                              </tr>
                            </thead>
                            <tbody>
                              {exact.manifest.content.files.map((file) => (
                                <tr key={file.path}>
                                  <td data-label="文件路径">{file.path}</td>
                                  <td data-label="用途">
                                    {file.role === "primary"
                                      ? "主文件"
                                      : file.role}
                                  </td>
                                  <td data-label="大小">{bytes(file.size)}</td>
                                  <td data-label="SHA-256">
                                    <code>{file.sha256}</code>
                                  </td>
                                </tr>
                              ))}
                            </tbody>
                          </table>
                        </div>
                      </details>
                    ) : (
                      <Empty title="没有可显示的文件清单">
                        此版本未返回文件清单。
                      </Empty>
                    )}
                  </>
                )}
              </Resource>
            </section>
            <details className="detail-metadata">
              <summary>著录与技术详情</summary>
              <div className="detail-grid">
                <section className="panel">
                  <h2>著录信息</h2>
                  <dl>
                    <dt>资源类型</dt>
                    <dd>{typeNames[asset.asset_type] || asset.asset_type}</dd>
                    <dt>资源路径名</dt>
                    <dd>{asset.description.slug}</dd>
                    <dt>著录修订</dt>
                    <dd>r{asset.description.revision}</dd>
                    <dt>标签</dt>
                    <dd className="tags">
                      {asset.description.tags?.length
                        ? asset.description.tags.map((tag) => (
                            <span key={tag}>{tag}</span>
                          ))
                        : "暂无标签"}
                    </dd>
                    <dt>主题</dt>
                    <dd>
                      {asset.description.subjects?.join("、") || "暂无主题"}
                    </dd>
                    <dt>资源标识</dt>
                    <dd>
                      <code>{asset.asset_id}</code>
                    </dd>
                  </dl>
                </section>
                <section className="panel">
                  <h2>
                    当前固定版本{" "}
                    <span className="badge">
                      v{asset.latest.version_number}
                    </span>
                  </h2>
                  <dl>
                    <dt>提交时间</dt>
                    <dd>{date(asset.latest.committed_at)}</dd>
                    <dt>版本标识</dt>
                    <dd>
                      <code>{asset.latest.version_id}</code>
                    </dd>
                    <dt>永久引用</dt>
                    <dd>
                      <code>{asset.latest.uri}</code>
                    </dd>
                    <dt>清单摘要</dt>
                    <dd>
                      <code>{asset.latest.manifest_digest}</code>
                    </dd>
                  </dl>
                  <p className="subtle">版本内容不可变，著录修订独立记录。</p>
                </section>
              </div>
            </details>
          </>
        )}
      </Resource>
    </div>
  );
}
function Workspace({
  who,
  expire,
  reauthenticate,
}: {
  who: Identity;
  expire: Expire;
  reauthenticate: () => void;
}) {
  const route = useRoute();
  const pathname = route.split("?")[0];
  const [loggingOut, setLoggingOut] = useState(false);
  const [logoutError, setLogoutError] = useState<unknown>();
  const logoutLock = useRef(false);
  const main = useRef<HTMLElement>(null);
  useEffect(() => {
    (main.current?.querySelector<HTMLElement>("h1") || main.current)?.focus();
    window.scrollTo(0, 0);
  }, [route]);
  const logout = async () => {
    if (logoutLock.current) return;
    if (!hasCSRF()) {
      reauthenticate();
      return;
    }
    logoutLock.current = true;
    setLoggingOut(true);
    setLogoutError(undefined);
    try {
      await api("/api/v1/sessions/current", { method: "DELETE" });
      expire(null);
    } catch (error) {
      if (isSessionError(error)) expire(error);
      else setLogoutError(error);
    } finally {
      logoutLock.current = false;
      setLoggingOut(false);
    }
  };
  const nav = [
    ["/", "home", "首页"],
    ["/projects", "folder", "我的项目"],
    ["/assets", "grid", "资源库"],
  ];
  const section = pathname.startsWith("/projects")
    ? "/projects"
    : pathname.startsWith("/assets")
      ? "/assets"
      : "/";
  return (
    <div className="app-shell">
      <a
        className="skip"
        href="#main"
        onClick={(event) => {
          event.preventDefault();
          main.current?.focus();
        }}
      >
        跳到主要内容
      </a>
      <aside className="sidebar">
        <a className="brand" href={href("/")}>
          <span className="brand-mark">兰</span>
          <span>
            兰台<small>LANTAI ARCHIVE</small>
          </span>
        </a>
        <p className="nav-caption">工作空间</p>
        <nav aria-label="主导航">
          {nav.map(([path, icon, label]) => (
            <a
              key={path}
              href={href(path)}
              aria-current={section === path ? "page" : undefined}
            >
              <Icon kind={icon} />
              {label}
            </a>
          ))}
          {helpAvailable && (
            <a className="help-link" href="/help/">
              使用帮助
            </a>
          )}
        </nav>
        <div className="sidebar-bottom">
          <Icon kind="lock" size={17} />
          <p>
            每一次读取
            <br />
            <strong>都在授权范围内</strong>
          </p>
        </div>
      </aside>
      <div className="workspace">
        <header className="topbar">
          <span>
            素材档案库 <span className="divider">/</span>{" "}
            {nav.find((n) => n[0] === section)?.[2]}
          </span>
          <div className="account">
            <span className="avatar">
              {(who.principal.display_name || who.principal.name).slice(0, 1)}
            </span>
            <span>{who.principal.display_name || who.principal.name}</span>
            <button
              className="text-button"
              onClick={logout}
              disabled={loggingOut}
            >
              <Icon kind="exit" size={16} />
              {loggingOut ? "正在退出" : hasCSRF() ? "退出" : "重新验证"}
            </button>
          </div>
        </header>
        <main id="main" ref={main} key={route} tabIndex={-1}>
          {logoutError ? <ErrorBox error={logoutError} /> : null}
          {!hasCSRF() && (
            <p className="notice">
              此标签页未保存登录校验信息，可继续浏览。退出会话请返回原登录标签页，或先重新验证。
            </p>
          )}
          {pathname === "/" ? (
            <Home who={who} expire={expire} />
          ) : pathname === "/projects" ? (
            <Projects expire={expire} />
          ) : pathname.startsWith("/projects/") &&
            decodeKey(pathname.slice(10)) ? (
            <ProjectDetail
              projectKey={decodeKey(pathname.slice(10))}
              route={route}
              expire={expire}
            />
          ) : pathname === "/assets" ? (
            <>
              <Heading eyebrow="工作台 / ASSETS" title="资源库">
                搜索著录，找到所需的固定版本。
              </Heading>
              <Search route={route} expire={expire} />
            </>
          ) : pathname.startsWith("/assets/") ? (
            <AssetDetail
              assetID={pathname.slice(8)}
              route={route}
              expire={expire}
            />
          ) : (
            <>
              <Heading eyebrow="PAGE NOT FOUND" title="这个页面不存在" />
              <a className="primary" href={href("/")}>
                回到首页
              </a>
            </>
          )}
          <footer>
            兰台 · 让素材有据可查{" "}
            <span>当前会话至 {date(who.session.expires_at)}</span>
          </footer>
        </main>
      </div>
    </div>
  );
}
function Login({
  onLogin,
  notice,
}: {
  onLogin: (who: Identity) => void;
  notice: string;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const inFlight = useRef(false);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    setError(undefined);
    const body = JSON.stringify({
      name: name.trim(),
      password,
      code,
      channel: "browser",
      scopes: ["self", "read"],
    });
    setPassword("");
    setCode("");
    try {
      const issued = await api<{ session: Session; csrf_token: string }>(
        "/api/v1/sessions/login",
        { method: "POST", body },
      );
      rememberCSRF(issued.session.session_id, issued.csrf_token);
      onLogin(await api<Identity>("/api/v1/whoami"));
    } catch (e) {
      setError(e);
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  };
  return (
    <div className="login-page">
      <div className="login-story">
        <a className="brand" href={href("/")}>
          <span className="brand-mark">兰</span>
          <span>
            兰台<small>LANTAI ARCHIVE</small>
          </span>
        </a>
        <div>
          <p className="eyebrow">素材有来处，协作有依据</p>
          <h1>
            让每一份创造，
            <br />
            都有迹可循。
          </h1>
          <p>
            连接项目、素材与它们的故事。
            <br />
            在你的授权范围内，安心浏览。
          </p>
        </div>
        <span className="story-foot">ARCHIVE · CONTEXT · COLLABORATION</span>
      </div>
      <main className="login-main">
        <form className="login-form" onSubmit={submit}>
          <span className="login-symbol">
            <Icon kind="lock" size={26} />
          </span>
          <h2>登录兰台</h2>
          <p>使用你的账户口令与验证器动态码。</p>
          {notice && (
            <div className="notice" role="status">
              {notice}
            </div>
          )}
          {error ? <ErrorBox error={error} /> : null}
          <label>
            账户名称
            <input
              autoComplete="username"
              required
              value={name}
              onChange={(e) => setName(e.target.value)}
              disabled={busy}
              autoCapitalize="none"
              spellCheck={false}
            />
          </label>
          <label>
            口令
            <input
              type="password"
              autoComplete="current-password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              disabled={busy}
            />
          </label>
          <label>
            验证器动态码
            <input
              inputMode="numeric"
              autoComplete="one-time-code"
              pattern="[0-9]{6}"
              maxLength={6}
              required
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
              disabled={busy}
              placeholder="6 位动态码"
            />
          </label>
          <button
            className="primary login-submit"
            type="submit"
            disabled={busy}
          >
            {busy ? "正在验证…" : "登录并浏览"}
            <Icon kind="arrow" size={17} />
          </button>
          <p className="security-note">
            <Icon kind="lock" size={15} />
            口令与动态码仅用于本次验证。浏览会话只申请读取权限。
          </p>
          <p className="setup-help">
            首次安装？<a href="/setup/">打开受保护的设置页</a>
            <small>
              设置完成后入口永久关闭。成员设置与验证器恢复请使用现有受保护流程。
            </small>
          </p>
          {helpAvailable && (
            <a className="help-link" href="/help/">
              使用帮助
            </a>
          )}
        </form>
      </main>
    </div>
  );
}
function App() {
  const [who, setWho] = useState<Identity | null>(null);
  const [booting, setBooting] = useState(true);
  const [notice, setNotice] = useState("");
  const [bootError, setBootError] = useState<unknown>();
  const [attempt, setAttempt] = useState(0);
  const expire = useCallback<Expire>((error) => {
    clearCSRF();
    setWho(null);
    setNotice(error ? message(error) : "已安全退出当前会话。");
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    api<Identity>("/api/v1/whoami", { signal: controller.signal })
      .then((identity) => {
        if (!controller.signal.aborted) {
          restoreCSRF(identity.session.session_id);
          setWho(identity);
        }
      })
      .catch((error) => {
        if (controller.signal.aborted) return;
        if (isSessionError(error)) clearCSRF();
        else setBootError(error);
      })
      .finally(() => {
        if (!controller.signal.aborted) setBooting(false);
      });
    return () => controller.abort();
  }, [attempt]);
  // Recheck current authorization after returning from another app/tab. No
  // background login, session issuance, renewal, or durable response cache.
  useEffect(() => {
    if (!who) return;
    const controller = new AbortController();
    const verify = () => {
      if (document.visibilityState !== "visible") return;
      api<Identity>("/api/v1/whoami", { signal: controller.signal })
        .then((identity) => {
          if (!controller.signal.aborted) {
            restoreCSRF(identity.session.session_id);
            setWho(identity);
          }
        })
        .catch((error) => {
          if (!controller.signal.aborted && isSessionError(error))
            expire(error);
        });
    };
    addEventListener("focus", verify);
    const expires = Math.max(
      0,
      new Date(who.session.expires_at).getTime() - Date.now(),
    );
    const timer = window.setTimeout(
      () => expire(new APIError("TOKEN_EXPIRED", 401)),
      Math.min(expires, 2147483647),
    );
    return () => {
      controller.abort();
      removeEventListener("focus", verify);
      clearTimeout(timer);
    };
  }, [who, expire]);
  if (booting) return <Loading />;
  if (bootError)
    return (
      <main className="boot-error">
        <h1>暂时无法连接兰台</h1>
        <ErrorBox
          error={bootError}
          retry={() => {
            setBootError(undefined);
            setBooting(true);
            setAttempt((n) => n + 1);
          }}
        />
        <p className="setup-help">
          首次安装？<a href="/setup/">打开受保护的设置页</a>
        </p>
        {helpAvailable && (
          <a className="help-link" href="/help/">
            使用帮助
          </a>
        )}
      </main>
    );
  if (
    who?.session.channel === "browser" &&
    who.session.session_kind === "normal"
  )
    return (
      <Workspace
        key={who.session.session_id}
        who={who}
        expire={expire}
        reauthenticate={() => {
          setWho(null);
          setNotice("请重新验证，以获取此标签页的退出校验信息。");
        }}
      />
    );
  return (
    <Login
      notice={who ? "当前为受限会话，请完成账户设置后重新登录。" : notice}
      onLogin={setWho}
    />
  );
}

createRoot(document.getElementById("root")!).render(<App />);
