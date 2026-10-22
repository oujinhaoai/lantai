"use strict";
const el = id => document.getElementById(id);
let claim = "", selected = null, report = null, recovery = false, polling = false, setupEnrollment = "";
const sections = ["choices", "create", "upload", "package", "administrator", "enroll", "review", "working", "complete"];
function show(...ids) { for (const id of sections) el(id).hidden = !ids.includes(id); }
function clearSecrets() { claim = setupEnrollment = ""; for (const id of ["claim", "new-password", "new-repeat", "admin-password", "admin-repeat", "otp"]) el(id).value = ""; el("secret").textContent = el("codes").textContent = ""; }
window.addEventListener("pagehide", clearSecrets);
window.addEventListener("pageshow", e => { if (e.persisted) location.reload(); });
async function request(path, body, binary) {
  const headers = { Authorization: "Bearer " + claim };
  if (body !== undefined) headers["Content-Type"] = binary ? "application/octet-stream" : "application/json";
  const response = await fetch(path, { method: body === undefined ? "GET" : "POST", credentials: "omit", cache: "no-store", headers, body: body === undefined ? undefined : binary ? body : JSON.stringify(body) });
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}
async function act(button, task) { button.disabled = true; el("status").textContent = ""; try { await task(); } catch (e) { el("status").textContent = e.message; } finally { button.disabled = false; } }
async function status() {
  const value = await request("/setup/recovery/status");
  el("connect").hidden = true;
  const names = { select: "设备已连接", uploaded: "包完整性校验通过", restoring: "正在恢复，业务保持关闭", cancelling: "正在安全取消恢复", interrupted: "恢复曾中断，可继续同一包", administrator: "请重建管理员", review: "等待恢复检查与确认", failed: "恢复未完成，可重试", activating: "正在完成恢复", complete: "恢复已完成" };
  el("status").textContent = names[value.phase] || value.phase;
  if (value.error) el("status").textContent += "（" + value.error + "）";
  if (value.package) { selected = value.package; el("package-summary").textContent = "捕获时间：" + new Date(selected.captured_at).toLocaleString() + "。完整包大小：" + (selected.size / 1048576).toFixed(2) + " MiB，包含 " + selected.files + " 个校验条目。"; el("package-info").textContent = JSON.stringify(selected, null, 2); }
  if (value.phase === "select") show("choices");
  if (["uploaded", "interrupted", "failed"].includes(value.phase) && selected) show("package");
  if (value.phase === "administrator") { recovery = true; const names=value.administrators || []; el("admin-names").replaceChildren(...names.map(v=>{ const o=document.createElement("option"); o.value=v.name; return o; })); if(names.length===1) el("admin-name").value=names[0].name; show("administrator"); }
  if (value.phase === "review") { recovery = true; await review(); }
  if (value.phase === "complete") { clearSecrets(); show("complete"); }
  if (value.busy || ["restoring", "cancelling"].includes(value.phase)) { show("working"); el("cancel-restore").disabled = value.phase === "cancelling"; if (!polling) { polling = true; setTimeout(() => { polling = false; status().catch(e => { el("status").textContent = e.message; }); }, 1000); } }
}
async function review() { report = await request("/setup/recovery/review", {}); el("review-summary").textContent = report.automatic_checks_passed ? "本机检查已通过，没有待处理的本地对账项。请完成下方恢复点和原实例隔离确认。" : "仍有 " + report.unresolved_operations + " 项需要处理，当前不能启用服务。"; el("review-info").textContent = JSON.stringify(report, null, 2); show("review"); el("activate").disabled = !report.automatic_checks_passed; }
function enrollment(value) { setupEnrollment = value.enrollment_id || ""; el("secret").textContent = value.totp_secret; el("codes").textContent = value.recovery_codes.join("\n"); el("saved").checked = false; for (const id of ["new-password", "new-repeat", "admin-password", "admin-repeat"]) el(id).value = ""; show("enroll"); el("otp").focus(); }
el("connect").addEventListener("submit", e => { e.preventDefault(); claim = el("claim").value.trim(); act(e.currentTarget.querySelector("button"), status); });
el("new-instance").addEventListener("click", () => { recovery = false; show("create"); });
el("restore-instance").addEventListener("click", () => { recovery = true; show("upload"); });
el("create").addEventListener("submit", e => { e.preventDefault(); act(e.currentTarget.querySelector("button"), async () => { if (el("new-password").value !== el("new-repeat").value) throw new Error("两次口令不一致"); enrollment(await request("/setup/begin", { name: el("new-name").value, password: el("new-password").value })); }); });
el("upload").addEventListener("submit", e => { e.preventDefault(); act(e.currentTarget.querySelector("button"), async () => { const file = el("file").files[0]; if (!file) throw new Error("请选择完整恢复包"); el("status").textContent = "正在上传并校验完整包…"; await request("/setup/recovery/upload", file, true); el("file").value = ""; await status(); }); });
el("restore-button").addEventListener("click", e => act(e.currentTarget, async () => { if (!selected || !el("target-confirm").checked) throw new Error("请先确认来源与新目标"); await request("/setup/recovery/restore", { package_sha256: selected.sha256, confirm_new_target: true }); await status(); }));
el("administrator").addEventListener("submit", e => { e.preventDefault(); act(e.currentTarget.querySelector("button"), async () => { if (el("admin-password").value !== el("admin-repeat").value) throw new Error("两次口令不一致"); enrollment(await request("/setup/recovery/admin", { name: el("admin-name").value, password: el("admin-password").value })); }); });
el("confirm").addEventListener("submit", e => { e.preventDefault(); act(e.currentTarget.querySelector("button"), async () => { if (recovery) { await request("/setup/recovery/admin-confirm", { totp_code: el("otp").value, recovery_codes_saved: el("saved").checked }); el("secret").textContent = el("codes").textContent = ""; el("otp").value = ""; await review(); } else { await request("/setup/confirm", { totp_code: el("otp").value, recovery_codes_saved: el("saved").checked, enrollment_id: setupEnrollment }); clearSecrets(); show("complete"); } }); });
el("activate").addEventListener("click", e => act(e.currentTarget, async () => { if (!el("known").checked || !el("point").checked || !el("offline").checked) throw new Error("请先完成恢复核对与原实例隔离确认"); await request("/setup/recovery/complete", { review_id: report.id, known_changes_reviewed: true, accept_recovery_point: true, source_offline: true, activate: true }); clearSecrets(); show("complete"); }));

el("cancel-restore").addEventListener("click", e => act(e.currentTarget, async () => { await request("/setup/recovery/cancel", {}); await status(); }));
