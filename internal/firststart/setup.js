"use strict";
const el = id => document.getElementById(id);
let enrollment = "";
let claim = "";
function clearSensitive() {
  claim = enrollment = "";
  for (const id of ["claim", "password", "repeat", "otp"]) el(id).value = "";
  el("secret").textContent = el("codes").textContent = "";
}
// History snapshots must not retain the one-time proof or enrollment secrets.
window.addEventListener("pagehide", clearSensitive);
window.addEventListener("pageshow", event => {
  if (event.persisted) window.location.reload();
});
async function post(path, body) {
  const response = await fetch(path, {
    method: "POST", credentials: "omit", cache: "no-store",
    headers: { "Content-Type": "application/json", "Authorization": "Bearer " + claim },
    body: JSON.stringify(body)
  });
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}
async function submit(form, action) {
  const button = form.querySelector("button");
  button.disabled = true;
  el("status").textContent = "";
  try { await action(); }
  catch (error) { el("status").textContent = error.message + " 如完成请求时连接中断，请保留恢复码并检查服务状态。"; }
  finally { button.disabled = false; }
}
el("begin").addEventListener("submit", event => {
  event.preventDefault();
  if (el("password").value !== el("repeat").value) {
    el("status").textContent = "两次输入的口令不一致。";
    return;
  }
  submit(event.currentTarget, async () => {
    claim = el("claim").value.trim();
    const result = await post("/setup/begin", { name: el("name").value, password: el("password").value });
    enrollment = result.enrollment_id;
    el("secret").textContent = result.totp_secret;
    el("codes").textContent = result.recovery_codes.join("\n");
    el("saved").checked = false;
    el("password").value = el("repeat").value = el("claim").value = "";
    el("begin").hidden = true;
    el("enroll").hidden = false;
    el("otp").focus();
  });
});
el("confirm").addEventListener("submit", event => {
  event.preventDefault();
  submit(event.currentTarget, async () => {
    await post("/setup/confirm", { enrollment_id: enrollment, totp_code: el("otp").value, recovery_codes_saved: el("saved").checked });
    clearSensitive();
    el("enroll").hidden = true;
    el("complete").hidden = false;
  });
});
