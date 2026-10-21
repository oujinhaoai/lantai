import { useEffect, useRef, useState } from "react";
import { APIError, hasCSRF, isSessionError, message } from "./api";
import { readAssetFile } from "./asset-read";
import {
  fitImage,
  imageByteLimit,
  imageSize,
  mediaToolFormat,
  objLimits,
  parseOBJ,
  type ManifestFile,
  type Wireframe,
} from "./asset-tools";
import "./asset-management.css";

function toolError(error: unknown) {
  return error instanceof Error && !(error instanceof APIError)
    ? error.message
    : message(error);
}

function WireframeCanvas({ model }: { model: Wireframe }) {
  const canvas = useRef<HTMLCanvasElement>(null);
  const [yaw, setYaw] = useState(30);
  const [pitch, setPitch] = useState(20);
  useEffect(() => {
    const ctx = canvas.current?.getContext("2d");
    if (!ctx) return;
    const a = (yaw * Math.PI) / 180,
      b = (pitch * Math.PI) / 180;
    const min = [Infinity, Infinity, Infinity],
      max = [-Infinity, -Infinity, -Infinity];
    for (const vertex of model.vertices)
      for (let i = 0; i < 3; i++) {
        min[i] = Math.min(min[i], vertex[i]);
        max[i] = Math.max(max[i], vertex[i]);
      }
    const middle = min.map((v, i) => (v + max[i]) / 2);
    const scale = 300 / Math.max(1e-12, ...max.map((v, i) => v - min[i]));
    const points = model.vertices.map(([x, y, z]) => {
      x -= middle[0];
      y -= middle[1];
      z -= middle[2];
      const rx = x * Math.cos(a) - z * Math.sin(a);
      const rz = x * Math.sin(a) + z * Math.cos(a);
      return [
        380 + rx * scale,
        230 - (y * Math.cos(b) - rz * Math.sin(b)) * scale,
      ];
    });
    ctx.fillStyle = "#f2f5f4";
    ctx.fillRect(0, 0, 760, 460);
    ctx.strokeStyle = "#235b4e";
    ctx.lineWidth = 1;
    ctx.beginPath();
    for (const [from, to] of model.edges) {
      ctx.moveTo(points[from][0], points[from][1]);
      ctx.lineTo(points[to][0], points[to][1]);
    }
    ctx.stroke();
  }, [model, yaw, pitch]);
  return (
    <div className="asset-wireframe">
      <canvas
        ref={canvas}
        width={760}
        height={460}
        role="img"
        aria-label={`OBJ 线框，${model.vertices.length} 顶点、${model.faces} 个面`}
      />
      <div className="asset-editor-grid">
        <label>
          水平旋转 {yaw}°
          <input
            type="range"
            min="-180"
            max="180"
            value={yaw}
            onChange={(event) => setYaw(Number(event.target.value))}
          />
        </label>
        <label>
          垂直旋转 {pitch}°
          <input
            type="range"
            min="-90"
            max="90"
            value={pitch}
            onChange={(event) => setPitch(Number(event.target.value))}
          />
        </label>
      </div>
      <p>
        {model.vertices.length.toLocaleString()} 顶点 ·{" "}
        {model.edges.length.toLocaleString()} 边 ·{" "}
        {model.faces.toLocaleString()} 面。未加载{" "}
        {model.materialLibraries.length} 个材质库声明；不请求纹理或外部路径。
      </p>
    </div>
  );
}

export function FileEnhancements({
  assetID,
  versionID,
  file,
  expire,
}: {
  assetID: string;
  versionID: string;
  file: ManifestFile;
  expire: (error: unknown) => void;
}) {
  const ext = file.path.split(".").pop()?.toLowerCase() || "";
  const format = mediaToolFormat(file.path);
  const image = format === "image";
  const obj = format === "obj";
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [edge, setEdge] = useState(1024);
  const [model, setModel] = useState<Wireframe>();
  const [result, setResult] = useState<{
    url: string;
    expires: number;
    width: number;
    height: number;
    download: boolean;
  }>();
  const current = useRef<AbortController | null>(null);
  const locked = useRef(false);
  useEffect(() => () => current.current?.abort(), []);
  useEffect(() => {
    if (!result) return;
    const timer = setTimeout(
      () => {
        current.current?.abort();
        setResult(undefined);
        setError(new Error("读取授权已到期，衍生预览已移除。"));
      },
      Math.max(0, result.expires - Date.now()),
    );
    return () => {
      clearTimeout(timer);
      URL.revokeObjectURL(result.url);
    };
  }, [result]);
  const [modelExpiry, setModelExpiry] = useState<number>();
  useEffect(() => {
    if (!modelExpiry) return;
    const timer = setTimeout(
      () => {
        setModel(undefined);
        setError(new Error("读取授权已到期，线框已移除。"));
      },
      Math.max(0, modelExpiry - Date.now()),
    );
    return () => clearTimeout(timer);
  }, [modelExpiry]);

  const run = async (download: boolean) => {
    if (locked.current) return;
    locked.current = true;
    current.current?.abort();
    const request = new AbortController();
    current.current = request;
    setBusy(true);
    setError(undefined);
    setResult(undefined);
    setModel(undefined);
    setModelExpiry(undefined);
    try {
      if (image && typeof createImageBitmap !== "function")
        throw new Error(
          "此浏览器不支持安全的本地图像解码，请使用原件下载或受控工作集。",
        );
      const { bytes, expires } = await readAssetFile({
        assetID,
        versionID,
        file,
        purpose: download ? "raw_export" : "archive_review",
        byteLimit: obj ? objLimits.bytes : imageByteLimit,
        signal: request.signal,
      });
      if (obj) {
        const source = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
        const parsed = parseOBJ(source);
        if (!request.signal.aborted && Date.now() < expires) {
          setModel(parsed);
          setModelExpiry(expires);
        }
      } else {
        const dimensions = imageSize(bytes); // Reject oversized pixels before browser decoding.
        const bitmap = await createImageBitmap(
          new Blob([bytes.buffer], { type: dimensions.mime }),
        );
        try {
          if (!(
            (bitmap.width === dimensions.width &&
              bitmap.height === dimensions.height) ||
            (bitmap.width === dimensions.height &&
              bitmap.height === dimensions.width)
          ))
            throw new Error("解码后的尺寸与文件头不一致。");
          const size = fitImage(
            bitmap.width,
            bitmap.height,
            download ? edge : 320,
          );
          const canvas = document.createElement("canvas");
          canvas.width = size.width;
          canvas.height = size.height;
          const ctx = canvas.getContext("2d");
          if (!ctx) throw new Error("此浏览器不支持本地图像代理。");
          ctx.drawImage(bitmap, 0, 0, size.width, size.height);
          const blob = await new Promise<Blob>((resolve, reject) =>
            canvas.toBlob(
              (value) =>
                value
                  ? resolve(value)
                  : reject(new Error("无法生成 PNG 代理。")),
              "image/png",
            ),
          );
          if (request.signal.aborted || Date.now() >= expires) return;
          setResult({
            url: URL.createObjectURL(blob),
            expires,
            width: size.width,
            height: size.height,
            download,
          });
        } finally {
          bitmap.close();
        }
      }
    } catch (value) {
      if (!request.signal.aborted) {
        if (isSessionError(value)) expire(value);
        setError(value);
      }
    } finally {
      locked.current = false;
      if (!request.signal.aborted) setBusy(false);
    }
  };
  if (!image && !obj) {
    if (format !== "unsupported-3d") return null;
    return (
      <p className="notice">
        此格式可按权限完整存取，本版不解析 {ext.toUpperCase()}。3D
        工具只显示有界 OBJ 多边形线框，不加载材质、纹理、动画或脚本。
      </p>
    );
  }
  return (
    <section
      className="asset-tools panel"
      aria-label={obj ? "OBJ 线框工具" : "图像缩略图与代理工具"}
    >
      <h3>{obj ? "OBJ 线框查看" : "图像缩略图与代理"}</h3>
      <p className="subtle">
        {obj
          ? "只解析 UTF-8 OBJ 多边形：最多 2 MiB、20,000 顶点、60,000 边；不执行工程，不读取 MTL/贴图。"
          : "静态 PNG/JPEG/WebP：输入最多 16 MiB、16,777,216 像素。缩略图仅在当前视图；导出 PNG 代理另申请 raw_export，用途拒绝不会自动降级。原件与版本均不修改。"}
      </p>
      <div className="preview-actions">
        <button
          className="secondary"
          disabled={
            !hasCSRF() ||
            busy ||
            file.size > (obj ? objLimits.bytes : imageByteLimit)
          }
          onClick={() => void run(false)}
        >
          {busy
            ? "正在核验并处理…"
            : obj
              ? "读取并显示线框"
              : "生成 320px 缩略图"}
        </button>
        {image && (
          <>
            <label>
              代理最长边
              <select
                value={edge}
                disabled={busy}
                onChange={(event) => setEdge(Number(event.target.value))}
              >
                {[512, 1024, 2048].map((n) => (
                  <option key={n} value={n}>
                    {n} px
                  </option>
                ))}
              </select>
            </label>
            <button
              className="secondary"
              disabled={!hasCSRF() || busy || file.size > imageByteLimit}
              onClick={() => void run(true)}
            >
              申请导出并生成代理
            </button>
          </>
        )}
        {(result || model || busy) && (
          <button
            className="text-button"
            onClick={() => {
              current.current?.abort();
              setResult(undefined);
              setModel(undefined);
              setModelExpiry(undefined);
              setBusy(false);
            }}
          >
            关闭工具结果
          </button>
        )}
      </div>
      {file.size > (obj ? objLimits.bytes : imageByteLimit) && (
        <p className="notice">
          文件超过此工具预算，未读取；可使用有权限的固定工作集下载后在原软件查看。
        </p>
      )}
      {!!error && (
        <p className="error" role="alert">
          {toolError(error)}
        </p>
      )}
      {busy && <p role="status">正在按当前用途核验固定版本的完整 SHA-256…</p>}
      {result && (
        <div>
          <img
            className="asset-derived-image"
            src={result.url}
            width={result.width}
            height={result.height}
            alt={`${file.path} 的 ${result.width}×${result.height} 本地衍生图`}
          />
          <p>
            {result.width} × {result.height} · PNG ·
            仅本地衍生副本，未自动入馆。
          </p>
          {result.download && (
            <a
              className="secondary"
              href={result.url}
              download={`${
                file.path
                  .split("/")
                  .pop()
                  ?.replace(/\.[^.]+$/u, "") || "image"
              }-proxy.png`}
            >
              下载 PNG 代理副本
            </a>
          )}
        </div>
      )}
      {model && <WireframeCanvas model={model} />}
    </section>
  );
}
