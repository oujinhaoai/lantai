import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import {
  WORKSET_SCHEMA,
  canonicalWorksetJSON,
  parseWorksetJSON,
  validateWorkset,
  worksetPath,
  checkWorksetConflicts,
  planBrowserWorkset,
  pullBrowserWorkset,
  checkWorksetGrant,
  makeWorksetAssistantJob,
  checkWorksetInspection,
  checkWorksetAssistantResult,
  checkWorksetDeviceContext,
  worksetTar,
  worksetSHA256,
  type WorksetInput,
  type WorksetRead,
  type BrowserWorksetPlan,
} from "../src/workset-helpers";
import {
  analyzeSelectedDependencies,
  MAX_DEPENDENCY_FILE_BYTES,
} from "../src/dependency-parser";

const origin = "https://synthetic.example";
const instance = "01J00000000000000000000001";
const project = "01J00000000000000000000002";
const asset = "01J00000000000000000000003";
const version = "01J00000000000000000000004";
const grantID = "01J00000000000000000000005";
const bytes = new TextEncoder().encode("synthetic workset bytes\n");
const hex = (input: string | Uint8Array) =>
  createHash("sha256").update(input).digest("hex");
const input = (): WorksetInput => ({
  schema: WORKSET_SCHEMA,
  entries: [{ asset_id: asset, version_id: version, destination: "project" }],
});
function fixture(
  files = [
    {
      path: "notes.txt",
      size: bytes.length,
      sha256: hex(bytes),
      role: "primary",
    },
  ],
) {
  const content = {
    asset_type: "doc",
    type_schema: "lantai.asset-types/v1",
    files,
    uses: [],
    rights: {
      usage: "reference",
      license: "LicenseRef-Synthetic",
      redistribute_raw: true,
      noai: true,
      sensitivity: "normal",
    },
    metadata: {},
  };
  const digest = `sha256:${hex(canonicalWorksetJSON(content))}`;
  return {
    version: {
      asset_id: asset,
      version_id: version,
      project_id: project,
      version_number: 1,
      uri: `lantai://${instance}/assets/${asset}/versions/${version}`,
      manifest_digest: digest,
    },
    manifest: {
      contract: "lantai.manifest/v1",
      instance_id: instance,
      asset_id: asset,
      version_id: version,
      project_id: project,
      version_number: 1,
      manifest_digest: digest,
      content,
    },
  };
}
function reader(value = fixture()) {
  const calls: string[] = [];
  const read: WorksetRead = async (path) => {
    calls.push(path);
    return path === "/api/v1/meta"
      ? { api_version: "v1", instance_id: instance }
      : value;
  };
  return { calls, read };
}
async function plan(): Promise<BrowserWorksetPlan> {
  return planBrowserWorkset(
    input(),
    origin,
    reader().read,
    new AbortController().signal,
  );
}

test("strict workset JSON preserves scalar values but rejects duplicate keys, unsafe Unicode and ambiguous documents", () => {
  const data = parseWorksetJSON(
    '{"z":false,"n":1.25,"__proto__":{"polluted":true},"value":null}',
  ) as Record<string, unknown>;
  assert.equal(data.n, 1.25);
  assert.equal(Object.getPrototypeOf(data), null);
  for (const text of [
    '{"x":1,"x":2}',
    '{"x":{"a":1,"a":2}}',
    '{"x":"\\ud800"}',
    '{"x":9007199254740993}',
    '{"x":1e999}',
    "{} {}",
    "[1,]",
    '{"x":truefalse}',
    '{"x":"bad\ntext"}',
  ])
    assert.throws(() => parseWorksetJSON(text), text);
});
test("JCS digest uses prefixed contract forms and Go-compatible omission of empty slices", async () => {
  const normalized = validateWorkset({
    ...input(),
    entrypoints: [],
    dependencies: [],
  });
  assert.deepEqual(normalized, input());
  assert.equal(
    canonicalWorksetJSON({ "\u20ac": "€", a: 1e-7, x: -0 }),
    '{"a":1e-7,"x":0,"€":"€"}',
  );
  assert.equal(
    await worksetSHA256("abc"),
    "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
  );
  const value = await plan();
  assert.equal(
    value.input_digest,
    `sha256:${hex(canonicalWorksetJSON(input()))}`,
  );
  assert.match(value.entries[0].manifest_digest, /^sha256:/);
});
test("unsafe paths and transfer-state aliases never enter a workset", () => {
  for (const path of [
    "",
    "/etc/passwd",
    "../outside",
    "a/../b",
    "a\\b",
    "a//b",
    "a/",
    "AUX.txt",
    "COM0.bin",
    "LPT².bin",
    "x.lantai-part",
    "x.LANTAI-LOCK",
    "file. ",
    "bad\u0085name",
    "\u202etxt",
    "e\u0301.txt",
    "a".repeat(201),
  ])
    assert.throws(() => worksetPath(path), path);
  assert.equal(worksetPath("素材/图像.png"), "素材/图像.png");
});
test("preflight catches destination nesting, full-case aliases and differently spelled intermediate directories", () => {
  for (const paths of [
    ["x", "x/y"],
    ["x/y", "x"],
    ["A/x", "a/y"],
    ["Straße/x", "STRASSE/y"],
    ["a", "a"],
  ])
    assert.throws(() => checkWorksetConflicts(paths));
  assert.doesNotThrow(() =>
    checkWorksetConflicts([
      "project/a.txt",
      "project/b.txt",
      "textures/color.png",
    ]),
  );
  assert.throws(() =>
    validateWorkset({
      ...input(),
      entries: [
        ...input().entries,
        { asset_id: asset, version_id: version, destination: "PROJECT/sub" },
      ],
    }),
  );
});
test("workset validation rejects unknown fields, latest aliases, malformed dependency and entry budgets before reads", () => {
  assert.throws(() => validateWorkset({ ...input(), token: "not-allowed" }));
  assert.throws(() =>
    validateWorkset({
      ...input(),
      entries: [{ ...input().entries[0], version_id: "latest" }],
    }),
  );
  assert.throws(() =>
    validateWorkset({
      ...input(),
      dependencies: [{ from: "project/a", requires: [] }],
    }),
  );
  assert.throws(() =>
    validateWorkset({
      ...input(),
      entries: Array.from({ length: 1001 }, (_, i) => ({
        asset_id: asset,
        version_id: version,
        destination: `entry-${i}`,
      })),
    }),
  );
});
test("metadata identity is checked before requesting any exact version", async () => {
  const calls: string[] = [];
  await assert.rejects(() =>
    planBrowserWorkset(
      input(),
      origin,
      async (path) => {
        calls.push(path);
        return { api_version: "v1", instance_id: "wrong" };
      },
      new AbortController().signal,
    ),
  );
  assert.deepEqual(calls, ["/api/v1/meta"]);
});
test("valid remote plan checks complete manifest and dependencies but never claims local filesystem validation", async () => {
  const io = reader();
  const value = await planBrowserWorkset(
    {
      ...input(),
      entrypoints: ["project/notes.txt"],
      dependencies: [
        { from: "project/notes.txt", requires: ["project/notes.txt"] },
      ],
    },
    origin,
    io.read,
    new AbortController().signal,
  );
  assert.equal(value.local_directory_checked, false);
  assert.equal(value.state, "remote_verified");
  assert.equal(value.file_count, 1);
  assert.equal(value.entries[0].files[0].path, "project/notes.txt");
  assert.equal(io.calls.length, 2);
  assert.ok(io.calls.every((p) => !p.includes("read-grants")));
});
test("wrong valid instance, version, project, version number and permanent URI all fail exact binding", async () => {
  for (const mutate of [
    (v: ReturnType<typeof fixture>) => {
      v.manifest.instance_id = project;
    },
    (v: ReturnType<typeof fixture>) => {
      v.version.version_id = asset;
    },
    (v: ReturnType<typeof fixture>) => {
      v.version.project_id = instance;
    },
    (v: ReturnType<typeof fixture>) => {
      v.manifest.version_number = 2;
    },
    (v: ReturnType<typeof fixture>) => {
      v.version.uri = `lantai://${project}/assets/${asset}/versions/${version}`;
    },
  ]) {
    const value = fixture();
    mutate(value);
    await assert.rejects(() =>
      planBrowserWorkset(
        input(),
        origin,
        reader(value).read,
        new AbortController().signal,
      ),
    );
  }
});
test("changed content or malformed digest fails without trusting the advertised manifest", async () => {
  const changed = fixture();
  changed.manifest.content.files[0].size++;
  await assert.rejects(
    () =>
      planBrowserWorkset(
        input(),
        origin,
        reader(changed).read,
        new AbortController().signal,
      ),
    /摘要/,
  );
  const noPrefix = fixture();
  noPrefix.manifest.manifest_digest =
    noPrefix.manifest.manifest_digest.slice(7);
  noPrefix.version.manifest_digest = noPrefix.manifest.manifest_digest;
  await assert.rejects(() =>
    planBrowserWorkset(
      input(),
      origin,
      reader(noPrefix).read,
      new AbortController().signal,
    ),
  );
});
test("missing declared dependencies and manifest file-directory conflicts are rejected", async () => {
  await assert.rejects(
    () =>
      planBrowserWorkset(
        {
          ...input(),
          dependencies: [
            { from: "project/notes.txt", requires: ["textures/missing.png"] },
          ],
        },
        origin,
        reader().read,
        new AbortController().signal,
      ),
    /缺少/,
  );
  const value = fixture([
    { path: "a", size: 0, sha256: hex(""), role: "primary" },
    { path: "a/b", size: 0, sha256: hex(""), role: "source" },
  ]);
  await assert.rejects(
    () =>
      planBrowserWorkset(
        input(),
        origin,
        reader(value).read,
        new AbortController().signal,
      ),
    /冲突/,
  );
});
test("workset total budget fails before an oversized archive or read-grant request", async () => {
  const value = fixture([
    { path: "large.bin", size: 2 ** 40 + 1, sha256: hex(""), role: "primary" },
  ]);
  await assert.rejects(
    () =>
      planBrowserWorkset(
        input(),
        origin,
        reader(value).read,
        new AbortController().signal,
      ),
    /1 TiB/,
  );
});
test("browser export rechecks fresh server authority, full bytes and standard TAR output", async () => {
  const io = reader();
  let grants = 0;
  const result = await pullBrowserWorkset({
    input: input(),
    origin,
    purpose: "archive_review",
    read: io.read,
    signal: new AbortController().signal,
    grant: async (_entry, file, purpose) => {
      grants++;
      assert.equal(purpose, "archive_review");
      return {
        path: file.source_path,
        size: file.size,
        sha256: file.sha256,
        url: `${origin}/xfer/v1/reads/${grantID}`,
        expires_at: new Date(Date.now() + 60000).toISOString(),
      };
    },
    transfer: async () => new Response(bytes),
  });
  assert.equal(grants, 1);
  assert.equal(io.calls.length, 4);
  assert.equal(result.receipt.local_directory_checked, false);
  const archive = Buffer.from(await result.archive.arrayBuffer());
  assert.equal(
    execFileSync("tar", ["-tf", "-"], { input: archive, encoding: "utf8" }),
    "project/notes.txt\n",
  );
  assert.deepEqual(
    execFileSync("tar", ["-xOf", "-", "project/notes.txt"], { input: archive }),
    Buffer.from(bytes),
  );
});
test("browser budget and unsupported TAR names fail before any read grant", async () => {
  let grant = 0;
  const value = fixture([
    {
      path: "large.bin",
      size: 64 * 1024 * 1024 + 1,
      sha256: hex(""),
      role: "primary",
    },
  ]);
  await assert.rejects(
    () =>
      pullBrowserWorkset({
        input: input(),
        origin,
        purpose: "archive_review",
        read: reader(value).read,
        signal: new AbortController().signal,
        grant: async () => {
          grant++;
          throw new Error("unreachable");
        },
        transfer: async () => new Response(bytes),
      }),
    /64 MiB/,
  );
  assert.equal(grant, 0);
  assert.throws(
    () => worksetTar([{ path: `project/${"图".repeat(40)}.obj`, bytes }]),
    /TAR/,
  );
});
test("foreign, expired and wrong-file grants never issue a byte transfer", async () => {
  const wanted = {
    source_path: "notes.txt",
    path: "project/notes.txt",
    sha256: hex(bytes),
    size: bytes.length,
  };
  const valid = {
    path: "notes.txt",
    size: bytes.length,
    sha256: hex(bytes),
    url: `${origin}/xfer/v1/reads/${grantID}`,
    expires_at: new Date(Date.now() + 60000).toISOString(),
  };
  for (const patch of [
    { url: `https://foreign.example/xfer/v1/reads/${grantID}` },
    { path: "other.txt" },
    { expires_at: "2000-01-01T00:00:00Z" },
    { url: `${origin}/xfer/v1/reads/${grantID}#fragment` },
    { url: `${origin}/xfer/v1/reads/../private` },
    { size: wanted.size + 1 },
  ])
    assert.throws(() =>
      checkWorksetGrant({ ...valid, ...patch }, wanted, origin),
    );
});
test("wrong bytes and late revocation cannot produce a successful archive", async () => {
  const grant = async (
    _entry: unknown,
    f: { source_path: string; size: number; sha256: string },
  ) => ({
    path: f.source_path,
    size: f.size,
    sha256: f.sha256,
    url: `${origin}/xfer/v1/reads/${grantID}`,
    expires_at: new Date(Date.now() + 60000).toISOString(),
  });
  await assert.rejects(
    () =>
      pullBrowserWorkset({
        input: input(),
        origin,
        purpose: "archive_review",
        read: reader().read,
        signal: new AbortController().signal,
        grant,
        transfer: async () => new Response("corrupt"),
      }),
    /SHA-256/,
  );
  let reads = 0;
  await assert.rejects(
    () =>
      pullBrowserWorkset({
        input: input(),
        origin,
        purpose: "archive_review",
        read: async (path) => {
          if (++reads > 2) throw new Error("AUTH_REQUIRED");
          return reader().read(path, new AbortController().signal);
        },
        signal: new AbortController().signal,
        grant,
        transfer: async () => new Response(bytes),
      }),
    /AUTH_REQUIRED/,
  );
});
test("cancelled transfer cannot publish a completed export", async () => {
  const controller = new AbortController();
  await assert.rejects(
    () =>
      pullBrowserWorkset({
        input: input(),
        origin,
        purpose: "archive_review",
        read: reader().read,
        signal: controller.signal,
        grant: async (_e, f) => ({
          path: f.source_path,
          size: f.size,
          sha256: f.sha256,
          url: `${origin}/xfer/v1/reads/${grantID}`,
          expires_at: new Date(Date.now() + 60000).toISOString(),
        }),
        transfer: async () => {
          controller.abort();
          return new Response(bytes);
        },
      }),
    { name: "AbortError" },
  );
});
test("assistant jobs are declarative, preserve fixed input and reject overlapping destinations", async () => {
  const value = makeWorksetAssistantJob(
    { ...input(), entrypoints: [], dependencies: [] },
    origin,
    "workset.pull",
    "work",
    "result.json",
    "reference",
    "workset_1",
  );
  assert.deepEqual(value.input, input());
  assert.equal(value.action, "workset.pull");
  assert.equal("token" in value, false);
  assert.equal("argv" in value, false);
  assert.throws(() =>
    makeWorksetAssistantJob(
      input(),
      origin,
      "workset.pull",
      "work",
      "WORK/report.json",
      "reference",
      "x",
    ),
  );
  assert.throws(() =>
    makeWorksetAssistantJob(
      input(),
      origin,
      "workset.pull",
      "../outside",
      "report.json",
      "reference",
      "x",
    ),
  );
  assert.equal(
    makeWorksetAssistantJob(
      input(),
      origin,
      "workset.plan",
      "work",
      "report.json",
      "production",
      "x",
    ).purpose,
    "archive_review",
  );
  const current = await plan();
  const inspectionValue = {
    schema: "lantai.local-assistant-inspection/v1",
    job_id: value.job_id,
    action: value.action,
    job_digest: hex(canonicalWorksetJSON(value)),
    origin,
    paths: { directory: "work", output: "result.json" },
    input_sha256: current.input_digest.slice(7),
    approval_digest: "a".repeat(64),
    workspace_binding: "b".repeat(64),
    source_deleted: false,
    remote_writes: false,
  };
  const inspection = await checkWorksetInspection(
    inspectionValue,
    value,
    current.input_digest,
  );
  assert.equal(inspection.input_sha256, hex(canonicalWorksetJSON(value.input)));
  assert.equal(inspection.job_digest, hex(canonicalWorksetJSON(value)));
  await assert.rejects(
    () =>
      checkWorksetInspection(
        { ...inspectionValue, job_digest: "c".repeat(64) },
        value,
        current.input_digest,
      ),
    /不匹配/,
  );
  await assert.rejects(
    () =>
      checkWorksetInspection(
        inspectionValue,
        { ...value, purpose: "production" },
        current.input_digest,
      ),
    /不匹配/,
  );
});
test("device workset result requires core success, stopped state and current workspace/job fence", () => {
  const inspection = {
    job_id: "workset_1",
    action: "workset.pull",
    job_digest: "a".repeat(64),
    approval_digest: "b".repeat(64),
    workspace_binding: "c".repeat(64),
    input_sha256: "d".repeat(64),
  };
  const context = {
    job_id: asset,
    device_id: version,
    operation_id: instance,
    state: "succeeded",
    stopped: true,
    job_digest: inspection.job_digest,
    approval_digest: inspection.approval_digest,
    workspace_binding: inspection.workspace_binding,
    job_fence: {
      job_id: asset,
      job_attempt_id: project,
      lease_fence: 1,
      recovery_epoch: 1,
    },
  };
  assert.doesNotThrow(() => checkWorksetDeviceContext(context, inspection));
  for (const bad of [
    { ...context, state: "needs_reconciliation" },
    { ...context, state: "cancelling" },
    { ...context, state: "failed" },
    { ...context, stopped: false },
    { ...context, workspace_binding: "e".repeat(64) },
    { ...context, job_digest: "e".repeat(64) },
    { ...context, approval_digest: "e".repeat(64) },
    { ...context, job_fence: { ...context.job_fence, job_id: version } },
    { ...context, job_fence: { ...context.job_fence, lease_fence: 0 } },
    { ...context, job_fence: { ...context.job_fence, recovery_epoch: -1 } },
    { ...context, job_fence: undefined },
  ])
    assert.throws(() => checkWorksetDeviceContext(bad, inspection));
});
test("assistant results bind original approval, fixed versions and every file; partial is not verified", async () => {
  const current = await plan();
  const inspection = {
    job_id: "workset_1",
    action: "workset.pull",
    job_digest: "d".repeat(64),
    approval_digest: "a".repeat(64),
    input_sha256: current.input_digest.slice(7),
    workspace_binding: "b".repeat(64),
  };
  const result = {
    schema: "lantai.local-assistant-result/v1",
    job_id: inspection.job_id,
    action: inspection.action,
    job_digest: inspection.job_digest,
    approval_digest: inspection.approval_digest,
    status: "completed",
    exit_code: 0,
    finished_at: new Date().toISOString(),
    source_deleted: false,
    automatic_retry: false,
    credential_issued: false,
    result: {
      schema: "lantai.client-workset-receipt/v1",
      origin,
      input_digest: current.input_digest,
      state: "completed",
      file_count: current.file_count,
      total_bytes: current.total_bytes,
      entries: current.entries.map((e) => ({
        ...e,
        files: e.files.map((f) => ({ ...f, state: "verified" })),
      })),
    },
  };
  assert.equal(
    checkWorksetAssistantResult(result, inspection, current)
      .native_files_verified,
    true,
  );
  assert.equal(
    checkWorksetAssistantResult(
      { ...result, status: "partial", exit_code: 3 },
      inspection,
      current,
    ).native_files_verified,
    false,
  );
  assert.throws(() =>
    checkWorksetAssistantResult(
      { ...result, job_digest: "e".repeat(64) },
      inspection,
      current,
    ),
  );
  assert.throws(() =>
    checkWorksetAssistantResult(
      { ...result, approval_digest: "c".repeat(64) },
      inspection,
      current,
    ),
  );
  result.result.entries[0].files[0].sha256 = "f".repeat(64);
  assert.throws(
    () => checkWorksetAssistantResult(result, inspection, current),
    /摘要/,
  );
});

const source = (path: string, text: string) => ({
  path,
  bytes: new TextEncoder().encode(text),
});
test("OBJ→MTL→texture dependency extraction is explicit and hash-bound, including safe MTL options", async () => {
  const result = await analyzeSelectedDependencies([
    source(
      "project/model.obj",
      "# synthetic\nmtllib materials.mtl\nv 0 0 0\nf 1 2 3\n",
    ),
    source(
      "project/materials.mtl",
      'newmtl Material\nKd 1 1 1\nmap_Kd -s 1 1 1 -o -1 0 "textures/color map.png"\n',
    ),
  ]);
  assert.equal(result.complete, true);
  assert.deepEqual(result.dependencies, [
    { from: "project/model.obj", requires: ["project/materials.mtl"] },
    {
      from: "project/materials.mtl",
      requires: ["project/textures/color map.png"],
    },
  ]);
  assert.equal(
    result.files[0].sha256,
    hex(
      source("", "# synthetic\nmtllib materials.mtl\nv 0 0 0\nf 1 2 3\n").bytes,
    ),
  );
  assert.deepEqual(result.missing_selected_files, [
    { from: "project/materials.mtl", path: "project/textures/color map.png" },
  ]);
});
test("unsafe dependency paths and unknown executable OBJ statements stay unresolved and never execute", async () => {
  const result = await analyzeSelectedDependencies([
    source(
      "project/model.obj",
      "mtllib ../secret.mtl\ncall arbitrary.sh\nmtllib /outside.mtl\n",
    ),
  ]);
  assert.equal(result.complete, false);
  assert.deepEqual(result.dependencies, []);
  assert.equal(result.files[0].notices.length, 3);
});
test("unknown MTL options do not guess the final filename", async () => {
  const result = await analyzeSelectedDependencies([
    source("project/a.mtl", "map_Kd -unknown output.png\n"),
  ]);
  assert.equal(result.complete, false);
  assert.deepEqual(result.files[0].references, []);
});
test("ComfyUI API/UI LoadImage recognizes explicit inputs, not custom nodes or model dependencies", async () => {
  const api = await analyzeSelectedDependencies(
    [
      source(
        "project/workflow.json",
        '{"1":{"class_type":"LoadImage","inputs":{"image":"input/image.png"}},"2":{"class_type":"SaveImage","inputs":{"images":["1",0]}}}',
      ),
    ],
    { comfyInputRoot: "project" },
  );
  assert.equal(api.complete, true);
  assert.deepEqual(api.dependencies[0].requires, ["project/input/image.png"]);
  const ui = await analyzeSelectedDependencies(
    [
      source(
        "project/ui.json",
        '{"nodes":[{"type":"LoadImage","widgets_values":["image.png"]},{"type":"CustomLoader","widgets_values":["model.safetensors"]}]}',
      ),
    ],
    { comfyInputRoot: "project" },
  );
  assert.equal(ui.complete, false);
  assert.deepEqual(ui.files[0].references, ["project/image.png"]);
});
test("ComfyUI never guesses its input root from the workflow location", async () => {
  const file = source(
    "config/workflow.json",
    '{"1":{"class_type":"LoadImage","inputs":{"image":"image.png"}}}',
  );
  const unresolved = await analyzeSelectedDependencies([file]);
  assert.equal(unresolved.complete, false);
  assert.deepEqual(unresolved.dependencies, []);
  const explicit = await analyzeSelectedDependencies([file], {
    comfyInputRoot: "textures",
  });
  assert.deepEqual(explicit.dependencies[0].requires, ["textures/image.png"]);
});
test("JSON duplicates, binary content, unsupported formats and resource excess fail visibly", async () => {
  for (const file of [
    source(
      "workflow.json",
      '{"1":{"class_type":"LoadImage","class_type":"SaveImage","inputs":{"image":"x.png"}}}',
    ),
    source("model.obj", "v 0\0 1"),
    source("scene.blend", "not a parser"),
  ]) {
    const result = await analyzeSelectedDependencies([file]);
    assert.equal(result.complete, false);
    assert.ok(result.files[0].notices.length);
  }
  await assert.rejects(
    () =>
      analyzeSelectedDependencies([
        {
          path: "large.obj",
          bytes: new Uint8Array(MAX_DEPENDENCY_FILE_BYTES + 1),
        },
      ]),
    /1 MiB/,
  );
  await assert.rejects(() =>
    analyzeSelectedDependencies([
      source("same.obj", ""),
      source("same.obj", ""),
    ]),
  );
});
