import type { VerificationReceipt } from "./managed-transfer";

export type CoreRestoreReceipt = {
  run_id: string;
  backup_id: string;
  manifest_digest: string;
  recovery_epoch: number;
  evidence_digest: string;
  acceptance_basis: "accepted_capture_point" | "legacy_reconciliation";
  unknown_after_capture: boolean;
};
export type InstanceManagementInfo = {
  instance_id: string;
  recovery_epoch: number;
  state: string;
  observed_at: string;
  free_bytes: number | null;
  min_free_bytes: number;
  capacity_error?: string;
  recovery_receipts: CoreRestoreReceipt[];
  recovery_error?: string;
  recovery_partial: boolean;
};
export type BackupMetadata = {
  id: string;
  backup_id?: string;
  state: string;
  updated_at: string;
  package?: {
    id: string;
    format: string;
    sha256: string;
    size: number;
    sensitive: boolean;
  };
};

// A client-provided boolean is never accepted as proof of a restore. Only a
// current, administrator-authenticated core receipt can be linked here.
export function buildRecoveryEvidence(
  info: InstanceManagementInfo,
  runID: string,
  verifications: VerificationReceipt[],
  backup?: BackupMetadata,
) {
  if (info.state !== "ready" || info.recovery_error)
    throw new Error("当前核心尚未就绪或恢复回执异常，请先处理原恢复状态。");
  const receipt = info.recovery_receipts.find((row) => row.run_id === runID);
  if (!receipt || receipt.recovery_epoch > info.recovery_epoch)
    throw new Error("当前核心没有所选已接受恢复回执。");
  if (!verifications.length)
    throw new Error("至少明确选择一个固定版本并完成本次远端逐文件校验。");
  const identities = new Set<string>();
  for (const verification of verifications) {
    const id = `${verification.asset_id}/${verification.version_id}`;
    if (
      identities.has(id) ||
      verification.instance_id !== info.instance_id ||
      verification.current_remote_verified !== true ||
      verification.completion_basis !== "current_remote_and_downloaded_files" ||
      !verification.files.length ||
      !Number.isFinite(Date.parse(verification.verified_at))
    )
      throw new Error("验证结果重复，或不属于当前恢复实例与固定版本。");
    identities.add(id);
  }
  if (
    backup &&
    (backup.state !== "complete" ||
      !backup.package ||
      backup.backup_id !== receipt.backup_id ||
      !/^[0-9a-f]{64}$/.test(backup.package.sha256) ||
      !Number.isSafeInteger(backup.package.size) ||
      backup.package.size <= 0)
  )
    throw new Error("完整包元数据与核心恢复回执的备份身份不一致。");
  return {
    schema: "lantai.recovery-evidence/v1",
    evidence_basis:
      "current_core_restore_receipt_and_selected_fixed_version_verification",
    instance_id: info.instance_id,
    current_recovery_epoch: info.recovery_epoch,
    observed_at: info.observed_at,
    restore: receipt,
    ...(backup ? { backup } : {}),
    verifications,
    scope: "explicit_selected_versions_only",
    native_software_continuation_verified: false,
    source_retirement_authorized: false,
    unknown_after_capture_remains_unknown: receipt.unknown_after_capture,
  };
}
