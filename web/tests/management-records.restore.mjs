// Executed only by TestManagementRecordsCompletePackageRestoreHTTPS. This is
// real TypeScript adapters + real HTTPS Go listeners, never browser acceptance.
import assert from "node:assert/strict";
import process from "node:process";
import tls from "node:tls";
import { api, rememberCSRF } from "../src/api.ts";
import {
  freezeMigration,
  importMigrationWorkspace,
} from "../src/migration-model.ts";
import {
  getExactManagedVersion,
  preparePushInput,
  newPushState,
  pushFiles,
  readManagedFile,
  verifyExactFiles,
} from "../src/managed-transfer.ts";
import {
  prepareManagedRecord,
  saveManagedRecord,
  loadManagedRecord,
  listManagedRecords,
} from "../src/managed-records.ts";
import { analyzeSelectedDependencies } from "../src/dependency-parser.ts";
import {
  planBrowserWorkset,
  pullBrowserWorkset,
} from "../src/workset-helpers.ts";
import {
  prepareWorksetReturn,
  reviseReturnedWorkset,
} from "../src/workset-return.ts";
import { buildRecoveryEvidence } from "../src/recovery-evidence.ts";
import {
  batchFromWorkspace,
  nextBatchRevision,
  validateMigrationBatch,
} from "../src/batch-acceptance-model.ts";

let phase = "configuration";
const {
  URL,
  Headers,
  AbortController,
  TextEncoder,
  TextDecoder,
  File,
  structuredClone,
  setTimeout,
} = globalThis;
try {
  let raw = "";
  for await (const chunk of process.stdin) raw += chunk;
  const config = JSON.parse(raw);
  raw = "";
  const origin = new URL(config.origin);
  assert.equal(origin.protocol, "https:");
  assert.equal(origin.hostname, "127.0.0.1");
  assert.equal(process.env.NODE_TLS_REJECT_UNAUTHORIZED, "1");
  assert.ok(process.env.NODE_EXTRA_CA_CERTS);
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
  const tab = new Map();
  globalThis.sessionStorage = {
    getItem: (key) => tab.get(key) ?? null,
    setItem: (key, value) => tab.set(key, value),
    removeItem: (key) => tab.delete(key),
  };
  const connect = (extra) =>
    new Promise((resolve, reject) => {
      const socket = tls.connect({
        host: origin.hostname,
        port: Number(origin.port),
        rejectUnauthorized: true,
        ...extra,
      });
      socket.setTimeout(5000, () =>
        socket.destroy(new Error("synthetic TLS timeout")),
      );
      socket.once("secureConnect", () => {
        socket.end();
        resolve();
      });
      socket.once("error", reject);
    });
  phase = "strict TLS negatives";
  await assert.rejects(connect({ ca: [] }), (error) =>
    [
      "DEPTH_ZERO_SELF_SIGNED_CERT",
      "SELF_SIGNED_CERT_IN_CHAIN",
      "UNABLE_TO_VERIFY_LEAF_SIGNATURE",
    ].includes(error.code),
  );
  await assert.rejects(
    connect({ servername: "wrong-host.synthetic.invalid" }),
    (error) => error.code === "ERR_TLS_CERT_ALTNAME_INVALID",
  );
  let cookie = "",
    csrf = "";
  const nativeFetch = globalThis.fetch;
  globalThis.fetch = async (path, init = {}) => {
    const url = new URL(path, origin);
    assert.equal(url.origin, origin.origin);
    assert.equal(init.credentials, "same-origin");
    assert.equal(init.redirect, "error");
    const headers = new Headers(init.headers);
    assert.equal(headers.has("Authorization"), false);
    if (cookie) headers.set("Cookie", cookie);
    headers.set("Origin", origin.origin);
    if (
      !["GET", "HEAD"].includes(init.method ?? "GET") &&
      url.pathname !== "/api/v1/sessions/login"
    )
      assert.ok(
        headers.get("X-CSRF-Token") === csrf,
        "real adapter must attach current tab CSRF",
      );
    const response = await nativeFetch(url, { ...init, headers });
    if (url.pathname === "/api/v1/sessions/login" && response.status === 201) {
      const set = response.headers
        .getSetCookie()
        .find((value) => value.startsWith("lantai_session="));
      assert.ok(
        typeof set === "string" &&
          /;\s*HttpOnly/i.test(set) &&
          /;\s*Secure/i.test(set),
        "browser login must issue a Secure HttpOnly session cookie",
      );
      cookie = set.split(";", 1)[0];
    }
    return response;
  };
  const credentials = {
    name: config.name,
    password: config.password,
    code: config.code,
    channel: "browser",
    scopes: ["read", "ingest", "organize", "admin"],
    ttl_seconds: 86400,
  };
  phase = "fresh restored administrator login";
  const login = await api("/api/v1/sessions/login", {
    method: "POST",
    body: JSON.stringify(credentials),
  });
  csrf = login.csrf_token;
  rememberCSRF(login.session.session_id, csrf);
  assert.equal(login.token, undefined);
  assert.ok(cookie);
  delete credentials.password;
  delete credentials.code;
  delete config.password;
  delete config.code;
  const meta = await api("/api/v1/meta");
  const who = await api("/api/v1/whoami");
  assert.equal(meta.instance_id, config.instance_id);
  const signal = new AbortController().signal;
  const read = (path, sig) => api(path, { signal: sig });
  const grant = (entry, file, purpose, sig) =>
    api(
      `/api/v1/assets/${entry.asset_id}/versions/${entry.version_id}/read-grants`,
      {
        method: "POST",
        signal: sig,
        body: JSON.stringify({ path: file.source_path, purpose }),
      },
    );
  const bytes = (string) => new TextEncoder().encode(string);
  const clean = (value) => JSON.parse(JSON.stringify(value));
  const makeFile = (path, text, role = "primary") => ({
    path,
    role,
    file: new File([text], path.split("/").at(-1)),
  });
  const save = async (kind, payload, previous) => {
    const options = {
      projectID: config.project_id,
      title: `Synthetic actual ${kind} record`,
      kind,
      payload,
      ...(previous
        ? { assetID: previous.asset_id, baseVersionID: previous.version_id }
        : {}),
    };
    const prepared = await prepareManagedRecord(options);
    const before = {
      create: prepared.state.create_key,
      commit: prepared.state.commit_key,
    };
    const ref = await saveManagedRecord({
      ...options,
      prepared,
      checkpoint: async (state) => {
        prepared.state = structuredClone(state);
      },
    });
    assert.equal(prepared.state.create_key, before.create);
    assert.equal(prepared.state.commit_key, before.commit);
    const loaded = await loadManagedRecord(ref, signal, {
      kind,
      projectID: config.project_id,
    });
    assert.deepEqual(clean(loaded), clean(payload));
    const exact = await getExactManagedVersion({
      assetID: ref.asset_id,
      versionID: ref.version_id,
    });
    assert.equal(exact.manifest.content.asset_type, "config");
    assert.deepEqual(clean(exact.manifest.content.rights), {
      usage: "reference",
      license: "LicenseRef-Lantai-Private-Record",
      redistribute_raw: false,
      noai: true,
      sensitivity: "normal",
    });
    return {
      kind,
      ref: { asset_id: ref.asset_id, version_id: ref.version_id },
      payload: clean(payload),
      record_sha256: exact.manifest.content.files[0].sha256,
    };
  };
  const checkRecord = async (record) => {
    const loaded = await loadManagedRecord(record.ref, signal, {
      kind: record.kind,
      projectID: config.project_id,
    });
    assert.deepEqual(clean(loaded), record.payload);
    const exact = await getExactManagedVersion({
      assetID: record.ref.asset_id,
      versionID: record.ref.version_id,
    });
    assert.equal(exact.manifest.content.files[0].sha256, record.record_sha256);
    const envelope = JSON.parse(
      new TextDecoder().decode(
        await readManagedFile({
          assetID: record.ref.asset_id,
          versionID: record.ref.version_id,
          file: exact.manifest.content.files[0],
          purpose: "archive_review",
        }),
      ),
    );
    assert.equal(envelope.schema, "lantai.management-record/v1");
    assert.equal(envelope.kind, record.kind);
    assert.equal(envelope.instance_id, config.instance_id);
    assert.equal(envelope.project_id, config.project_id);
    await assert.rejects(
      loadManagedRecord(record.ref, signal, {
        kind: record.kind === "workset" ? "migration" : "workset",
      }),
      /记录类型/,
    );
    await assert.rejects(
      loadManagedRecord(record.ref, signal, {
        projectID: "01J00000000000000000000000",
      }),
      /项目/,
    );
    for (const purpose of ["raw_export", "generative_input"])
      await assert.rejects(
        readManagedFile({
          assetID: record.ref.asset_id,
          versionID: record.ref.version_id,
          file: exact.manifest.content.files[0],
          purpose,
        }),
        (error) => error.code === "USE_RESTRICTED",
      );
    return loaded;
  };
  let oracle;
  if (config.mode === "save") {
    phase = "real migration workspace freeze and upload";
    const obj =
      "# synthetic\nmtllib materials.mtl\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n";
    const mtl = "newmtl Test\nmap_Kd textures/check.png\n";
    const workflow = JSON.stringify({
      1: { class_type: "LoadImage", inputs: { image: "textures/check.png" } },
    });
    const texts = [
      { path: "project/model.obj", bytes: bytes(obj) },
      { path: "project/materials.mtl", bytes: bytes(mtl) },
      { path: "project/workflow.json", bytes: bytes(workflow) },
    ];
    const parsed = await analyzeSelectedDependencies(texts, {
      comfyInputRoot: "project",
    });
    assert.equal(parsed.complete, true);
    assert.equal(parsed.dependencies.length, 3);
    const unsupported = await analyzeSelectedDependencies([
      {
        path: "project/scene.blend",
        bytes: new Uint8Array([66, 76, 69, 78, 68, 69, 82, 0]),
      },
    ]);
    assert.equal(unsupported.complete, false);
    assert.ok(unsupported.files[0].notices.length);
    const files = [
      makeFile("model.obj", obj),
      makeFile("materials.mtl", mtl),
      makeFile("workflow.json", workflow, "recipe"),
      makeFile("textures/check.png", "synthetic texture bytes", "texture"),
    ];
    const draft = {
      schema: "lantai.browser-migration-plan/v1",
      name: "Synthetic actual record restore",
      source_alias: "explicit synthetic files",
      project_id: config.project_id,
      assets: [
        {
          key: "project",
          directory: ".",
          classification: "final",
          push: {
            project_id: config.project_id,
            slug: "records/model",
            content: {
              asset_type: "model",
              files: files.map((file) => ({
                path: file.path,
                role: file.role,
                size: file.file.size,
                sha256: "",
              })),
              rights: {
                usage: "reference",
                license: "LicenseRef-Synthetic-Record-Restore",
                redistribute_raw: false,
                noai: true,
                sensitivity: "normal",
              },
            },
          },
        },
      ],
      excluded: [],
      working_inputs: [],
    };
    const workspace = await freezeMigration({
      plan: draft,
      files,
      instanceID: meta.instance_id,
      origin: origin.origin,
    });
    const first = await pushFiles({
      input: workspace.plan.assets[0].push,
      files,
      state: workspace.states.project,
      checkpoint: async (state) => {
        workspace.states.project = structuredClone(state);
      },
    });
    workspace.rows.project = { status: "committed" };
    const verification = await verifyExactFiles({
      assetID: first.asset_id,
      versionID: first.version_id,
      expectedFiles: workspace.plan.assets[0].push.content.files,
      localFiles: files,
      purpose: "archive_review",
    });
    const entry = {
      asset_id: first.asset_id,
      version_id: first.version_id,
      destination: "project",
    };
    const input = {
      schema: "lantai.client-workset/v1",
      entries: [entry],
      entrypoints: ["project/model.obj", "project/workflow.json"],
      dependencies: parsed.dependencies,
    };
    const plan = await planBrowserWorkset(input, origin.origin, read, signal);
    const migration = {
      schema: "lantai.migration-record/v1",
      workspace,
      verifications: { project: verification },
      recovery_refs: [],
    };
    const workset = {
      schema: "lantai.managed-workset/v1",
      title: "Synthetic fixed original workset",
      input,
      remote_check: plan,
    };
    const info = await api("/api/v1/admin/instance");
    assert.ok(
      info.recovery_receipts.length > 0,
      "actual core recovery receipt required",
    );
    const recovery = buildRecoveryEvidence(
      info,
      info.recovery_receipts.at(-1).run_id,
      [verification],
    );
    phase = "save actual migration/workset/recovery management envelopes";
    const records = [
      await save("migration", migration),
      await save("workset", workset),
      await save("recovery", recovery),
    ];
    phase = "explicit workset return new version and stale base rejection";
    const changed = files.map((file) =>
      file.path === "model.obj"
        ? makeFile(file.path, obj + "# returned new content\n", file.role)
        : file,
    );
    const options = {
      entry,
      files: changed,
      purpose: "reference",
      usage: "reference",
      note: "Synthetic explicit return",
    };
    const prepared = await prepareWorksetReturn(options);
    const competing = await prepareWorksetReturn({
      ...options,
      note: "Synthetic competing return",
    });
    const originalKeys = {
      create: prepared.state.create_key,
      commit: prepared.state.commit_key,
    };
    const resumed = await prepareWorksetReturn({
      ...options,
      resume: JSON.stringify(prepared),
    });
    assert.equal(resumed.state.create_key, originalKeys.create);
    const returned = await pushFiles({
      input: prepared.input,
      files: changed,
      state: prepared.state,
      checkpoint: async (state) => {
        prepared.state = structuredClone(state);
      },
    });
    assert.equal(returned.version_number, 2);
    assert.equal(prepared.state.commit_key, originalKeys.commit);
    assert.equal(input.entries[0].version_id, first.version_id);
    await assert.rejects(
      pushFiles({
        input: competing.input,
        files: changed,
        state: competing.state,
        checkpoint: async (state) => {
          competing.state = structuredClone(state);
        },
      }),
      (error) => error.code === "BASE_VERSION_CONFLICT",
    );
    assert.ok(competing.state.upload_id && competing.state.committing);
    assert.equal(competing.state.result, undefined);
    const revised = reviseReturnedWorkset(input, entry, returned);
    assert.equal(revised.entries[0].version_id, returned.version_id);
    const revisedPlan = await planBrowserWorkset(
      revised,
      origin.origin,
      read,
      signal,
    );
    records.push(
      await save(
        "workset",
        {
          schema: "lantai.managed-workset/v1",
          title: "Explicit newer workset revision",
          input: revised,
          remote_check: revisedPlan,
        },
        records[1].ref,
      ),
    );
    phase = "save current migration batch and fixed verification schema";
    const batch = batchFromWorkspace({
      workspace,
      projectID: config.project_id,
      instanceID: meta.instance_id,
      actorID: who.principal.principal_id,
    });
    batch.intended_purpose = "reference";
    batch.reference_scope = "none";
    batch.stage = "acceptance";
    batch.acceptance = "requested";
    const verificationRecord = await save("migration", {
      schema: "lantai.migration-verification-record/v1",
      batch_id: batch.batch_id,
      instance_id: meta.instance_id,
      project_id: config.project_id,
      verifications: [verification],
    });
    batch.evidence.worksets = [records[1].ref];
    batch.evidence.recovery = [records[2].ref];
    batch.evidence.verifications = [verificationRecord.ref];
    records.push(
      verificationRecord,
      await save("migration", validateMigrationBatch(batch)),
    );
    phase = "schema rejection";
    const badFiles = [
      makeFile(
        "record.json",
        JSON.stringify({ schema: "wrong.management/v1", kind: "workset" }),
        "record",
      ),
    ];
    const badInput = await preparePushInput({
      input: {
        project_id: config.project_id,
        slug: "records/invalid-schema",
        content: {
          asset_type: "config",
          files: [],
          rights: {
            usage: "reference",
            license: "LicenseRef-Synthetic-Record-Restore",
            redistribute_raw: false,
            noai: true,
            sensitivity: "normal",
          },
        },
      },
      files: badFiles,
    });
    const badState = await newPushState({ input: badInput });
    const bad = await pushFiles({
      input: badInput,
      files: badFiles,
      state: badState,
      checkpoint: async () => {},
    });
    await assert.rejects(loadManagedRecord(bad), /格式不匹配/);
    oracle = {
      records,
      input,
      parsed_dependencies: parsed.dependencies,
      source_session_id: login.session.session_id,
      first,
      returned,
      invalid_ref: { asset_id: bad.asset_id, version_id: bad.version_id },
    };
    for (const record of records) await checkRecord(record);
  } else {
    oracle = config.oracle;
    phase =
      "restore all actual management records by fixed ref with current permissions";
    for (const record of oracle.records) await checkRecord(record);
    await assert.rejects(loadManagedRecord(oracle.invalid_ref), /格式不匹配/);
    for (const kind of ["migration", "workset", "recovery"]) {
      let found = false;
      for (let attempt = 0; attempt < 30 && !found; attempt++) {
        const list = await listManagedRecords({
          projectID: config.project_id,
          kind,
        });
        found = oracle.records
          .filter((record) => record.kind === kind)
          .some((record) =>
            list.items.some((item) => item.asset_id === record.ref.asset_id),
          );
        if (!found) await new Promise((resolve) => setTimeout(resolve, 20));
      }
      assert.ok(found, `${kind} record listed through real adapter`);
    }
    const migration = await loadManagedRecord(oracle.records[0].ref, signal, {
      kind: "migration",
    });
    assert.equal(migration.schema, "lantai.migration-record/v1");
    assert.equal(
      migration.workspace.states.project.result.version_id,
      oracle.first.version_id,
    );
    await assert.rejects(
      importMigrationWorkspace(migration.workspace, origin.origin),
      /origin|同源|来源/,
    );
    const old = await loadManagedRecord(oracle.records[1].ref, signal, {
      kind: "workset",
    });
    const next = await loadManagedRecord(oracle.records[3].ref, signal, {
      kind: "workset",
    });
    assert.equal(old.input.entries[0].version_id, oracle.first.version_id);
    assert.equal(next.input.entries[0].version_id, oracle.returned.version_id);
    phase = "restored original fixed workset recheck and full byte read";
    const pulled = await pullBrowserWorkset({
      input: old.input,
      origin: origin.origin,
      purpose: "archive_review",
      signal,
      read,
      grant,
      transfer: (url, signal) =>
        globalThis.fetch(url, {
          credentials: "same-origin",
          cache: "no-store",
          redirect: "error",
          signal,
        }),
    });
    assert.equal(pulled.receipt.state, "archive_verified");
    assert.equal(pulled.plan.entries[0].version_id, oracle.first.version_id);
    assert.equal(pulled.plan.file_count, 4);
    assert.ok(pulled.archive.size > 0);
    const exact = await getExactManagedVersion({
      assetID: oracle.first.asset_id,
      versionID: oracle.first.version_id,
    });
    const selected = [];
    for (const file of exact.manifest.content.files.filter((file) =>
      /\.(obj|mtl|json)$/.test(file.path),
    ))
      selected.push({
        path: `project/${file.path}`,
        bytes: await readManagedFile({
          assetID: oracle.first.asset_id,
          versionID: oracle.first.version_id,
          file,
          purpose: "archive_review",
        }),
      });
    const parsed = await analyzeSelectedDependencies(selected, {
      comfyInputRoot: "project",
    });
    assert.equal(parsed.complete, true);
    assert.deepEqual(
      parsed.dependencies.toSorted((a, b) => a.from.localeCompare(b.from)),
      oracle.parsed_dependencies.toSorted((a, b) =>
        a.from.localeCompare(b.from),
      ),
    );
    const info = await api("/api/v1/admin/instance");
    assert.equal(info.recovery_epoch, config.recovery_epoch);
    assert.ok(info.recovery_receipts.length >= 2);
    phase = "current restored batch recheck preserves core unknown";
    const originalBatch = await loadManagedRecord(
      oracle.records[5].ref,
      signal,
      { kind: "migration", projectID: config.project_id },
    );
    assert.equal(originalBatch.schema, "lantai.migration-batch/v1");
    assert.equal(originalBatch.import_summary.committed, 1);
    const freshVerification = await verifyExactFiles({
      assetID: oracle.first.asset_id,
      versionID: oracle.first.version_id,
      expectedFiles: exact.manifest.content.files,
      purpose: "archive_review",
    });
    const currentReceipt = info.recovery_receipts.find(
      (row) => row.recovery_epoch === info.recovery_epoch,
    );
    assert.ok(currentReceipt);
    const freshRecovery = await save(
      "recovery",
      buildRecoveryEvidence(info, currentReceipt.run_id, [freshVerification]),
    );
    const freshFiles = await save("migration", {
      schema: "lantai.migration-verification-record/v1",
      batch_id: originalBatch.batch_id,
      instance_id: info.instance_id,
      project_id: config.project_id,
      verifications: [freshVerification],
    });
    const currentBatch = nextBatchRevision(
      originalBatch,
      oracle.records[5].ref,
      who.principal.principal_id,
    );
    currentBatch.evidence.recovery = [freshRecovery.ref];
    currentBatch.evidence.verifications = [freshFiles.ref];
    const currentRecord = await save(
      "migration",
      validateMigrationBatch(currentBatch),
      oracle.records[5].ref,
    );
    const checked = await api("/api/v1/management/batches/check", {
      method: "POST",
      body: JSON.stringify(currentRecord.ref),
    });
    assert.equal(checked.schema, "lantai.batch-check/v1");
    assert.equal(checked.acceptance, "blocked");
    assert.equal(
      checked.checks.find((row) => row.key === "recovery")?.status,
      "unknown",
    );
    for (const key of [
      "record",
      "assets",
      "purpose",
      "worksets",
      "verifications",
    ])
      assert.equal(
        checked.checks.find((row) => row.key === key)?.status,
        "passed",
        `${key} must be currently rechecked by server`,
      );
    const withoutCookie = cookie;
    cookie = "";
    await assert.rejects(loadManagedRecord(oracle.records[0].ref), (error) =>
      [401, 403].includes(error.status),
    );
    cookie = withoutCookie;
    phase = "old administrator password rejected";
    await assert.rejects(
      api("/api/v1/sessions/login", {
        method: "POST",
        body: JSON.stringify({
          name: config.name,
          password: config.old_password,
          code: config.fresh_code_for_old_password,
          channel: "browser",
          scopes: ["read"],
        }),
      }),
      (error) => [401, 403].includes(error.status),
    );
  }
  process.stdout.write(
    JSON.stringify({
      schema: "lantai.management-records-restore-live/v1",
      mode: config.mode,
      strict_tls: true,
      oracle,
      adapter_load_count: oracle.records.length,
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
