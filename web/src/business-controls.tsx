import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type FormEvent,
} from "react";
import { type Project } from "./api";
import {
  currentObjects,
  identifier,
  reason,
  revision,
  type CommandBody,
  type CurrentObject,
  type SensitiveIntent,
  type Transport,
} from "./system-operations";
import { Receipt, SystemError } from "./system-confirmation";

export type StageDirect = (
  title: string,
  path: string,
  body: CommandBody,
) => void;
type StageSensitive = (intent: SensitiveIntent) => void;
interface ReviewTarget {
  target_id: string;
  target_revision: number;
  project_id: string;
  asset_id: string;
  version_id: string;
  manifest_digest: string;
  maker_id: string;
  profile_ref: string;
  profile: unknown;
  evidence: unknown[];
  [key: string]: unknown;
}
interface TrashEntry {
  trash_id: string;
  project_id: string;
  asset_id: string;
  revision: number;
  files_digest: string;
  state: string;
  hold: boolean;
  original_slug: string;
  due_at: string;
  version_ids: string[];
  reason: string;
}
interface InboxItem {
  object: CurrentObject;
  sequence: number;
  read: boolean;
}
interface EventItem {
  event_id: string;
  sequence: number;
  type: string;
  object: CurrentObject;
}
function csvIDs(value: string) {
  return [
    ...new Set(
      value
        .split(/[\s,]+/)
        .filter(Boolean)
        .map(identifier),
    ),
  ];
}
function BoundID({
  label,
  value,
  onChange,
  disabled,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
}) {
  return (
    <label>
      {label}
      <input
        value={value}
        onChange={(e) => onChange(e.target.value.trim())}
        maxLength={26}
        autoComplete="off"
        spellCheck={false}
        disabled={disabled}
      />
    </label>
  );
}

export function BusinessControls({
  project,
  request,
  sensitive,
  direct,
  disabled,
  refresh,
  view = "collaboration",
}: {
  project: Project;
  request: Transport;
  sensitive: StageSensitive;
  direct: StageDirect;
  disabled: boolean;
  refresh: number;
  view?: "collaboration" | "audit";
}) {
  const [objects, setObjects] = useState<CurrentObject[]>([]);
  const [inbox, setInbox] = useState<{
    items: InboxItem[];
    through: number;
    read_through: number;
  }>();
  const [loaded, setLoaded] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>();
  const [targetID, setTargetID] = useState("");
  const [target, setTarget] = useState<ReviewTarget>();
  const [trashID, setTrashID] = useState("");
  const [trash, setTrash] = useState<TrashEntry>();
  const [note, setNote] = useState("");
  const [verdict, setVerdict] = useState("approve");
  const [effectiveReviewID, setEffectiveReviewID] = useState("");
  const [newSlug, setNewSlug] = useState("");
  const [assetID, setAssetID] = useState("");
  const [versionID, setVersionID] = useState("");
  const [whole, setWhole] = useState(false);
  const [preview, setPreview] = useState<CommandBody>();
  const [submitVersion, setSubmitVersion] = useState("");
  const [submitRevision, setSubmitRevision] = useState("1");
  const [profileRef, setProfileRef] = useState("");
  const [evidence, setEvidence] = useState("");
  const [initialProfile, setInitialProfile] = useState(false);
  const [taskID, setTaskID] = useState("");
  const [attemptID, setAttemptID] = useState("");
  const [round, setRound] = useState("1");
  const [fence, setFence] = useState("1");
  const [candidateGroup, setCandidateGroup] = useState("");
  const [publicationVersion, setPublicationVersion] = useState("");
  const [reviewID, setReviewID] = useState("");
  const [publicationRevision, setPublicationRevision] = useState("0");
  const [publicationAction, setPublicationAction] = useState("publish");
  const [audit, setAudit] = useState<EventItem[]>([]);
  const [after, setAfter] = useState(0);
  const [auditLoaded, setAuditLoaded] = useState(false);
  const alive = useRef(true);
  const actionLock = useRef(false);
  const readAbort = useRef<AbortController | undefined>(undefined);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      readAbort.current?.abort();
    };
  }, []);
  const work = async (action: () => Promise<void>) => {
    if (actionLock.current) return;
    actionLock.current = true;
    setLoading(true);
    setError(undefined);
    try {
      await action();
    } catch (e) {
      if (
        alive.current &&
        !(e instanceof DOMException && e.name === "AbortError")
      ) {
        setTarget(undefined);
        setTrash(undefined);
        setObjects([]);
        setInbox(undefined);
        setAudit([]);
        setError(e);
      }
    } finally {
      actionLock.current = false;
      if (alive.current) setLoading(false);
    }
  };
  const readCurrent = useCallback(async () => {
    readAbort.current?.abort();
    const controller = new AbortController();
    readAbort.current = controller;
    setObjects([]);
    setInbox(undefined);
    setTarget(undefined);
    setTrash(undefined);
    setLoaded(false);
    setError(undefined);
    setLoading(true);
    try {
      const snapshot = await currentObjects(
        project.project_id,
        request,
        controller.signal,
      );
      const currentInbox = await request<{
        items: InboxItem[];
        through: number;
        read_through: number;
      }>(`/api/v1/inbox?project_id=${encodeURIComponent(project.project_id)}`, {
        signal: controller.signal,
      });
      if (controller.signal.aborted || !alive.current) return;
      if (!Array.isArray(currentInbox.items))
        throw new Error("待办响应不完整。");
      setObjects(snapshot.objects);
      setInbox(currentInbox);
      setLoaded(true);
    } catch (e) {
      if (!controller.signal.aborted && alive.current) {
        setObjects([]);
        setInbox(undefined);
        setError(e);
      }
    } finally {
      if (!controller.signal.aborted && alive.current) setLoading(false);
    }
  }, [project.project_id, request]);
  useEffect(() => {
    let active = true;
    queueMicrotask(() => {
      if (active && view === "collaboration") void readCurrent();
    });
    return () => {
      active = false;
    };
  }, [readCurrent, refresh, view]);
  const loadTarget = (id = targetID) =>
    void work(async () => {
      setTarget(undefined);
      const value = await request<ReviewTarget>(
        `/api/v1/review-targets/${encodeURIComponent(identifier(id))}`,
      );
      if (value.project_id !== project.project_id || value.target_id !== id)
        throw new Error("审定目标不属于当前项目或标识不符。");
      if (alive.current) {
        setTargetID(id);
        setTarget(value);
        setPublicationVersion(value.version_id);
      }
    });
  const loadTrash = (id = trashID) =>
    void work(async () => {
      setTrash(undefined);
      const value = await request<TrashEntry>(
        `/api/v1/trash/${encodeURIComponent(identifier(id))}?project_id=${encodeURIComponent(project.project_id)}`,
      );
      if (value.project_id !== project.project_id || value.trash_id !== id)
        throw new Error("回收记录不属于当前项目或标识不符。");
      if (alive.current) {
        setTrashID(id);
        setTrash(value);
      }
    });
  const stage = (action: () => void) => {
    setError(undefined);
    try {
      action();
    } catch (e) {
      setError(e);
    }
  };
  const review = () =>
    stage(() => {
      if (!target) throw new Error("先从服务器读取准确审定目标。");
      sensitive({
        mode: "domain",
        title: verdict === "revoke" ? "撤销当前审定" : "记录人审结论",
        kind: "review",
        request: {
          action:
            verdict === "revoke"
              ? "ledger.revoke_review"
              : "ledger.record_review",
          target_id: target.target_id,
          expected_revision: revision(target.target_revision),
          verdict,
          reason: reason(note),
          waivers: [],
          ...(verdict === "revoke"
            ? { effective_review_id: identifier(effectiveReviewID) }
            : {}),
        },
      });
    });
  const submitReview = (e: FormEvent) => {
    e.preventDefault();
    stage(() => {
      if (
        !/^lantai:\/\/[0-7][0-9A-HJKMNP-TV-Z]{25}\/assets\/[0-7][0-9A-HJKMNP-TV-Z]{25}\/versions\/[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(
          profileRef,
        )
      )
        throw new Error("验收配置必须是完整固定版本 lantai:// 引用。");
      const flow = initialProfile
        ? {}
        : {
            task_id: identifier(taskID),
            attempt_id: identifier(attemptID),
            round: revision(round),
            fence: revision(fence),
            ...(candidateGroup
              ? { candidate_group: identifier(candidateGroup) }
              : {}),
          };
      direct("提交精确版本供人审", "/api/v1/review-targets", {
        initial_profile: initialProfile,
        project_id: project.project_id,
        version_id: identifier(submitVersion),
        expected_revision: revision(submitRevision),
        profile_ref: profileRef,
        evidence_ids: csvIDs(evidence),
        flow,
      });
    });
  };
  const previewTrash = (e: FormEvent) => {
    e.preventDefault();
    void work(async () => {
      setPreview(undefined);
      const selector = {
        project_id: project.project_id,
        asset_id: identifier(assetID),
        whole_asset: whole,
        ...(whole ? {} : { version_id: identifier(versionID) }),
        reason: reason(note),
      };
      const value = await request<CommandBody>("/api/v1/trash/preview", {
        method: "POST",
        body: JSON.stringify(selector),
      });
      if (alive.current) setPreview(value);
    });
  };
  const humanTrash = () =>
    stage(() => {
      if (!preview) throw new Error("先预览当前精确回收集合。");
      sensitive({
        mode: "domain",
        title: "确认回收固定集合",
        kind: "trash",
        request: {
          project_id: project.project_id,
          asset_id: preview.asset_id,
          whole_asset: preview.whole_asset,
          ...(preview.version_id ? { version_id: preview.version_id } : {}),
          reason: preview.reason,
        },
      });
    });
  const mutateTrash = (action: string) =>
    stage(() => {
      if (!trash) throw new Error("先读取准确回收记录。");
      sensitive({
        mode: "domain",
        title:
          action === "ledger.hold"
            ? "设置保留 Hold"
            : action === "ledger.unhold"
              ? "解除 Hold"
              : "到期永久清除",
        kind: "trash_mutation",
        request: {
          action,
          project_id: project.project_id,
          trash_id: trash.trash_id,
          expected_revision: revision(trash.revision),
          files_digest: trash.files_digest,
          reason: reason(note),
        },
      });
    });
  const readEvents = () =>
    void work(async () => {
      const q = new URLSearchParams({
        project_id: project.project_id,
        after: String(after),
        limit: "100",
        wait_seconds: "0",
      });
      try {
        const page = await request<{
          events: EventItem[];
          last_seq: number;
          replace_required: boolean;
        }>(`/api/v1/events?${q}`);
        if (
          !Array.isArray(page.events) ||
          !Number.isSafeInteger(page.last_seq) ||
          page.last_seq < after
        )
          throw new Error("审计事件序列不合法。");
        if (page.replace_required) {
          setAudit([]);
          setAuditLoaded(false);
          const snapshot = await currentObjects(project.project_id, request);
          if (alive.current) {
            setObjects(snapshot.objects);
            setAfter(snapshot.replayAfter);
          }
          throw new Error(
            "授权范围或对象已变化，旧事件展示已清除；当前范围已重读，请继续读取新水位之后的事件。",
          );
        }
        let previous = after;
        for (const item of page.events) {
          if (
            item.object.ref.project_id !== project.project_id ||
            !Number.isSafeInteger(item.sequence) ||
            item.sequence <= previous ||
            item.sequence > page.last_seq
          )
            throw new Error("事件混入其他范围或顺序错误，已清除展示。");
          previous = item.sequence;
        }
        if (alive.current) {
          setAudit((old) => [...old, ...page.events].slice(-500));
          setAfter(page.last_seq);
          setAuditLoaded(true);
        }
      } catch (e) {
        if (alive.current) setAudit([]);
        throw e;
      }
    });
  const showObject = (object: CurrentObject) => {
    if (object.ref.kind === "review") loadTarget(object.ref.id);
    if (object.ref.kind === "trash") loadTrash(object.ref.id);
  };
  if (view === "audit")
    return (
      <section className="panel system-section">
        <h2>项目审计与当前授权事件</h2>
        <p>
          只读取当前项目和身份仍可见的事件。服务端要求范围替换时清除旧展示并重新同步；不显示全馆或隐藏对象数量。页面最多保留最近读取的
          500 条。
        </p>
        {error !== undefined && <SystemError error={error} />}
        <div className="system-actions">
          <button className="primary" onClick={readEvents} disabled={loading}>
            {loading ? "读取中…" : "读取下一页事件"}
          </button>
          <button
            className="secondary"
            disabled={loading}
            onClick={() => {
              setAudit([]);
              setAfter(0);
              setAuditLoaded(false);
            }}
          >
            从头读取当前范围
          </button>
        </div>
        <p role="status">
          已核对水位：{after}；当前展示 {audit.length} 条已授权事件。
        </p>
        {auditLoaded && !audit.length && <p>本次读取没有可见事件。</p>}
        <div className="system-table-scroll">
          <table>
            <thead>
              <tr>
                <th>序列／类型</th>
                <th>当前对象</th>
                <th>当前状态</th>
                <th>修订</th>
              </tr>
            </thead>
            <tbody>
              {audit.map((event) => (
                <tr key={event.event_id}>
                  <td>
                    {event.sequence}
                    <br />
                    {event.type}
                  </td>
                  <td>
                    {event.object.title}
                    <br />
                    <code>
                      {event.object.ref.kind}:{event.object.ref.id}
                    </code>
                  </td>
                  <td>{event.object.state}</td>
                  <td>{event.object.revision}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    );
  return (
    <div className="system-stack">
      <section className="panel system-section">
        <h2>协作待办与可见对象</h2>
        <p>
          这是当前领域状态，不把导入标签或历史“已通过”转换为人审、发布或回收事实。
        </p>
        <button
          className="secondary"
          disabled={loading}
          onClick={() => void readCurrent()}
        >
          {loading ? "正在读取完整授权范围…" : "刷新当前待办与对象"}
        </button>
        {error !== undefined && <SystemError error={error} />}
        {loaded && (
          <p role="status">
            当前可见待办 {inbox?.items.length || 0} 项；完整对象读取已完成。
          </p>
        )}
        {inbox && (
          <>
            <ul className="system-object-list">
              {inbox.items.map((item) => (
                <li key={`${item.object.ref.kind}:${item.object.ref.id}`}>
                  <strong>{item.object.title}</strong>
                  <span>
                    {item.object.ref.kind} · {item.object.state} ·{" "}
                    {item.read ? "已读" : "未读"}
                  </span>
                  {["review", "trash"].includes(item.object.ref.kind) && (
                    <button
                      className="secondary"
                      disabled={loading}
                      onClick={() => showObject(item.object)}
                    >
                      读取精确对象
                    </button>
                  )}
                </li>
              ))}
            </ul>
            <button
              className="secondary"
              disabled={disabled || inbox.through <= inbox.read_through}
              onClick={() =>
                direct("标记当前已读取待办", "/api/v1/inbox/read", {
                  project_id: project.project_id,
                  through: inbox.through,
                })
              }
            >
              将当前读取水位标为已读
            </button>
          </>
        )}
        <details>
          <summary>当前可见审定与回收对象</summary>
          <ul className="system-object-list">
            {objects
              .filter((o) => ["review", "trash"].includes(o.ref.kind))
              .map((o) => (
                <li key={`${o.ref.kind}:${o.ref.id}`}>
                  <strong>{o.title}</strong>
                  <span>
                    {o.ref.kind} · {o.state} · 修订 {o.revision}
                  </span>
                  <code>{o.ref.id}</code>
                  <button
                    className="secondary"
                    disabled={loading}
                    onClick={() => showObject(o)}
                  >
                    读取精确对象
                  </button>
                </li>
              ))}
          </ul>
        </details>
      </section>
      <section className="panel system-section">
        <h2>审定与发布</h2>
        <div className="system-inline">
          <BoundID
            label="审定目标标识"
            disabled={loading}
            value={targetID}
            onChange={(v) => {
              setTargetID(v);
              setTarget(undefined);
            }}
          />
          <button
            className="secondary"
            disabled={loading || !targetID}
            onClick={() => loadTarget()}
          >
            读取当前目标
          </button>
        </div>
        {target && (
          <div className="system-subsection">
            <dl>
              <dt>目标修订</dt>
              <dd>{target.target_revision}</dd>
              <dt>固定版本</dt>
              <dd>
                <code>{target.version_id}</code>
              </dd>
              <dt>制作者</dt>
              <dd>
                <code>{target.maker_id}</code>
              </dd>
              <dt>清单摘要</dt>
              <dd>
                <code>{target.manifest_digest}</code>
              </dd>
            </dl>
            <details>
              <summary>完整验收目标、配置与证据</summary>
              <pre>{JSON.stringify(target, null, 2)}</pre>
            </details>
            <label>
              审定结论
              <select
                value={verdict}
                onChange={(e) => setVerdict(e.target.value)}
              >
                <option value="approve">批准</option>
                <option value="return">退回</option>
                <option value="reject">驳回</option>
                <option value="revoke">撤销当前审定</option>
              </select>
            </label>
            {verdict === "revoke" && (
              <BoundID
                label="当前生效审定标识"
                value={effectiveReviewID}
                onChange={setEffectiveReviewID}
              />
            )}
            <label>
              操作理由
              <textarea
                value={note}
                onChange={(e) => setNote(e.target.value)}
                maxLength={4096}
              />
            </label>
            <button
              className="primary"
              disabled={disabled || !note.trim()}
              onClick={review}
            >
              核对并请求人审授权
            </button>
            <p className="subtle">
              本页面不创建豁免。缺少必需检查或独立审查时由核心拒绝，不以审批文字或标签绕过。
            </p>
          </div>
        )}
        <details>
          <summary>提交一个固定版本供人审</summary>
          <form onSubmit={submitReview} className="system-form">
            <BoundID
              label="待审固定版本标识"
              value={submitVersion}
              onChange={setSubmitVersion}
            />
            <label>
              版本控制预期修订
              <input
                type="number"
                min="1"
                value={submitRevision}
                onChange={(e) => setSubmitRevision(e.target.value)}
              />
            </label>
            <label>
              验收配置固定版本 URI
              <input
                value={profileRef}
                onChange={(e) => setProfileRef(e.target.value.trim())}
                placeholder="lantai://…/assets/…/versions/…"
                spellCheck={false}
              />
            </label>
            <label>
              已接受检查证据标识（逗号或换行分隔）
              <textarea
                value={evidence}
                onChange={(e) => setEvidence(e.target.value)}
              />
            </label>
            <label className="system-check">
              <input
                type="checkbox"
                checked={initialProfile}
                onChange={(e) => setInitialProfile(e.target.checked)}
              />
              这是由核心约束的首次验收配置初始化
            </label>
            {!initialProfile && (
              <>
                <BoundID label="任务标识" value={taskID} onChange={setTaskID} />
                <BoundID
                  label="当前 Attempt 标识"
                  value={attemptID}
                  onChange={setAttemptID}
                />
                <label>
                  当前轮次
                  <input
                    type="number"
                    min="1"
                    value={round}
                    onChange={(e) => setRound(e.target.value)}
                  />
                </label>
                <label>
                  当前租约 fence
                  <input
                    type="number"
                    min="1"
                    value={fence}
                    onChange={(e) => setFence(e.target.value)}
                  />
                </label>
                <BoundID
                  label="候选组标识（可选）"
                  value={candidateGroup}
                  onChange={setCandidateGroup}
                />
              </>
            )}
            <button className="secondary" disabled={disabled}>
              核对并提交目标
            </button>
          </form>
        </details>
        <details>
          <summary>明确发布或回退发布指针</summary>
          <form
            className="system-form"
            onSubmit={(e) => {
              e.preventDefault();
              stage(() =>
                direct("变更发布指针", "/api/v1/publications", {
                  project_id: project.project_id,
                  version_id: identifier(publicationVersion),
                  review_id: identifier(reviewID),
                  expected_publication_revision: revision(
                    publicationRevision,
                    0,
                  ),
                  action: publicationAction,
                  reason: reason(note),
                }),
              );
            }}
          >
            <BoundID
              label="发布固定版本标识"
              value={publicationVersion}
              onChange={setPublicationVersion}
            />
            <BoundID
              label="有效审定标识"
              value={reviewID}
              onChange={setReviewID}
            />
            <label>
              预期发布修订
              <input
                type="number"
                min="0"
                value={publicationRevision}
                onChange={(e) => setPublicationRevision(e.target.value)}
              />
            </label>
            <label>
              动作
              <select
                value={publicationAction}
                onChange={(e) => setPublicationAction(e.target.value)}
              >
                <option value="publish">发布</option>
                <option value="rollback">回退到已明确审定的固定版本</option>
              </select>
            </label>
            <label>
              理由
              <textarea
                value={note}
                onChange={(e) => setNote(e.target.value)}
                maxLength={4096}
              />
            </label>
            <button className="secondary" disabled={disabled}>
              核对发布请求
            </button>
            <p className="subtle">
              发布由当前项目角色、有效审定、权利和精确修订共同裁决；不自动发布新版本。
            </p>
          </form>
        </details>
      </section>
      <section className="panel system-section">
        <h2>回收站与保留</h2>
        <p>
          回收只影响 Lantai 领域对象；不会删除电脑或 NAS
          上的素材源件。永久清除有独立授权与保留门禁。
        </p>
        <div className="system-inline">
          <BoundID
            label="回收记录标识"
            disabled={loading}
            value={trashID}
            onChange={(v) => {
              setTrashID(v);
              setTrash(undefined);
            }}
          />
          <button
            className="secondary"
            disabled={loading || !trashID}
            onClick={() => loadTrash()}
          >
            读取当前回收记录
          </button>
        </div>
        {trash && (
          <div className="system-subsection">
            <dl>
              <dt>原路径</dt>
              <dd>{trash.original_slug}</dd>
              <dt>状态／修订</dt>
              <dd>
                {trash.state} / {trash.revision}
              </dd>
              <dt>Hold</dt>
              <dd>{trash.hold ? "已保留" : "未设置"}</dd>
              <dt>保留到期</dt>
              <dd>{trash.due_at}</dd>
              <dt>文件集合摘要</dt>
              <dd>
                <code>{trash.files_digest}</code>
              </dd>
            </dl>
            <label>
              操作理由
              <textarea
                value={note}
                onChange={(e) => setNote(e.target.value)}
                maxLength={4096}
              />
            </label>
            <label>
              恢复到的新路径名（留空使用原路径，不自动改名）
              <input
                value={newSlug}
                onChange={(e) => setNewSlug(e.target.value)}
              />
            </label>
            <div className="system-actions">
              <button
                className="primary"
                disabled={disabled || !note.trim()}
                onClick={() =>
                  stage(() =>
                    direct("恢复回收对象", "/api/v1/trash/restore", {
                      project_id: project.project_id,
                      trash_id: trash.trash_id,
                      expected_revision: revision(trash.revision),
                      ...(newSlug ? { new_slug: newSlug } : {}),
                      reason: reason(note),
                    }),
                  )
                }
              >
                核对恢复请求
              </button>
              <button
                className="secondary"
                disabled={disabled || !note.trim()}
                onClick={() =>
                  mutateTrash(trash.hold ? "ledger.unhold" : "ledger.hold")
                }
              >
                {trash.hold ? "核对解除 Hold" : "核对设置 Hold"}
              </button>
              <button
                className="secondary"
                disabled={disabled || trash.hold || !note.trim()}
                onClick={() => mutateTrash("ledger.purge")}
              >
                核对到期永久清除
              </button>
            </div>
          </div>
        )}
        <details>
          <summary>预览并回收精确资产／版本</summary>
          <form className="system-form" onSubmit={previewTrash}>
            <BoundID
              label="资产标识"
              value={assetID}
              onChange={(v) => {
                setAssetID(v);
                setPreview(undefined);
              }}
            />
            <label className="system-check">
              <input
                type="checkbox"
                checked={whole}
                onChange={(e) => {
                  setWhole(e.target.checked);
                  setPreview(undefined);
                }}
              />
              回收整个资产的明确集合
            </label>
            {!whole && (
              <BoundID
                label="固定版本标识"
                value={versionID}
                onChange={(v) => {
                  setVersionID(v);
                  setPreview(undefined);
                }}
              />
            )}
            <label>
              回收理由
              <textarea
                value={note}
                onChange={(e) => {
                  setNote(e.target.value);
                  setPreview(undefined);
                }}
                maxLength={4096}
              />
            </label>
            <button className="secondary" disabled={loading || disabled}>
              只读预览集合
            </button>
          </form>
          {preview && (
            <>
              <Receipt value={preview} />
              <div className="system-actions">
                <button
                  className="secondary"
                  disabled={disabled}
                  onClick={humanTrash}
                >
                  请求人审回收
                </button>
                <button
                  className="secondary"
                  disabled={disabled}
                  onClick={() =>
                    direct(
                      "按本人回收策略执行原预览集合",
                      "/api/v1/trash/own",
                      preview,
                    )
                  }
                >
                  按本人回收策略提交
                </button>
              </div>
              <p className="subtle">
                最终接受时复核固定集合、版本修订、用途引用和配额。失败不代表源文件已移动，不自动强制回收。
              </p>
            </>
          )}
        </details>
      </section>
    </div>
  );
}
