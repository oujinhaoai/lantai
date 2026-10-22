import { useCallback, useState } from "react";
import {
  DeviceJobConsole,
  type DeviceJobInput,
  type DeviceResultContext,
} from "./device-management";
import { parseManagedJSON } from "./managed-transfer";
import { bindReferenceResult, referenceJob } from "./batch-acceptance-model";
import type { Transport } from "./system-operations";

const object = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
export function ReferenceJobs({
  request,
  projectID,
  batchID,
  onEvidence,
  onPendingChange,
}: {
  request: Transport;
  projectID: string;
  batchID: string;
  onPendingChange?: (pending: boolean) => void;
  onEvidence: (value: {
    job_id: string;
    action: "references.plan" | "references.copy" | "references.rollback";
  }) => void;
}) {
  const [inputText, setInputText] = useState(
    '{\n  "schema": "lantai.reference-input/v1",\n  "notes": [],\n  "mappings": [],\n  "preserve": []\n}',
  );
  const [source, setSource] = useState("notes-source");
  const [copyRoot, setCopyRoot] = useState(`copies/${batchID}`);
  const [rollbackRoot, setRollbackRoot] = useState(`rollback/${batchID}`);
  const [postRefs, setPostRefs] = useState("[]");
  const [localReviewed, setLocalReviewed] = useState(false);
  const [plan, setPlan] = useState<Record<string, unknown>>();
  const [copyReceipt, setCopyReceipt] = useState<Record<string, unknown>>();
  const [job, setJob] = useState<DeviceJobInput>();
  const [error, setError] = useState<unknown>();
  const [result, setResult] = useState<unknown>();
  const [pending, setPending] = useState(false);
  const [context, setContext] = useState<DeviceResultContext>();
  const updatePending = useCallback(
    (value: boolean) => {
      setPending(value);
      onPendingChange?.(value);
    },
    [onPendingChange],
  );
  const stage = (
    action: "references.plan" | "references.copy" | "references.rollback",
  ) => {
    if (pending) return;
    try {
      let input: unknown;
      if (action === "references.copy") setCopyReceipt(undefined);
      if (action === "references.plan") {
        input = parseManagedJSON(inputText);
        if (
          !object(input) ||
          input.schema !== "lantai.reference-input/v1" ||
          !Array.isArray(input.notes) ||
          !input.notes.length ||
          !Array.isArray(input.mappings) ||
          !input.mappings.length
        )
          throw new Error(
            "先显式列出选中笔记和精确 old_target → 固定引用映射。",
          );
      } else {
        if (
          !plan ||
          (plan.schema === "lantai.local-assistant-result-ref/v1"
            ? !localReviewed
            : plan.complete !== true ||
              !Array.isArray(plan.unresolved) ||
              !!plan.unresolved.length)
        )
          throw new Error(
            "先由设备运行 plan，并解决显式映射的全部 unresolved 项。",
          );
        if (
          action === "references.rollback" &&
          (!copyReceipt ||
            (copyReceipt.schema !== "lantai.local-assistant-result-ref/v1" &&
              copyReceipt.complete !== true))
        )
          throw new Error("回退必须关联本页已完成的原 copy 回执与原计划。");
        input = plan;
      }
      const refs =
        action === "references.rollback"
          ? parseManagedJSON(postRefs)
          : undefined;
      if (
        refs !== undefined &&
        (!Array.isArray(refs) || refs.some((ref) => typeof ref !== "string"))
      )
        throw new Error(
          "切换后新增成果库存必须是固定 URI 字符串数组；[] 必须明确填写。",
        );
      const next = referenceJob({
        action,
        input,
        batchID,
        directory: action === "references.rollback" ? copyRoot : source,
        destination: action === "references.rollback" ? rollbackRoot : copyRoot,
        postCutoverRefs: refs as string[] | undefined,
      });
      setJob(next as DeviceJobInput);
      setError(undefined);
      setResult(undefined);
      setContext(undefined);
      if (action === "references.plan") {
        setPlan(undefined);
        setCopyReceipt(undefined);
      }
    } catch (cause) {
      setError(cause);
    }
  };
  const importResult = useCallback(
    async (value: unknown, current: DeviceResultContext) => {
      try {
        if (!job) throw new Error("缺少当前固定作业。");
        onEvidence({
          job_id: current.job_id,
          action: job.action as
            "references.plan" | "references.copy" | "references.rollback",
        });
        const bound = await bindReferenceResult({
          job: job as unknown as Record<string, unknown>,
          envelope: value,
          context: current,
        });
        setResult(value);
        setContext(current);
        setError(undefined);
        const { completed, result: resultValue, kind } = bound;
        if (job.action === "references.plan") {
          setPlan(undefined);
          setLocalReviewed(false);
          setCopyReceipt(undefined);
          if (
            completed &&
            (kind === "local_ref" ||
              (resultValue.schema === "lantai.reference-plan/v1" &&
                resultValue.complete === true &&
                Array.isArray(resultValue.unresolved) &&
                resultValue.unresolved.length === 0))
          )
            setPlan(resultValue);
        } else {
          if (
            kind !== "local_ref" &&
            (resultValue.schema !== "lantai.reference-receipt/v1" ||
              resultValue.plan_digest !== plan?.digest ||
              resultValue.mode !==
                (job.action === "references.copy" ? "copy" : "rollback"))
          )
            throw new Error("原计划与副本／回退回执身份不匹配。");
          if (job.action === "references.copy")
            setCopyReceipt(
              completed &&
                (kind === "local_ref" || resultValue.complete === true)
                ? resultValue
                : undefined,
            );
        }
      } catch (cause) {
        setError(cause);
      }
    },
    [job, onEvidence, plan],
  );
  const adoptOriginal = useCallback((original: DeviceJobInput) => {
    if (
      !["references.plan", "references.copy", "references.rollback"].includes(
        original.action,
      )
    )
      throw new Error("此面板只续办所选笔记的引用作业。");
    if (original.action !== "references.plan" && !object(original.input))
      throw new Error(
        "原作业使用input_file且缺少网页可核对的完整计划；请保留原状态，通过设备管理原作业控制台继续，不在本面板猜测接受证据。",
      );
    setJob(original);
    setResult(undefined);
    setContext(undefined);
    setCopyReceipt(undefined);
    setLocalReviewed(false);
    if (original.action === "references.plan") {
      setInputText(JSON.stringify(original.input, null, 2));
      setSource(original.directory);
      setPlan(undefined);
    } else {
      if (!object(original.input))
        throw new Error("原引用作业缺少完整计划，不能猜测补齐。");
      setPlan(original.input);
      if (original.action === "references.copy") {
        setSource(original.directory);
        setCopyRoot(original.destination ?? "");
      } else {
        setCopyRoot(original.directory);
        setRollbackRoot(original.destination ?? "");
        setPostRefs(
          JSON.stringify(original.post_cutover_refs ?? null, null, 2),
        );
      }
    }
  }, []);
  const placeholder: DeviceJobInput = {
    schema: "lantai.local-assistant-job/v1",
    job_id: `references-${batchID}-lookup`,
    action: "references.plan",
    input: { schema: "lantai.reference-input/v1", notes: [], mappings: [] },
    directory: "notes-source",
    output: `results/${batchID}-lookup.json`,
  };
  const change = (edit: () => void) => {
    edit();
    setLocalReviewed(false);
    setPlan(undefined);
    setCopyReceipt(undefined);
  };
  return (
    <section className="panel" aria-label="选中笔记改链与回退">
      <h3>选中笔记的改链计划、副本与独立回退</h3>
      <p>
        只处理所选笔记与精确映射；原知识库与素材源保持只读。模型、工具、中间资料路径由既有保守规划器保护。作业通过受权设备实际执行，设备检查后须明确批准原摘要。
      </p>
      <fieldset disabled={pending}>
        <label>
          显式笔记、映射与保护路径 JSON
          <textarea
            rows={8}
            value={inputText}
            onChange={(event) => change(() => setInputText(event.target.value))}
          />
        </label>
        <div className="migration-fields">
          <label>
            工作区内原笔记目录
            <input
              value={source}
              onChange={(event) => change(() => setSource(event.target.value))}
            />
          </label>
          <label>
            独立改链副本目录
            <input
              value={copyRoot}
              onChange={(event) =>
                change(() => setCopyRoot(event.target.value))
              }
            />
          </label>
          <label>
            第三个独立回退目录
            <input
              value={rollbackRoot}
              onChange={(event) => setRollbackRoot(event.target.value)}
            />
          </label>
        </div>
        <label>
          切换后新增成果固定 URI 库存（原顺序与重复均保留）
          <textarea
            rows={3}
            value={postRefs}
            onChange={(event) => setPostRefs(event.target.value)}
          />
        </label>
        <div className="migration-actions">
          <button
            type="button"
            className="secondary"
            onClick={() => stage("references.plan")}
          >
            由设备生成只读改链计划
          </button>
          <button
            type="button"
            className="secondary"
            disabled={
              !plan ||
              (plan.schema === "lantai.local-assistant-result-ref/v1" &&
                !localReviewed)
            }
            onClick={() => stage("references.copy")}
          >
            按原计划创建改链副本
          </button>
          <button
            type="button"
            className="secondary"
            disabled={!plan || !copyReceipt}
            onClick={() => stage("references.rollback")}
          >
            保留新增成果并创建回退副本
          </button>
        </div>
      </fieldset>
      {error !== undefined && (
        <p className="error" role="alert">
          {error instanceof Error
            ? error.message
            : "作业未完成，保留原目录与原状态。"}
        </p>
      )}
      {plan?.schema === "lantai.local-assistant-result-ref/v1" && (
        <div className="notice">
          <p>
            原完整计划保留在设备私有工作区。网页只保存原引用和摘要，不显示或伪造本机计划内容。
          </p>
          <p>
            请在设备本机运行{" "}
            <code>
              lantai assistant result --workspace ROOT --job-id{" "}
              {String(plan.job_id)} --result-sha256 {String(plan.result_sha256)}
            </code>
            ，核对原计划 diff、unresolved、skipped 与原件保留范围。
          </p>
          <label>
            <input
              type="checkbox"
              checked={localReviewed}
              disabled={pending}
              onChange={(event) => setLocalReviewed(event.target.checked)}
            />
            我已在设备本机查看并审阅该原摘要的计划；后续动作仍需批准实际 inspect
            摘要
          </label>
        </div>
      )}
      {plan && plan.schema !== "lantai.local-assistant-result-ref/v1" && (
        <details open>
          <summary>
            原计划 diff / unresolved / skipped（只覆盖本批映射）
          </summary>
          <pre>{JSON.stringify(plan, null, 2)}</pre>
        </details>
      )}
      <DeviceJobConsole
        request={request}
        projectID={projectID}
        job={job ?? placeholder}
        disabled={!job}
        onOriginalJob={adoptOriginal}
        onPendingChange={updatePending}
        onResult={(value, current) => {
          void importResult(value, current);
        }}
      />
      {result !== undefined && (
        <details>
          <summary>本次设备原结果 · {context?.job_id}</summary>
          <pre>{JSON.stringify(result, null, 2)}</pre>
        </details>
      )}
      <p>
        failed、pending、cancelled 与 unknown
        不能作为副本完成。回退输出第三个目录，保留原件、改链副本和所有服务端新版本；不删除源或回滚入馆事实。
      </p>
    </section>
  );
}
