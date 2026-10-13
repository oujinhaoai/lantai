import { lazy, Suspense, useEffect, useRef, useState } from "react";
import { api, APIError, hasCSRF, isSessionError, message } from "./api";

const TextPreview = lazy(() =>
  import("./text-preview").catch(() => ({
    default: () => (
      <p className="error" role="alert">
        文本阅读模块未能加载，请刷新页面后重试。
      </p>
    ),
  })),
);
export type PreviewFile = {
  path: string;
  size: number;
  role: string;
  sha256: string;
};
type Grant = {
  url: string;
  size: number;
  expires_at: string;
  path: string;
  sha256: string;
  range: boolean;
};
type Kind = "image" | "audio" | "video" | "markdown" | "text" | "unsupported";
const initialLimit = 64 * 1024;
const maxLimit = 256 * 1024;
const imageLimit = 16 * 1024 * 1024;
export function fileKind(path: string): Kind {
  const ext = path.split(".").pop()?.toLowerCase() || "";
  if (["png", "jpg", "jpeg", "gif", "webp", "avif"].includes(ext))
    return "image";
  if (["mp3", "wav", "ogg", "m4a", "aac", "flac"].includes(ext)) return "audio";
  if (["mp4", "webm", "mov", "m4v", "ogv"].includes(ext)) return "video";
  if (["md", "markdown"].includes(ext)) return "markdown";
  if (
    [
      "txt",
      "csv",
      "tsv",
      "log",
      "rst",
      "py",
      "go",
      "js",
      "jsx",
      "ts",
      "tsx",
      "java",
      "rs",
      "c",
      "h",
      "cpp",
      "css",
      "html",
      "htm",
      "svg",
      "xml",
      "sh",
      "bash",
      "sql",
      "json",
      "yaml",
      "yml",
      "toml",
      "ini",
      "conf",
      "env",
    ].includes(ext)
  )
    return "text";
  return "unsupported";
}
const kindNames: Record<Kind, string> = {
  image: "图片",
  audio: "音频",
  video: "视频",
  markdown: "Markdown",
  text: "文本 / 代码 / 配置",
  unsupported: "不支持预览",
};

// Use only the server-issued, same-origin transfer path. Never construct a
// content-store URL, persist a signed URL, or forward credentials elsewhere.
function transferURL(value: string) {
  const url = new URL(value, location.origin);
  if (
    url.origin !== location.origin ||
    !/^\/xfer\/v1\/reads\/[A-Z0-9]+$/.test(url.pathname) ||
    url.username ||
    url.password ||
    url.hash
  )
    throw new APIError("INVALID_RESPONSE", 0);
  return url.href;
}
async function transfer(
  url: string,
  signal: AbortSignal,
  headers?: HeadersInit,
  method = "GET",
) {
  let response: Response;
  try {
    response = await fetch(url, {
      credentials: "same-origin",
      cache: "no-store",
      redirect: "error",
      signal,
      headers,
      method,
    });
  } catch (e) {
    if (signal.aborted) throw e;
    throw new APIError("NETWORK_ERROR", 0);
  }
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new APIError(body.error?.code || "SERVER_ERROR", response.status);
  }
  return response;
}
async function readText(
  url: string,
  fileSize: number,
  limit: number,
  signal: AbortSignal,
) {
  const end = Math.min(fileSize, limit) - 1;
  if (end < 0) return "";
  const response = await transfer(url, signal, { Range: `bytes=0-${end}` });
  if (
    (fileSize > limit && response.status !== 206) ||
    (response.status === 206 &&
      response.headers.get("Content-Range") !== `bytes 0-${end}/${fileSize}`)
  ) {
    await response.body?.cancel();
    throw new APIError("PREVIEW_RANGE", 0);
  }
  const reader = response.body?.getReader();
  if (!reader) throw new APIError("INVALID_RESPONSE", 0);
  const parts: Uint8Array[] = [];
  let count = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      count += value.length;
      if (count > limit) throw new APIError("PREVIEW_RANGE", 0);
      parts.push(value);
    }
  } finally {
    await reader.cancel().catch(() => {});
  }
  if (count !== end + 1) throw new APIError("INVALID_RESPONSE", 0);
  const bytes = new Uint8Array(count);
  let offset = 0;
  for (const part of parts) {
    bytes.set(part, offset);
    offset += part.length;
  }
  const signatures = [
    [0x50, 0x4b],
    [0x1f, 0x8b],
    [0x52, 0x61, 0x72, 0x21],
    [0x37, 0x7a, 0xbc, 0xaf],
    [0xfd, 0x37, 0x7a, 0x58],
    [0x42, 0x5a, 0x68],
  ];
  if (
    signatures.some((signature) =>
      signature.every((value, index) => bytes[index] === value),
    ) ||
    bytes.some(
      (value) => (value < 32 && ![9, 10, 13].includes(value)) || value === 127,
    )
  )
    throw new APIError("PREVIEW_BINARY", 0);
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes, {
      stream: fileSize > limit,
    });
  } catch {
    throw new APIError("PREVIEW_ENCODING", 0);
  }
  if (text.includes("\u0000")) throw new APIError("PREVIEW_ENCODING", 0);
  return text;
}
function previewError(error: unknown) {
  if (error instanceof APIError && error.code === "PREVIEW_BINARY")
    return "检测到压缩包或二进制内容，不进行预览。可按权限下载原文件。";
  if (error instanceof APIError && error.code === "PREVIEW_ENCODING")
    return "无法按 UTF-8 安全读取，或内容不是文本。未替换乱码；可按权限下载原文件。";
  if (error instanceof APIError && error.code === "PREVIEW_RANGE")
    return "服务未按请求返回有限字节，已停止预览，避免读取整个大文件。";
  return message(error);
}

function Viewer({
  file,
  assetID,
  versionID,
  expire,
}: {
  file: PreviewFile;
  assetID: string;
  versionID: string;
  expire: (error: unknown) => void;
}) {
  const kind = fileKind(file.path);
  const [state, setState] = useState<{
    url?: string;
    text?: string;
    expires?: string;
    error?: unknown;
    busy?: boolean;
    failed?: boolean;
  }>({});
  const [limit, setLimit] = useState(initialLimit);
  const [download, setDownload] = useState<{
    url?: string;
    expires?: string;
    busy?: boolean;
    error?: unknown;
  }>({});
  const previewRequest = useRef<AbortController | null>(null);
  const downloadRequest = useRef<AbortController | null>(null);
  const locked = useRef(false);
  const downloading = useRef(false);
  useEffect(
    () => () => {
      previewRequest.current?.abort();
      downloadRequest.current?.abort();
    },
    [],
  );
  useEffect(() => {
    if (!state.expires) return;
    const timer = setTimeout(
      () => {
        previewRequest.current?.abort();
        setState({ error: new APIError("PREVIEW_EXPIRED", 0) });
      },
      Math.max(0, new Date(state.expires).getTime() - Date.now()),
    );
    return () => clearTimeout(timer);
  }, [state.expires]);
  useEffect(() => {
    if (!download.expires) return;
    const timer = setTimeout(
      () => setDownload({ error: new APIError("PREVIEW_EXPIRED", 0) }),
      Math.max(0, new Date(download.expires).getTime() - Date.now()),
    );
    return () => clearTimeout(timer);
  }, [download.expires]);
  const issue = async (purpose: string, signal: AbortSignal) => {
    const grant = await api<Grant>(
      `/api/v1/assets/${encodeURIComponent(assetID)}/versions/${encodeURIComponent(versionID)}/read-grants`,
      {
        method: "POST",
        body: JSON.stringify({ path: file.path, purpose }),
        signal,
      },
    );
    if (
      grant.path !== file.path ||
      grant.size !== file.size ||
      grant.sha256 !== file.sha256 ||
      !grant.range ||
      !Number.isFinite(Date.parse(grant.expires_at))
    )
      throw new APIError("INVALID_RESPONSE", 0);
    return { ...grant, url: transferURL(grant.url) };
  };
  const report = (error: unknown) => {
    if (isSessionError(error)) expire(error);
  };
  const load = async (nextLimit = initialLimit) => {
    if (locked.current) return;
    locked.current = true;
    previewRequest.current?.abort();
    const request = new AbortController();
    previewRequest.current = request;
    setState({ busy: true });
    setLimit(nextLimit);
    try {
      const grant = await issue("archive_review", request.signal);
      if (kind === "text" || kind === "markdown") {
        const text = await readText(
          grant.url,
          grant.size,
          nextLimit,
          request.signal,
        );
        if (!request.signal.aborted)
          setState({ text, expires: grant.expires_at });
      } else if (!request.signal.aborted)
        setState({ url: grant.url, expires: grant.expires_at });
    } catch (error) {
      if (!request.signal.aborted) {
        report(error);
        setState({ error });
      }
    } finally {
      locked.current = false;
    }
  };
  const prepareDownload = async () => {
    if (downloading.current) return;
    downloading.current = true;
    const request = new AbortController();
    downloadRequest.current = request;
    setDownload({ busy: true });
    try {
      const grant = await issue("raw_export", request.signal);
      if (!request.signal.aborted)
        setDownload({ url: grant.url, expires: grant.expires_at });
    } catch (error) {
      if (!request.signal.aborted) {
        report(error);
        setDownload({ error });
      }
    } finally {
      downloading.current = false;
    }
  };
  const failed = async () => {
    const request = previewRequest.current;
    if (!state.url || !request || request.signal.aborted) return;
    try {
      await transfer(state.url, request.signal, undefined, "HEAD");
      if (!request.signal.aborted) setState({ failed: true });
    } catch (error) {
      if (!request.signal.aborted) {
        report(error);
        setState({ error });
      }
    }
  };
  const unavailable =
    kind === "unsupported" || (kind === "image" && file.size > imageLimit);
  return (
    <div className="file-viewer">
      <div className="preview-meta">
        <strong>{file.path}</strong>
        <span>
          {kindNames[kind]} · 格式：
          {file.path.split(".").pop()?.toUpperCase() || "未知"} ·{" "}
          {file.size.toLocaleString("zh-CN")} 字节
        </span>
      </div>
      {kind === "unsupported" && (
        <p className="notice">
          此格式不支持预览。压缩包及其他二进制仅显示基础信息，不解压、不执行、不展示内容。
        </p>
      )}
      {kind === "image" && file.size > imageLimit && (
        <p className="notice">
          图片超过 16 MiB，未自动读取原图。可按权限下载后查看。
        </p>
      )}
      {!hasCSRF() && (
        <p className="notice">
          此标签页没有读取授权所需的登录校验信息。请返回原登录标签页，或使用页面右上角重新验证。
        </p>
      )}
      <div className="preview-actions">
        {!unavailable && (
          <button
            className="primary"
            disabled={!hasCSRF() || state.busy}
            onClick={() => void load()}
          >
            {state.busy
              ? "正在读取…"
              : state.url || state.text !== undefined
                ? "重新读取"
                : "预览文件"}
          </button>
        )}
        <button
          className="secondary"
          disabled={!hasCSRF() || download.busy}
          onClick={() => void prepareDownload()}
        >
          {download.busy ? "正在申请…" : "申请下载原文件"}
        </button>
        {download.url && (
          <a
            className="secondary"
            href={download.url}
            download={file.path.split("/").pop()}
            referrerPolicy="no-referrer"
          >
            下载原文件
          </a>
        )}
        {(state.url || state.text !== undefined) && (
          <button
            className="text-button"
            onClick={() => {
              previewRequest.current?.abort();
              setState({});
            }}
          >
            关闭预览
          </button>
        )}
      </div>
      {!!state.error && (
        <p className="error" role="alert">
          {state.error instanceof APIError &&
          state.error.code === "PREVIEW_EXPIRED"
            ? "读取授权已到期，请点击重新读取。"
            : previewError(state.error)}
        </p>
      )}
      {!!download.error && (
        <p className="error" role="alert">
          {download.error instanceof APIError &&
          download.error.code === "PREVIEW_EXPIRED"
            ? "下载链接已到期，请重新申请。"
            : previewError(download.error)}
        </p>
      )}
      {state.busy && <p role="status">正在按当前权限读取文件…</p>}
      {state.failed && (
        <p className="notice" role="status">
          浏览器不支持此文件的格式或编码，未继续加载。可按权限下载原文件。
        </p>
      )}
      {state.url && kind === "image" && (
        <img
          className="media-image"
          src={state.url}
          alt={`文件预览：${file.path}`}
          onError={() => void failed()}
        />
      )}
      {state.url && kind === "audio" && (
        <audio
          className="media-audio"
          controls
          preload="metadata"
          src={state.url}
          onError={() => void failed()}
        >
          浏览器不支持音频播放。
        </audio>
      )}
      {state.url && kind === "video" && (
        <video
          className="media-video"
          controls
          playsInline
          preload="metadata"
          src={state.url}
          onError={() => void failed()}
        >
          浏览器不支持视频播放。
        </video>
      )}
      {state.url && (kind === "video" || kind === "audio") && (
        <p className="subtle">
          不自动播放；浏览器可能为读取媒体信息加载较多数据，能否解码取决于浏览器。
          新请求继续校验权限，关闭预览可取消后续加载。
        </p>
      )}
      {state.text !== undefined && (
        <Suspense fallback={<p role="status">正在打开文本视图…</p>}>
          <TextPreview
            text={state.text}
            path={file.path}
            markdown={kind === "markdown"}
            truncated={file.size > limit}
          />
        </Suspense>
      )}
      {state.text !== undefined && file.size > limit && (
        <div className="preview-actions">
          <p className="notice">
            仅预览前 {limit / 1024} KiB；后续内容已截断。
          </p>
          {limit < maxLimit && (
            <button
              className="secondary"
              onClick={() =>
                void load(Math.min(maxLimit, limit + initialLimit))
              }
            >
              读取更多（最多 256 KiB）
            </button>
          )}
        </div>
      )}
    </div>
  );
}
export function FilePreview({
  files,
  assetID,
  versionID,
  expire,
}: {
  files: PreviewFile[];
  assetID: string;
  versionID: string;
  expire: (error: unknown) => void;
}) {
  const [selected, setSelected] = useState(files[0]?.path || "");
  const file = files.find((item) => item.path === selected);
  if (!file) return null;
  return (
    <section className="preview-panel" aria-label="文件预览">
      <h3>文件预览与下载</h3>
      <label className="file-picker">
        选择版本文件
        <select
          value={selected}
          onChange={(event) => setSelected(event.target.value)}
        >
          {files.map((item) => (
            <option value={item.path} key={item.path}>
              {item.path}
            </option>
          ))}
        </select>
        <span className="file-picker-arrow" aria-hidden="true">
          ⌄
        </span>
      </label>
      <Viewer
        key={file.path}
        file={file}
        assetID={assetID}
        versionID={versionID}
        expire={expire}
      />
    </section>
  );
}
