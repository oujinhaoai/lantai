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
  sha256,
} from "../src/managed-transfer.ts";
import {
  imageSize,
  parseOBJ,
  mediaToolFormat,
  metadataPatch,
} from "../src/asset-tools.ts";
import { emptyProfile, profileFromDraft } from "../src/asset-profile.ts";
import {
  getVersionRights,
  emptyRightsDraft,
  rightsAssertion,
  prepareRightsApproval,
  rightsEvidence,
} from "../src/asset-rights.ts";
import { executeApproval } from "../src/system-operations.ts";

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
      scopes: ["read", "ingest", "organize", "admin"],
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

  const cases = [];
  const initialProfile = {
    ...emptyProfile(),
    characters: "synthetic-character",
    source: "synthetic-source",
    business_state: "pending_review",
    legacy_state: "awaiting-human",
    legacy_version: "v001",
    source_paths: "/synthetic/inert",
    notes: "generated fixture",
  };
  for (const fixture of config.fixtures) {
    phase = `${fixture.name} real fixed-byte upload/read`;
    const bytes = Uint8Array.from(
      globalThis.Buffer.from(fixture.base64, "base64"),
    );
    const files = [
      {
        path: fixture.name,
        role: "primary",
        file: new globalThis.File([bytes], fixture.name),
      },
    ];
    const description = metadataPatch({
      title: `synthetic ${fixture.name}`,
      summary: "",
      tags: "freeform",
      subjects: "",
      profile: initialProfile,
    });
    const input = await preparePushInput({
      input: {
        project_id: config.project_id,
        slug: `media/${fixture.name}`,
        content: {
          asset_type: fixture.name.endsWith(".obj") ? "model" : "image",
          files: [],
          rights: {
            usage: "production",
            license: "CC0-1.0",
            redistribute_raw: fixture.name !== "synthetic.png",
            noai: false,
            sensitivity: "normal",
          },
          metadata: {
            extra: {
              asset_profile: profileFromDraft(initialProfile),
              unknown_custom: { counter: "9007199254740993", kept: true },
            },
          },
        },
        describe: {
          title: description.title,
          summary: description.summary,
          tags: description.tags,
          subjects: description.subjects,
          extra: {
            asset_profile: description.profile,
            unknown_custom: { counter: "9007199254740993", kept: true },
          },
        },
      },
      files,
    });
    const state = await newPushState({ input });
    const receipt = await pushFiles({
      input,
      files,
      state,
      checkpoint: async () => {},
    });
    const exact = await getExactManagedVersion({
      assetID: receipt.asset_id,
      versionID: receipt.version_id,
    });
    const original = await readAssetFile({
      assetID: receipt.asset_id,
      versionID: receipt.version_id,
      file: input.content.files[0],
      purpose: "archive_review",
      byteLimit: 16 * 1024 * 1024,
      signal: new globalThis.AbortController().signal,
    });
    assert.deepEqual(original.bytes, bytes);
    assert.equal(await sha256(original.bytes), input.content.files[0].sha256);
    const format = mediaToolFormat(fixture.name);
    if (format === "image") {
      const size = imageSize(original.bytes);
      assert.equal(size.width, fixture.width);
      assert.equal(size.height, fixture.height);
    } else {
      const model = parseOBJ(
        new globalThis.TextDecoder("utf-8", { fatal: true }).decode(
          original.bytes,
        ),
      );
      assert.equal(model.vertices.length, 3);
      assert.equal(model.edges.length, 3);
      assert.equal(model.materialLibraries.length, 1);
    }
    if (fixture.name !== "synthetic.png") {
      const exported = await readAssetFile({
        assetID: receipt.asset_id,
        versionID: receipt.version_id,
        file: input.content.files[0],
        purpose: "raw_export",
        byteLimit: 16 * 1024 * 1024,
        signal: new globalThis.AbortController().signal,
      });
      assert.equal(await sha256(exported.bytes), input.content.files[0].sha256);
    }
    cases.push({
      name: fixture.name,
      input,
      state,
      asset_id: receipt.asset_id,
      version_id: receipt.version_id,
      manifest_digest: receipt.manifest_digest,
      version: exact.version,
    });
  }
  assert.equal(mediaToolFormat("synthetic.glb"), "unsupported-3d");
  assert.equal(mediaToolFormat("synthetic.fbx"), "unsupported-3d");
  assert.equal(mediaToolFormat("synthetic.svg"), "none");
  const selected = cases[0];
  const readExport = () =>
    readAssetFile({
      assetID: selected.asset_id,
      versionID: selected.version_id,
      file: selected.input.content.files[0],
      purpose: "raw_export",
      byteLimit: 16 * 1024 * 1024,
      signal: new globalThis.AbortController().signal,
    });
  phase =
    "conditional structured profile preserves unknown extra and never grants use";
  const current = await api(`/api/v1/assets/${selected.asset_id}?view=full`);
  const patch = metadataPatch({
    title: current.description.title,
    summary: "structured update",
    tags: current.description.tags.join("\n"),
    subjects: current.description.subjects.join("\n"),
    profile: {
      ...initialProfile,
      source: "updated-source",
      business_state: "active",
      notes: "new descriptive note",
    },
  });
  const options = {
    method: "PATCH",
    headers: {
      "If-Match": `"${current.description.revision}"`,
      "Idempotency-Key": "synthetic-profile-original",
    },
    body: JSON.stringify(patch),
  };
  const updated = await api(
    `/api/v1/assets/${selected.asset_id}/metadata`,
    options,
  );
  assert.equal(updated.extra.unknown_custom.counter, "9007199254740993");
  assert.equal(updated.extra.asset_profile.source, "updated-source");
  const replay = await api(
    `/api/v1/assets/${selected.asset_id}/metadata`,
    options,
  );
  assert.equal(replay.revision, updated.revision);
  await assert.rejects(
    api(`/api/v1/assets/${selected.asset_id}/metadata`, {
      ...options,
      headers: {
        ...options.headers,
        "Idempotency-Key": "synthetic-profile-stale",
      },
      body: JSON.stringify({ ...patch, title: "must not overwrite" }),
    }),
    (e) => e.status === 412,
  );
  const countBeforeDenied = requests.length;
  await assert.rejects(readExport(), (e) => e.code === "USE_RESTRICTED");
  assert.equal(
    requests.length,
    countBeforeDenied + 1,
    "export refusal must not fetch content or use archive_review fallback",
  );
  const fixedAfterProfile = await getExactManagedVersion({
    assetID: selected.asset_id,
    versionID: selected.version_id,
  });
  assert.equal(
    fixedAfterProfile.manifest.content.metadata.extra.asset_profile.source,
    profileFromDraft(initialProfile).source,
  );
  assert.equal(
    fixedAfterProfile.version.manifest_digest,
    selected.manifest_digest,
  );
  phase = "actual accepted provenance evidence and monotone restriction";
  let snapshot = await getVersionRights(selected.version);
  const proof1 = await api("/api/v1/rights/evidence", {
    method: "POST",
    headers: { "Idempotency-Key": "synthetic-rights-evidence-1" },
    body: JSON.stringify(
      rightsEvidence(snapshot, "initial synthetic accepted evidence", ""),
    ),
  });
  snapshot = await getVersionRights(selected.version);
  const restriction = rightsAssertion(snapshot, {
    ...emptyRightsDraft(),
    noai: "true",
    evidence_ids: proof1.record_id,
    reason: "synthetic monotone no-AI restriction",
  });
  await api("/api/v1/rights/assertions", {
    method: "POST",
    headers: { "Idempotency-Key": "synthetic-rights-restrict" },
    body: JSON.stringify(restriction),
  });
  snapshot = await getVersionRights(selected.version);
  assert.equal(snapshot.rights.noai, true);
  assert.equal(
    snapshot.purposes.find((p) => p.purpose === "generative_input").allowed,
    false,
  );
  const lower = rightsAssertion(snapshot, {
    ...emptyRightsDraft(),
    noai: "false",
    evidence_ids: proof1.record_id,
    reason: "ordinary writes cannot loosen",
  });
  await assert.rejects(
    api("/api/v1/rights/assertions", {
      method: "POST",
      headers: { "Idempotency-Key": "synthetic-forbidden-lowering" },
      body: JSON.stringify(lower),
    }),
    (e) => e.code === "HUMAN_PROOF_REQUIRED",
  );
  phase = "new evidence and exact human correction release";
  const proof2 = await api("/api/v1/rights/evidence", {
    method: "POST",
    headers: { "Idempotency-Key": "synthetic-rights-evidence-2" },
    body: JSON.stringify(
      rightsEvidence(
        snapshot,
        "new synthetic evidence for explicit human release",
        "",
      ),
    ),
  });
  snapshot = await getVersionRights(selected.version);
  let approval = await prepareRightsApproval(
    snapshot,
    {
      ...emptyRightsDraft(),
      kind: "release",
      noai: "false",
      redistribute_raw: "true",
      evidence_ids: proof2.record_id,
      reason: "new evidence and explicit synthetic owner approval",
    },
    config.project_id,
    api,
  );
  await executeApproval(approval, config.approval_code, api, (next) => {
    approval = next;
  });
  delete config.approval_code;
  snapshot = await getVersionRights(selected.version);
  assert.equal(snapshot.rights.noai, false);
  assert.equal(snapshot.rights.redistribute_raw, true);
  assert.equal(
    snapshot.purposes.find((p) => p.purpose === "raw_export").allowed,
    true,
  );
  assert.equal(
    await sha256((await readExport()).bytes),
    selected.input.content.files[0].sha256,
  );
  const fixedAfterRights = await getExactManagedVersion({
    assetID: selected.asset_id,
    versionID: selected.version_id,
  });
  assert.equal(
    fixedAfterRights.version.manifest_digest,
    selected.manifest_digest,
  );
  assert.equal(
    fixedAfterRights.manifest.content.rights.redistribute_raw,
    false,
  );
  process.stdout.write(
    JSON.stringify({
      schema: "lantai.web-media-profile-rights-live/v1",
      cases,
      tls: {
        trusted_certificate: true,
        untrusted_rejected: true,
        wrong_hostname_rejected: true,
      },
      profile_unknown_extra_preserved: true,
      profile_if_match_conflict: true,
      metadata_not_rights: true,
      ordinary_lowering_denied: true,
      human_release: true,
      original_manifest_retained: true,
      browser_executed: false,
    }) + "\n",
  );
} catch (error) {
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
