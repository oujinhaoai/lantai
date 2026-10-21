import { useEffect, useRef, useState } from "react";
import {
  api,
  isSessionError,
  message,
  type Identity,
  type Page,
  type Project,
} from "./api";
import { BackupManagement } from "./backup-management";
import { ManagedRecordSaver } from "./managed-record-saver";
import {
  listManagedRecords,
  loadManagedRecord,
  type ManagedRecordItem,
} from "./managed-records";
import {
  downloadManagedJSON,
  identifierPattern,
  parseManagedJSON,
  verifyExactFiles,
  type VerificationReceipt,
} from "./managed-transfer";
import {
  buildRecoveryEvidence,
  type BackupMetadata,
  type InstanceManagementInfo,
} from "./recovery-evidence";

export function BackupsAndRecovery({
  who,
  expire,
}: {
  who: Identity;
  expire: (error: unknown) => void;
}) {
  const [info, setInfo] = useState<InstanceManagementInfo>();
  const [history, setHistory] = useState<BackupMetadata[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectID, setProjectID] = useState("");
  const [runID, setRunID] = useState("");
  const [fixedInput, setFixedInput] = useState("[]");
  const [backupInput, setBackupInput] = useState("");
  const [verified, setVerified] = useState<VerificationReceipt[]>([]);
  const [evidence, setEvidence] = useState<unknown>();
  const [records, setRecords] = useState<ManagedRecordItem[]>([]);
  const [opened, setOpened] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [notice, setNotice] = useState("");
  const inFlight = useRef(false);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => () => controller.current?.abort(), []);
  const action = async (run: (signal: AbortSignal) => Promise<void>) => {
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    setError(undefined);
    const c = new AbortController();
    controller.current = c;
    try {
      await run(c.signal);
    } catch (cause) {
      if (!c.signal.aborted) {
        if (isSessionError(cause)) expire(cause);
        else setError(cause);
      }
    } finally {
      inFlight.current = false;
      if (!c.signal.aborted) setBusy(false);
    }
  };
  const refresh = async (signal: AbortSignal) => {
    const value = await api<InstanceManagementInfo>("/api/v1/admin/instance", {
      signal,
    });
    const backups = await api<{ items: BackupMetadata[] }>(
      "/api/v1/admin/backups/history",
      { signal },
    );
    const page = await api<Page<Project>>("/api/v1/projects?limit=100", {
      signal,
    });
    if (signal.aborted) return;
    setInfo(value);
    setHistory(backups.items.filter((row) => row.state === "complete"));
    setProjects(page.items ?? []);
    if (!runID && value.recovery_receipts.length)
      setRunID(value.recovery_receipts[0].run_id);
    setNotice(
      page.next_cursor
        ? "项目仅列当前一页；可手填已授权项目 ID。恢复回执列表标明自身范围。"
        : "已读取当前核心；完整包生成与独立恢复接受是不同阶段。",
    );
  };
  const verify = async (signal: AbortSignal) => {
    setEvidence(undefined);
    setVerified([]);
    const requested = parseManagedJSON(fixedInput);
    if (
      !Array.isArray(requested) ||
      !requested.length ||
      requested.length > 100
    )
      throw new Error("请提供 1–100 项明确固定资产/版本。");
    const before = await api<InstanceManagementInfo>("/api/v1/admin/instance", {
      signal,
    });
    const results: VerificationReceipt[] = [];
    for (const item of requested) {
      if (
        !item ||
        typeof item !== "object" ||
        Array.isArray(item) ||
        Object.keys(item).some(
          (key) => !["asset_id", "version_id"].includes(key),
        ) ||
        !identifierPattern.test(item.asset_id) ||
        !identifierPattern.test(item.version_id)
      )
        throw new Error("固定版本列表必须仅包含合法 asset_id/version_id。");
      results.push(
        await verifyExactFiles({
          assetID: item.asset_id,
          versionID: item.version_id,
          purpose: "archive_review",
          signal,
        }),
      );
      if (!signal.aborted) setVerified([...results]);
    }
    const current = await api<InstanceManagementInfo>(
      "/api/v1/admin/instance",
      { signal },
    );
    if (
      current.instance_id !== before.instance_id ||
      current.recovery_epoch !== before.recovery_epoch
    )
      throw new Error("验证期间恢复代际变化，不能接受旧验证结果。");
    const backup = backupInput.trim()
      ? (parseManagedJSON(backupInput) as BackupMetadata)
      : undefined;
    const next = buildRecoveryEvidence(current, runID, results, backup);
    if (!signal.aborted) {
      setInfo(current);
      setEvidence(next);
      setNotice(
        "所选固定版本已逐文件读取并核验；这不代表全部馆藏、源机隔离或原生软件续做已经验证。",
      );
    }
  };
  return (
    <>
      <BackupManagement expire={expire} />
      <section
        className="panel recovery-management"
        aria-label="独立恢复与验证记录"
      >
        <h2>独立恢复与验证记录</h2>
        <p>
          新目标仍通过受保护的 <a href="/setup/">安装与完整包恢复入口</a>{" "}
          认领和恢复。它只接受空目标；备份单包无需另搬旧密钥或恢复口令。此处关联核心已接受的恢复点与当前固定版本校验。
        </p>
        <button
          className="secondary"
          disabled={busy}
          onClick={() => void action(refresh)}
        >
          读取核心恢复回执与备份记录
        </button>
        {info && (
          <>
            <dl>
              <dt>当前实例</dt>
              <dd>
                <code>{info.instance_id}</code>
              </dd>
              <dt>恢复代际</dt>
              <dd>{info.recovery_epoch}</dd>
              <dt>主库余量观察</dt>
              <dd>
                {info.free_bytes === null
                  ? "未知"
                  : `${info.free_bytes.toLocaleString()} 字节`}
                ；最低保留 {info.min_free_bytes.toLocaleString()} 字节
              </dd>
            </dl>
            {info.capacity_error && (
              <p className="notice">
                容量读取失败，不能判断空间充足。备份卷、setup、恢复和校验副本需要分别预算。
              </p>
            )}
            {info.recovery_error && (
              <p className="error" role="alert">
                核心恢复回执不可用或异常：{info.recovery_error}
                。不能将缺记录当作恢复通过。
              </p>
            )}
            {info.recovery_partial && (
              <p className="notice">恢复回执达到显示预算，仅呈现部分记录。</p>
            )}
            {!info.recovery_receipts.length ? (
              <p>
                当前核心没有已完成恢复回执。完整包生成成功可在上方查看；不能因此标记为独立恢复通过。
              </p>
            ) : (
              <label>
                核心已接受的恢复运行
                <select
                  value={runID}
                  onChange={(event) => {
                    setRunID(event.target.value);
                    setEvidence(undefined);
                  }}
                  disabled={busy}
                >
                  {info.recovery_receipts.map((row) => (
                    <option key={row.run_id} value={row.run_id}>
                      {row.run_id} · 备份 {row.backup_id} · 代际{" "}
                      {row.recovery_epoch}
                    </option>
                  ))}
                </select>
              </label>
            )}
          </>
        )}
        <details>
          <summary>导出已生成完整包的非秘密元数据</summary>
          <p>
            仅导出标识、摘要和大小，不包含包、密钥或下载授权。该文件不是恢复必需品；恢复仍只需完整包。
          </p>
          {history
            .filter((row) => row.package)
            .map((row) => (
              <p key={row.id}>
                <code>{row.id}</code>{" "}
                <button
                  className="secondary"
                  onClick={() =>
                    downloadManagedJSON(`backup-${row.id}-metadata.json`, row)
                  }
                >
                  导出包标识与摘要
                </button>
              </p>
            ))}
        </details>
        <label>
          待核验固定版本 JSON
          <textarea
            rows={4}
            value={fixedInput}
            disabled={busy}
            onChange={(event) => {
              setFixedInput(event.target.value);
              setEvidence(undefined);
            }}
            placeholder='[{"asset_id":"…","version_id":"…"}]'
          />
        </label>
        <label>
          可选：对应完整包的标识与摘要 JSON
          <textarea
            rows={3}
            value={backupInput}
            disabled={busy}
            onChange={(event) => {
              setBackupInput(event.target.value);
              setEvidence(undefined);
            }}
          />
        </label>
        <button
          className="primary"
          disabled={busy || !runID}
          onClick={() => void action(verify)}
        >
          {busy ? "正在核验…" : "逐文件核验并关联当前恢复回执"}
        </button>
        {busy && (
          <button
            className="secondary"
            onClick={() => {
              controller.current?.abort();
              setBusy(false);
              setNotice("本机读取已取消；未声明服务器操作回滚。");
            }}
          >
            停止本次读取
          </button>
        )}
        {!!verified.length && (
          <p>
            本次已核验 {verified.length}{" "}
            个固定版本；只有全部选中项成功后才形成接受记录。
          </p>
        )}
        {evidence != null && (
          <>
            <pre className="management-json">
              {JSON.stringify(evidence, null, 2)}
            </pre>
            <button
              className="secondary"
              onClick={() =>
                downloadManagedJSON("lantai-recovery-evidence.json", evidence)
              }
            >
              导出本次恢复验证记录
            </button>
            <label>
              档案项目
              <select
                value={projectID}
                onChange={(event) => setProjectID(event.target.value)}
              >
                <option value="">选择有权保存的项目</option>
                {projects.map((project) => (
                  <option key={project.project_id} value={project.project_id}>
                    {project.description.name}
                  </option>
                ))}
              </select>
            </label>
            <label>
              或明确项目 ID
              <input
                value={projectID}
                onChange={(event) => setProjectID(event.target.value)}
              />
            </label>
            <ManagedRecordSaver
              who={who}
              expire={expire}
              projectID={projectID}
              title="独立恢复与固定版本核验"
              kind="recovery"
              payload={evidence}
            />
          </>
        )}
        <details>
          <summary>打开馆内已有恢复记录</summary>
          <label>
            项目 ID
            <input
              value={projectID}
              onChange={(event) => setProjectID(event.target.value)}
              disabled={busy}
            />
          </label>
          <button
            className="secondary"
            disabled={busy}
            onClick={() =>
              void action(async (signal) => {
                const page = await listManagedRecords({
                  projectID,
                  kind: "recovery",
                  signal,
                });
                if (!signal.aborted) {
                  setRecords(page.items);
                  setNotice(
                    page.next_cursor
                      ? "当前只列一页，请使用固定记录引用或其他页面继续查找。"
                      : "已读取当前可见恢复档案。",
                  );
                }
              })
            }
          >
            列出该项目恢复档案
          </button>
          {records.map((record) => (
            <p key={record.asset_id}>
              <button
                className="text-button"
                disabled={busy}
                onClick={() =>
                  void action(async (signal) => {
                    const value = await loadManagedRecord(record, signal, {
                      kind: "recovery",
                      projectID,
                    });
                    if (!signal.aborted) setOpened(value);
                  })
                }
              >
                {record.title}
              </button>
            </p>
          ))}
          {opened != null && (
            <>
              <p className="notice">
                这是历史档案。当前状态需重新请求核心与固定版本核验，不能只信旧
                complete。
              </p>
              <pre className="management-json">
                {JSON.stringify(opened, null, 2)}
              </pre>
            </>
          )}
        </details>
        {notice && (
          <p role="status" aria-live="polite">
            {notice}
          </p>
        )}
        {error != null && (
          <p className="error" role="alert">
            {error instanceof Error && !("code" in error)
              ? error.message
              : message(error)}
          </p>
        )}
      </section>
    </>
  );
}
