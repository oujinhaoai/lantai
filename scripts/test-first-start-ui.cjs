// Dependency-free regression for sensitive state when leaving/history-restoring
// the setup page. HTTP policy is tested in Go; this is not a browser/TLS test.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const elements = new Map();
const listeners = new Map();
let reloads = 0;
const context = {
  document: { getElementById(id) {
    if (!elements.has(id)) elements.set(id, { value: "synthetic-sensitive-value", textContent: "synthetic-sensitive-value", addEventListener() {} });
    return elements.get(id);
  } },
  window: {
    addEventListener(name, fn) { listeners.set(name, fn); },
    location: { reload() { reloads++; } }
  }
};
vm.createContext(context);
vm.runInContext(fs.readFileSync(path.join(__dirname, "../internal/firststart/setup.js"), "utf8"), context);
vm.runInContext('claim = "synthetic-proof"; enrollment = "synthetic-enrollment";', context);
listeners.get("pagehide")();
for (const id of ["claim", "password", "repeat", "otp"]) assert.equal(elements.get(id).value, "");
for (const id of ["secret", "codes"]) assert.equal(elements.get(id).textContent, "");
assert.equal(vm.runInContext("claim + enrollment", context), "");
listeners.get("pageshow")({ persisted: false });
assert.equal(reloads, 0);
listeners.get("pageshow")({ persisted: true });
assert.equal(reloads, 1);
console.log("PASS: leaving setup clears sensitive fields; history restoration reloads");
