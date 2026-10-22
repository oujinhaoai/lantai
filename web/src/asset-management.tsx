import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  api,
  APIError,
  hasCSRF,
  isSessionError,
  message,
  typeNames,
  type Asset,
  type ExactVersion,
  type Identity,
  type Item,
  type Page,
  type Project,
  type Version,
} from "./api";
import {
  assetIdentifier,
  assetSearchQuery,
  compareFiles,
  frozenValueText,
  metadataPatch,
  metadataDraftLimit,
  type MetadataDraft,
  restoreMetadataDraft,
  type FileDifference,
  type ManifestFile,
} from "./asset-tools";
import { parseManagedJSON } from "./managed-transfer";
import { emptyProfile, profileDraft } from "./asset-profile";
import { ProfileFields } from "./asset-profile-view";
import { AssetRights } from "./asset-rights-view";
import "./asset-management.css";

type Props = { who: Identity; expire: (error: unknown) => void; route: string };
type Expire = Props["expire"];
function failure(error: unknown) {
  return error instanceof APIError
    ? message(error)
    : error instanceof Error
      ? error.message
      : message(error);
}

function AssetResults({
  query,
  from,
  expire,
}: {
  query: string;
  from: string;
  expire: Expire;
}) {
  const [items, setItems] = useState<Item[]>([]);
  const [cursor, setCursor] = useState<string>();
  const [busy, setBusy] = useState(true);
  const [error, setError] = useState<unknown>();
  const controller = useRef<AbortController | null>(null);
  const lock = useRef(false);
  const read = async (next?: string) => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    const request = new AbortController();
    controller.current = request;
    try {
      const params = new URLSearchParams(query);
      if (next) params.set("cursor", next);
      const page = await api<Page<Item>>(`/api/v1/assets?${params}`, {
        signal: request.signal,
      });
      if (page.items === null) page.items = [];
      if (
        !Array.isArray(page.items) ||
        page.items.length > 100 ||
        page.items.some(
          (item) =>
            !assetIdentifier.test(item.asset_id) ||
            !assetIdentifier.test(item.version_id) ||
            !assetIdentifier.test(item.project_id),
        ) ||
        (page.next_cursor &&
          (typeof page.next_cursor !== "string" ||
            page.next_cursor === next ||
            page.next_cursor.length > 4096))
      )
        throw new APIError("INVALID_RESPONSE", 0);
      if (request.signal.aborted) return;
      setItems((old) =>
        next
          ? [
              ...old,
              ...page.items!.filter(
                (item) => !old.some((v) => v.asset_id === item.asset_id),
              ),
            ]
          : page.items!,
      );
      setCursor(page.next_cursor);
    } catch (value) {
      if (!request.signal.aborted) {
        setItems([]);
        setCursor(undefined);
        if (isSessionError(value)) expire(value);
        else setError(value);
      }
    } finally {
      lock.current = false;
      if (!request.signal.aborted) setBusy(false);
    }
  };
  useEffect(() => {
    const request = new AbortController();
    async function first() {
      try {
        const page = await api<Page<Item>>(`/api/v1/assets?${query}`, {
          signal: request.signal,
        });
        if (page.items === null) page.items = [];
        if (
          !Array.isArray(page.items) ||
          page.items.length > 100 ||
          page.items.some(
            (item) =>
              !assetIdentifier.test(item.asset_id) ||
              !assetIdentifier.test(item.version_id),
          )
        )
          throw new APIError("INVALID_RESPONSE", 0);
        if (!request.signal.aborted) {
          setItems(page.items);
          setCursor(page.next_cursor);
        }
      } catch (value) {
        if (!request.signal.aborted) {
          if (isSessionError(value)) expire(value);
          else setError(value);
        }
      } finally {
        if (!request.signal.aborted) setBusy(false);
      }
    }
    void first();
    return () => {
      request.abort();
      controller.current?.abort();
    };
  }, [query, expire]);
  return (
    <section aria-label="素材检索结果">
      {busy && <p role="status">正在读取当前权限内的素材…</p>}
      {!!error && (
        <p className="error" role="alert">
          {failure(error)}{" "}
          <button className="text-button" onClick={() => void read()}>
            从第一页重试
          </button>
        </p>
      )}
      {!busy && !error && !items.length && (
        <p className="notice">
          当前页没有可见的匹配素材。
          {cursor
            ? "仍有后续授权分页，可以继续加载。"
            : "可调整筛选或确认项目权限；不显示隐藏素材数量。"}
        </p>
      )}
      {!!items.length && (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">素材与固定版本</th>
                <th scope="col">类型</th>
                <th scope="col">标签</th>
                <th scope="col">摘要</th>
              </tr>
            </thead>
            <tbody>
              {items.map((item) => (
                <tr key={item.asset_id}>
                  <td>
                    <a
                      href={`#/assets/${encodeURIComponent(item.asset_id)}?version=${encodeURIComponent(item.version_id)}&from=${encodeURIComponent(from)}`}
                    >
                      {item.title || item.slug}
                    </a>
                    <div>
                      v{item.version_number} · <code>{item.asset_id}</code>
                    </div>
                  </td>
                  <td>{typeNames[item.asset_type] || item.asset_type}</td>
                  <td>{item.tags?.join("、") || "—"}</td>
                  <td>{item.summary || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {cursor && (
        <button
          className="secondary"
          disabled={busy}
          onClick={() => void read(cursor)}
        >
          加载下一页
        </button>
      )}
      <p className="subtle">
        精确标签由服务端在授权分页前组合筛选，区分大小写，不只过滤当前 24
        条。检索投影可能滞后；入藏核验请使用固定版本回执。
      </p>
    </section>
  );
}

function ProjectChoices({ expire }: { expire: Expire }) {
  const [items, setItems] = useState<Project[]>([]);
  const [cursor, setCursor] = useState<string>();
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  const locked = useRef(false);
  useEffect(() => () => request.current?.abort(), []);
  const read = async () => {
    if (locked.current) return;
    locked.current = true;
    setBusy(true);
    setError(undefined);
    const controller = new AbortController();
    request.current = controller;
    try {
      const params = new URLSearchParams({ limit: "100" });
      if (cursor) params.set("cursor", cursor);
      const value = await api<Page<Project>>(`/api/v1/projects?${params}`, {
        signal: controller.signal,
      });
      if (value.items === null) value.items = [];
      if (
        !Array.isArray(value.items) ||
        value.items.length > 100 ||
        value.items.some((p) => !assetIdentifier.test(p.project_id)) ||
        (value.next_cursor && value.next_cursor === cursor)
      )
        throw new APIError("INVALID_RESPONSE", 0);
      if (!controller.signal.aborted) {
        setItems((old) => [
          ...old,
          ...value.items!.filter(
            (p) => !old.some((x) => x.project_id === p.project_id),
          ),
        ]);
        setCursor(value.next_cursor);
      }
    } catch (value) {
      if (!controller.signal.aborted) {
        setItems([]);
        if (isSessionError(value)) expire(value);
        else setError(value);
      }
    } finally {
      locked.current = false;
      if (!controller.signal.aborted) setBusy(false);
    }
  };
  return (
    <div>
      <datalist id="asset-visible-projects">
        {items.map((p) => (
          <option key={p.project_id} value={p.project_id}>
            {p.description.name} · {p.key}
          </option>
        ))}
      </datalist>
      <button
        type="button"
        className="text-button"
        disabled={busy || (items.length > 0 && !cursor)}
        onClick={() => void read()}
      >
        {busy
          ? "正在读取项目…"
          : cursor
            ? "继续读取项目选项"
            : "读取可见项目选项"}
      </button>
      {!!error && <span role="alert">{failure(error)}</span>}
    </div>
  );
}

export function AssetLibrary({ who, expire, route }: Props) {
  const params = new URLSearchParams(route.split("?")[1]);
  const [draft, setDraft] = useState(() =>
    Object.fromEntries(
      [
        "q",
        "type",
        "project",
        "character",
        "source",
        "state",
        "legacy",
        "tags",
      ].map((key) => [key, params.get(key) || ""]),
    ),
  );
  const [inputError, setInputError] = useState<unknown>();
  let query = "",
    routeError: unknown;
  try {
    query = assetSearchQuery(route).toString();
  } catch (error) {
    routeError = error;
  }
  const change = (key: string, value: string) =>
    setDraft((old) => ({ ...old, [key]: value }));
  const submit = (event: FormEvent) => {
    event.preventDefault();
    const next = new URLSearchParams();
    for (const [key, value] of Object.entries(draft))
      if (value.trim()) next.set(key, value.trim());
    const target = `/assets${next.size ? `?${next}` : ""}`;
    try {
      assetSearchQuery(target);
      setInputError(undefined);
      location.hash = target;
    } catch (error) {
      setInputError(error);
    }
  };
  return (
    <div className="asset-library">
      <header className="page-heading">
        <p className="eyebrow">素材库 / ASSETS</p>
        <h1 tabIndex={-1}>素材库</h1>
        <p>查看当前有权读取的资料、固定版本、完整文件与当前著录。</p>
      </header>
      <form className="asset-library-filters" role="search" onSubmit={submit}>
        <div className="asset-editor-grid">
          <label>
            著录文字
            <input
              value={draft.q}
              maxLength={512}
              onChange={(event) => change("q", event.target.value)}
              placeholder="标题、摘要、标签的文字检索"
            />
          </label>
          <label>
            资源类型
            <select
              value={draft.type}
              onChange={(event) => change("type", event.target.value)}
            >
              <option value="">全部类型</option>
              {Object.entries(typeNames).map(([key, label]) => (
                <option key={key} value={key}>
                  {label}
                </option>
              ))}
            </select>
          </label>
          <label>
            项目
            <input
              value={draft.project}
              list="asset-visible-projects"
              onChange={(event) => change("project", event.target.value)}
              placeholder="全部可见项目，或选择项目标识"
            />
            <ProjectChoices expire={expire} />
          </label>
        </div>
        <details>
          <summary>精确组合筛选（全部条件 AND）</summary>
          <div className="asset-editor-grid">
            <label>
              角色／人物标识
              <input
                value={draft.character}
                onChange={(event) => change("character", event.target.value)}
                placeholder="hero-a, hero-b"
              />
            </label>
            <label>
              来源标识
              <input
                value={draft.source}
                onChange={(event) => change("source", event.target.value)}
                placeholder="来源描述，不授予使用许可"
              />
            </label>
            <label>
              业务状态
              <select
                value={draft.state}
                onChange={(event) => change("state", event.target.value)}
              >
                <option value="">不限</option>
                {[
                  "active",
                  "reference",
                  "rejected",
                  "superseded",
                  "archived",
                  "pending_review",
                  "unknown",
                ].map((v) => (
                  <option key={v}>{v}</option>
                ))}
              </select>
            </label>
            <label>
              原历史版本标签
              <input
                value={draft.legacy}
                onChange={(event) => change("legacy", event.target.value)}
                placeholder="v001，不推断资产身份"
              />
            </label>
            <label>
              其他精确标签
              <input
                value={draft.tags}
                onChange={(event) => change("tags", event.target.value)}
                placeholder="多个标签用逗号分隔"
              />
            </label>
          </div>
          <p className="subtle">
            这些是普通描述标签；active、pending_review
            或旧版状态都不等于核心审定、发布或生产使用许可。
          </p>
        </details>
        <div className="preview-actions">
          <button className="primary" type="submit">
            应用筛选
          </button>
          <a className="secondary" href="#/assets">
            清除筛选
          </a>
          <a className="text-button" href="#/migrations">
            打开迁移中心
          </a>
        </div>
      </form>
      {!who.session.scopes.includes("organize") && (
        <p className="subtle">
          当前为浏览范围；素材信息编辑需要明确申请整理范围并由项目角色授权。
        </p>
      )}
      {!!(inputError || routeError) && (
        <p className="error" role="alert">
          {failure(inputError || routeError)}
        </p>
      )}
      {!routeError && (
        <AssetResults key={query} query={query} from={route} expire={expire} />
      )}
    </div>
  );
}

type Frozen = ExactVersion & {
  manifest?: NonNullable<ExactVersion["manifest"]> & {
    instance_id?: string;
    manifest_digest?: string;
    version_number?: number;
    asset_id?: string;
    version_id?: string;
    project_id?: string;
    content: {
      asset_type?: string;
      version_note?: string;
      rights?: unknown;
      metadata?: unknown;
      files: ManifestFile[];
    };
  };
};
type Draft = MetadataDraft;
function descriptionDraft(description: Asset["description"]): Draft {
  const base: Draft = {
    title: description.title,
    summary: description.summary || "",
    tags: description.tags.join("\n"),
    subjects: description.subjects.join("\n"),
  };
  if (description.extra?.asset_profile !== undefined) {
    try {
      base.profile = profileDraft(description.extra.asset_profile);
    } catch {
      /* Ordinary PATCH omits extra and preserves unsupported data. */
    }
  }
  return base;
}
type Attempt = {
  key: string;
  revision: number;
  body: ReturnType<typeof metadataPatch>;
};

function MetadataEditor({
  asset,
  who,
  expire,
  updated,
}: {
  asset: Asset;
  who?: Identity;
  expire: Expire;
  updated: (description: Asset["description"]) => void;
}) {
  const [draft, setDraft] = useState<Draft>(() =>
    descriptionDraft(asset.description),
  );
  const [attempt, setAttempt] = useState<Attempt>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [notice, setNotice] = useState("");
  const [remote, setRemote] = useState<Asset>();
  const [uncertain, setUncertain] = useState(false);
  const request = useRef<AbortController | null>(null);
  const lock = useRef(false);
  useEffect(() => () => request.current?.abort(), []);
  const editable = !!who?.session.scopes.includes("organize") && hasCSRF();
  const change = (key: keyof Draft, value: string) => {
    setDraft((d) => ({ ...d, [key]: value }));
    setAttempt(undefined);
    setError(undefined);
    setNotice("");
  };
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (lock.current || !editable) return;
    let frozen: Attempt;
    try {
      frozen = attempt || {
        key: crypto.randomUUID(),
        revision: asset.description.revision,
        body: metadataPatch(draft),
      };
    } catch (value) {
      setError(value);
      return;
    }
    lock.current = true;
    setAttempt(frozen);
    setBusy(true);
    setError(undefined);
    setNotice("");
    const controller = new AbortController();
    request.current = controller;
    try {
      const value = await api<Asset["description"]>(
        `/api/v1/assets/${encodeURIComponent(asset.asset_id)}/metadata`,
        {
          method: "PATCH",
          headers: {
            "If-Match": `"${frozen.revision}"`,
            "Idempotency-Key": frozen.key,
          },
          body: JSON.stringify(frozen.body),
          signal: controller.signal,
        },
      );
      if (
        !Number.isSafeInteger(value.revision) ||
        value.revision < frozen.revision ||
        typeof value.title !== "string" ||
        !Array.isArray(value.tags) ||
        !Array.isArray(value.subjects)
      )
        throw new APIError("INVALID_RESPONSE", 0);
      if (!controller.signal.aborted) {
        updated(value);
        setAttempt(undefined);
        setUncertain(false);
        setNotice(`当前著录已保存为 r${value.revision}；固定版本字节未改变。`);
      }
    } catch (value) {
      if (!controller.signal.aborted) {
        if (isSessionError(value)) expire(value);
        setError(value);
        setUncertain(
          !(value instanceof APIError) ||
            value.status === 0 ||
            value.status >= 500,
        );
      }
    } finally {
      lock.current = false;
      if (!controller.signal.aborted) setBusy(false);
    }
  };
  const refresh = async () => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    const controller = new AbortController();
    request.current = controller;
    try {
      const value = await api<Asset>(
        `/api/v1/assets/${encodeURIComponent(asset.asset_id)}?view=full`,
        { signal: controller.signal },
      );
      if (!controller.signal.aborted) {
        setRemote(value);
        setNotice(
          "最新著录已读取；草稿和原修订保持不变，请对照后明确采用。结果不明的提交须先按原键重试。",
        );
      }
    } catch (value) {
      if (!controller.signal.aborted) {
        if (isSessionError(value)) expire(value);
        setError(value);
      }
    } finally {
      lock.current = false;
      if (!controller.signal.aborted) setBusy(false);
    }
  };
  const importDraft = async (file?: File) => {
    if (!file || lock.current || uncertain) return;
    if (file.size > metadataDraftLimit) {
      setError(new Error("著录恢复草稿最多 8 MiB。"));
      return;
    }
    try {
      const value = restoreMetadataDraft(
        parseManagedJSON(await file.text()),
        location.origin,
        asset.asset_id,
      );
      setDraft(value.draft);
      setAttempt(value.attempt);
      setUncertain(value.uncertain);
      setError(undefined);
      setNotice(
        "恢复草稿已载入；原提交修订和键保持不变，不根据当前著录替换原操作。",
      );
    } catch (error) {
      setError(error);
    }
  };
  const exportDraft = () => {
    const value = {
      schema: "lantai.web-metadata-draft/v1",
      origin: location.origin,
      asset_id: asset.asset_id,
      draft,
      attempt,
      uncertain,
    };
    const blob = new Blob([JSON.stringify(value, null, 2)], {
      type: "application/json",
    });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `metadata-${asset.asset_id}.json`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  return (
    <details className="asset-admin-section">
      <summary>编辑当前素材信息</summary>
      {asset.description.revision === 0 && (
        <p className="notice">
          文件版本已提交，当前尚无生效著录。完善信息只写著录，不重传原件或创建新版本。
        </p>
      )}
      <p className="subtle">
        只修改当前著录。历史版本快照、rights、审定和发布不由此表单改变；未知
        extra 保持原样；结构化画像可在下方显式编辑。当前修订 r
        {asset.description.revision}。
      </p>
      {!editable && (
        <p className="notice">
          此操作需要当前标签页校验信息、明确的 organize
          范围和项目角色。请使用主应用的新鲜登录申请素材写入与整理范围；不会自动扩权。
        </p>
      )}
      <form className="asset-editor" onSubmit={(event) => void submit(event)}>
        <div className="asset-editor-grid">
          <label>
            标题
            <input
              required
              maxLength={200}
              value={draft.title}
              disabled={busy || uncertain || !editable}
              onChange={(event) => change("title", event.target.value)}
            />
          </label>
          <label>
            摘要
            <textarea
              maxLength={4000}
              value={draft.summary}
              disabled={busy || uncertain || !editable}
              onChange={(event) => change("summary", event.target.value)}
            />
          </label>
          <label>
            标签（每行或逗号分隔）
            <textarea
              value={draft.tags}
              disabled={busy || uncertain || !editable}
              onChange={(event) => change("tags", event.target.value)}
            />
          </label>
          <label>
            主题（每行或逗号分隔）
            <textarea
              value={draft.subjects}
              disabled={busy || uncertain || !editable}
              onChange={(event) => change("subjects", event.target.value)}
            />
          </label>
        </div>
        <label>
          <input
            type="checkbox"
            checked={!!draft.profile}
            disabled={
              busy ||
              uncertain ||
              !editable ||
              (asset.description.extra?.asset_profile !== undefined &&
                !descriptionDraft(asset.description).profile)
            }
            onChange={(event) => {
              setDraft((old) => ({
                ...old,
                profile: event.target.checked
                  ? descriptionDraft(asset.description).profile ||
                    emptyProfile()
                  : undefined,
              }));
              setAttempt(undefined);
              setError(undefined);
            }}
          />
          编辑结构化画像（保留未知 extra）
        </label>
        {asset.description.extra?.asset_profile !== undefined &&
          !descriptionDraft(asset.description).profile && (
            <p className="notice">
              现有画像或 extra
              超出可精确保留的格式／预算，结构化编辑已停用；普通著录提交不会覆盖它。
            </p>
          )}
        {draft.profile && (
          <ProfileFields
            value={draft.profile}
            disabled={busy || uncertain || !editable}
            change={(key, value) => {
              setDraft((old) => ({
                ...old,
                profile: { ...(old.profile || emptyProfile()), [key]: value },
              }));
              setAttempt(undefined);
              setError(undefined);
            }}
          />
        )}
        {uncertain && (
          <p className="notice">
            提交结果不明，草稿和原幂等键已冻结。请重试同一提交，不要更换键；如离开页面，先导出恢复草稿。
          </p>
        )}
        <div className="preview-actions">
          <button
            className="primary"
            type="submit"
            disabled={busy || !editable}
          >
            {busy
              ? "正在提交…"
              : uncertain
                ? "按原键重试相同提交"
                : "保存当前著录"}
          </button>
          <button
            type="button"
            className="secondary"
            disabled={busy}
            onClick={() => void refresh()}
          >
            读取最新著录（保留草稿）
          </button>
          <button
            type="button"
            className="text-button"
            disabled={busy}
            onClick={exportDraft}
          >
            导出私有草稿
          </button>
        </div>
      </form>
      <label>
        恢复已导出的私有草稿
        <input
          type="file"
          accept="application/json,.json"
          disabled={busy || uncertain}
          onChange={(event) => {
            void importDraft(event.target.files?.[0]);
            event.target.value = "";
          }}
        />
      </label>
      {!!error && (
        <p className="error" role="alert">
          {failure(error)}{" "}
          {error instanceof APIError && error.status === 412
            ? "草稿仍绑定原修订，未覆盖其他人的修改。请读取最新著录对照。"
            : ""}
        </p>
      )}
      {notice && <p role="status">{notice}</p>}
      {remote && (
        <div className="asset-attempt-note">
          <p>
            服务器 r{remote.description.revision}：{remote.description.title}
          </p>
          <p>{remote.description.summary}</p>
          <p>标签：{remote.description.tags.join("、")}</p>
          <p>
            服务器画像及额外字段：
            <code>{frozenValueText(remote.description.extra).text}</code>
          </p>
          <button
            className="secondary"
            disabled={busy || uncertain}
            onClick={() => {
              updated(remote.description);
              setDraft(descriptionDraft(remote.description));
              setAttempt(undefined);
              setRemote(undefined);
              setError(undefined);
            }}
          >
            放弃本地草稿，采用服务器内容
          </button>
        </div>
      )}
    </details>
  );
}

export function AssetAdministration({
  asset,
  exact,
  from,
  who,
  expire,
  updated,
  metadataAvailable = true,
}: {
  asset: Asset;
  exact: Frozen;
  metadataAvailable?: boolean;
  from: string;
  who?: Identity;
  expire: Expire;
  updated: (description: Asset["description"]) => void;
}) {
  const [versions, setVersions] = useState<Version[]>([]);
  const [cursor, setCursor] = useState<string>();
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [compareID, setCompareID] = useState("");
  const [difference, setDifference] = useState<{
    before: Frozen;
    files: FileDifference[];
  }>();
  const request = useRef<AbortController | null>(null);
  const lock = useRef(false);
  useEffect(() => () => request.current?.abort(), []);
  const read = async (next?: string) => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    const controller = new AbortController();
    request.current = controller;
    try {
      const query = new URLSearchParams({ limit: "20" });
      if (next) query.set("cursor", next);
      const value = await api<Page<Version>>(
        `/api/v1/assets/${encodeURIComponent(asset.asset_id)}/versions?${query}`,
        { signal: controller.signal },
      );
      if (value.items === null) value.items = [];
      if (
        !Array.isArray(value.items) ||
        value.items.length > 100 ||
        value.items.some(
          (v) =>
            v.asset_id !== asset.asset_id ||
            v.project_id !== asset.project_id ||
            !assetIdentifier.test(v.version_id) ||
            !Number.isSafeInteger(v.version_number),
        ) ||
        (value.next_cursor && value.next_cursor === next)
      )
        throw new APIError("INVALID_RESPONSE", 0);
      if (!controller.signal.aborted) {
        setVersions((old) =>
          next
            ? [
                ...old,
                ...value.items!.filter(
                  (v) => !old.some((x) => x.version_id === v.version_id),
                ),
              ]
            : value.items!,
        );
        setCursor(value.next_cursor);
        setLoaded(true);
      }
    } catch (value) {
      if (!controller.signal.aborted) {
        setVersions([]);
        setDifference(undefined);
        if (isSessionError(value)) expire(value);
        else setError(value);
      }
    } finally {
      lock.current = false;
      if (!controller.signal.aborted) setBusy(false);
    }
  };
  const compare = async () => {
    if (lock.current) return;
    if (!assetIdentifier.test(compareID)) {
      setError(new Error("请选择或输入规范的固定版本标识。"));
      return;
    }
    lock.current = true;
    setBusy(true);
    setError(undefined);
    setDifference(undefined);
    const controller = new AbortController();
    request.current = controller;
    try {
      const before = await api<Frozen>(
        `/api/v1/assets/${encodeURIComponent(asset.asset_id)}/versions/${encodeURIComponent(compareID)}?view=full`,
        { signal: controller.signal },
      );
      if (
        before.version.asset_id !== asset.asset_id ||
        before.version.project_id !== asset.project_id ||
        before.version.version_id !== compareID ||
        before.manifest?.asset_id !== asset.asset_id ||
        before.manifest?.version_id !== compareID ||
        before.manifest?.project_id !== asset.project_id ||
        before.manifest?.version_number !== before.version.version_number ||
        before.manifest?.manifest_digest !== before.version.manifest_digest ||
        before.manifest?.instance_id !== exact.manifest?.instance_id ||
        before.version.uri !==
          `lantai://${before.manifest?.instance_id}/assets/${asset.asset_id}/versions/${compareID}` ||
        !exact.manifest
      )
        throw new APIError("INVALID_RESPONSE", 0);
      const files = compareFiles(
        before.manifest.content.files,
        exact.manifest.content.files,
      );
      if (!controller.signal.aborted) setDifference({ before, files });
    } catch (value) {
      if (!controller.signal.aborted) {
        if (isSessionError(value)) expire(value);
        else setError(value);
      }
    } finally {
      lock.current = false;
      if (!controller.signal.aborted) setBusy(false);
    }
  };
  return (
    <section className="asset-admin-section" aria-label="版本与素材管理">
      <h2>版本与素材管理</h2>
      <p className="subtle">
        历史列表只显示当前可读的版本。标签和当前著录不能代替历史审定、发布或许可事实。
      </p>
      <div className="preview-actions">
        <button
          className="secondary"
          disabled={busy}
          onClick={() => void read()}
        >
          {busy ? "正在读取…" : "读取可见历史版本"}
        </button>
        {cursor && (
          <button
            className="secondary"
            disabled={busy}
            onClick={() => void read(cursor)}
          >
            更多可见版本
          </button>
        )}
      </div>
      {loaded && !versions.length && (
        <p className="notice">
          当前页没有可见版本；有后续游标时仍可继续读取，不显示隐藏数量。
        </p>
      )}
      {!!versions.length && (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">版本</th>
                <th scope="col">固定标识</th>
                <th scope="col">提交时间</th>
                <th scope="col">操作</th>
              </tr>
            </thead>
            <tbody>
              {versions.map((v) => (
                <tr key={v.version_id}>
                  <td>v{v.version_number}</td>
                  <td>
                    <code>{v.version_id}</code>
                  </td>
                  <td>{v.committed_at}</td>
                  <td>
                    <a
                      href={`#/assets/${encodeURIComponent(asset.asset_id)}?version=${encodeURIComponent(v.version_id)}&from=${encodeURIComponent(from)}`}
                    >
                      打开固定版本
                    </a>{" "}
                    <button
                      className="text-button"
                      onClick={() => setCompareID(v.version_id)}
                    >
                      选为对比基线
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div className="asset-editor-grid">
        <label>
          与当前 v{exact.version.version_number} 比较的基线版本
          <input
            value={compareID}
            maxLength={26}
            onChange={(event) => setCompareID(event.target.value.trim())}
            placeholder="选择历史版本或输入版本标识"
          />
        </label>
        <div>
          <button
            className="secondary"
            disabled={busy || !compareID}
            onClick={() => void compare()}
          >
            比较冻结清单
          </button>
        </div>
      </div>
      {!!error && (
        <p className="error" role="alert">
          {failure(error)}
        </p>
      )}
      {difference && (
        <section aria-label="冻结版本差异">
          <p>
            基线 v{difference.before.version.version_number} → 当前固定 v
            {exact.version.version_number}
            。只比较清单中的文件及冻结技术信息，不把当前著录称作历史著录。
          </p>
          <div className="asset-version-stats">
            {[
              ["added", "新增"],
              ["removed", "移除"],
              ["changed", "变化"],
              ["unchanged", "相同"],
            ].map(([kind, label]) => (
              <span key={kind}>
                {label} {difference.files.filter((f) => f.kind === kind).length}
              </span>
            ))}
          </div>
          <dl>
            <dt>基线清单摘要</dt>
            <dd>
              <code>{difference.before.version.manifest_digest}</code>
            </dd>
            <dt>当前清单摘要</dt>
            <dd>
              <code>{exact.version.manifest_digest}</code>
            </dd>
            <dt>基线版本说明</dt>
            <dd>{difference.before.manifest?.content.version_note || "—"}</dd>
            <dt>当前版本说明</dt>
            <dd>{exact.manifest?.content.version_note || "—"}</dd>
          </dl>
          <details>
            <summary>冻结技术信息、rights 与 metadata 对比</summary>
            <p className="subtle">
              这里只展示各版冻结内容，不修改许可，也不代表当前用途授权。长字段显示前
              4,096 字符并明确截断。
            </p>
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>冻结字段</th>
                    <th>基线</th>
                    <th>当前固定版</th>
                  </tr>
                </thead>
                <tbody>
                  {(
                    [
                      "asset_type",
                      "version_note",
                      "rights",
                      "metadata",
                    ] as const
                  ).map((field) => {
                    const before = frozenValueText(
                      difference.before.manifest?.content[field],
                    );
                    const after = frozenValueText(
                      exact.manifest?.content[field],
                    );
                    return (
                      <tr key={field}>
                        <th>
                          {field}
                          {before.truncated || after.truncated
                            ? "（信息有截断）"
                            : before.text === after.text
                              ? "（相同）"
                              : "（变化）"}
                        </th>
                        <td>
                          <code>{before.text}</code>
                        </td>
                        <td>
                          <code>{after.text}</code>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </details>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th scope="col">路径</th>
                  <th scope="col">变化</th>
                  <th scope="col">基线角色／大小／SHA-256</th>
                  <th scope="col">当前角色／大小／SHA-256</th>
                </tr>
              </thead>
              <tbody>
                {difference.files
                  .filter((f) => f.kind !== "unchanged")
                  .map((f) => (
                    <tr key={f.path}>
                      <td>{f.path}</td>
                      <td>
                        {f.kind === "added"
                          ? "新增"
                          : f.kind === "removed"
                            ? "移除"
                            : "变化"}
                      </td>
                      <td>
                        {f.before ? (
                          <>
                            {f.before.role} · {f.before.size} B<br />
                            <code>{f.before.sha256}</code>
                          </>
                        ) : (
                          "—"
                        )}
                      </td>
                      <td>
                        {f.after ? (
                          <>
                            {f.after.role} · {f.after.size} B<br />
                            <code>{f.after.sha256}</code>
                          </>
                        ) : (
                          "—"
                        )}
                      </td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
      <AssetRights
        key={exact.version.version_id}
        version={exact.version}
        projectID={asset.project_id}
        who={who}
        expire={expire}
      />
      {metadataAvailable && (
        <MetadataEditor
          asset={asset}
          who={who}
          expire={expire}
          updated={updated}
        />
      )}
    </section>
  );
}
