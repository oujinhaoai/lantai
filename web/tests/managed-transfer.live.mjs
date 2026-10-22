// Opt-in live adapter integration, launched only by TestWebManagedTransferLiveHTTPS.
// All API and transfer responses come from the real temporary Go application.
import assert from "node:assert/strict";
import process from "node:process";
import tls from "node:tls";
import { api, rememberCSRF } from "../src/api.ts";
import { readAssetFile } from "../src/asset-read.ts";
import {
  getExactManagedVersion,
  newPushState,
  preparePushInput,
  pushFiles,
  readManagedFile,
  sha256,
  verifyExactFiles,
} from "../src/managed-transfer.ts";
import {
  loadManagedRecord,
  managedRecordResume,
  prepareManagedRecord,
  restoreManagedRecordResume,
  saveManagedRecord,
} from "../src/managed-records.ts";

let phase = "configuration";
try {
  let text = "";
  for await (const chunk of process.stdin) text += chunk;
  const config = JSON.parse(text);
  text = "";
  const origin = new globalThis.URL(config.origin);
  assert.equal(origin.protocol, "https:");
  assert.equal(origin.hostname, "127.0.0.1");
  assert.equal(process.env.NODE_TLS_REJECT_UNAUTHORIZED, "1");
  assert.ok(process.env.NODE_EXTRA_CA_CERTS);
  // The existing identity/storage fixture has a fixed test clock. Match its
  // browser-side grant-expiry observation without changing TLS's real clock.
  assert.ok(Number.isSafeInteger(config.clock_ms));
  const SystemDate = Date;
  globalThis.Date = class extends SystemDate {
    constructor(...args) {
      super(...(args.length ? args : [config.clock_ms]));
    }
    static now() {
      return config.clock_ms;
    }
  };
  Object.defineProperty(globalThis, "location", {
    value: origin,
    configurable: true,
  });
  // Per-tab memory only. No session credential is stored through this adapter.
  const tab = new Map();
  globalThis.sessionStorage = {
    getItem: (key) => tab.get(key) ?? null,
    setItem: (key, value) => tab.set(key, value),
    removeItem: (key) => tab.delete(key),
  };

  phase = "strict TLS negatives";
  const connectTLS = (extra) =>
    new Promise((resolve, reject) => {
      const socket = tls.connect({
        host: origin.hostname,
        port: Number(origin.port),
        rejectUnauthorized: true,
        ...extra,
      });
      socket.setTimeout(5000, () =>
        socket.destroy(new Error("synthetic TLS test timeout")),
      );
      socket.once("secureConnect", () => {
        socket.end();
        resolve();
      });
      socket.once("error", reject);
    });
  await assert.rejects(connectTLS({ ca: [] }), (error) =>
    [
      "DEPTH_ZERO_SELF_SIGNED_CERT",
      "SELF_SIGNED_CERT_IN_CHAIN",
      "UNABLE_TO_VERIFY_LEAF_SIGNATURE",
    ].includes(error.code),
  );
  await assert.rejects(
    connectTLS({ servername: "wrong-host.synthetic.invalid" }),
    (error) => error.code === "ERR_TLS_CERT_ALTNAME_INVALID",
  );

  const nativeFetch = globalThis.fetch;
  let cookie = "",
    csrf = "",
    loseCommitResponse = false;
  const requests = [];
  globalThis.fetch = async (path, init = {}) => {
    const url = new globalThis.URL(path, origin);
    assert.equal(
      url.origin,
      origin.origin,
      "browser transport must stay same-origin",
    );
    assert.ok(
      url.pathname.startsWith("/api/v1/") ||
        url.pathname.startsWith("/xfer/v1/"),
    );
    assert.equal(init.credentials, "same-origin");
    assert.equal(init.redirect, "error");
    const method = init.method ?? "GET";
    const headers = new globalThis.Headers(init.headers);
    assert.equal(headers.has("Authorization"), false);
    if (cookie) headers.set("Cookie", cookie);
    headers.set("Origin", origin.origin);
    if (
      !["GET", "HEAD"].includes(method) &&
      url.pathname !== "/api/v1/sessions/login"
    )
      assert.equal(
        headers.get("X-CSRF-Token"),
        csrf,
        "real adapter must attach the current tab CSRF",
      );
    // No signed URL, credential, cookie or request body is recorded here.
    requests.push({
      method,
      path: url.pathname,
      key: headers.get("Idempotency-Key"),
    });
    const response = await nativeFetch(url, { ...init, headers });
    if (url.pathname === "/api/v1/sessions/login") {
      const sessionCookie = response.headers
        .getSetCookie()
        .find((value) => value.startsWith("lantai_session="));
      assert.ok(
        sessionCookie,
        "real browser login must issue an HttpOnly session cookie",
      );
      assert.match(sessionCookie, /;\s*HttpOnly/i);
      assert.match(sessionCookie, /;\s*Secure/i);
      cookie = sessionCookie.split(";", 1)[0];
    }
    if (
      loseCommitResponse &&
      method === "POST" &&
      /^\/api\/v1\/uploads\/[^/]+\/commit$/.test(url.pathname)
    ) {
      loseCommitResponse = false;
      assert.equal(response.status, 201);
      // Drain the real successful response, then withhold it from the adapter:
      // the server side effect is complete, but the caller sees an unknown.
      await response.arrayBuffer();
      throw new TypeError("synthetic loss of the accepted commit response");
    }
    return response;
  };

  phase = "real cookie login";
  const session = await api("/api/v1/sessions/login", {
    method: "POST",
    body: JSON.stringify({
      name: config.name,
      password: config.password,
      code: config.code,
      channel: "browser",
      scopes: ["read", "ingest", "organize"],
      ttl_seconds: 86400,
    }),
  });
  delete config.password;
  delete config.code;
  assert.equal(session.token, undefined);
  assert.ok(session.session?.session_id && session.csrf_token && cookie);
  csrf = session.csrf_token;
  rememberCSRF(session.session.session_id, csrf);
  const meta = await api("/api/v1/meta");
  assert.equal(meta.instance_id, config.instance_id);

  const count = (method, path, key) =>
    requests.filter(
      (request) =>
        request.method === method &&
        request.path === path &&
        request.key === key,
    ).length;
  const originalKeys = (state) => ({
    create: state.create_key,
    commit: state.commit_key,
    hash: state.request_hash,
  });
  const cases = [];
  for (const name of ["normal", "metadata-null", "unicode-html"]) {
    phase = `${name} prepare/push`;
    const contentText = `synthetic ${name} 汉字 & <>\n`.repeat(
      name === "normal" ? 4096 : 3,
    );
    const files = [
      {
        path: "sample.txt",
        role: "primary",
        file: new globalThis.File([contentText], "sample.txt", {
          type: "text/plain",
        }),
      },
    ];
    const content = {
      asset_type: "doc",
      files: [],
      rights: {
        usage: "reference",
        license: "LicenseRef-Synthetic-Live",
        redistribute_raw: false,
        noai: true,
        sensitivity: "normal",
      },
    };
    if (name === "metadata-null") content.metadata = null;
    if (name === "unicode-html") {
      content.version_note = "合成 é 🧪 & <note>\u2028\u2029";
      content.metadata = {
        title: "中文 & <标题>",
        extra: { synthetic: "é 🧪 & <>\u2028\u2029" },
      };
    }
    const input = await preparePushInput({
      input: {
        project_id: config.project_id,
        slug: `live/${name}`,
        content,
        describe: { title: `合成 ${name} & <>` },
      },
      files,
    });
    let state = await newPushState({ input });
    const keys = originalKeys(state);
    let persisted;
    let stopAfterCreate = name === "normal";
    const checkpoint = async (value) => {
      persisted = globalThis.structuredClone(value);
      if (stopAfterCreate && value.upload_id && !value.committing) {
        stopAfterCreate = false;
        throw new Error("synthetic stop after durable upload identity");
      }
    };
    if (name === "normal") {
      await assert.rejects(
        pushFiles({ input, files, state, checkpoint }),
        /synthetic stop/,
      );
      assert.ok(persisted.upload_id && persisted.operation_id);
      state = globalThis.structuredClone(persisted);
    } else if (name === "unicode-html") {
      loseCommitResponse = true;
      await assert.rejects(
        pushFiles({ input, files, state, checkpoint }),
        (error) => error.code === "NETWORK_ERROR",
      );
      assert.ok(
        persisted.committing && persisted.operation_id && !persisted.result,
      );
      state = globalThis.structuredClone(persisted);
    }
    const receipt = await pushFiles({ input, files, state, checkpoint });
    assert.equal(receipt.version_number, 1);
    assert.equal(receipt.description_pending, undefined);
    assert.deepEqual(originalKeys(state), keys);
    assert.equal(count("POST", "/api/v1/uploads", state.create_key), 1);
    const beforeReplay = requests.length;
    assert.deepEqual(
      await pushFiles({ input, files, state, checkpoint }),
      receipt,
    );
    assert.equal(
      requests.length,
      beforeReplay,
      "completed original state must not create or commit again",
    );
    phase = `${name} exact-byte verification`;
    const verification = await verifyExactFiles({
      assetID: receipt.asset_id,
      versionID: receipt.version_id,
      expectedFiles: input.content.files,
      localFiles: files,
      purpose: "archive_review",
    });
    assert.equal(verification.current_remote_verified, true);
    assert.equal(verification.local_source_verified, true);
    assert.equal(verification.instance_id, config.instance_id);
    assert.equal(verification.manifest_digest, receipt.manifest_digest);
    phase = `${name} asset-read exact-byte verification`;
    const toolRead = await readAssetFile({
      assetID: receipt.asset_id,
      versionID: receipt.version_id,
      file: input.content.files[0],
      purpose: "archive_review",
      byteLimit: 256 * 1024,
      signal: new globalThis.AbortController().signal,
    });
    assert.deepEqual(
      toolRead.bytes,
      new globalThis.TextEncoder().encode(contentText),
    );
    assert.equal(await sha256(toolRead.bytes), input.content.files[0].sha256);
    assert.ok(toolRead.expires > Date.now());
    cases.push({
      name,
      input,
      state,
      asset_id: receipt.asset_id,
      version_id: receipt.version_id,
      asset_read_sha256: await sha256(toolRead.bytes),
      commit_posts: count(
        "POST",
        `/api/v1/uploads/${state.upload_id}/commit`,
        state.commit_key,
      ),
    });
  }

  phase = "management record prepare/save/load";
  const payload = {
    schema: "synthetic.live-record/v1",
    original_operation: {
      create_key: "synthetic-original-create",
      commit_key: "synthetic-original-commit",
      operation_id: cases[0].state.operation_id,
    },
    selected: { asset_id: cases[0].asset_id, version_id: cases[0].version_id },
    observation: "合成 & <> é 🧪",
    unknown_after_capture: true,
  };
  const options = {
    projectID: config.project_id,
    title: "合成管理档案 & <>",
    kind: "workset",
    payload,
  };
  let prepared = await prepareManagedRecord(options);
  const recordKeys = originalKeys(prepared.state);
  const checkpoint = async (value) => {
    prepared.state = globalThis.structuredClone(value);
  };
  const record = await saveManagedRecord({ ...options, prepared, checkpoint });
  assert.equal(record.description_pending, undefined);
  prepared = await restoreManagedRecordResume(
    JSON.stringify(managedRecordResume(prepared)),
    options,
  );
  assert.deepEqual(originalKeys(prepared.state), recordKeys);
  assert.deepEqual(
    await saveManagedRecord({ ...options, prepared, checkpoint }),
    record,
  );
  assert.equal(count("POST", "/api/v1/uploads", prepared.state.create_key), 1);
  const loaded = await loadManagedRecord(record, undefined, {
    kind: "workset",
    projectID: config.project_id,
  });
  assert.deepEqual(JSON.parse(JSON.stringify(loaded)), payload);
  const exact = await getExactManagedVersion({
    assetID: record.asset_id,
    versionID: record.version_id,
  });
  assert.equal(exact.version.version_number, 1);
  assert.deepEqual(exact.manifest.content.rights, {
    usage: "reference",
    license: "LicenseRef-Lantai-Private-Record",
    redistribute_raw: false,
    noai: true,
    sensitivity: "normal",
  });
  for (const purpose of ["raw_export", "generative_input"]) {
    await assert.rejects(
      readManagedFile({
        assetID: record.asset_id,
        versionID: record.version_id,
        file: exact.manifest.content.files[0],
        purpose,
      }),
      (error) => error.code === "USE_RESTRICTED",
    );
  }
  phase = "asset-read private record export denied";
  const beforeExport = requests.length;
  await assert.rejects(
    readAssetFile({
      assetID: record.asset_id,
      versionID: record.version_id,
      file: exact.manifest.content.files[0],
      purpose: "raw_export",
      byteLimit: 2 * 1024 * 1024,
      signal: new globalThis.AbortController().signal,
    }),
    (error) => error.code === "USE_RESTRICTED",
  );
  assert.equal(
    requests.length,
    beforeExport + 1,
    "asset-read must not fall back or request transfer bytes after export denial",
  );
  assert.equal(requests[beforeExport].method, "POST");
  assert.equal(
    requests[beforeExport].path,
    `/api/v1/assets/${record.asset_id}/versions/${record.version_id}/read-grants`,
  );
  const verifiedRecord = await verifyExactFiles({
    assetID: record.asset_id,
    versionID: record.version_id,
    expectedFiles: prepared.input.content.files,
    localFiles: prepared.files,
  });
  assert.equal(verifiedRecord.current_remote_verified, true);
  assert.equal(verifiedRecord.local_source_verified, true);

  process.stdout.write(
    JSON.stringify({
      schema: "lantai.web-managed-transfer-live/v1",
      tls: {
        trusted_certificate: true,
        untrusted_rejected: true,
        wrong_hostname_rejected: true,
      },
      cases,
      record: {
        name: "management-record",
        input: prepared.input,
        state: prepared.state,
        asset_id: record.asset_id,
        version_id: record.version_id,
        commit_posts: count(
          "POST",
          `/api/v1/uploads/${prepared.state.upload_id}/commit`,
          prepared.state.commit_key,
        ),
      },
    }) + "\n",
  );
} catch (error) {
  // Never dump login input, session cookies, CSRF or signed transfer URLs.
  process.stderr.write(
    JSON.stringify({
      phase,
      code: error.code ?? error.name,
      status: error.status,
      message: String(error.message).slice(0, 1000),
    }) + "\n",
  );
  process.exitCode = 1;
}
