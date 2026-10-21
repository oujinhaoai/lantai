import assert from "node:assert/strict";
import { test } from "node:test";
import { buildRecoveryEvidence } from "../src/recovery-evidence.ts";

const receipt = { run_id: "run", backup_id: "backup", recovery_epoch: 2, unknown_after_capture: true, acceptance_basis: "accepted_capture_point" };
const info = { instance_id: "instance", recovery_epoch: 2, state: "ready", recovery_receipts: [receipt], observed_at: "2026-10-20T00:00:00Z" };
const verified = { instance_id: "instance", asset_id: "asset", version_id: "v1", current_remote_verified: true, completion_basis: "current_remote_and_downloaded_files", verified_at: "2026-10-20T00:00:01Z", files: [{ path: "file" }] };

test("generated backup or imported complete flag cannot substitute a current core restore receipt", () => {
  assert.throws(() => buildRecoveryEvidence({ ...info, recovery_receipts: [] }, "run", [verified]), /回执/);
  assert.throws(() => buildRecoveryEvidence({ ...info, state: "maintenance" }, "run", [verified]), /尚未就绪/);
  assert.throws(() => buildRecoveryEvidence(info, "run", []), /固定版本/);
});
test("restore evidence refuses cross-instance, duplicates and a different backup", () => {
  assert.throws(() => buildRecoveryEvidence(info, "run", [{ ...verified, instance_id: "foreign" }]), /当前恢复实例/);
  assert.throws(() => buildRecoveryEvidence(info, "run", [verified, verified]), /重复/);
  assert.throws(() => buildRecoveryEvidence(info, "run", [verified], { state: "complete", backup_id: "foreign", package: { sha256: "a".repeat(64), size: 100 } }), /备份身份/);
  const result = buildRecoveryEvidence(info, "run", [verified]);
  assert.equal(result.native_software_continuation_verified, false);
  assert.equal(result.source_retirement_authorized, false);
  assert.equal(result.unknown_after_capture_remains_unknown, true);
  assert.equal(result.scope, "explicit_selected_versions_only");
});
