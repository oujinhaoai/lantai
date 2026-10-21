import test from "node:test";
import assert from "node:assert/strict";
import {
  credentialIntent,
  currentObjects,
  executeApproval,
  prepareApproval,
  publicReceipt,
  roleIntent,
  type Approval,
  type Transport,
  operationDiagnostic,
  restoreOperationDiagnostic,
} from "../src/system-operations";

const project = "01J00000000000000000000001";
const principal = "01J00000000000000000000002";
const operation = "01J00000000000000000000003";
const grant = "01J00000000000000000000004";
const challenge = {
  challenge_id: "01J00000000000000000000005",
  operation_id: operation,
  action: "identity.grant_project_role",
  summary: "Grant viewer in synthetic project",
  targets: [{ kind: "project", id: project, expected_revision: 3 }],
  expires_at: "2099-10-20T08:00:00Z",
};

test("lost mutation response retries exact frozen command/key/grant without a second TOTP", async () => {
  const calls: { path: string; options?: RequestInit }[] = [];
  let fail = true;
  const request: Transport = async <T>(path: string, options?: RequestInit) => {
    calls.push({ path, options });
    if (path.endsWith("/challenges")) return challenge as T;
    if (path.endsWith("/verify"))
      return {
        grant_id: grant,
        operation_id: operation,
        action: challenge.action,
      } as T;
    if (fail) throw new Error("synthetic response lost after acceptance");
    return { operation_id: operation, replayed: true, result: {} } as T;
  };
  let pending = await prepareApproval(
    roleIntent(project, 3, principal, "viewer", true),
    request,
  );
  const initial = pending;
  await assert.rejects(
    executeApproval(pending, "123456", request, (p) => (pending = p)),
  );
  assert.equal(pending.executionAttempted, true);
  assert.equal(pending.grantID, grant);
  assert.equal(pending.key, initial.key);
  fail = false;
  await executeApproval(pending, "", request, (p) => (pending = p));
  const writes = calls.filter((x) => x.path.endsWith("/commands"));
  assert.equal(writes.length, 2);
  assert.deepEqual(writes[0].options, writes[1].options);
  assert.equal(calls.filter((x) => x.path.endsWith("/verify")).length, 1);
  assert.equal(pending.intent.mode, "identity");
  if (pending.intent.mode === "identity") {
    const command = pending.intent.command;
    assert.throws(() => {
      command.role = "owner";
    });
  }
});

test("rechallenge keeps original operation/key and rejects changed drafts or swapped server operation", async () => {
  const intent = roleIntent(project, 3, principal, "viewer", true);
  const original: Approval = {
    intent,
    challenge,
    key: "synthetic-stable-key",
    grantID: grant,
  };
  let sent: unknown;
  const request: Transport = async <T>(
    _path: string,
    options?: RequestInit,
  ) => {
    sent = JSON.parse(String(options?.body));
    return challenge as T;
  };
  const retried = await prepareApproval(intent, request, original);
  assert.equal(retried.key, original.key);
  assert.equal((sent as { operation_id: string }).operation_id, operation);
  assert.equal(retried.grantID, undefined);
  await assert.rejects(
    prepareApproval(
      roleIntent(project, 4, principal, "owner", true),
      request,
      original,
    ),
  );
  await assert.rejects(
    prepareApproval(
      intent,
      async <T>() => ({ ...challenge, operation_id: principal }) as T,
      original,
    ),
  );
});

test("domain grant target substitution fails before execute, original child retained for retry", async () => {
  const action = {
    action: "ledger.record_review",
    resource_id: principal,
    resource_revision: 2,
    request: { verdict: "approve" },
  };
  const approved: Approval = {
    intent: {
      mode: "domain",
      title: "Review",
      kind: "review",
      request: { target_id: principal },
    },
    challenge: { ...challenge, action: "identity.human_batch" },
    key: "same-key",
    grantID: grant,
    actions: [action],
  };
  let writes = 0;
  const request: Transport = async <T>(path: string) => {
    if (path.endsWith("/execute")) {
      writes++;
      return {} as T;
    }
    return [
      { operation_id: operation, action: { ...action, resource_revision: 3 } },
    ] as T;
  };
  await assert.rejects(executeApproval(approved, "", request, () => {}));
  assert.equal(writes, 0);
});

test("lost domain response keeps the original child, grant and key; diagnostic reproof preserves the child", async () => {
  const child = "01J00000000000000000000006";
  const action = {
    action: "ledger.record_review",
    project_id: project,
    resource_id: principal,
    resource_revision: 2,
    request: { target_id: principal, verdict: "approve" },
  };
  let pending: Approval = {
    intent: {
      mode: "domain",
      title: "Review",
      kind: "review",
      request: action.request,
    },
    challenge: { ...challenge, action: "identity.human_batch" },
    key: "same-domain-key",
    actions: [action],
  };
  const writes: RequestInit[] = [];
  let verify = 0;
  const request: Transport = async <T>(path: string, options?: RequestInit) => {
    if (path.endsWith("/verify")) {
      verify++;
      return {
        grant_id: grant,
        operation_id: operation,
        action: "identity.human_batch",
      } as T;
    }
    if (path.includes("/grants/"))
      return [{ operation_id: child, action }] as T;
    if (path.endsWith("/rechallenge"))
      return { ...challenge, action: "identity.human_batch" } as T;
    writes.push(options!);
    if (writes.length === 1)
      throw new Error("synthetic accepted response lost");
    return { operation_id: child, replayed: true } as T;
  };
  await assert.rejects(
    executeApproval(pending, "123456", request, (next) => (pending = next)),
  );
  assert.equal(pending.childOperationID, child);
  assert.equal(pending.executionAttempted, true);
  await executeApproval(pending, "", request, (next) => (pending = next));
  assert.equal(verify, 1);
  assert.deepEqual(writes[0], writes[1]);
  const diagnostic = operationDiagnostic(pending, "https://synthetic.invalid");
  const restored = restoreOperationDiagnostic(
    JSON.parse(JSON.stringify(diagnostic)),
    "https://synthetic.invalid",
  );
  const reproved = await prepareApproval(restored.intent, request, restored);
  assert.equal(reproved.grantID, undefined);
  assert.equal(reproved.childOperationID, child);
  assert.equal(reproved.key, pending.key);
});

test("credential scopes cannot manufacture admin for an agent; secret is removed recursively", () => {
  assert.throws(() =>
    credentialIntent({
      principalID: principal,
      expectedRevision: 1,
      kind: "agent",
      scopes: ["admin"],
      projects: [project],
      ttl: 3600,
      label: "synthetic",
    }),
  );
  const input = credentialIntent({
    principalID: principal,
    expectedRevision: 1,
    kind: "agent",
    scopes: ["read", "self", "read"],
    projects: [project, project],
    ttl: 3600,
    label: "synthetic",
  });
  assert.deepEqual(input.command.scopes, ["read", "self"]);
  assert.deepEqual(input.command.projects, [project]);
  assert.deepEqual(
    publicReceipt({
      secret: "never persist",
      result: { token: "never persist", principal_id: principal },
      list: [{ code: "never persist", status: "active" }],
    }),
    { result: { principal_id: principal }, list: [{ status: "active" }] },
  );
});

test("diagnostic round trip keeps original operation and keys but never restores a grant or secret", () => {
  const original: Approval = {
    intent: roleIntent(project, 3, principal, "viewer", true),
    challenge,
    key: "synthetic-stable-key",
    grantID: grant,
    executionAttempted: true,
  };
  const diagnostic = JSON.parse(
    JSON.stringify(operationDiagnostic(original, "https://synthetic.invalid")),
  );
  assert.equal(JSON.stringify(diagnostic).includes(grant), false);
  const restored = restoreOperationDiagnostic(
    diagnostic,
    "https://synthetic.invalid",
  );
  assert.equal(restored.key, original.key);
  assert.equal(
    restored.challenge.operation_id,
    original.challenge.operation_id,
  );
  assert.equal(restored.executionAttempted, true);
  assert.equal(restored.grantID, undefined);
  assert.throws(() =>
    restoreOperationDiagnostic(
      { ...diagnostic, grantID: grant },
      "https://synthetic.invalid",
    ),
  );
  assert.throws(() =>
    restoreOperationDiagnostic(diagnostic, "https://different.invalid"),
  );
  assert.throws(() =>
    restoreOperationDiagnostic(
      {
        ...diagnostic,
        intent: { ...diagnostic.intent, action: "identity.grant_system_role" },
      },
      "https://synthetic.invalid",
    ),
  );
});

test("authorized resync continues an empty page; no partial rows survive revocation or cross-project data", async () => {
  const paths: string[] = [];
  const request: Transport = async <T>(path: string) => {
    paths.push(path);
    return (
      paths.length === 1
        ? { objects: [], replay_after: 9, next_cursor: "opaque", replace: true }
        : {
            objects: [
              {
                ref: { project_id: project, kind: "review", id: principal },
                revision: 2,
                state: "pending",
                title: "synthetic",
              },
            ],
            replay_after: 9,
            replace: true,
          }
    ) as T;
  };
  assert.equal((await currentObjects(project, request)).objects.length, 1);
  assert.ok(paths[1].includes("cursor=opaque"));
  await assert.rejects(
    currentObjects(
      project,
      async <T>() =>
        ({
          objects: [
            {
              ref: { project_id: principal, kind: "trash", id: principal },
              revision: 2,
            },
          ],
          replay_after: 9,
          replace: true,
        }) as T,
    ),
  );
  let i = 0;
  await assert.rejects(
    currentObjects(project, async <T>() => {
      if (i++) throw new Error("revoked");
      return {
        objects: [
          {
            ref: { project_id: project, kind: "trash", id: principal },
            revision: 2,
          },
        ],
        replay_after: 9,
        next_cursor: "more",
        replace: true,
      } as T;
    }),
  );
});
