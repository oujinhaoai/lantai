import { api, APIError } from "./api";
import type { ManifestFile } from "./asset-tools";
import { identifierPattern, managedTransferURL } from "./managed-transfer";

export async function readAssetFile(options: {
  assetID: string;
  versionID: string;
  file: ManifestFile;
  purpose: "archive_review" | "raw_export";
  byteLimit: number;
  signal: AbortSignal;
}) {
  const { assetID, versionID, file, purpose, byteLimit, signal } = options;
  if (
    !Number.isSafeInteger(file.size) ||
    file.size < 0 ||
    file.size > byteLimit
  )
    throw new Error("文件超过此工具的读取预算，请使用受控本机工作集。");
  if (!identifierPattern.test(assetID) || !identifierPattern.test(versionID))
    throw new APIError("INVALID_RESPONSE", 0);
  const grant = await api<{
    grant_id: string;
    asset_id: string;
    version_id: string;
    purpose: string;
    method: string;
    path: string;
    size: number;
    sha256: string;
    url: string;
    expires_at: string;
  }>(
    `/api/v1/assets/${encodeURIComponent(assetID)}/versions/${encodeURIComponent(versionID)}/read-grants`,
    {
      method: "POST",
      body: JSON.stringify({ path: file.path, purpose }),
      signal,
    },
  );
  const expires = Date.parse(grant.expires_at);
  if (
    !identifierPattern.test(grant.grant_id) ||
    grant.asset_id !== assetID ||
    grant.version_id !== versionID ||
    grant.purpose !== purpose ||
    grant.method !== "GET" ||
    grant.path !== file.path ||
    grant.size !== file.size ||
    grant.sha256 !== file.sha256 ||
    !Number.isFinite(expires) ||
    expires <= Date.now()
  )
    throw new APIError("INVALID_RESPONSE", 0);
  let url: URL;
  try {
    url = new URL(managedTransferURL(grant.url, "read"));
  } catch {
    throw new APIError("INVALID_RESPONSE", 0);
  }
  if (url.pathname !== `/xfer/v1/reads/${grant.grant_id}`)
    throw new APIError("INVALID_RESPONSE", 0);
  const response = await fetch(url.href, {
    credentials: "same-origin",
    cache: "no-store",
    redirect: "error",
    signal,
  });
  if (!response.ok) {
    const error = await response.json().catch(() => ({}));
    throw new APIError(error.error?.code || "SERVER_ERROR", response.status);
  }
  const stream = response.body?.getReader();
  if (!stream) throw new APIError("INVALID_RESPONSE", 0);
  const chunks: Uint8Array[] = [];
  let count = 0;
  try {
    for (;;) {
      const { done, value } = await stream.read();
      if (done) break;
      count += value.length;
      if (count > file.size || count > byteLimit)
        throw new Error("服务返回的字节超过冻结清单，已停止读取。");
      chunks.push(value);
    }
  } finally {
    await stream.cancel().catch(() => {});
  }
  if (count !== file.size || signal.aborted || Date.now() >= expires)
    throw new APIError("INVALID_RESPONSE", 0);
  const bytes = new Uint8Array(count);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.length;
  }
  const digest = new Uint8Array(
    await crypto.subtle.digest("SHA-256", bytes.buffer),
  );
  const sha256 = Array.from(digest, (n) =>
    n.toString(16).padStart(2, "0"),
  ).join("");
  if (sha256 !== file.sha256)
    throw new Error("文件 SHA-256 与固定清单不一致，已停止处理。");
  if (signal.aborted || Date.now() >= expires)
    throw new APIError("INVALID_RESPONSE", 0);
  return { bytes, expires };
}
