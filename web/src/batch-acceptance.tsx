import { useCallback, useEffect, useRef, useState } from "react";
import { api, isSessionError, message, type Identity } from "./api";
import {
  loadManagedRecord,
  listManagedRecords,
  type ManagedRecordRef,
} from "./managed-records";
import { ManagedRecordSaver } from "./managed-record-saver";
import {
  downloadManagedJSON,
  identifierPattern,
  parseManagedJSON,
  type VerificationReceipt,
} from "./managed-transfer";
import type { MigrationWorkspace } from "./migration-model";
import {
  batchFromWorkspace,
  batchPurposes,
  batchReadiness,
  batchRefLines,
  batchStages,
  checklistLabels,
  nextBatchRevision,
  syncBatchImport,
  parseBatchRefLines,
  validateBatchCheck,
  validateMigrationBatch,
  type BatchCheck,
  type MigrationBatch,
} from "./batch-acceptance-model";
import { ReferenceJobs } from "./reference-jobs";
import type { Transport } from "./system-operations";

type Row = { ref: ManagedRecordRef; batch: MigrationBatch };
export function BatchAcceptance({
  who,
  expire,
  projectID,
  workspace,
  verifications,
  onPendingChange,
}: {
  who: Identity;
  expire: (error: unknown) => void;
  projectID: string;
  workspace?: MigrationWorkspace;
  verifications: Record<string, VerificationReceipt>;
  onPendingChange?: (pending: boolean) => void;
}) {
  const [devicePending, setDevicePending] = useState(false);
  const [stagedJobs, setStagedJobs] = useState<
    MigrationBatch["evidence"]["reference_jobs"]
  >([]);
  const [rows, setRows] = useState<Row[]>([]);
  const [cursor, setCursor] = useState<string>();
  const [batch, setBatch] = useState<MigrationBatch>();
  const [snapshot, setSnapshot] = useState<MigrationBatch>();
  const [recordRef, setRecordRef] = useState<ManagedRecordRef>();
  const [recordPending, setRecordPending] = useState(false);
  const [check, setCheck] = useState<BatchCheck>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [notice, setNotice] = useState("");
  const [refAsset, setRefAsset] = useState("");
  const [refVersion, setRefVersion] = useState("");
  const [assetsText, setAssetsText] = useState("[]");
  const [worksetsText, setWorksetsText] = useState("");
  const [verificationsText, setVerificationsText] = useState("");
  const [recoveryText, setRecoveryText] = useState("");
  const [nativeText, setNativeText] = useState("");
  const active = useRef<AbortController | undefined>(undefined);
  const alive = useRef(true);
  const actorID = who.principal.principal_id ?? "";
  const request: Transport = useCallback(
    async <T,>(path: string, options?: RequestInit) => {
      try {
        return await api<T>(path, options);
      } catch (cause) {
        if (isSessionError(cause)) expire(cause);
        throw cause;
      }
    },
    [expire],
  );
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      active.current?.abort();
    };
  }, []);
  const work = async (action: (signal: AbortSignal) => Promise<void>) => {
    if (busy || devicePending) return;
    const c = new AbortController();
    active.current = c;
    setBusy(true);
    setError(undefined);
    try {
      await action(c.signal);
    } catch (cause) {
      if (!c.signal.aborted) {
        if (isSessionError(cause)) expire(cause);
        else setError(cause);
      }
    } finally {
      if (alive.current) setBusy(false);
    }
  };
  const choose = (value: MigrationBatch, ref?: ManagedRecordRef) => {
    setBatch(value);
    setSnapshot(undefined);
    setRecordRef(ref);
    setRecordPending(false);
    setCheck(undefined);
    setAssetsText(JSON.stringify(value.evidence.assets, null, 2));
    setWorksetsText(batchRefLines(value.evidence.worksets));
    setVerificationsText(batchRefLines(value.evidence.verifications));
    setRecoveryText(batchRefLines(value.evidence.recovery));
    setNativeText(batchRefLines(value.evidence.native_continuations ?? []));
  };
  const create = () =>
    void work(async (signal) => {
      const meta = await request<{ instance_id: string }>("/api/v1/meta", {
        signal,
      });
      choose(
        batchFromWorkspace({
          workspace,
          projectID,
          instanceID: meta.instance_id,
          actorID,
        }),
      );
      setNotice("批次台账尚未写入。完善责任人、证据与统一切换清单后冻结修订。");
    });
  const read = () =>
    void work(async (signal) => {
      const ref = { asset_id: refAsset.trim(), version_id: refVersion.trim() };
      const value = validateMigrationBatch(
        await loadManagedRecord(ref, signal, { kind: "migration", projectID }),
      );
      choose(value, ref);
    });
  const list = (next?: string) =>
    void work(async (signal) => {
      const page = await listManagedRecords({
        projectID,
        kind: "migration",
        cursor: next,
        signal,
      });
      const found: Row[] = [];
      for (const item of page.items) {
        const value = await loadManagedRecord<unknown>(item, signal, {
          kind: "migration",
          projectID,
        });
        if (
          typeof value !== "object" ||
          value === null ||
          !("schema" in value) ||
          value.schema !== "lantai.migration-batch/v1"
        )
          continue;
        found.push({
          ref: { asset_id: item.asset_id, version_id: item.version_id },
          batch: validateMigrationBatch(value),
        });
      }
      if (!signal.aborted) {
        setRows((old) => (next ? [...old, ...found] : found));
        setCursor(page.next_cursor);
        setNotice(
          "已逐项读取当前授权范围内的真实批次记录。历史导入存档不作为批次接受记录。",
        );
      }
    });
  const edit = <K extends keyof MigrationBatch>(
    key: K,
    value: MigrationBatch[K],
  ) => {
    if (!batch || snapshot || recordRef) return;
    setBatch({ ...batch, [key]: value });
    setCheck(undefined);
  };
  const freeze = () => {
    try {
      if (!batch) return;
      const next = validateMigrationBatch({
        ...batch,
        evidence: {
          ...batch.evidence,
          assets: parseManagedJSON(assetsText),
          worksets: parseBatchRefLines(worksetsText),
          verifications: parseBatchRefLines(verificationsText),
          recovery: parseBatchRefLines(recoveryText),
          native_continuations: parseBatchRefLines(nativeText),
        },
        last_action: {
          ...batch.last_action,
          actor_id: actorID,
          at: new Date().toISOString(),
        },
      });
      setBatch(next);
      setSnapshot(next);
      setError(undefined);
      setCheck(undefined);
    } catch (cause) {
      setError(cause);
    }
  };
  const currentCheck = () =>
    void work(async (signal) => {
      if (!batch || !recordRef || recordPending)
        throw new Error("先保存完整、无pending的固定批次修订。");
      setCheck(undefined);
      const value = await request<unknown>("/api/v1/management/batches/check", {
        method: "POST",
        body: JSON.stringify(recordRef),
        signal,
      });
      const result = validateBatchCheck(value, batch, recordRef);
      if (!signal.aborted) {
        setCheck(result);
        setNotice(
          result.acceptance === "ready"
            ? "服务端对本固定批次修订的本次接受观察可用；不授权删除原件。"
            : "服务端接受检查仍有阻断，原件与原状态继续保留。",
        );
      }
    });
  const onReferenceEvidence = useCallback(
    (item: {
      job_id: string;
      action: "references.plan" | "references.copy" | "references.rollback";
    }) => {
      setStagedJobs((old) => [
        ...old.filter((job) => job.action !== item.action),
        item,
      ]);
      setNotice(`设备作业 ${item.job_id} 已保留，可在当前可编辑修订中关联。`);
      setCheck(undefined);
      setBatch((old) =>
        old &&
        !snapshot &&
        !recordRef &&
        !old.evidence.reference_jobs.some(
          (job) => job.action === item.action && job.job_id !== item.job_id,
        )
          ? {
              ...old,
              evidence: {
                ...old.evidence,
                reference_jobs: [
                  ...old.evidence.reference_jobs.filter(
                    (job) => job.job_id !== item.job_id,
                  ),
                  item,
                ],
              },
            }
          : old,
      );
    },
    [snapshot, recordRef],
  );
  const onDevicePending = useCallback(
    (value: boolean) => {
      setDevicePending(value);
      onPendingChange?.(value);
      if (value) setCheck(undefined);
    },
    [onPendingChange],
  );
  const disabled = busy || devicePending || !!snapshot || !!recordRef;
  let blockers: string[] = [];
  try {
    if (batch) blockers = batchReadiness(batch);
  } catch {
    blockers = ["草稿字段尚未完整"];
  }
  return (
    <section className="panel" aria-label="批次使用与切换接受">
      <h2>批次台账与切换接受</h2>
      <p>
        稳定批次和不可变修订记录责任人、最后动作、固定资料与切换范围。客户端声明不能代替服务端当前证据；缺证据、failed、pending、unknown
        都不能成为完成。原件一直保留。
      </p>
      <div className="migration-actions">
        <button
          className="secondary"
          disabled={
            busy ||
            devicePending ||
            !projectID ||
            !identifierPattern.test(actorID)
          }
          onClick={create}
        >
          从当前明确批次建立台账
        </button>
        <button
          className="secondary"
          disabled={busy || devicePending || !projectID}
          onClick={() => list()}
        >
          读取本项目批次台账
        </button>
        {cursor && (
          <button
            className="secondary"
            disabled={busy}
            onClick={() => list(cursor)}
          >
            读取下一页批次
          </button>
        )}
      </div>
      {!identifierPattern.test(actorID) && (
        <p className="notice">
          当前身份尚未返回规范主体 ID，不能猜测操作者或建立接受记录。
        </p>
      )}
      <div className="migration-table-scroll">
        <table>
          <thead>
            <tr>
              <th>批次／修订</th>
              <th>责任人</th>
              <th>阶段</th>
              <th>最后动作</th>
              <th>接受状态</th>
              <th>固定记录</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={`${row.ref.asset_id}/${row.ref.version_id}`}>
                <td>
                  {row.batch.title}
                  <br />
                  <code>
                    {row.batch.batch_id} / r{row.batch.revision}
                  </code>
                </td>
                <td>
                  <code>{row.batch.owner_id}</code>
                </td>
                <td>{row.batch.stage}</td>
                <td>
                  {row.batch.last_action.action}
                  <br />
                  {row.batch.last_action.at}
                </td>
                <td>待当前服务端复核 · {row.batch.acceptance}</td>
                <td>
                  <button
                    className="text-button"
                    disabled={busy || devicePending}
                    onClick={() => choose(row.batch, row.ref)}
                  >
                    打开固定修订
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <details>
        <summary>打开准确的批次档案</summary>
        <div className="migration-fields">
          <label>
            批次 config asset_id
            <input
              value={refAsset}
              onChange={(e) => setRefAsset(e.target.value)}
            />
          </label>
          <label>
            固定 version_id
            <input
              value={refVersion}
              onChange={(e) => setRefVersion(e.target.value)}
            />
          </label>
        </div>
        <button
          className="secondary"
          disabled={
            busy || devicePending || !projectID || !refAsset || !refVersion
          }
          onClick={read}
        >
          读取固定批次修订
        </button>
      </details>
      {batch && (
        <>
          {devicePending && (
            <p className="notice">
              当前设备作业仍在进行或结果不明，保留原批次与作业，先对账后再切换／修订台账。
            </p>
          )}
          <div className="migration-actions">
            <button
              className="secondary"
              disabled={disabled || !workspace}
              onClick={() => {
                try {
                  if (workspace) {
                    const next = syncBatchImport(batch, workspace, actorID);
                    setBatch(next);
                    setAssetsText(
                      JSON.stringify(next.evidence.assets, null, 2),
                    );
                    setCheck(undefined);
                  }
                } catch (cause) {
                  setError(cause);
                }
              }}
            >
              从同批原状态更新全部导入事实
            </button>
            <button
              className="secondary"
              disabled={disabled || !stagedJobs.length}
              onClick={() => {
                edit("evidence", {
                  ...batch.evidence,
                  reference_jobs: [
                    ...batch.evidence.reference_jobs.filter(
                      (job) =>
                        !stagedJobs.some((item) => item.action === job.action),
                    ),
                    ...stagedJobs,
                  ],
                });
              }}
            >
              采用本页各动作最新已观察原作业（保留旧作业）
            </button>
          </div>
          <p>
            批次 <code>{batch.batch_id}</code> · 修订 {batch.revision} · 实例{" "}
            <code>{batch.instance_id}</code>
          </p>
          {batch.previous_ref && (
            <p>
              条件基线{" "}
              <code>
                {batch.previous_ref.asset_id}/{batch.previous_ref.version_id}
              </code>
              ；当前版本已变化时必须重读，不自动重建新资产。
            </p>
          )}
          <fieldset disabled={disabled}>
            <div className="migration-fields">
              <label>
                批次名称
                <input
                  value={batch.title}
                  onChange={(e) => edit("title", e.target.value)}
                />
              </label>
              <label>
                责任人主体 ID
                <input
                  value={batch.owner_id}
                  onChange={(e) => edit("owner_id", e.target.value)}
                />
              </label>
              <label>
                阶段
                <select
                  value={batch.stage}
                  onChange={(e) =>
                    edit("stage", e.target.value as MigrationBatch["stage"])
                  }
                >
                  {batchStages.map((stage) => (
                    <option key={stage}>{stage}</option>
                  ))}
                </select>
              </label>
            </div>
            <p>
              计划 {batch.import_summary.planned} 项：committed{" "}
              {batch.import_summary.committed}，pending{" "}
              {batch.import_summary.description_pending}，failed{" "}
              {batch.import_summary.failed}，unknown{" "}
              {batch.import_summary.unknown}，未开始{" "}
              {batch.import_summary.not_attempted}
              。统计由原导入工作区生成，不省略无回执项。
            </p>
            <label>
              接受使用的明确用途
              <select
                value={batch.intended_purpose}
                onChange={(event) =>
                  edit(
                    "intended_purpose",
                    event.target.value as MigrationBatch["intended_purpose"],
                  )
                }
              >
                {batchPurposes.map((purpose) => (
                  <option key={purpose}>{purpose}</option>
                ))}
              </select>
            </label>
            <p>
              服务端逐资产重新检查该用途的当前 rights
              与授权。档案查看通过不会自动成为制作、生成式输入或原件导出授权。
            </p>
            <label>
              本次动作说明
              <textarea
                value={batch.last_action.note}
                onChange={(e) =>
                  edit("last_action", {
                    ...batch.last_action,
                    note: e.target.value,
                  })
                }
              />
            </label>
            <label>
              固定资产版本、完整 manifest 摘要及原逐项状态
              <textarea
                rows={7}
                value={assetsText}
                onChange={(e) => setAssetsText(e.target.value)}
              />
            </label>
            <div className="migration-fields">
              <label>
                工作集固定档案（每行 asset_id/version_id）
                <textarea
                  rows={3}
                  value={worksetsText}
                  onChange={(e) => setWorksetsText(e.target.value)}
                />
              </label>
              <label>
                当前核验固定档案
                <textarea
                  rows={3}
                  value={verificationsText}
                  onChange={(e) => setVerificationsText(e.target.value)}
                />
              </label>
              <label>
                按当前策略人审的原生续做报告（每行 asset_id/version_id）
                <textarea
                  rows={3}
                  value={nativeText}
                  onChange={(e) => setNativeText(e.target.value)}
                />
                <small>
                  报告须绑定原工作集、入口、回交版本与佐证文件，并通过原审定流程确认。项目策略允许自审时可由操作者本人完成，禁止时须其他合格审定人；新审定仍需本人会话与新鲜动态码。服务器复核当前审定；人审接受不代表核心自动证明
                  DCC 执行。
                </small>
              </label>
              <label>
                完整包恢复固定证据
                <textarea
                  rows={3}
                  value={recoveryText}
                  onChange={(e) => setRecoveryText(e.target.value)}
                />
              </label>
            </div>
            <label>
              本批笔记改链范围
              <select
                value={batch.reference_scope}
                onChange={(e) =>
                  edit(
                    "reference_scope",
                    e.target.value as MigrationBatch["reference_scope"],
                  )
                }
              >
                <option value="selected_notes">
                  已选定笔记，必须有原计划与副本／回退作业
                </option>
                <option value="none">
                  明确无选中笔记，只验收本批素材与工作集
                </option>
              </select>
            </label>
            <h3>统一切换清单：责任声明</h3>
            {Object.entries(checklistLabels).map(([key, label]) => (
              <label key={key}>
                <input
                  type="checkbox"
                  checked={batch.checklist[key as keyof typeof checklistLabels]}
                  onChange={(e) =>
                    edit("checklist", {
                      ...batch.checklist,
                      [key]: e.target.checked,
                    })
                  }
                />
                {label}
              </label>
            ))}
            <p>
              原生软件打开勾选只是操作者声明；没有可复核的原生续做证据，服务端仍应阻断接受。副本完成也不等于全馆引用完成。
            </p>
            <label>
              <input
                type="checkbox"
                checked={batch.acceptance === "requested"}
                onChange={(e) =>
                  edit(
                    "acceptance",
                    e.target.checked ? "requested" : "not_requested",
                  )
                }
              />
              明确请求对本修订进行切换接受检查（不授权删源）
            </label>
          </fieldset>
          {blockers.length > 0 && (
            <div className="notice">
              <strong>当前声明仍有阻断</strong>
              <ul>
                {blockers.map((blocker) => (
                  <li key={blocker}>{blocker}</li>
                ))}
              </ul>
            </div>
          )}
          <div className="migration-actions">
            {!snapshot && !recordRef && (
              <button
                className="secondary"
                disabled={busy || devicePending}
                onClick={freeze}
              >
                冻结本修订并准备保存
              </button>
            )}
            {recordRef && (
              <>
                <button
                  className="secondary"
                  disabled={busy || devicePending || recordPending}
                  onClick={() => {
                    try {
                      choose(nextBatchRevision(batch, recordRef, actorID));
                    } catch (cause) {
                      setError(cause);
                    }
                  }}
                >
                  创建基于原版本的条件修订
                </button>
                <button
                  className="primary"
                  disabled={busy || devicePending || recordPending}
                  onClick={currentCheck}
                >
                  服务端当前复核与接受判定
                </button>
              </>
            )}
            <button
              className="secondary"
              disabled={busy}
              onClick={() =>
                downloadManagedJSON(
                  `batch-${batch.batch_id}-r${batch.revision}.json`,
                  batch,
                )
              }
            >
              导出本修订副本
            </button>
          </div>
          {snapshot && !recordRef && (
            <ManagedRecordSaver
              who={who}
              expire={expire}
              projectID={projectID}
              title={snapshot.title}
              kind="migration"
              payload={snapshot}
              assetID={snapshot.previous_ref?.asset_id}
              baseVersionID={snapshot.previous_ref?.version_id}
              onSaved={(ref) => {
                setRecordRef({
                  asset_id: ref.asset_id,
                  version_id: ref.version_id,
                });
                setRecordPending(!!ref.description_pending);
                setCheck(undefined);
                setNotice(
                  ref.description_pending
                    ? "档案著录pending，保留原操作，不进入接受。"
                    : "固定批次修订已保存，接受状态仍待服务端当前复核。",
                );
              }}
            />
          )}
          {Object.keys(verifications).length > 0 && workspace && (
            <details>
              <summary>保存本次真实逐文件核验为固定证据</summary>
              <ManagedRecordSaver
                who={who}
                expire={expire}
                projectID={projectID}
                title={`${batch.title} · 本次核验`}
                kind="migration"
                payload={{
                  schema: "lantai.migration-verification-record/v1",
                  batch_id: batch.batch_id,
                  instance_id: batch.instance_id,
                  project_id: projectID,
                  origin: workspace.origin,
                  workspace_plan_digest: workspace.plan_digest,
                  verifications: Object.values(verifications),
                }}
                onSaved={(ref) => {
                  setNotice(
                    `核验档案已保存：${ref.asset_id}/${ref.version_id}，请在可编辑修订中关联。`,
                  );
                  if (!disabled && !ref.description_pending)
                    setVerificationsText((old) =>
                      [old, `${ref.asset_id}/${ref.version_id}`]
                        .filter(Boolean)
                        .join("\n"),
                    );
                }}
              />
            </details>
          )}
          {check && (
            <div role="status">
              <h3>
                {check.acceptance === "ready"
                  ? "本次服务端接受观察：可用"
                  : "本次服务端接受观察：阻断"}
              </h3>
              <p>
                {check.checked_at} · 固定 r{check.revision}
                ；稍后权限、版本或恢复代次变化需重新复核。
              </p>
              <ul>
                {check.checks.map((item) => (
                  <li key={item.key}>
                    {item.key}：{item.status}
                    {item.code ? ` · ${item.code}` : ""}
                  </li>
                ))}
              </ul>
            </div>
          )}
          <ReferenceJobs
            key={`${projectID}/${batch.batch_id}`}
            request={request}
            projectID={projectID}
            batchID={batch.batch_id}
            onPendingChange={onDevicePending}
            onEvidence={onReferenceEvidence}
          />
          <details>
            <summary>已关联实际设备引用作业</summary>
            <ul>
              {batch.evidence.reference_jobs.map((job) => (
                <li key={job.job_id}>
                  <code>
                    {job.action} / {job.job_id}
                  </code>
                  <button
                    className="text-button"
                    disabled={disabled}
                    onClick={() =>
                      edit("evidence", {
                        ...batch.evidence,
                        reference_jobs: batch.evidence.reference_jobs.filter(
                          (item) => item.job_id !== job.job_id,
                        ),
                      })
                    }
                  >
                    从本修订移除关联（不删除或取消原作业）
                  </button>
                </li>
              ))}
            </ul>
            <p>
              同动作的新观察须明确采用；旧批次修订与实际作业始终保留。失败或不明作业不会因移除关联而被视为成功或回滚。
            </p>
          </details>
        </>
      )}
      {notice && <p role="status">{notice}</p>}
      {error !== undefined && (
        <p className="error" role="alert">
          {error instanceof Error && !("code" in error)
            ? error.message
            : message(error)}
        </p>
      )}
    </section>
  );
}
