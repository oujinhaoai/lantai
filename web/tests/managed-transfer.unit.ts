import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import {
  canonicalJSON,
  getExactManagedVersion,
  managedTransferURL,
  newPushState,
  parseManagedJSON,
  preparePushInput,
  pushFiles,
  readManagedFile,
  sha256,
  validateManagedFiles,
  validateManagedPath,
  validatePushState,
  verifyExactFiles,
  type InputFile,
  type PushReceipt,
  type PushState,
} from "../src/managed-transfer";
import { clearCSRF, rememberCSRF } from "../src/api";

const origin = "https://synthetic.example";
Object.defineProperty(globalThis, "location", {
  configurable: true,
  value: new URL(origin),
});
const originalFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = originalFetch;
  clearCSRF();
});
const project = "01J00000000000000000000001";
const asset = "01J00000000000000000000002";
const version = "01J00000000000000000000003";
const upload = "01J00000000000000000000004";
const operation = "01J00000000000000000000005";
const instance = "01J00000000000000000000006";
const grant = "01J00000000000000000000007";
const raw = new TextEncoder().encode("synthetic payload\n");
const asJSON = (value: unknown, status = 200) =>
  new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });

async function fixture() {
  const file = new File([raw], "sample.txt");
  const files = [{ path: "sample.txt", role: "primary", file }];
  const input = await preparePushInput({
    input: {
      project_id: project,
      slug: "synthetic/sample",
      content: {
        asset_type: "doc",
        files: [],
        rights: {
          usage: "reference",
          license: "LicenseRef-Synthetic",
          redistribute_raw: false,
          noai: true,
          sensitivity: "normal",
        },
      },
      describe: { title: "合成资料" },
    },
    files,
  });
  const state = await newPushState({ input });
  const manifestDigest = `sha256:${await sha256(canonicalJSON(input.content))}`;
  const receipt: PushReceipt = {
    operation_id: operation,
    project_id: project,
    asset_id: asset,
    version_id: version,
    version_number: 1,
    manifest_digest: manifestDigest,
    committed_at: new Date().toISOString(),
    uri: `lantai://${instance}/assets/${asset}/versions/${version}`,
  };
  return { files, input, state, receipt, manifestDigest };
}

test("strict imports reject duplicate keys, unsafe integers, lone surrogates and trailing content", () => {
  for (const text of [
    '{"x":1,"x":2}',
    '{"n":9007199254740993}',
    '"\\ud800"',
    "{}{}",
    "[1,]",
    '{"x":1,}',
    "\u00a0{}",
  ])
    assert.throws(() => parseManagedJSON(text));
  assert.equal(
    canonicalJSON(parseManagedJSON('{"b":true,"a":["中文",0]}')),
    '{"a":["中文",0],"b":true}',
  );
  assert.equal(
    Object.getPrototypeOf(parseManagedJSON('{"__proto__":{"safe":true}}')),
    null,
  );
});
test("path and complete-set preflight rejects aliases and file-directory collisions", () => {
  for (const path of [
    "../x",
    "/x",
    "a\\b",
    "NUL.txt",
    "a/",
    "a/../x",
    "x. ",
    "a\u202eb",
    "e\u0301.txt",
  ])
    assert.throws(() => validateManagedPath(path));
  const f = (path: string): InputFile => ({
    path,
    role: "primary",
    size: 1,
    sha256: "a".repeat(64),
  });
  for (const paths of [
    ["A/x", "a/y"],
    ["a", "a/x"],
    ["Straße", "STRASSE"],
    ["ſource/x", "source/y"],
  ])
    assert.throws(() => validateManagedFiles(paths.map(f)));
  assert.doesNotThrow(() =>
    validateManagedFiles([f("资料/one.txt"), f("资料/two.txt")]),
  );
});
test("file preparation preserves declared order and detects changed bytes before any request", async () => {
  const f = await fixture();
  let requests = 0;
  globalThis.fetch = async () => {
    requests++;
    return asJSON({});
  };
  await assert.rejects(() =>
    pushFiles({
      ...f,
      files: [
        {
          path: "sample.txt",
          role: "primary",
          file: new File(["changed"], "sample.txt"),
        },
      ],
      checkpoint: async () => {},
    }),
  );
  assert.equal(requests, 0);
  await assert.rejects(() =>
    preparePushInput({
      input: f.input,
      files: [
        { path: ".env", role: "source", file: new File(["synthetic"], ".env") },
      ],
    }),
  );
});
test("failure to preserve the initial keys prevents every network write", async () => {
  const f = await fixture();
  let requests = 0;
  globalThis.fetch = async () => {
    requests++;
    return asJSON({});
  };
  await assert.rejects(
    () =>
      pushFiles({
        ...f,
        checkpoint: async () => {
          throw new Error("state save failed");
        },
      }),
    /state save failed/,
  );
  assert.equal(requests, 0);
});
test("lost accepted commit resumes the same operation and key without a second upload/version", async () => {
  const f = await fixture();
  rememberCSRF("synthetic-session", "synthetic-csrf");
  const commits: string[] = [];
  const checkpoints: PushState[] = [];
  let puts = 0;
  let creates = 0;
  let operations = 0;
  let first = true;
  globalThis.fetch = async (url, options) => {
    const path = new URL(String(url), origin).pathname;
    if (path === "/api/v1/uploads") {
      creates++;
      assert.equal(
        new Headers(options?.headers).get("Idempotency-Key"),
        f.state.create_key,
      );
      return asJSON({
        upload_id: upload,
        operation_id: operation,
        project_id: project,
        files: [
          {
            ...f.input.content.files[0],
            state: "pending",
            part_size: raw.length,
            part_count: 1,
            received_parts: [],
            part_url_template: `/xfer/v1/uploads/${upload}/files/${f.input.content.files[0].sha256}/parts/{part_number}`,
          },
        ],
      });
    }
    if (path.startsWith("/xfer/")) {
      puts++;
      assert.equal(options?.redirect, "error");
      assert.equal(options?.credentials, "same-origin");
      assert.equal(
        new Headers(options?.headers).get("X-CSRF-Token"),
        "synthetic-csrf",
      );
      assert.equal(
        new Headers(options?.headers).get("Lantai-Part-Sha256"),
        f.input.content.files[0].sha256,
      );
      return asJSON({});
    }
    if (path.endsWith("/complete")) return asJSON({});
    if (path.startsWith("/api/v1/operations/")) {
      operations++;
      return asJSON({ operation_id: operation });
    }
    if (path.endsWith("/commit")) {
      commits.push(new Headers(options?.headers).get("Idempotency-Key")!);
      if (first) {
        first = false;
        throw new TypeError("connection lost after server acceptance");
      }
      return asJSON(f.receipt);
    }
    throw new Error("unexpected path " + path);
  };
  const checkpoint = async (state: PushState) => {
    checkpoints.push(state);
  };
  await assert.rejects(() => pushFiles({ ...f, checkpoint }));
  assert.equal(f.state.committing, true);
  assert.equal(checkpoints.at(-1)?.committing, true);
  const receipt = await pushFiles({ ...f, checkpoint });
  assert.equal(receipt.version_id, version);
  assert.equal(creates, 1);
  assert.equal(puts, 1);
  assert.equal(operations, 1);
  assert.deepEqual(commits, [f.state.commit_key, f.state.commit_key]);
  const calls = commits.length;
  await pushFiles({ ...f, checkpoint });
  assert.equal(
    commits.length,
    calls,
    "a complete historical receipt is not a second write",
  );
});
test("description pending replays the same commit and preserves its fixed identities", async () => {
  const f = await fixture();
  f.state.upload_id = upload;
  f.state.operation_id = operation;
  f.state.committing = true;
  f.state.result = { ...f.receipt, description_pending: true };
  let commits = 0;
  globalThis.fetch = async (url, options) => {
    if (String(url).includes("/operations/"))
      return asJSON({ operation_id: operation });
    commits++;
    assert.equal(
      new Headers(options?.headers).get("Idempotency-Key"),
      f.state.commit_key,
    );
    return asJSON(f.receipt);
  };
  const r = await pushFiles({ ...f, checkpoint: async () => {} });
  assert.equal(r.version_id, version);
  assert.equal(r.description_pending, undefined);
  assert.equal(commits, 1);
});
test("unsafe upload geometry/foreign URLs stop before credentials or payload are sent", async () => {
  const f = await fixture();
  rememberCSRF("s", "csrf");
  let transfers = 0;
  globalThis.fetch = async (url) => {
    if (String(url).includes("/xfer/")) {
      transfers++;
      throw new Error("should not transfer");
    }
    return asJSON({
      upload_id: upload,
      operation_id: operation,
      project_id: project,
      files: [
        {
          ...f.input.content.files[0],
          state: "pending",
          part_size: raw.length,
          part_count: 1,
          received_parts: [],
          part_url_template: `https://foreign.example/xfer/v1/uploads/${upload}/files/${f.input.content.files[0].sha256}/parts/{part_number}`,
        },
      ],
    });
  };
  await assert.rejects(() => pushFiles({ ...f, checkpoint: async () => {} }));
  assert.equal(transfers, 0);
  for (const url of [
    `https://foreign.example/xfer/v1/reads/${grant}?sig=${"a".repeat(64)}`,
    `/xfer/v1/reads/${grant}?sig=a&sig=b`,
    `/xfer/v1/reads/${grant}?sig=${"a".repeat(64)}&redirect=evil`,
  ])
    assert.throws(() => managedTransferURL(url, "read"));
});

async function remoteFixture() {
  const f = await fixture();
  const file = f.input.content.files[0];
  let grants = 0;
  let reads = 0;
  const exact = {
    version: f.receipt,
    manifest: {
      contract: "lantai.manifest/v1",
      instance_id: instance,
      project_id: project,
      asset_id: asset,
      version_id: version,
      version_number: 1,
      manifest_digest: f.manifestDigest,
      content: f.input.content,
    },
  };
  globalThis.fetch = async (url) => {
    const path = new URL(String(url), origin).pathname;
    if (path === "/api/v1/meta")
      return asJSON({ api_version: "v1", instance_id: instance });
    if (path.endsWith("/read-grants")) {
      grants++;
      return asJSON({
        grant_id: grant,
        project_id: project,
        asset_id: asset,
        version_id: version,
        path: file.path,
        sha256: file.sha256,
        size: file.size,
        purpose: "archive_review",
        method: "GET",
        expires_at: new Date(Date.now() + 30000).toISOString(),
        url: `/xfer/v1/reads/${grant}?sig=${"a".repeat(64)}`,
      });
    }
    if (path.startsWith("/xfer/")) {
      reads++;
      return new Response(raw, {
        headers: { "Content-Length": String(raw.length) },
      });
    }
    return asJSON(exact);
  };
  return { ...f, exact, calls: () => ({ grants, reads }) };
}
test("current verification rechecks the exact remote even with identical local files", async () => {
  const f = await remoteFixture();
  const receipt = await verifyExactFiles({
    assetID: asset,
    versionID: version,
    expectedFiles: f.input.content.files,
    localFiles: f.files,
  });
  assert.equal(receipt.current_remote_verified, true);
  assert.equal(receipt.local_source_verified, true);
  assert.deepEqual(f.calls(), { grants: 1, reads: 1 });
  globalThis.fetch = async () => asJSON({ error: { code: "NOT_FOUND" } }, 404);
  await assert.rejects(() =>
    verifyExactFiles({
      assetID: asset,
      versionID: version,
      expectedFiles: f.input.content.files,
      localFiles: f.files,
    }),
  );
});
test("wrong instance, manifest digest or expected file set fails before a read grant", async () => {
  for (const mutate of [
    (f: Awaited<ReturnType<typeof remoteFixture>>) => {
      f.exact.manifest.instance_id = project;
    },
    (f: Awaited<ReturnType<typeof remoteFixture>>) => {
      f.exact.manifest.content.version_note = "changed";
    },
  ]) {
    const f = await remoteFixture();
    mutate(f);
    await assert.rejects(() =>
      getExactManagedVersion({ assetID: asset, versionID: version }),
    );
    assert.deepEqual(f.calls(), { grants: 0, reads: 0 });
  }
  const f = await remoteFixture();
  await assert.rejects(() =>
    verifyExactFiles({
      assetID: asset,
      versionID: version,
      expectedFiles: [{ ...f.input.content.files[0], role: "source" }],
    }),
  );
  assert.deepEqual(f.calls(), { grants: 0, reads: 0 });
});
test("grant mismatch and oversized stream fail closed and do not return a successful receipt", async () => {
  const f = await remoteFixture();
  const fetch = globalThis.fetch;
  globalThis.fetch = async (url, options) => {
    const response = await fetch(url, options);
    if (String(url).includes("/read-grants")) {
      const grant = await response.json();
      grant.purpose = "raw_export";
      return asJSON(grant);
    }
    return response;
  };
  await assert.rejects(() =>
    readManagedFile({
      assetID: asset,
      versionID: version,
      file: f.input.content.files[0],
    }),
  );
  assert.equal(f.calls().reads, 0);
  globalThis.fetch = async (url, options) =>
    String(url).includes("/xfer/")
      ? new Response(new Uint8Array(raw.length + 1))
      : fetch(url, options);
  await assert.rejects(() =>
    readManagedFile({
      assetID: asset,
      versionID: version,
      file: f.input.content.files[0],
    }),
  );
});
test("cancellation after initial checkpoint never starts an upload", async () => {
  const f = await fixture();
  const controller = new AbortController();
  let writes = 0;
  globalThis.fetch = async () => {
    writes++;
    return asJSON({});
  };
  await assert.rejects(
    () =>
      pushFiles({
        ...f,
        signal: controller.signal,
        checkpoint: async () => {
          controller.abort();
        },
      }),
    { name: "AbortError" },
  );
  assert.equal(writes, 0);
});

test("imported receipts reject credential fields and invalid public metadata", async () => {
  const f = await fixture();
  for (const extra of [
    { access_token: "synthetic-secret" },
    { description_error: "https://secret.example" },
    { description_pending: "yes" },
  ]) {
    const state = {
      ...f.state,
      upload_id: upload,
      operation_id: operation,
      committing: true,
      result: { ...f.receipt, ...extra },
    } as PushState;
    await assert.rejects(() => validatePushState(f.input, state), /提交回执/);
  }
  await assert.doesNotReject(() =>
    validatePushState(f.input, {
      ...f.state,
      upload_id: upload,
      operation_id: operation,
      committing: true,
      result: {
        ...f.receipt,
        description_pending: true,
        description_error: "FORBIDDEN",
      },
    }),
  );
});

test("frozen verification binding rejects a self-consistent different instance or altered manifest before grants", async () => {
  for (const change of ["instance", "manifest", "project", "number"] as const) {
    const f = await remoteFixture();
    const expectedIdentity = {
      instance_id: instance,
      project_id: project,
      asset_id: asset,
      version_id: version,
      manifest_digest: f.manifestDigest,
      version_number: 1,
      uri: f.receipt.uri,
    };
    if (change === "manifest") {
      f.exact.manifest.content.version_note = "changed but same file bytes";
      const digest = `sha256:${await sha256(canonicalJSON(f.exact.manifest.content))}`;
      f.exact.manifest.manifest_digest = digest;
      f.exact.version.manifest_digest = digest;
    }
    if (change === "project") {
      f.exact.version.project_id = upload;
      f.exact.manifest.project_id = upload;
    }
    if (change === "number") {
      f.exact.version.version_number = 2;
      f.exact.manifest.version_number = 2;
    }
    if (change === "instance") {
      f.exact.manifest.instance_id = upload;
      f.exact.version.uri = `lantai://${upload}/assets/${asset}/versions/${version}`;
      const fetch = globalThis.fetch;
      globalThis.fetch = async (url, options) =>
        String(url).endsWith("/meta")
          ? asJSON({ api_version: "v1", instance_id: upload })
          : fetch(url, options);
    }
    await assert.rejects(
      () =>
        verifyExactFiles({
          assetID: asset,
          versionID: version,
          expectedFiles: f.input.content.files,
          expectedIdentity,
        }),
      /原工作区/,
    );
    assert.deepEqual(f.calls(), { grants: 0, reads: 0 });
  }
});
test("real commit fields are accepted while nested credential fields and changed permanent refs are rejected", async () => {
  const f = await fixture();
  const receipt = {
    ...f.receipt,
    slug: f.input.slug,
    alias_generation: 1,
    ref: { instance_id: instance, asset_id: asset, version_id: version },
    description: {
      asset_id: asset,
      project_id: project,
      title: "合成",
      asset_type: "doc",
      slug: f.input.slug,
      tags: [],
      subjects: [],
      sensitivity: "normal",
      revision: 1,
      updated_by: instance,
    },
  };
  const state = {
    ...f.state,
    upload_id: upload,
    operation_id: operation,
    committing: true,
    result: receipt,
  };
  await assert.doesNotReject(() => validatePushState(f.input, state));
  await assert.rejects(() =>
    validatePushState(f.input, {
      ...state,
      result: { ...receipt, ref: { ...receipt.ref, version_id: upload } },
    }),
  );
  await assert.rejects(
    () =>
      validatePushState(f.input, {
        ...state,
        result: {
          ...receipt,
          description: {
            ...receipt.description,
            extra: { access_token: "synthetic-only" },
          },
        },
      }),
    /凭据/,
  );
});
test("native RawMessage lexical hashes are not rewritten into browser recovery state", async () => {
  const nativeInput = parseManagedJSON(
    '{"project_id":"01J00000000000000000000001","asset_id":"01J00000000000000000000002","base_version_id":"01J00000000000000000000003","content":{"asset_type":"doc","files":[{"path":"sample.txt","role":"primary","sha256":"9bb32dc90f1c4ac12b36f31e9d527389d727d4d814d9eb006c9152a258c5c81b","size":24}],"metadata":{"n":1.0}}}',
  ) as Parameters<typeof newPushState>[0]["input"];
  const browser = await newPushState({ input: nativeInput });
  const original = {
    ...browser,
    request_hash:
      "7034dc9fd1627b86e5c6b3f4d8cd2fcf2ebd94da218eda4b1d588b5105820d02",
  };
  await assert.rejects(
    () => validatePushState(nativeInput, original),
    /原生 RawMessage/,
  );
  assert.equal(
    original.request_hash,
    "7034dc9fd1627b86e5c6b3f4d8cd2fcf2ebd94da218eda4b1d588b5105820d02",
  );
  assert.equal(original.create_key, browser.create_key);
  assert.equal(original.commit_key, browser.commit_key);
});
