import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ChangeEvent,
} from "react";
import {
  api,
  APIError,
  hasCSRF,
  isSessionError,
  message,
  type Identity,
  type Project,
} from "./api";
import { ManagedRecordSaver } from "./managed-record-saver";
import { BatchAcceptance } from "./batch-acceptance";
import {
  DeviceJobConsole,
  type DeviceJobInput,
  type DeviceResultContext,
} from "./device-management";
import type { Transport } from "./system-operations";
import {
  listManagedRecords,
  loadManagedRecord,
  type ManagedRecordItem,
  type ManagedRecordRef,
} from "./managed-records";
import {
  assertNoTransferSecrets,
  browserBatchLimit,
  browserFileLimit,
  canonicalJSON,
  downloadManagedJSON,
  identifierPattern,
  parseManagedJSON,
  pushFiles,
  sha256,
  validateManagedPath,
  verifyExactFiles,
  type ManagedFile,
  type PushInput,
  type Rights,
  type TransferProgress,
  type VerificationReceipt,
} from "./managed-transfer";
import {
  assertNativeJSONRoundTrip,
  bindImportDeviceResult,
  blankMigrationDraft,
  filesForAsset,
  freezeMigration,
  importMigrationWorkspace,
  makeAssistantJob,
  nativeImportInput,
  preflightMigration,
  validateMigrationDraft,
  type MigrationDraft,
  type MigrationWorkspace,
  type MigrationPreflight,
} from "./migration-model";
import "./migration-center.css";

type Props = { who: Identity; expire: (error: unknown) => void; route: string };
type MigrationRecord = {
  schema: "lantai.migration-record/v1";
  workspace: MigrationWorkspace;
  verifications: Record<string, VerificationReceipt>;
  recovery_refs: ManagedRecordRef[];
};
const roles = [
  "primary",
  "source",
  "texture",
  "interchange",
  "recipe",
  "record",
  "preview",
  "doc",
];
const typeLabels: Record<string, string> = {
  image: "图像",
  video: "视频",
  audio: "音频",
  doc: "文档",
  config: "配置／工作流",
  model: "模型",
  motion: "动作／轨迹",
  scene: "场景工程",
  production: "制作成果",
  plugin: "受控扩展包",
};
const rowLabels: Record<string, string> = {
  not_attempted: "未开始",
  running: "操作进行中",
  committed: "历史版本回执已保存",
  description_pending: "版本已提交，著录待完善",
  failed: "明确拒绝／失败",
  unknown: "结果不明，保留原状态对账",
};
const phaseLabels: Record<string, string> = {
  hashing: "源文件散列",
  uploading: "分片上传",
  uploaded: "字节已上传，尚未提交版本",
  committing: "提交版本",
  committed: "固定回执已接受",
  verifying: "当前远端逐文件核验",
};
const bytes = (value: number) =>
  value < 1048576
    ? `${(value / 1024).toFixed(1)} KiB`
    : `${(value / 1073741824).toFixed(3)} GiB`;
const errorText = (error: unknown) =>
  error instanceof APIError
    ? message(error)
    : error instanceof Error
      ? error.message
      : "操作未完成，请保留原状态。";
const object = (value: unknown): value is Record<string, unknown> =>
  !!value && typeof value === "object" && !Array.isArray(value);
function fileInput(event: ChangeEvent<HTMLInputElement>) {
  const file = event.target.files?.[0];
  event.target.value = "";
  return file;
}
function publicCode(error: unknown) {
  return error instanceof APIError && /^[A-Z][A-Z0-9_]{0,79}$/.test(error.code)
    ? error.code
    : "MIGRATION_INCOMPLETE";
}

export function MigrationCenter({ who, expire, route }: Props) {
  const [draft, setDraft] = useState<MigrationDraft>(() =>
    blankMigrationDraft(),
  );
  const [files, setFiles] = useState<ManagedFile[]>([]);
  const [chosen, setChosen] = useState<string[]>([]);
  const [projects, setProjects] = useState<Project[]>([]);
  const [projectsCursor, setProjectsCursor] = useState<string>();
  const [workspace, setWorkspace] = useState<MigrationWorkspace>();
  const [verifications, setVerifications] = useState<
    Record<string, VerificationReceipt>
  >({});
  const [recoveryRefs, setRecoveryRefs] = useState<ManagedRecordRef[]>([]);
  const [recoveryInfo, setRecoveryInfo] = useState<unknown>();
  const [recordItems, setRecordItems] = useState<ManagedRecordItem[]>([]);
  const [recordCursor, setRecordCursor] = useState<string>();
  const [recordAsset, setRecordAsset] = useState("");
  const [recordVersion, setRecordVersion] = useState("");
  const [recoveryAsset, setRecoveryAsset] = useState("");
  const [recoveryVersion, setRecoveryVersion] = useState("");
  const [groupKey, setGroupKey] = useState("asset-1");
  const [groupDirectory, setGroupDirectory] = useState(".");
  const [groupTitle, setGroupTitle] = useState("");
  const [groupSlug, setGroupSlug] = useState("");
  const [groupType, setGroupType] = useState("doc");
  const [classification, setClassification] = useState<
    "final" | "intermediate" | "unknown"
  >("unknown");
  const [appendAsset, setAppendAsset] = useState("");
  const [baseVersion, setBaseVersion] = useState("");
  const [usage, setUsage] = useState<Rights["usage"]>("restricted");
  const [license, setLicense] = useState("");
  const [noai, setNoai] = useState(true);
  const [redistribute, setRedistribute] = useState(false);
  const [sensitivity, setSensitivity] =
    useState<Rights["sensitivity"]>("normal");
  const [reason, setReason] = useState("");
  const [planText, setPlanText] = useState("");
  const [acknowledged, setAcknowledged] = useState(false);
  const [stateSaved, setStateSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState<unknown>();
  const [progress, setProgress] = useState<TransferProgress>();
  const [instanceCapacity, setInstanceCapacity] = useState("");
  const [assistantSource, setAssistantSource] = useState("source");
  const [assistantBatchID, setAssistantBatchID] = useState<string>(() =>
    crypto.randomUUID(),
  );
  const [assistantStatePath, setAssistantStatePath] = useState<string>();
  const [assistantDestination, setAssistantDestination] = useState<string>();
  const [nativePlan, setNativePlan] = useState<unknown>();
  const [nativeReport, setNativeReport] = useState<unknown>();
  const [batchDevicePending, setBatchDevicePending] = useState(false);
  const [assistantDevicePending, setAssistantDevicePending] = useState(false);
  const [assistantActiveJob, setAssistantActiveJob] =
    useState<DeviceJobInput>();
  const [assistantLocalReviewed, setAssistantLocalReviewed] = useState(false);
  const [assistantResult, setAssistantResult] = useState<unknown>();
  const [assistantPlanJob, setAssistantPlanJob] = useState<unknown>();
  const [assistantApplyJob, setAssistantApplyJob] = useState<unknown>();
  const [assistantVerifyJob, setAssistantVerifyJob] = useState<unknown>();
  const [worksetEntrypoints, setWorksetEntrypoints] = useState("");
  const [worksetDependencies, setWorksetDependencies] = useState("[]");
  const controller = useRef<AbortController | null>(null);
  const live = useRef(true);
  const anyDevicePending = assistantDevicePending || batchDevicePending;
  const locked = busy || anyDevicePending || !!workspace;
  const mayWrite = who.session.scopes.includes("ingest") && hasCSRF();
  const mayAdmin =
    who.system_roles?.includes("admin") && who.session.scopes.includes("admin");
  const currentPlan = workspace?.plan ?? draft;
  const inventory = files.map((file) => ({
    path: file.path,
    size: file.file.size,
  }));
  let preflight: MigrationPreflight | undefined;
  let preflightIssue = "";
  try {
    preflight = preflightMigration(
      currentPlan,
      workspace?.inventory ?? inventory,
    );
  } catch (cause) {
    preflightIssue = errorText(cause);
  }
  const incomplete =
    !!workspace &&
    workspace.plan.assets.some(
      (item) =>
        workspace.rows[item.key].status === "unknown" ||
        workspace.rows[item.key].status === "description_pending",
    );

  const assistantRequest: Transport = useCallback(
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
  const fail = (cause: unknown) => {
    if (isSessionError(cause)) expire(cause);
    else setError(cause);
  };
  useEffect(() => {
    live.current = true;
    const c = new AbortController();
    void api<{ items: Project[] | null; next_cursor?: string }>(
      "/api/v1/projects?limit=100&view=full",
      { signal: c.signal },
    )
      .then((page) => {
        if (!c.signal.aborted) {
          setProjects(page.items ?? []);
          setProjectsCursor(page.next_cursor);
        }
      })
      .catch((cause: unknown) => {
        if (!c.signal.aborted) {
          if (isSessionError(cause)) expire(cause);
          else setError(cause);
        }
      });
    return () => {
      live.current = false;
      c.abort();
      controller.current?.abort();
    };
  }, [expire]);
  useEffect(() => {
    const c = controller.current;
    return () => {
      c?.abort();
    };
  }, [route]);
  const execute = async (work: (signal: AbortSignal) => Promise<void>) => {
    if (busy || anyDevicePending) return;
    const c = new AbortController();
    controller.current = c;
    setBusy(true);
    setError(undefined);
    setNotice("");
    try {
      await work(c.signal);
    } catch (cause) {
      if (live.current && !c.signal.aborted) fail(cause);
    } finally {
      if (live.current) {
        setBusy(false);
        setProgress(undefined);
      }
    }
  };
  const resetAssistant = () => {
    setAssistantActiveJob(undefined);
    setAssistantLocalReviewed(false);
    setAssistantBatchID(crypto.randomUUID());
    setNativePlan(undefined);
    setNativeReport(undefined);
    setAssistantResult(undefined);
    setAssistantPlanJob(undefined);
    setAssistantApplyJob(undefined);
    setAssistantVerifyJob(undefined);
    setAssistantStatePath(undefined);
    setAssistantDestination(undefined);
  };
  const setDraftField = <K extends keyof MigrationDraft>(
    key: K,
    value: MigrationDraft[K],
  ) => {
    if (locked) return;
    setDraft((old) => ({ ...old, [key]: value }));
    setAcknowledged(false);
    resetAssistant();
  };
  const chooseFiles = (
    event: ChangeEvent<HTMLInputElement>,
    folder: boolean,
  ) => {
    const chosenFiles = Array.from(event.target.files ?? []);
    event.target.value = "";
    if (!chosenFiles.length) return;
    if (chosenFiles.length > 1000) {
      fail(
        new Error(
          "本页最多审阅 1,000 个明确来源文件；更多项请用原生清单和受控助手，不会截断选取。",
        ),
      );
      return;
    }
    const mapped = chosenFiles.map((file) => ({
      path: folder
        ? file.webkitRelativePath.split("/").slice(1).join("/")
        : file.name,
      role: "source",
      file,
    }));
    if (new Set(mapped.map((file) => file.path)).size !== mapped.length) {
      fail(new Error("所选来源存在重复相对路径，请分开明确来源根。"));
      return;
    }
    if (!workspace) resetAssistant();
    setFiles(mapped);
    setChosen([]);
    setAcknowledged(false);
    setError(undefined);
    setNotice(
      folder
        ? "已选定一个目录中的文件。路径相对该目录；这不是整机或 NAS 全量盘点。"
        : "已选择明确文件，未扫描其余磁盘。请分组或记录排除／工作输入责任。",
    );
  };
  const addGroup = () => {
    if (!chosen.length || locked) return;
    try {
      const selected = files.filter((file) => chosen.includes(file.path));
      const base = groupDirectory === "." ? "" : groupDirectory + "/";
      if (selected.some((file) => base && !file.path.startsWith(base)))
        throw new Error("所选文件必须位于本分组的明确相对目录内。");
      const input: PushInput = {
        project_id: draft.project_id,
        ...(appendAsset
          ? {
              asset_id: appendAsset.trim(),
              base_version_id: baseVersion.trim(),
            }
          : {
              slug: groupSlug.trim(),
              describe: { title: groupTitle.trim() || groupSlug.trim() },
            }),
        content: {
          asset_type: groupType,
          files: selected.map((file, index) => ({
            path: base ? file.path.slice(base.length) : file.path,
            role: file.role === "source" && index === 0 ? "primary" : file.role,
            size: file.file.size,
            sha256: "",
          })),
          ...(!appendAsset
            ? {
                rights: {
                  usage,
                  license: license.trim(),
                  redistribute_raw: redistribute,
                  noai,
                  sensitivity,
                },
              }
            : {}),
        },
      };
      const next = {
        ...draft,
        assets: [
          ...draft.assets,
          {
            key: groupKey.trim(),
            directory: groupDirectory.trim(),
            classification,
            push: input,
          },
        ],
      };
      validateMigrationDraft(next);
      preflightMigration(next, inventory);
      resetAssistant();
      setDraft(next);
      setChosen([]);
      setGroupKey(`asset-${next.assets.length + 1}`);
      setGroupSlug("");
      setGroupTitle("");
      setAppendAsset("");
      setBaseVersion("");
      setError(undefined);
      setAcknowledged(false);
    } catch (cause) {
      fail(cause);
    }
  };
  const disposition = (kind: "excluded" | "working_inputs") => {
    if (locked || !chosen.length) return;
    if (!reason.trim()) {
      fail(new Error("请先说明排除或工作输入的保留／备份责任。"));
      return;
    }
    const next = {
      ...draft,
      [kind]: [
        ...draft[kind],
        ...chosen.map((path) => ({ path, reason: reason.trim() })),
      ],
    };
    try {
      if (draft.assets.length) preflightMigration(next, inventory);
      resetAssistant();
      setDraft(next);
      setChosen([]);
      setReason("");
      setAcknowledged(false);
    } catch (cause) {
      fail(cause);
    }
  };
  const prepare = () =>
    void execute(async (signal) => {
      if (!acknowledged || !preflight)
        throw new Error(
          "请先完成清单审阅，并确认保留源件及未知／工作输入的备份责任。",
        );
      const meta = await api<{ api_version: string; instance_id: string }>(
        "/api/v1/meta",
        { signal },
      );
      if (meta.api_version !== "v1") throw new Error("当前 API 版本不受支持。");
      const next = await freezeMigration({
        plan: draft,
        files,
        instanceID: meta.instance_id,
        origin: location.origin,
        signal,
        onProgress: setProgress,
      });
      if (!signal.aborted) {
        setWorkspace(next);
        setStateSaved(false);
        setVerifications({});
        setNotice(
          "清单和原 create/commit 键已冻结；尚未写入。请先下载并确认保存原状态。冻结后不允许改目标或源文件。",
        );
      }
    });
  const saveOriginal = () => {
    if (!workspace) return;
    downloadManagedJSON(
      `lantai-migration-${workspace.batch_id}-state.json`,
      workspace,
    );
    setNotice(
      "原状态已交给浏览器下载。请实际保存在私有目录；页面不能证明下载已经完成。",
    );
  };
  const apply = () =>
    void execute(async (signal) => {
      if (!workspace || !stateSaved || !mayWrite)
        throw new Error("执行前需要原状态保存确认与合法 ingest 会话。");
      const current = structuredClone(workspace);
      const meta = await api<{ instance_id: string }>("/api/v1/meta", {
        signal,
      });
      if (meta.instance_id !== current.instance_id)
        throw new Error(
          "当前实例与冻结计划不符；保留原状态，不更换服务器重试。",
        );
      const orderedInventory = (items: { path: string; size: number }[]) =>
        [...items].sort((a, b) =>
          a.path < b.path ? -1 : a.path > b.path ? 1 : 0,
        );
      if (
        canonicalJSON(orderedInventory(inventory)) !==
        canonicalJSON(orderedInventory(current.inventory))
      )
        throw new Error(
          "本次来源选择或未覆盖项的大小已经变化。先恢复原来源清单，不沿旧键悄悄调整范围。",
        );
      preflightMigration(current.plan, inventory);
      window.dispatchEvent(
        new CustomEvent("lantai:unsaved-operation", { detail: true }),
      );
      for (const asset of current.plan.assets) {
        if (signal.aborted) break;
        const state = current.states[asset.key];
        if (state.result && !state.result.description_pending) continue;
        current.rows[asset.key] = { status: "running" };
        setWorkspace(structuredClone(current));
        try {
          const receipt = await pushFiles({
            input: asset.push,
            files: filesForAsset(asset, files),
            state,
            signal,
            onProgress: setProgress,
            checkpoint: async (next) => {
              current.states[asset.key] = next;
              if (live.current) setWorkspace(structuredClone(current));
            },
          });
          current.rows[asset.key] = {
            status: receipt.description_pending
              ? "description_pending"
              : "committed",
          };
        } catch (cause) {
          const definite =
            cause instanceof APIError &&
            cause.status >= 400 &&
            cause.status < 500;
          current.rows[asset.key] = {
            status: definite ? "failed" : "unknown",
            error_code: publicCode(cause),
          };
          if (live.current) setWorkspace(structuredClone(current));
          if (isSessionError(cause)) expire(cause);
          if (!signal.aborted) {
            setError(cause);
            setNotice(
              "已保留逐项结果。仅按原计划、原状态和原键续办；结果不明不等于没有提交，不能创建替代上传。",
            );
          }
          break;
        }
        if (live.current) setWorkspace(structuredClone(current));
      }
      if (!signal.aborted && live.current) {
        const complete = current.plan.assets.every(
          (asset) => current.rows[asset.key].status === "committed",
        );
        setNotice(
          complete
            ? "各项历史固定版本回执已保存，尚不表示当前远端核验、工程续做或灾备验收通过。请导出最新状态并继续验证。"
            : "批次部分完成或著录待完善。保存原状态，按明确下一步续办。取消只停止本机请求，不回滚已接受的文件／版本。",
        );
        window.dispatchEvent(
          new CustomEvent("lantai:unsaved-operation", { detail: !complete }),
        );
      }
    });
  const verify = () =>
    void execute(async (signal) => {
      if (!workspace) return;
      const next: Record<string, VerificationReceipt> = {};
      setVerifications({});
      for (const asset of workspace.plan.assets) {
        const receipt = workspace.states[asset.key].result;
        if (!receipt) continue;
        next[asset.key] = await verifyExactFiles({
          assetID: receipt.asset_id,
          versionID: receipt.version_id,
          expectedFiles: asset.push.content.files,
          expectedIdentity: {
            instance_id: workspace.instance_id,
            project_id: receipt.project_id,
            asset_id: receipt.asset_id,
            version_id: receipt.version_id,
            manifest_digest: receipt.manifest_digest,
            version_number: receipt.version_number,
            uri: receipt.uri,
          },
          localFiles: files.length ? filesForAsset(asset, files) : undefined,
          purpose: "archive_review",
          signal,
          onProgress: setProgress,
        });
        if (!signal.aborted) setVerifications({ ...next });
      }
      if (!signal.aborted)
        setNotice(
          `本次完成 ${Object.keys(next).length} 项当前远端逐文件核验。它不替代著录、人审、原生软件续做或独立恢复。`,
        );
    });
  const restore = async (value: unknown) => {
    const payload =
      object(value) && value.schema === "lantai.migration-record/v1"
        ? value.workspace
        : value;
    const next = await importMigrationWorkspace(payload, location.origin);
    resetAssistant();
    setWorkspace(next);
    setDraft(next.plan);
    setStateSaved(false);
    setVerifications({});
    setRecoveryRefs([]);
    setFiles([]);
    setChosen([]);
    setAcknowledged(false);
    if (object(value) && Array.isArray(value.recovery_refs))
      setRecoveryRefs(
        value.recovery_refs.filter(
          (ref): ref is ManagedRecordRef =>
            object(ref) &&
            typeof ref.asset_id === "string" &&
            identifierPattern.test(ref.asset_id) &&
            typeof ref.version_id === "string" &&
            identifierPattern.test(ref.version_id),
        ),
      );
    setNotice(
      "已加载历史计划／原键；历史验证未作为当前通过。执行需重新选择相同源文件并保存原状态；当前核验会重新请求服务端。不能只凭旧回执认定恢复后仍完整。",
    );
  };
  const importState = (event: ChangeEvent<HTMLInputElement>) => {
    const file = fileInput(event);
    if (!file) return;
    void execute(async () => {
      if (file.size > 8 * 1024 * 1024) throw new Error("原状态包超过 8 MiB。");
      await restore(parseManagedJSON(await file.text()));
    });
  };
  const loadRecord = () =>
    void execute(async (signal) => {
      await restore(
        await loadManagedRecord<unknown>(
          { asset_id: recordAsset.trim(), version_id: recordVersion.trim() },
          signal,
          { kind: "migration", projectID: currentPlan.project_id || undefined },
        ),
      );
    });
  const readRecords = (cursor?: string) =>
    void execute(async (signal) => {
      const page = await listManagedRecords({
        projectID: currentPlan.project_id,
        kind: "migration",
        cursor,
        signal,
      });
      if (!signal.aborted) {
        setRecordItems((old) =>
          cursor ? [...old, ...page.items] : page.items,
        );
        setRecordCursor(page.next_cursor);
      }
    });
  const linkRecovery = () =>
    void execute(async (signal) => {
      const ref = {
        asset_id: recoveryAsset.trim(),
        version_id: recoveryVersion.trim(),
      };
      const payload = await loadManagedRecord<unknown>(ref, signal, {
        kind: "recovery",
      });
      if (!object(payload) || !String(payload.schema).includes("recovery"))
        throw new Error(
          "该固定档案不是恢复记录；不会把其他 JSON 当作恢复通过证据。",
        );
      if (!signal.aborted) {
        setRecoveryRefs((old) =>
          old.some(
            (item) =>
              item.asset_id === ref.asset_id &&
              item.version_id === ref.version_id,
          )
            ? old
            : [...old, ref],
        );
        setRecoveryInfo(payload);
        setNotice(
          "恢复档案已读取并关联。关联只说明存在该记录，请核对其包摘要、捕获点、核心接受记录和核验范围，不将客户端自报等同核心恢复通过。",
        );
      }
    });
  const loadCapacity = () =>
    void execute(async (signal) => {
      const info = await api<{
        instance_id: string;
        observed_at: string;
        free_bytes: number | null;
        min_free_bytes: number;
        capacity_error?: string;
      }>("/api/v1/admin/instance", { signal });
      if (info.free_bytes === null) {
        setInstanceCapacity(
          `当前容量未知：${info.capacity_error || "无法读取"}`,
        );
        return;
      }
      if (
        !Number.isSafeInteger(info.free_bytes) ||
        info.free_bytes < 0 ||
        !Number.isSafeInteger(info.min_free_bytes) ||
        info.min_free_bytes < 0
      )
        throw new Error("核心容量响应无效。");
      setDraft((old) => ({
        ...old,
        capacity: {
          ...old.capacity,
          free_main_bytes: Math.max(0, info.free_bytes! - info.min_free_bytes),
        },
      }));
      setInstanceCapacity(
        `核心观察 ${info.observed_at}：余量 ${bytes(info.free_bytes)}；扣除最低保留 ${bytes(info.min_free_bytes)}。这不是备份／恢复卷容量，也不是长期保证。`,
      );
    });
  const assistantJob = (
    action: "import.plan" | "import.apply" | "import.verify",
  ) => {
    if (anyDevicePending) return;
    try {
      if (!identifierPattern.test(currentPlan.project_id))
        throw new Error("请先选择原项目。");
      const input =
        action === "import.plan"
          ? nativeImportInput(validateMigrationDraft(currentPlan))
          : nativePlan;
      if (!input || (action === "import.verify" && !nativeReport))
        throw new Error(
          "先导入原助手返回的完整计划／导入回执，不能猜测或新建替代状态。",
        );
      // Assistant state belongs to its own original batch, never a later browser workspace.
      if (
        action !== "import.plan" &&
        object(nativePlan) &&
        nativePlan.schema === "lantai.local-assistant-result-ref/v1" &&
        !assistantLocalReviewed
      )
        throw new Error("请先在设备本机审阅原摘要计划，再准备后续动作。");
      const batchID = assistantBatchID;
      if (
        action !== "import.plan" &&
        object(nativePlan) &&
        object(nativePlan.input) &&
        Array.isArray(nativePlan.input.assets)
      ) {
        const projects = nativePlan.input.assets.map((asset) =>
          object(asset) && object(asset.push)
            ? asset.push.project_id
            : undefined,
        );
        if (
          !projects.length ||
          projects.some((project) => project !== currentPlan.project_id)
        )
          throw new Error(
            "原助手计划与当前项目不同；请打开原项目，不能改写原计划或替换 state。",
          );
      }
      const statePath = assistantStatePath ?? `state/${batchID}`;
      const destinationPath = assistantDestination ?? `verified/${batchID}`;
      setAssistantStatePath(statePath);
      setAssistantDestination(destinationPath);
      const job = makeAssistantJob({
        action,
        input,
        report: nativeReport,
        source: assistantSource,
        state: statePath,
        output: `results/${batchID}-${action.replace(".", "-")}.json`,
        origin: location.origin,
        destination: destinationPath,
        jobID: `migration-${batchID}-${action.split(".")[1]}`,
      });
      if (action === "import.plan") {
        setAssistantPlanJob(job);
        setNativePlan(undefined);
        setNativeReport(undefined);
        setAssistantApplyJob(undefined);
        setAssistantVerifyJob(undefined);
        setAssistantResult(undefined);
      }
      if (action === "import.apply") setAssistantApplyJob(job);
      if (action === "import.verify") setAssistantVerifyJob(job);
      setAssistantActiveJob(job as DeviceJobInput);
      setNotice(
        "作业已准备到下方设备控制台；尚未执行。选择本人受权设备，等待真实检查并明确批准摘要后，才执行本动作。原state和键保持。",
      );
    } catch (cause) {
      fail(cause);
    }
  };
  const observeAssistantJob = useCallback((job: DeviceJobInput) => {
    if (!["import.plan", "import.apply", "import.verify"].includes(job.action))
      throw new Error("本分区只续办原导入计划、执行与验证。");
    setAssistantActiveJob(job);
    setAssistantSource(job.directory);
    setAssistantLocalReviewed(false);
    if (job.state) setAssistantStatePath(job.state);
    if (job.destination) setAssistantDestination(job.destination);
    const originalBatch = /^migration-(.+)-(?:plan|apply|verify)$/.exec(
      job.job_id,
    )?.[1];
    if (originalBatch) setAssistantBatchID(originalBatch);
    if (job.action === "import.plan") {
      setAssistantPlanJob(job);
      setNativePlan(undefined);
      setNativeReport(undefined);
    } else {
      setNativePlan(job.input);
      if (job.action === "import.apply") {
        setAssistantApplyJob(job);
        setNativeReport(undefined);
      } else {
        setAssistantVerifyJob(job);
        setNativeReport(job.report);
      }
    }
  }, []);
  const observeAssistantResult = async (
    value: unknown,
    context: DeviceResultContext,
  ) => {
    try {
      if (!assistantActiveJob) throw new Error("请先读取原设备作业。");
      const bound = await bindImportDeviceResult({
        job: assistantActiveJob as unknown as Record<string, unknown>,
        envelope: value,
        context,
      });
      setAssistantResult(value);
      if (assistantActiveJob.action === "import.plan") {
        setNativePlan(undefined);
        setNativeReport(undefined);
        setAssistantLocalReviewed(false);
        if (bound.completed) setNativePlan(bound.reference);
      }
      if (assistantActiveJob.action === "import.apply") {
        setNativeReport(bound.completed ? bound.reference : undefined);
      }
      setNotice(
        bound.completed
          ? "已保留本次设备动作的原结果引用。完整计划／逐项报告保留在本机，用下方原摘要查看命令核对；不把动作完成冒充人审、当前网页逐文件核验或恢复接受。"
          : "设备结果未达到明确完成；保留原作业、state和键，pending/failed/unknown不能换状态或自动重派。",
      );
    } catch (cause) {
      fail(cause);
    }
  };
  const importAssistantJob = (event: ChangeEvent<HTMLInputElement>) => {
    const file = fileInput(event);
    if (!file) return;
    void execute(async () => {
      if (file.size > 8 * 1024 * 1024) throw new Error("原作业超过 8 MiB。");
      const rawJSON = await file.text();
      const value = parseManagedJSON(rawJSON);
      assertNoTransferSecrets(value);
      if (
        !object(value) ||
        value.schema !== "lantai.local-assistant-job/v1" ||
        typeof value.job_id !== "string" ||
        !["import.plan", "import.apply", "import.verify"].includes(
          String(value.action),
        ) ||
        !object(value.input) ||
        (value.origin && value.origin !== location.origin)
      )
        throw new Error(
          "不是当前服务端受支持的原作业。不能改 origin 或猜测原计划。",
        );
      if (value.action !== "import.plan")
        assertNativeJSONRoundTrip(rawJSON, value);
      if (typeof value.directory !== "string")
        throw new Error("原作业缺少工作区内明确源目录。");
      validateManagedPath(value.directory);
      if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value.job_id))
        throw new Error("原作业 ID 无效。");
      if (value.action !== "import.plan") {
        if (typeof value.state !== "string")
          throw new Error(
            "原 apply/verify 作业缺少原 state 路径，不能生成替代状态。",
          );
        validateManagedPath(value.state);
        setAssistantStatePath(value.state);
      }
      if (value.action === "import.verify") {
        if (typeof value.destination !== "string")
          throw new Error("原核验作业缺少独立目标。");
        validateManagedPath(value.destination);
        setAssistantDestination(value.destination);
      }
      const importedBatch = /^migration-(.+)-(?:plan|apply|verify)$/.exec(
        value.job_id,
      )?.[1];
      if (importedBatch) setAssistantBatchID(importedBatch);
      else if (value.action === "import.plan")
        setAssistantBatchID(value.job_id);
      if (value.action === "import.plan") {
        setAssistantPlanJob(value);
        setNativePlan(undefined);
        setNativeReport(undefined);
        setAssistantApplyJob(undefined);
        setAssistantVerifyJob(undefined);
        setAssistantStatePath(undefined);
        setAssistantDestination(undefined);
      }
      if (value.action === "import.apply") {
        setAssistantPlanJob(undefined);
        setAssistantVerifyJob(undefined);
        setNativeReport(undefined);
        setAssistantResult(undefined);
        setAssistantApplyJob(value);
        setNativePlan(value.input);
      }
      if (value.action === "import.verify") {
        setAssistantPlanJob(undefined);
        setAssistantApplyJob(undefined);
        setAssistantResult(undefined);
        setAssistantVerifyJob(value);
        setNativePlan(value.input);
        setNativeReport(value.report);
      }
      if (typeof value.directory === "string")
        setAssistantSource(value.directory);
      setNotice(
        "原助手作业已加载，用于核对返回结果的 job ID、动作和内容摘要；没有执行或授予任何权限。",
      );
    });
  };
  const importAssistantResult = (event: ChangeEvent<HTMLInputElement>) => {
    const file = fileInput(event);
    if (!file) return;
    void execute(async () => {
      if (file.size > 8 * 1024 * 1024) throw new Error("助手结果超过 8 MiB。");
      const rawJSON = await file.text();
      const value = parseManagedJSON(rawJSON);
      assertNoTransferSecrets(value);
      if (
        !object(value) ||
        value.schema !== "lantai.local-assistant-result/v1" ||
        !object(value.result) ||
        !["import.plan", "import.apply", "import.verify"].includes(
          String(value.action),
        )
      )
        throw new Error("不是受支持的助手原始结果。");
      const originalJob =
        value.action === "import.plan"
          ? assistantPlanJob
          : value.action === "import.apply"
            ? assistantApplyJob
            : assistantVerifyJob;
      if (!object(originalJob))
        throw new Error(
          "请先显式加载相应的原作业 JSON；不能只信一个结果文件。",
        );
      if (
        value.job_id !== originalJob.job_id ||
        value.action !== originalJob.action ||
        value.job_digest !== (await sha256(canonicalJSON(originalJob))) ||
        typeof value.approval_digest !== "string" ||
        !/^[0-9a-f]{64}$/.test(value.approval_digest)
      )
        throw new Error("助手结果与原作业的动作、稳定 ID 或内容摘要不匹配。");
      if (
        (value.action === "import.plan" &&
          value.result.schema !== "lantai.asset-import-plan/v1") ||
        (value.action === "import.apply" &&
          value.result.schema !== "lantai.asset-import-report/v1") ||
        (value.action === "import.verify" &&
          value.result.schema !== "lantai.asset-import-verify/v1")
      )
        throw new Error("助手动作与原计划／回执 schema 不匹配。");
      if (
        value.action !== "import.plan" &&
        (value.result.origin !== location.origin ||
          !object(originalJob.input) ||
          value.result.plan_digest !== originalJob.input.digest)
      )
        throw new Error("助手回执的服务器或原计划摘要不匹配。");
      setAssistantResult(value);
      if (
        value.action === "import.plan" &&
        value.status === "completed" &&
        value.exit_code === 0
      ) {
        setNativePlan(undefined);
        setNativeReport(undefined);
        assertNativeJSONRoundTrip(rawJSON, value);
        setNativePlan(value.result);
      }
      if (value.action === "import.apply") setNativeReport(value.result);
      setNotice(
        "已读取助手观察结果；失败／partial 原样保留。它不是生产设备授权，也不会使网页把旧 current_remote_verified 当作本次远端核验。后续继续原计划和本机原状态。",
      );
    });
  };
  const exportWorkset = () => {
    try {
      if (!workspace) return;
      const dependencies = parseManagedJSON(worksetDependencies);
      if (!Array.isArray(dependencies))
        throw new Error("工作集依赖必须为显式数组。");
      const entries = workspace.plan.assets.map((asset) => {
        const result = workspace.states[asset.key].result;
        if (!result)
          throw new Error("尚未取得全部固定版本回执，不能生成完整工作集。");
        return {
          asset_id: result.asset_id,
          version_id: result.version_id,
          destination: asset.key,
        };
      });
      downloadManagedJSON(`lantai-workset-${workspace.batch_id}.json`, {
        schema: "lantai.client-workset/v1",
        entries,
        entrypoints: worksetEntrypoints
          .split("\n")
          .map((line) => line.trim())
          .filter(Boolean),
        dependencies,
      });
      setNotice(
        "已导出固定版本工作集。入口与依赖必须在工程工作集页重新核验闭合；此操作没有自动解析、运行或改写工程。",
      );
    } catch (cause) {
      fail(cause);
    }
  };
  const recordPayload: MigrationRecord | undefined = workspace
    ? {
        schema: "lantai.migration-record/v1",
        workspace,
        verifications,
        recovery_refs: recoveryRefs,
      }
    : undefined;
  const hasReceipts = !!workspace?.plan.assets.some(
    (asset) => workspace.states[asset.key].result,
  );

  return (
    <div className="migration-center">
      <header className="migration-heading">
        <p className="eyebrow">统一存储 / MIGRATION</p>
        <h1>迁移中心</h1>
        <p>
          显式选定素材，复制入藏、原键续办并核对固定版本。源文件始终保留；工作输入和未知资料继续承担原备份责任。
        </p>
      </header>
      {error != null && (
        <p className="error" role="alert">
          {errorText(error)}
          {error instanceof APIError && error.requestID && (
            <small> 请求编号：{error.requestID}</small>
          )}
        </p>
      )}
      <div
        className="migration-status"
        role="status"
        aria-live="polite"
        aria-atomic="true"
      >
        {busy
          ? `${progress ? phaseLabels[progress.phase] : "处理中"}${progress?.path ? `：${progress.path}` : ""}${progress ? `（${progress.completed}/${progress.total}）` : ""}`
          : notice || "尚未执行任何写入。"}
      </div>
      {busy && (
        <button
          type="button"
          className="secondary"
          onClick={() => {
            controller.current?.abort();
            setNotice(
              "已请求停止本机请求；已被核心接受的副作用不会回滚。请保留原状态，核对原操作后续办。",
            );
          }}
        >
          停止本机请求，保留原状态
        </button>
      )}
      <section className="panel">
        <h2>1. 来源与目标</h2>
        <div className="migration-fields">
          <label>
            批次名称
            <input
              value={currentPlan.name}
              disabled={locked}
              onChange={(event) => setDraftField("name", event.target.value)}
              maxLength={200}
            />
          </label>
          <label>
            来源别名（不要填凭据或私密主机地址）
            <input
              value={currentPlan.source_alias}
              disabled={locked}
              onChange={(event) =>
                setDraftField("source_alias", event.target.value)
              }
              maxLength={200}
            />
          </label>
          <label>
            目标项目
            <select
              value={currentPlan.project_id}
              disabled={locked}
              onChange={(event) => {
                resetAssistant();
                const id = event.target.value;
                setDraft((old) => ({
                  ...old,
                  project_id: id,
                  assets: old.assets.map((asset) => ({
                    ...asset,
                    push: { ...asset.push, project_id: id },
                  })),
                }));
                setAcknowledged(false);
              }}
            >
              <option value="">请选择授权项目</option>
              {projects.map((project) => (
                <option key={project.project_id} value={project.project_id}>
                  {project.description.name} · {project.key}
                </option>
              ))}
            </select>
          </label>
        </div>
        {projectsCursor && (
          <button
            type="button"
            className="secondary"
            disabled={busy || anyDevicePending}
            onClick={() =>
              void execute(async (signal) => {
                const page = await api<{
                  items: Project[] | null;
                  next_cursor?: string;
                }>(
                  `/api/v1/projects?limit=100&view=full&cursor=${encodeURIComponent(projectsCursor)}`,
                  { signal },
                );
                setProjects((old) => [...old, ...(page.items ?? [])]);
                setProjectsCursor(page.next_cursor);
              })
            }
          >
            继续读取授权项目
          </button>
        )}
        <div className="migration-actions">
          <label className="migration-file-button">
            选择明确文件
            <input
              type="file"
              multiple
              disabled={busy || anyDevicePending}
              onChange={(event) => chooseFiles(event, false)}
            />
          </label>
          <label className="migration-file-button">
            选择一个来源目录
            <input
              type="file"
              multiple
              disabled={busy || anyDevicePending}
              ref={(node) => {
                node?.setAttribute("webkitdirectory", "");
              }}
              onChange={(event) => chooseFiles(event, true)}
            />
          </label>
          <label className="migration-file-button">
            打开原批次状态／记录 JSON
            <input
              type="file"
              accept="application/json,.json"
              disabled={busy || anyDevicePending}
              onChange={importState}
            />
          </label>
        </div>
        <p className="subtle">
          目录选择取决于浏览器支持；未选中的目录未盘点。文件不能从浏览器恢复状态中还原，续办时需重新选择相同文件。浏览器单文件
          ≤64 MiB、整批 ≤256 MiB、256 个选中文件；超限使用受控本机助手。
        </p>
        <p>
          {files.length
            ? `本次明确选择 ${files.length} 个来源文件、${bytes(files.reduce((sum, file) => sum + file.file.size, 0))}。`
            : "还没有选择本机文件；可以先打开馆内历史计划，当前服务端校验仍需单独执行。"}
        </p>
        {files.length > 0 && (
          <details>
            <summary>来源清单与分组选择（{files.length} 项）</summary>
            <div className="migration-table">
              <table>
                <thead>
                  <tr>
                    <th>选择</th>
                    <th>来源相对路径</th>
                    <th>大小</th>
                    <th>文件角色</th>
                  </tr>
                </thead>
                <tbody>
                  {files.map((file) => (
                    <tr key={file.path}>
                      <td>
                        <input
                          type="checkbox"
                          aria-label={`选择 ${file.path}`}
                          checked={chosen.includes(file.path)}
                          disabled={locked}
                          onChange={(event) =>
                            setChosen((old) =>
                              event.target.checked
                                ? [...old, file.path]
                                : old.filter((path) => path !== file.path),
                            )
                          }
                        />
                      </td>
                      <td>
                        <code>{file.path}</code>
                      </td>
                      <td>{bytes(file.file.size)}</td>
                      <td>
                        <select
                          aria-label={`${file.path} 文件角色`}
                          value={file.role}
                          disabled={locked}
                          onChange={(event) =>
                            setFiles((old) =>
                              old.map((item) =>
                                item.path === file.path
                                  ? { ...item, role: event.target.value }
                                  : item,
                              ),
                            )
                          }
                        >
                          {roles.map((role) => (
                            <option key={role}>{role}</option>
                          ))}
                        </select>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </details>
        )}
      </section>
      {!workspace && (
        <section className="panel">
          <h2>2. 明确素材分组与用途／权利</h2>
          <p>
            一个版本包含可理解的一套文件。历史 v001
            或同哈希不自动合并身份；待审标签不等于
            approved/published。选中首项如仍为
            source，会在下方明确新分组清单中设为 primary，请审阅后冻结。
          </p>
          <div className="migration-fields">
            <label>
              稳定分组 key
              <input
                value={groupKey}
                onChange={(event) => setGroupKey(event.target.value)}
                disabled={locked}
              />
            </label>
            <label>
              相对来源目录（根目录用 .）
              <input
                value={groupDirectory}
                onChange={(event) => setGroupDirectory(event.target.value)}
                disabled={locked}
              />
            </label>
            <label>
              分类
              <select
                value={classification}
                onChange={(event) =>
                  setClassification(event.target.value as typeof classification)
                }
                disabled={locked}
              >
                <option value="unknown">未知，明确选择后入藏</option>
                <option value="final">最终资料</option>
                <option value="intermediate">明确需要保留的中间资料</option>
              </select>
            </label>
            <label>
              素材类型
              <select
                value={groupType}
                onChange={(event) => setGroupType(event.target.value)}
                disabled={locked}
              >
                {Object.entries(typeLabels).map(([type, label]) => (
                  <option value={type} key={type}>
                    {label}
                  </option>
                ))}
              </select>
            </label>
            <label>
              新资产路径名
              <input
                value={groupSlug}
                onChange={(event) => setGroupSlug(event.target.value)}
                disabled={locked || !!appendAsset}
                placeholder="selected/scene-a"
              />
            </label>
            <label>
              新资产标题
              <input
                value={groupTitle}
                onChange={(event) => setGroupTitle(event.target.value)}
                disabled={locked || !!appendAsset}
              />
            </label>
          </div>
          <details>
            <summary>追加原资产固定版本（不会修改当前著录）</summary>
            <div className="migration-fields">
              <label>
                原资产 ID
                <input
                  value={appendAsset}
                  onChange={(event) => setAppendAsset(event.target.value)}
                  disabled={locked}
                />
              </label>
              <label>
                原 base_version_id
                <input
                  value={baseVersion}
                  onChange={(event) => setBaseVersion(event.target.value)}
                  disabled={locked}
                />
              </label>
            </div>
            <p>
              追加版本沿用核心权利继承规则，不凭文件名推断旧版本；当前标签需另走条件著录编辑。
            </p>
          </details>
          {!appendAsset && (
            <fieldset disabled={locked}>
              <legend>新资产必须由本人明确确认 rights</legend>
              <div className="migration-fields">
                <label>
                  用途限制
                  <select
                    value={usage}
                    onChange={(event) =>
                      setUsage(event.target.value as Rights["usage"])
                    }
                  >
                    <option value="restricted">restricted · 受限</option>
                    <option value="reference">reference · 仅参考</option>
                    <option value="production">
                      production · 具有相应生产权利
                    </option>
                  </select>
                </label>
                <label>
                  许可证／明确自定义条款
                  <input
                    value={license}
                    onChange={(event) => setLicense(event.target.value)}
                    placeholder="须有实际依据，不自动补 CC0"
                    maxLength={256}
                  />
                </label>
                <label>
                  敏感级别
                  <select
                    value={sensitivity}
                    onChange={(event) =>
                      setSensitivity(
                        event.target.value as Rights["sensitivity"],
                      )
                    }
                  >
                    <option value="normal">normal</option>
                    <option value="personal">personal（仍需正式权限）</option>
                  </select>
                </label>
              </div>
              <label>
                <input
                  type="checkbox"
                  checked={redistribute}
                  onChange={(event) => setRedistribute(event.target.checked)}
                />
                明确允许原件转发
              </label>
              <label>
                <input
                  type="checkbox"
                  checked={noai}
                  onChange={(event) => setNoai(event.target.checked)}
                />
                noai，禁止相应生成式用途
              </label>
            </fieldset>
          )}
          <button
            type="button"
            className="primary"
            disabled={locked || !chosen.length || !draft.project_id}
            onClick={addGroup}
          >
            把所选 {chosen.length} 个文件加入明确分组
          </button>
          <div className="migration-fields">
            <label>
              保留／排除原因与备份责任
              <input
                value={reason}
                onChange={(event) => setReason(event.target.value)}
                disabled={locked}
                placeholder="例如：仍为 Shared/input，由原工作输入灾备保护"
              />
            </label>
          </div>
          <div className="migration-actions">
            <button
              type="button"
              className="secondary"
              disabled={locked || !chosen.length}
              onClick={() => disposition("working_inputs")}
            >
              作为工作输入保留，不入素材库
            </button>
            <button
              type="button"
              className="secondary"
              disabled={locked || !chosen.length}
              onClick={() => disposition("excluded")}
            >
              排除本批入藏，保留源件责任
            </button>
          </div>
        </section>
      )}
      <section className="panel">
        <h2>3. 清单与空间预检</h2>
        <p>
          容量按卷分别核对：新增原件＋暂存、快照＋完整包、新目标上传／解包／恢复、独立验证与工作副本。以下是保守下界，不含软件临时文件、历史保留及其他并发任务；同盘须合并峰值。
        </p>
        {!workspace && (
          <div className="migration-fields">
            {(
              [
                ["existing_library_bytes", "现有馆藏逻辑量 GiB"],
                ["free_main_bytes", "主库可用余量 GiB"],
                ["free_backup_bytes", "备份卷可用余量 GiB"],
                ["free_restore_bytes", "新目标恢复余量 GiB"],
                ["free_verification_bytes", "验证／工作副本余量 GiB"],
              ] as const
            ).map(([key, label]) => (
              <label key={key}>
                {label}
                <input
                  type="number"
                  min="0"
                  step="0.01"
                  placeholder="未知请留空"
                  disabled={busy || anyDevicePending}
                  value={
                    draft.capacity?.[key] === undefined
                      ? ""
                      : draft.capacity[key]! / 1073741824
                  }
                  onChange={(event) => {
                    const next = { ...draft.capacity };
                    if (event.target.value === "") delete next[key];
                    else
                      next[key] = Math.floor(
                        Number(event.target.value) * 1073741824,
                      );
                    setDraftField("capacity", next);
                  }}
                />
              </label>
            ))}
          </div>
        )}
        {!workspace && (
          <button
            type="button"
            className="secondary"
            disabled={busy || anyDevicePending || !mayAdmin}
            onClick={loadCapacity}
          >
            按当前管理员权限读取主库实际余量
          </button>
        )}
        {instanceCapacity && <p role="status">{instanceCapacity}</p>}
        {preflight ? (
          <>
            <div className="migration-facts">
              <p>
                选中：
                <strong>
                  {preflight.selected_files} 文件 /{" "}
                  {bytes(preflight.logical_bytes)}
                </strong>
              </p>
              <p>
                排除：{preflight.excluded.length} 项；工作输入：
                {preflight.working_inputs.length} 项；未知未覆盖：
                {preflight.uncovered.length} 项。
              </p>
              <p>
                主库增量下界 {bytes(preflight.budgets.main)}；备份下界{" "}
                {bytes(preflight.budgets.backup)}；新目标恢复下界{" "}
                {bytes(preflight.budgets.restore)}；验证／工作副本{" "}
                {bytes(preflight.budgets.verification)}。
              </p>
            </div>
            {preflight.capacity_failures.length > 0 && (
              <p className="error" role="alert">
                声明容量不足：{preflight.capacity_failures.join("、")}
                。冻结被阻止。
              </p>
            )}
            {preflight.capacity_unknown.length > 0 && (
              <p className="notice">
                尚未验证：{preflight.capacity_unknown.join("、")}
                。不能宣称空间充足；核心在实际准入仍会检查。
              </p>
            )}
            {!preflight.browser_eligible && (
              <p className="notice">
                超过浏览器 {bytes(browserFileLimit)} 单文件／
                {bytes(browserBatchLimit)}{" "}
                整批预算；保留完整清单并使用下方本机受控助手，绝不截断。
              </p>
            )}
            {preflight.uncovered.length > 0 && (
              <details>
                <summary>未覆盖资料：未知，保留原件／灾备</summary>
                <ul>
                  {preflight.uncovered.map((item) => (
                    <li key={item.path}>
                      <code>{item.path}</code> · {bytes(item.size)}
                    </li>
                  ))}
                </ul>
              </details>
            )}
          </>
        ) : (
          <p className="notice">预检待完成：{preflightIssue}</p>
        )}
        {currentPlan.assets.length > 0 && (
          <div className="migration-table">
            <table>
              <thead>
                <tr>
                  <th>分组／项目映射</th>
                  <th>版本和来源分类</th>
                  <th>文件／用途权利</th>
                  <th>动作</th>
                </tr>
              </thead>
              <tbody>
                {currentPlan.assets.map((asset) => (
                  <tr key={asset.key}>
                    <td>
                      <strong>{asset.key}</strong>
                      <br />
                      <code>{asset.push.project_id}</code>
                      <br />
                      {asset.directory}
                    </td>
                    <td>
                      {asset.push.asset_id ? (
                        <code>
                          追加 {asset.push.asset_id} /{" "}
                          {asset.push.base_version_id}
                        </code>
                      ) : (
                        asset.push.slug
                      )}
                      <br />
                      {asset.classification}
                    </td>
                    <td>
                      {asset.push.content.files.length} 文件
                      <br />
                      {asset.push.content.rights
                        ? `${asset.push.content.rights.usage} · ${asset.push.content.rights.license}`
                        : "原权利继承，核心复验"}
                      <details>
                        <summary>冻结前审阅完整请求</summary>
                        <pre>{JSON.stringify(asset.push, null, 2)}</pre>
                      </details>
                    </td>
                    <td>
                      {!workspace && (
                        <button
                          type="button"
                          className="text-button"
                          disabled={busy || anyDevicePending}
                          onClick={() =>
                            setDraftField(
                              "assets",
                              draft.assets.filter(
                                (item) => item.key !== asset.key,
                              ),
                            )
                          }
                        >
                          从草稿移除此分组
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {(currentPlan.excluded.length > 0 ||
          currentPlan.working_inputs.length > 0) && (
          <details>
            <summary>排除和工作输入责任</summary>
            <ul>
              {[
                ...currentPlan.excluded.map((item) => ({
                  ...item,
                  kind: "排除本批",
                })),
                ...currentPlan.working_inputs.map((item) => ({
                  ...item,
                  kind: "工作输入",
                })),
              ].map((item) => (
                <li key={`${item.kind}:${item.path}`}>
                  {item.kind} · <code>{item.path}</code>：{item.reason}
                </li>
              ))}
            </ul>
          </details>
        )}
        {!workspace && (
          <>
            <details>
              <summary>审阅／导入完整计划 JSON</summary>
              <p>
                本页支持 lantai.browser-migration-plan/v1。原 CLI
                冻结计划请在下方助手路径沿原状态续办，不能改造成新键。
              </p>
              <textarea
                rows={12}
                value={planText}
                onChange={(event) => setPlanText(event.target.value)}
                disabled={busy || anyDevicePending}
                aria-label="完整迁移计划 JSON"
              />
              <div className="migration-actions">
                <button
                  type="button"
                  className="secondary"
                  disabled={busy || anyDevicePending}
                  onClick={() => setPlanText(JSON.stringify(draft, null, 2))}
                >
                  载入当前草稿供审阅
                </button>
                <button
                  type="button"
                  className="secondary"
                  disabled={busy || anyDevicePending || !planText}
                  onClick={() => {
                    try {
                      const next = validateMigrationDraft(
                        parseManagedJSON(planText),
                      );
                      if (files.length) preflightMigration(next, inventory);
                      resetAssistant();
                      setDraft(next);
                      setAcknowledged(false);
                      setError(undefined);
                    } catch (cause) {
                      fail(cause);
                    }
                  }}
                >
                  严格校验并采用草稿
                </button>
                <button
                  type="button"
                  className="secondary"
                  disabled={busy || anyDevicePending}
                  onClick={() =>
                    downloadManagedJSON("lantai-migration-draft.json", draft)
                  }
                >
                  导出草稿
                </button>
              </div>
            </details>
            <label>
              <input
                type="checkbox"
                checked={acknowledged}
                disabled={busy || anyDevicePending}
                onChange={(event) => setAcknowledged(event.target.checked)}
              />
              我已审阅分组、rights、排除／未知、工作输入和容量未知项；保留源文件及其备份责任，只执行复制入藏
            </label>
            <button
              type="button"
              className="primary"
              disabled={
                busy ||
                !acknowledged ||
                !preflight ||
                !preflight.browser_eligible ||
                !!preflight.capacity_failures.length ||
                !files.length
              }
              onClick={prepare}
            >
              冻结明确计划并生成原操作状态（尚不写入）
            </button>
          </>
        )}
      </section>
      {workspace && (
        <section className="panel">
          <h2>4. 原状态、执行与逐项回执</h2>
          <p>
            本页只续办本浏览器导出的原状态。原生客户端的 RawMessage
            词法可能影响请求摘要；其原
            state、原键和原计划必须沿受控助手使用，不能在此重算 hash
            或转换新键。
          </p>
          <p>
            批次 <code>{workspace.batch_id}</code> · 固定实例{" "}
            <code>{workspace.instance_id}</code>
            <br />
            计划摘要 <code>{workspace.plan_digest}</code>
          </p>
          <div className="migration-actions">
            <button type="button" className="secondary" onClick={saveOriginal}>
              下载最新原状态与计划
            </button>
            <label>
              <input
                type="checkbox"
                checked={stateSaved}
                disabled={busy || anyDevicePending}
                onChange={(event) => setStateSaved(event.target.checked)}
              />
              我已实际保存原状态；断线和补著录只用原键
            </label>
          </div>
          {!mayWrite && (
            <p className="notice">
              当前页面可审阅和核验。入藏需目标项目 ingest，补著录另需
              organize，并须当前标签页
              CSRF。请明确选择写入范围重新登录，不自动扩大 admin。
            </p>
          )}
          <div className="migration-actions">
            <button
              type="button"
              className="primary"
              disabled={
                busy ||
                assistantDevicePending ||
                !stateSaved ||
                !mayWrite ||
                !files.length
              }
              onClick={apply}
            >
              按原计划／原键执行或续办
            </button>
            <button
              type="button"
              className="secondary"
              disabled={
                busy || assistantDevicePending || !hasReceipts || !hasCSRF()
              }
              onClick={verify}
            >
              重新核验当前远端全部固定文件
            </button>
          </div>
          {incomplete && (
            <p className="notice">
              部分状态未完成。unknown
              不等于失败；待著录不等于要再上传。停止本机请求不会回滚已提交版本；不要新建另一批来修复原项。
            </p>
          )}
          <div className="migration-table">
            <table>
              <thead>
                <tr>
                  <th>分组</th>
                  <th>上传／提交及著录</th>
                  <th>固定身份／原操作</th>
                  <th>当前验证</th>
                </tr>
              </thead>
              <tbody>
                {workspace.plan.assets.map((asset) => {
                  const state = workspace.states[asset.key];
                  const row = workspace.rows[asset.key];
                  const receipt = state.result;
                  const verification = verifications[asset.key];
                  return (
                    <tr key={asset.key}>
                      <td>{asset.key}</td>
                      <td>
                        {rowLabels[row.status]}
                        {row.error_code && (
                          <>
                            <br />
                            <code>{row.error_code}</code>
                          </>
                        )}
                        {receipt?.description_pending && (
                          <p>
                            取得合法 organize 后按原 commit 补办；不创建新版。
                          </p>
                        )}
                      </td>
                      <td>
                        {state.operation_id ? (
                          <code>{state.operation_id}</code>
                        ) : (
                          "尚无服务端操作身份"
                        )}
                        {receipt && (
                          <>
                            <br />
                            <a
                              href={`#/assets/${receipt.asset_id}?version=${receipt.version_id}`}
                              className="text-button"
                            >
                              查看固定版本
                            </a>
                            <br />
                            <code>
                              {receipt.asset_id} / {receipt.version_id}
                            </code>
                          </>
                        )}
                      </td>
                      <td>
                        {verification ? (
                          <>
                            {verification.verified_at}
                            <br />
                            当前远端＋完整下载 SHA 通过
                            {verification.local_source_verified
                              ? "；本次源文件亦匹配"
                              : "；未核对当前本机源"}
                          </>
                        ) : (
                          "未做本次远端验证；历史 complete 不算当前通过"
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          {recordPayload && (
            <ManagedRecordSaver
              who={who}
              expire={expire}
              projectID={workspace.plan.project_id}
              title={`${workspace.plan.name} · 计划与回执`}
              kind="migration"
              payload={recordPayload}
            />
          )}
          <p className="subtle">
            馆内档案仅是受权记录的持久版本，随完整包备份。包捕获点外的本地操作仍须保全原状态和对账，记录里的历史验证不能自动成为当前通过。
          </p>
        </section>
      )}
      <section className="panel">
        <h2>5. 工程工作集与备份／恢复关联</h2>
        <p>
          上传成功、当前字节核验、工程依赖闭合、原生软件续做和独立恢复分别验收。完整备份沿原入口生成单包，无额外恢复口令或另搬主密钥要求。
        </p>
        <div className="migration-actions">
          <a className="secondary" href="#/worksets">
            进入工程工作集
          </a>
          <a className="secondary" href="#/admin/backups">
            进入完整备份与恢复
          </a>
        </div>
        {workspace && (
          <details>
            <summary>由本批固定回执导出工作集</summary>
            <label>
              工程入口（每行一个工作根相对路径）
              <textarea
                value={worksetEntrypoints}
                onChange={(event) => setWorksetEntrypoints(event.target.value)}
                rows={3}
              />
            </label>
            <label>
              显式依赖数组（from / requires）
              <textarea
                value={worksetDependencies}
                onChange={(event) => setWorksetDependencies(event.target.value)}
                rows={4}
              />
            </label>
            <button
              type="button"
              className="secondary"
              disabled={busy || anyDevicePending || !hasReceipts}
              onClick={exportWorkset}
            >
              导出固定工作集，在工作集页复核
            </button>
          </details>
        )}
        <div className="migration-fields">
          <label>
            恢复档案 asset_id
            <input
              value={recoveryAsset}
              onChange={(event) => setRecoveryAsset(event.target.value)}
              disabled={busy || anyDevicePending}
            />
          </label>
          <label>
            恢复档案固定 version_id
            <input
              value={recoveryVersion}
              onChange={(event) => setRecoveryVersion(event.target.value)}
              disabled={busy || anyDevicePending}
            />
          </label>
        </div>
        <button
          type="button"
          className="secondary"
          disabled={
            busy ||
            assistantDevicePending ||
            !hasCSRF() ||
            !recoveryAsset ||
            !recoveryVersion
          }
          onClick={linkRecovery}
        >
          读取固定恢复档案并关联
        </button>
        {recoveryRefs.length > 0 && (
          <ul>
            {recoveryRefs.map((ref) => (
              <li key={`${ref.asset_id}/${ref.version_id}`}>
                <a href={`#/assets/${ref.asset_id}?version=${ref.version_id}`}>
                  <code>
                    {ref.asset_id} / {ref.version_id}
                  </code>
                </a>{" "}
                · 已关联，非自动恢复验收
              </li>
            ))}
          </ul>
        )}
        {recoveryInfo !== undefined && (
          <details>
            <summary>恢复证据原记录（核对捕获点与范围）</summary>
            <pre>{JSON.stringify(recoveryInfo, null, 2)}</pre>
          </details>
        )}
      </section>
      <BatchAcceptance
        key={currentPlan.project_id}
        who={who}
        expire={expire}
        projectID={currentPlan.project_id}
        workspace={workspace}
        verifications={verifications}
        onPendingChange={setBatchDevicePending}
      />
      <section className="panel">
        <h2>原导入状态档案（历史续办）</h2>
        <p>
          按当前项目权限读取。列表是可收敛投影；刚提交的档案请使用返回的固定 ID
          打开，不因暂时搜不到而重新提交。
        </p>
        <div className="migration-actions">
          <button
            type="button"
            className="secondary"
            disabled={busy || anyDevicePending || !currentPlan.project_id}
            onClick={() => readRecords()}
          >
            读取本项目迁移记录
          </button>
          {recordCursor && (
            <button
              type="button"
              className="secondary"
              disabled={busy || anyDevicePending}
              onClick={() => readRecords(recordCursor)}
            >
              继续读取
            </button>
          )}
        </div>
        {recordItems.length > 0 && (
          <ul>
            {recordItems.map((item) => (
              <li key={`${item.asset_id}/${item.version_id}`}>
                <button
                  type="button"
                  className="text-button"
                  disabled={busy || anyDevicePending}
                  onClick={() =>
                    void execute(async (signal) => {
                      await restore(
                        await loadManagedRecord<unknown>(item, signal, {
                          kind: "migration",
                          projectID: currentPlan.project_id || undefined,
                        }),
                      );
                    })
                  }
                >
                  {item.title} · <code>{item.version_id}</code>
                </button>
              </li>
            ))}
          </ul>
        )}
        <div className="migration-fields">
          <label>
            档案 asset_id
            <input
              value={recordAsset}
              disabled={busy || anyDevicePending}
              onChange={(event) => setRecordAsset(event.target.value)}
            />
          </label>
          <label>
            固定 version_id
            <input
              value={recordVersion}
              disabled={busy || anyDevicePending}
              onChange={(event) => setRecordVersion(event.target.value)}
            />
          </label>
        </div>
        <button
          type="button"
          className="secondary"
          disabled={
            busy ||
            assistantDevicePending ||
            !recordAsset ||
            !recordVersion ||
            !hasCSRF()
          }
          onClick={loadRecord}
        >
          打开固定档案与原状态
        </button>
      </section>
      <section className="panel">
        <h2>大文件／原生批次：受控设备实际执行</h2>
        <p>
          通过本人当前受权设备执行白名单作业。源根、state
          和独立校验目标都位于操作者以后明确批准的本机工作区。沿现有 import
          plan/apply/verify，原状态、原键和 unknown 语义保持。
        </p>
        <label>
          以后在助手工作区内使用的相对源目录
          <input
            value={assistantSource}
            onChange={(event) => {
              resetAssistant();
              setAssistantSource(event.target.value);
            }}
            disabled={busy || anyDevicePending}
          />
        </label>
        <div className="migration-actions">
          <button
            type="button"
            className="secondary"
            disabled={
              busy ||
              anyDevicePending ||
              !currentPlan.assets.length ||
              (!acknowledged && !workspace)
            }
            onClick={() => assistantJob("import.plan")}
          >
            准备设备 import.plan 作业
          </button>
          <label className="migration-file-button">
            打开原助手作业
            <input
              type="file"
              accept="application/json,.json"
              disabled={busy || anyDevicePending}
              onChange={importAssistantJob}
            />
          </label>
          <label className="migration-file-button">
            导入助手原结果
            <input
              type="file"
              accept="application/json,.json"
              disabled={busy || anyDevicePending}
              onChange={importAssistantResult}
            />
          </label>
          <button
            type="button"
            className="secondary"
            disabled={busy || anyDevicePending || !nativePlan}
            onClick={() => assistantJob("import.apply")}
          >
            用原计划准备设备 apply
          </button>
          <button
            type="button"
            className="secondary"
            disabled={
              busy || assistantDevicePending || !nativePlan || !nativeReport
            }
            onClick={() => assistantJob("import.verify")}
          >
            用原回执准备设备 verify
          </button>
        </div>
        <p>
          固定原 state：
          <code>{assistantStatePath ?? `state/${assistantBatchID}`}</code>
          ；独立核验目标：
          <code>{assistantDestination ?? `verified/${assistantBatchID}`}</code>
          。重新打开 apply／verify 作业时保留其原路径。
        </p>
        <p>
          兼容本机离线复核：在已授权终端可先运行{" "}
          <code>
            lantai assistant inspect --job 作业文件 --workspace 明确工作区
          </code>
          ，审阅后将返回的 approval_digest 用于 <code>run --approve</code>
          。联网操作使用已有合法会话与可信
          CA；网页不替人授权，不把客户端代码当安全沙箱。
        </p>
        <p>
          当前包仅保护已入馆资料；Shared/input、尚未入馆文件和切换后新成果仍需明确备份。不得为了省空间删源，排除项也不代表可删除。
        </p>
        {object(nativePlan) &&
          nativePlan.schema === "lantai.local-assistant-result-ref/v1" && (
            <div className="notice">
              <p>
                原完整计划在设备私有工作区。先运行{" "}
                <code>
                  lantai assistant result --workspace ROOT --job-id{" "}
                  {String(nativePlan.job_id)} --result-sha256{" "}
                  {String(nativePlan.result_sha256)}
                </code>{" "}
                审阅完整清单与原计划范围。
              </p>
              <label>
                <input
                  type="checkbox"
                  disabled={anyDevicePending}
                  checked={assistantLocalReviewed}
                  onChange={(event) =>
                    setAssistantLocalReviewed(event.target.checked)
                  }
                />
                我已在本机审阅该原摘要计划；后续仍须批准实际设备检查摘要
              </label>
            </div>
          )}
        {identifierPattern.test(currentPlan.project_id) && (
          <>
            <DeviceJobConsole
              request={assistantRequest}
              projectID={currentPlan.project_id}
              job={
                assistantActiveJob ?? {
                  schema: "lantai.local-assistant-job/v1",
                  job_id: `migration-${assistantBatchID}-lookup`,
                  action: "import.plan",
                  input: { schema: "lantai.asset-import-input/v1", assets: [] },
                  directory: "source",
                  output: `results/${assistantBatchID}-lookup.json`,
                }
              }
              disabled={!assistantActiveJob || !currentPlan.project_id}
              onPendingChange={setAssistantDevicePending}
              onOriginalJob={observeAssistantJob}
              onResult={(value, context) => {
                void observeAssistantResult(value, context);
              }}
            />
          </>
        )}
        {assistantActiveJob && (
          <button
            className="secondary"
            disabled={anyDevicePending}
            onClick={() =>
              downloadManagedJSON(
                `lantai-${assistantActiveJob.action}-original-job.json`,
                assistantActiveJob,
              )
            }
          >
            导出当前原设备作业副本
          </button>
        )}
        {assistantResult !== undefined && (
          <>
            <details>
              <summary>原助手结果：历史／部分结果均保留</summary>
              <pre>{JSON.stringify(assistantResult, null, 2)}</pre>
            </details>
            <ManagedRecordSaver
              who={who}
              expire={expire}
              projectID={currentPlan.project_id}
              title={`${currentPlan.name} · 助手结果`}
              kind="assistant"
              payload={{
                schema: "lantai.migration-assistant-record/v1",
                result: assistantResult,
                jobs: {
                  plan: assistantPlanJob ?? null,
                  apply: assistantApplyJob ?? null,
                  verify: assistantVerifyJob ?? null,
                },
              }}
            />
          </>
        )}
      </section>
    </div>
  );
}
