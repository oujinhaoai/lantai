import { useEffect, useState, type FormEvent } from "react";
import {
  api,
  APIError,
  isSessionError,
  message,
  typeNames,
  type Asset,
  type ExactVersion,
  type Identity,
} from "./api";
import { FilePreview } from "./file-preview";
import { AssetAdministration } from "./asset-management";

type Expire = (error: unknown) => void;
type FullVersion = ExactVersion & {
  manifest: NonNullable<ExactVersion["manifest"]> & {
    instance_id: string;
    project_id: string;
    asset_id: string;
    version_id: string;
    version_number: number;
    manifest_digest: string;
    content: { asset_type: string };
  };
};
// Match the existing public ULID contract. Invalid or ambiguous explicit
// selections must fail closed, rather than silently displaying latest.
const identifier = /^[0-7][0-9A-HJKMNP-TV-Z]{25}$/;

export function assetVersionPath(
  assetID: string,
  versionID?: string,
  from?: string,
) {
  const query = new URLSearchParams();
  if (versionID !== undefined) query.set("version", versionID);
  if (from) query.set("from", from);
  return `/assets/${encodeURIComponent(assetID)}${query.size ? `?${query}` : ""}`;
}

function selection(route: string) {
  const query = new URLSearchParams(route.split("?")[1]);
  const versions = query.getAll("version");
  const from = query.get("from") || "/assets";
  return {
    versionID: versions[0],
    valid:
      versions.length === 0 ||
      (versions.length === 1 && identifier.test(versions[0])),
    from: /^\/(?:assets(?:\?|$)|projects(?:\/|$)|$)/.test(from)
      ? from
      : "/assets",
  };
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

function Failure({ error, retry }: { error: unknown; retry?: () => void }) {
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

function VersionSelection({
  assetID,
  versionID,
  from,
}: {
  assetID: string;
  versionID: string;
  from: string;
}) {
  const [value, setValue] = useState(versionID);
  const [invalid, setInvalid] = useState(false);
  function submit(event: FormEvent) {
    event.preventDefault();
    if (!identifier.test(value)) {
      setInvalid(true);
      return;
    }
    setInvalid(false);
    location.hash = assetVersionPath(assetID, value, from);
  }
  return (
    <details className="file-manifest">
      <summary>按版本标识打开历史版本</summary>
      <form onSubmit={submit}>
        <label>
          版本标识
          <input
            value={value}
            onChange={(event) => setValue(event.target.value)}
            autoComplete="off"
            spellCheck={false}
            aria-invalid={invalid}
            aria-describedby={invalid ? "version-selection-error" : undefined}
          />
        </label>
        <button className="secondary" type="submit">
          打开固定版本
        </button>
        {invalid && (
          <p id="version-selection-error" className="error" role="alert">
            请输入完整的 26 位大写版本标识。
          </p>
        )}
      </form>
    </details>
  );
}

function VersionContent({
  asset: initialAsset,
  who,
  exact,
  metadataAvailable,
  selected,
  from,
  expire,
}: {
  asset: Asset;
  who?: Identity;
  exact: FullVersion;
  metadataAvailable: boolean;
  selected: boolean;
  from: string;
  expire: Expire;
}) {
  const [asset, setAsset] = useState(initialAsset);
  const version = exact.version;
  const files = exact.manifest?.content.files || [];
  return (
    <>
      <header className="page-heading">
        <p className="eyebrow">
          资源 / {typeNames[asset.asset_type] || asset.asset_type}
        </p>
        <h1 tabIndex={-1}>
          {asset.description.title || asset.description.slug}
        </h1>
        {asset.description.summary && <p>{asset.description.summary}</p>}
      </header>
      <section aria-label="固定版本内容">
        <div className="section-head">
          <h2>
            当前固定版本{" "}
            <span className="badge">v{version.version_number}</span>
          </h2>
          <span>{date(version.committed_at)}</span>
        </div>
        <p className="subtle">
          {selected
            ? "此链接指定固定版本；新版本入藏后仍打开这一版。"
            : "此入口读取最新版本。保存固定版本链接后，可再次打开这一版。"}
          著录显示当前修订，与不可变的版本内容分别记录。
        </p>
        {!metadataAvailable && (
          <p className="notice">
            当前著录不可读取，只显示你有权读取的固定版本清单。
          </p>
        )}
        <div className="preview-actions">
          <a
            className="secondary"
            href={`#${assetVersionPath(asset.asset_id, version.version_id, from)}`}
          >
            固定此版本链接
          </a>
          {selected && (
            <a
              className="text-button"
              href={`#${assetVersionPath(asset.asset_id, undefined, from)}`}
            >
              查看最新版
            </a>
          )}
        </div>
        {exact.manifest?.content.version_note && (
          <p>{exact.manifest.content.version_note}</p>
        )}
        {!!files.length && (
          <FilePreview
            key={`${asset.asset_id}/${version.version_id}`}
            files={files}
            assetType={asset.asset_type}
            assetID={asset.asset_id}
            versionID={version.version_id}
            expire={expire}
          />
        )}
        {files.length ? (
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
                  {files.map((file) => (
                    <tr key={file.path}>
                      <td data-label="文件路径">{file.path}</td>
                      <td data-label="用途">
                        {file.role === "primary" ? "主文件" : file.role}
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
          <div className="empty">
            <h3>没有可显示的文件清单</h3>
            <p>此版本未返回文件清单。</p>
          </div>
        )}
        <VersionSelection
          assetID={asset.asset_id}
          versionID={version.version_id}
          from={from}
        />
      </section>
      <AssetAdministration
        asset={asset}
        exact={exact}
        from={from}
        who={who}
        expire={expire}
        metadataAvailable={metadataAvailable}
        updated={(description) =>
          setAsset((current) => ({ ...current, description }))
        }
      />
      <details className="detail-metadata">
        <summary>著录与技术详情</summary>
        <div className="detail-grid">
          {metadataAvailable && (
            <section className="panel">
              <h2>当前著录信息</h2>
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
                <dd>{asset.description.subjects?.join("、") || "暂无主题"}</dd>
                <dt>资源标识</dt>
                <dd>
                  <code>{asset.asset_id}</code>
                </dd>
              </dl>
            </section>
          )}
          <section className="panel">
            <h2>
              固定版本详情{" "}
              <span className="badge">v{version.version_number}</span>
            </h2>
            <dl>
              <dt>提交时间</dt>
              <dd>{date(version.committed_at)}</dd>
              <dt>版本标识</dt>
              <dd>
                <code>{version.version_id}</code>
              </dd>
              <dt>永久引用</dt>
              <dd>
                <code>{version.uri}</code>
              </dd>
              <dt>清单摘要</dt>
              <dd>
                <code>{version.manifest_digest}</code>
              </dd>
            </dl>
            <p className="subtle">预览和下载均向此固定版本申请当前读取权限。</p>
          </section>
        </div>
      </details>
    </>
  );
}

function VersionRequest({
  assetID,
  who,
  versionID,
  from,
  expire,
  retry,
}: {
  assetID: string;
  who?: Identity;
  versionID?: string;
  from: string;
  expire: Expire;
  retry: () => void;
}) {
  const [state, setState] = useState<{
    asset?: Asset;
    exact?: FullVersion;
    error?: unknown;
  }>({});
  useEffect(() => {
    const controller = new AbortController();
    async function read() {
      const assetPath = `/api/v1/assets/${encodeURIComponent(assetID)}?view=full`;
      // An old readable version must not depend on latest being readable.
      // GetAsset currently checks latest rights; explicit selection starts at
      // GetVersion and may omit unavailable current metadata, without fallback.
      let asset =
        versionID === undefined
          ? await api<Asset>(assetPath, { signal: controller.signal })
          : undefined;
      const chosenID = versionID ?? asset!.latest.version_id;
      if ((asset && asset.asset_id !== assetID) || !identifier.test(chosenID))
        throw new APIError("INVALID_RESPONSE", 0);
      if (controller.signal.aborted) return;
      const exact = await api<FullVersion>(
        `/api/v1/assets/${encodeURIComponent(assetID)}/versions/${encodeURIComponent(chosenID)}?view=full`,
        { signal: controller.signal },
      );
      // Never accept a different asset/version as a fallback response. Current
      // metadata is mutable; the frozen manifest and all reads use exact.version.
      if (
        exact.version.asset_id !== assetID ||
        exact.version.version_id !== chosenID ||
        (asset && exact.version.project_id !== asset.project_id) ||
        !exact.manifest ||
        exact.manifest.asset_id !== assetID ||
        exact.manifest.version_id !== chosenID ||
        exact.manifest.project_id !== exact.version.project_id ||
        exact.manifest.version_number !== exact.version.version_number ||
        exact.manifest.manifest_digest !== exact.version.manifest_digest ||
        !identifier.test(exact.manifest.instance_id) ||
        exact.version.uri !==
          `lantai://${exact.manifest.instance_id}/assets/${assetID}/versions/${chosenID}` ||
        !Number.isFinite(Date.parse(exact.version.committed_at))
      )
        throw new APIError("INVALID_RESPONSE", 0);
      if (versionID !== undefined && !controller.signal.aborted) {
        try {
          asset = await api<Asset>(assetPath, { signal: controller.signal });
        } catch (error) {
          if (!(error instanceof APIError) || error.code !== "NOT_FOUND")
            throw error;
        }
      }
      if (
        asset &&
        (asset.asset_id !== assetID ||
          asset.project_id !== exact.version.project_id ||
          asset.asset_type !== exact.manifest.content.asset_type)
      )
        throw new APIError("INVALID_RESPONSE", 0);
      if (!controller.signal.aborted) setState({ asset, exact });
    }
    void read().catch((error: unknown) => {
      if (controller.signal.aborted) return;
      if (isSessionError(error)) expire(error);
      else setState({ error });
    });
    return () => controller.abort();
  }, [assetID, versionID, expire]);
  if (state.error) return <Failure error={state.error} retry={retry} />;
  if (!state.exact)
    return (
      <div className="loading" role="status">
        <span className="spinner" />
        正在读取…
      </div>
    );
  return (
    <VersionContent
      asset={
        state.asset || {
          asset_id: assetID,
          project_id: state.exact.version.project_id,
          asset_type: state.exact.manifest.content.asset_type,
          latest: state.exact.version,
          description: {
            title: "固定版本",
            slug: "",
            tags: [],
            subjects: [],
            revision: 0,
          },
        }
      }
      exact={state.exact}
      who={who}
      metadataAvailable={!!state.asset}
      selected={versionID !== undefined}
      from={from}
      expire={expire}
    />
  );
}

// Independent entry point: the app shell owns authentication and hash routing.
// The request subtree is keyed even if its host does not unmount on navigation,
// so old manifests, selection state, short-lived URLs and pending reads vanish.
export function AssetVersionView({
  assetID,
  who,
  route,
  expire,
}: {
  assetID: string;
  who?: Identity;
  route: string;
  expire: Expire;
}) {
  const { versionID, valid, from } = selection(route);
  const [attempt, setAttempt] = useState(0);
  return (
    <div className="asset-detail">
      <a className="back" href={`#${from}`}>
        ← 返回列表
      </a>
      {!valid || !identifier.test(assetID) ? (
        <Failure error={new APIError("SCHEMA_INVALID", 0)} />
      ) : (
        <VersionRequest
          key={`${assetID}/${versionID ?? "latest"}/${attempt}`}
          assetID={assetID}
          who={who}
          versionID={versionID}
          from={from}
          expire={expire}
          retry={() => setAttempt((value) => value + 1)}
        />
      )}
    </div>
  );
}
