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
  type Version,
} from "./api";
import {
  emptyRightsDraft,
  getVersionRights,
  rightsAssertion,
  prepareRightsApproval,
  rightsEvidence,
  rightsPurposes,
  type RightsDraft,
  type RightsSnapshot,
} from "./asset-rights";
import {
  type Approval,
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

export function AssetRights({
  version,
  projectID,
  who,
  expire,
}: {
  version: Version;
  projectID: string;
  who?: Identity;
  expire: (error: unknown) => void;
}) {
  const [snapshot, setSnapshot] = useState<RightsSnapshot>();
  const [draft, setDraft] = useState(emptyRightsDraft);
  const [note, setNote] = useState("");
  const [unknownURLs, setUnknownURLs] = useState("");
  const [error, setError] = useState<unknown>();
  const [busy, setBusy] = useState(false);
  const [receipt, setReceipt] = useState<SensitiveResult>();
  const [direct, setDirect] = useState<DirectIntent>();
  const [approval, setApproval] = useState<Approval>();
  const controller = useRef<AbortController | null>(null);
  const lock = useRef(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      controller.current?.abort();
    };
  }, []);
  const request: Transport = useCallback(
    async <T,>(path: string, options?: RequestInit) => {
      try {
        return await api<T>(path, options);
      } catch (error) {
        if (isSessionError(error)) expire(error);
        throw error;
      }
    },
    [expire],
  );
  const roles = who?.project_roles?.[projectID] || [];
  const organize = !!who?.session.scopes.includes("organize") && hasCSRF();
  const evidenceAllowed =
    organize && roles.some((r) => ["owner", "curator"].includes(r));
  const restrictAllowed =
    organize && roles.some((r) => ["owner", "checker"].includes(r));
  const indexAllowed =
    organize && roles.some((r) => ["owner", "checker", "curator"].includes(r));
  const humanAllowed =
    hasCSRF() &&
    roles.includes("owner") &&
    !!who?.session.scopes.includes("admin") &&
    who.principal.kind === "human" &&
    who.session.channel === "browser" &&
    who.session.session_kind === "normal" &&
    !who.session.delegated_by;
  const blocked = busy || !!direct || !!approval;
  const read = async () => {
    if (lock.current) return;
    lock.current = true;
    setBusy(true);
    setError(undefined);
    const next = new AbortController();
    controller.current?.abort();
    controller.current = next;
    try {
      const value = await getVersionRights(version, next.signal);
      if (!next.signal.aborted) setSnapshot(value);
    } catch (error) {
      if (!next.signal.aborted) {
        setSnapshot(undefined);
        setError(error);
        if (isSessionError(error)) expire(error);
      }
    } finally {
      lock.current = false;
      if (!next.signal.aborted) setBusy(false);
    }
  };
  const change = <K extends keyof RightsDraft>(key: K, value: RightsDraft[K]) =>
    setDraft((old) => ({ ...old, [key]: value }));
  const stageEvidence = (event: FormEvent) => {
    event.preventDefault();
    if (!snapshot || !evidenceAllowed || blocked) return;
    try {
      setError(undefined);
      setDirect({
        title: "追加此固定版本的来源证据",
        path: "/api/v1/rights/evidence",
        body: rightsEvidence(snapshot, note, unknownURLs),
        key: crypto.randomUUID(),
        attempted: false,
      });
    } catch (error) {
      setError(error);
    }
  };
  const stageAssertion = async (event: FormEvent) => {
    event.preventDefault();
    if (!snapshot || blocked || lock.current) return;
    try {
      const body = rightsAssertion(snapshot, draft);
      setError(undefined);
      if (draft.kind === "correct" || draft.kind === "release") {
        if (!humanAllowed)
          throw new Error(
            "更正／解除需要普通人类项目 owner、明确 admin 范围及新动态码；不会自动扩权。",
          );
        lock.current = true;
        setBusy(true);
        const next = await prepareRightsApproval(
          snapshot,
          draft,
          projectID,
          request,
        );
        if (alive.current) setApproval(next);
      } else {
        if (!(draft.kind === "evidence" ? indexAllowed : restrictAllowed))
          throw new Error("当前角色或 organize 范围不允许此权利断言。");
        setDirect({
          title: "提交单调限制／证据索引断言",
          path: "/api/v1/rights/assertions",
          body,
          key: crypto.randomUUID(),
          attempted: false,
        });
      }
    } catch (error) {
      if (alive.current) setError(error);
    } finally {
      lock.current = false;
      if (alive.current) setBusy(false);
    }
  };
  const complete = (value: SensitiveResult) => {
    setReceipt(value);
    setDirect(undefined);
    setApproval(undefined);
    setSnapshot(undefined);
    setDraft(emptyRightsDraft());
  };
  return (
    <details className="asset-admin-section">
      <summary>固定版本用途与 rights 管理</summary>
      <p className="notice">
        这是当前核心权利事实，与当前著录和冻结 manifest
        分开。普通画像、标签或业务状态不授予用途。读取结果是本次观察；实际取件仍逐次申请绑定用途的
        ReadGrant。
      </p>
      <button
        className="secondary"
        disabled={blocked}
        onClick={() => void read()}
      >
        {busy ? "正在核验…" : "读取当前权利与用途"}
      </button>
      {error !== undefined && <SystemError error={error} />}
      {receipt && <Receipt value={receipt} />}
      {snapshot && (
        <>
          <p>
            断言修订 r{snapshot.revision} · 来源证据修订 r
            {snapshot.evidence_revision} ·{" "}
            <code>{snapshot.manifest_digest}</code>
          </p>
          <dl>
            <dt>用途声明</dt>
            <dd>{snapshot.rights.usage}</dd>
            <dt>许可表达式</dt>
            <dd>{snapshot.rights.license}</dd>
            <dt>敏感级别</dt>
            <dd>{snapshot.rights.sensitivity}</dd>
            <dt>原件再分发</dt>
            <dd>{String(snapshot.rights.redistribute_raw)}</dd>
            <dt>禁止AI</dt>
            <dd>{String(snapshot.rights.noai)}</dd>
            <dt>受限来源</dt>
            <dd>{String(snapshot.restricted_source)}</dd>
            <dt>显式禁止用途</dt>
            <dd>{snapshot.deny_purposes.join("、") || "无"}</dd>
          </dl>
          <table>
            <thead>
              <tr>
                <th>请求用途</th>
                <th>当前判定</th>
                <th>权利水位</th>
              </tr>
            </thead>
            <tbody>
              {snapshot.purposes.map((p) => (
                <tr key={p.purpose}>
                  <td>{p.purpose}</td>
                  <td>{p.allowed ? "允许" : p.code || "拒绝"}</td>
                  <td>{p.rights_epoch}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <details>
            <summary>已接受来源证据（只列当前版本）</summary>
            {snapshot.evidence.length ? (
              <ul>
                {snapshot.evidence.map((e) => (
                  <li key={e.record_id}>
                    <code>{e.record_id}</code> · r{e.revision}
                  </li>
                ))}
              </ul>
            ) : (
              <p>尚无已接受来源证据。孤立文件不算证据。</p>
            )}
          </details>
          <form className="asset-editor" onSubmit={stageEvidence}>
            <h3>追加来源证据</h3>
            <label>
              证据说明
              <textarea
                required
                maxLength={4000}
                value={note}
                disabled={blocked || !evidenceAllowed}
                onChange={(e) => setNote(e.target.value)}
              />
            </label>
            <label>
              仍未知的外部来源文字／URL（每行一项，可留空）
              <textarea
                value={unknownURLs}
                disabled={blocked || !evidenceAllowed}
                onChange={(e) => setUnknownURLs(e.target.value)}
              />
            </label>
            <p className="subtle">
              只记录证据，不访问 URL、不确认未知来源许可、不解除现有限制。需要
              owner／curator 与 organize。
            </p>
            <button
              className="secondary"
              disabled={blocked || !evidenceAllowed}
            >
              核对追加证据
            </button>
          </form>
          <form
            className="asset-editor"
            onSubmit={(event) => void stageAssertion(event)}
          >
            <h3>权利断言</h3>
            <fieldset disabled={blocked} className="asset-editor-grid">
              <label>
                操作
                <select
                  value={draft.kind}
                  onChange={(e) =>
                    change("kind", e.target.value as RightsDraft["kind"])
                  }
                >
                  <option value="restrict">restrict · 只增加限制</option>
                  <option value="evidence">evidence · 仅索引证据</option>
                  <option value="correct">correct · 人审更正</option>
                  <option value="release">release · 人审解除</option>
                </select>
              </label>
              <label>
                用途声明
                <select
                  value={draft.usage}
                  onChange={(e) => change("usage", e.target.value)}
                >
                  <option value="">保持</option>
                  {["production", "reference", "restricted"].map((v) => (
                    <option key={v}>{v}</option>
                  ))}
                </select>
              </label>
              <label>
                许可表达式（留空保持）
                <input
                  maxLength={256}
                  value={draft.license_expression}
                  onChange={(e) => change("license_expression", e.target.value)}
                />
              </label>
              <label>
                敏感级别
                <select
                  value={draft.sensitivity}
                  onChange={(e) => change("sensitivity", e.target.value)}
                >
                  <option value="">保持</option>
                  <option>normal</option>
                  <option>personal</option>
                </select>
              </label>
              {(["redistribute_raw", "noai", "restricted_source"] as const).map(
                (key) => (
                  <label key={key}>
                    {key}
                    <select
                      value={draft[key]}
                      onChange={(e) => change(key, e.target.value)}
                    >
                      <option value="">保持</option>
                      <option value="true">true</option>
                      <option value="false">false</option>
                    </select>
                  </label>
                ),
              )}
              <label>
                <input
                  type="checkbox"
                  checked={draft.replace_deny}
                  onChange={(e) => change("replace_deny", e.target.checked)}
                />
                显式替换禁止用途集合
              </label>
              {draft.replace_deny &&
                rightsPurposes.map((p) => (
                  <label key={p}>
                    <input
                      type="checkbox"
                      checked={draft.deny_purposes.includes(p)}
                      onChange={(e) =>
                        change(
                          "deny_purposes",
                          e.target.checked
                            ? [...draft.deny_purposes, p]
                            : draft.deny_purposes.filter((v) => v !== p),
                        )
                      }
                    />
                    {p}
                  </label>
                ))}
              <label>
                本次依据的已接受证据 ID（必填）
                <textarea
                  required
                  value={draft.evidence_ids}
                  onChange={(e) => change("evidence_ids", e.target.value)}
                />
              </label>
              <label>
                明确确认的旧未知证据 ID（留空保持）
                <textarea
                  value={draft.confirmed_evidence_ids}
                  onChange={(e) =>
                    change("confirmed_evidence_ids", e.target.value)
                  }
                />
              </label>
              <label>
                完整来源关系更正（可选 JSON 数组；留空保持、[] 清空）
                <textarea
                  maxLength={65536}
                  value={draft.uses}
                  onChange={(e) => change("uses", e.target.value)}
                  placeholder='[{"instance_id":"完整ID","asset_id":"完整ID","version_id":"完整ID","relation":"derived_from"}]'
                />
              </label>
              <label>
                理由
                <textarea
                  required
                  maxLength={4096}
                  value={draft.reason}
                  onChange={(e) => change("reason", e.target.value)}
                />
              </label>
            </fieldset>
            <p className="subtle">
              restrict 只允许单调收紧；evidence
              不含限制字段。任何放宽、更正来源关系或解除都需要新证据和人类 owner
              的明确 admin
              范围、服务器挑战与新动态码。普通元数据不改变上述判定，旧断言和冻结字节均保留。
            </p>
            <button
              className="secondary"
              disabled={
                blocked ||
                (draft.kind === "correct" || draft.kind === "release"
                  ? !humanAllowed
                  : draft.kind === "evidence"
                    ? !indexAllowed
                    : !restrictAllowed)
              }
            >
              核对并准备此固定版本操作
            </button>
          </form>
        </>
      )}
      {direct && (
        <DirectConfirmation
          intent={direct}
          request={request}
          retain={setDirect}
          complete={complete}
          dismiss={() => setDirect(undefined)}
        />
      )}
      {approval && (
        <SensitiveConfirmation
          approval={approval}
          request={request}
          retain={setApproval}
          complete={complete}
          dismiss={() => setApproval(undefined)}
        />
      )}
    </details>
  );
}
