// Shared real-CLI regression driver; called only by the opted-in Go integration
// test. No server routes are mocked and the verifier consumes actual record.json.
import assert from "node:assert/strict";
import fs from "node:fs";
import process from "node:process";
import console from "node:console";
import {
  blankMigrationDraft,
  freezeMigration,
  importMigrationWorkspace,
} from "../src/migration-model.ts";
import {
  canonicalJSON,
  sha256,
  parseManagedJSON,
  pushJSON,
} from "../src/managed-transfer.ts";

const [mode, ...args] = process.argv.slice(2);
if (mode === "freeze") {
  const [project, instance, origin, output] = args;
  const file = new globalThis.File(
    ["synthetic browser migration archive bytes\n"],
    "sample.txt",
  );
  const plan = blankMigrationDraft(project);
  plan.assets = [
    {
      key: "source",
      directory: ".",
      classification: "final",
      push: {
        project_id: project,
        slug: "migration/lexical-roundtrip",
        describe: { title: "Synthetic migration lexical roundtrip" },
        content: {
          asset_type: "doc",
          files: [
            {
              path: "sample.txt",
              role: "primary",
              size: file.size,
              sha256: "",
            },
          ],
          rights: {
            usage: "reference",
            license: "LicenseRef-Synthetic",
            redistribute_raw: false,
            noai: true,
            sensitivity: "normal",
          },
          metadata: {
            tool: "synthetic-browser",
            extra: { z: 1, a: "<>&中文" },
          },
        },
      },
    },
  ];
  const workspace = await freezeMigration({
    plan,
    files: [{ path: "sample.txt", role: "primary", file }],
    instanceID: instance,
    origin,
  });
  fs.writeFileSync(
    output,
    JSON.stringify({
      project_id: project,
      kind: "migration",
      title: "Synthetic migration lexical roundtrip",
      payload: {
        schema: "lantai.migration-record/v1",
        workspace,
        verifications: {},
        recovery_refs: [],
      },
    }),
    { mode: 0o600 },
  );
  console.log("MIGRATION_LEXICAL_FREEZE_OK");
} else if (mode === "verify") {
  const [originalFile, archivedFile] = args;
  const original = parseManagedJSON(fs.readFileSync(originalFile, "utf8"))
    .payload.workspace;
  const archivedBytes = fs.readFileSync(archivedFile, "utf8");
  const record = parseManagedJSON(archivedBytes);
  const archived = record.payload.workspace;
  assert.equal(record.schema, "lantai.management-record/v1");
  assert.notEqual(
    JSON.stringify(archived.plan.assets[0].push.content.rights),
    JSON.stringify(original.plan.assets[0].push.content.rights),
    "fixture must exercise real canonical object reordering",
  );
  const resumed = await importMigrationWorkspace(archived, original.origin);
  assert.equal(canonicalJSON(resumed.states), canonicalJSON(original.states));
  assert.equal(resumed.plan_digest, original.plan_digest);
  assert.equal(resumed.state_format, original.state_format);
  assert.equal(
    pushJSON(resumed.plan.assets[0].push),
    original.push_json.source,
  );
  const legacyOriginal = globalThis.structuredClone(original);
  delete legacyOriginal.push_json;
  assert.equal(
    canonicalJSON(
      (await importMigrationWorkspace(legacyOriginal, original.origin)).states,
    ),
    canonicalJSON(original.states),
  );
  const missing = globalThis.structuredClone(archived);
  delete missing.push_json;
  await assert.rejects(
    () => importMigrationWorkspace(missing, original.origin),
    /摘要/,
  );
  const dropped = globalThis.structuredClone(archived);
  dropped.push_json = {};
  await assert.rejects(
    () => importMigrationWorkspace(dropped, original.origin),
    /缺少分组/,
  );
  const tampered = globalThis.structuredClone(archived);
  tampered.push_json.source += " ";
  await assert.rejects(
    () => importMigrationWorkspace(tampered, original.origin),
    /摘要/,
  );
  const hash = globalThis.structuredClone(archived);
  hash.states.source.request_hash = "0".repeat(64);
  await assert.rejects(
    () => importMigrationWorkspace(hash, original.origin),
    /摘要/,
  );
  const semantic = globalThis.structuredClone(archived);
  semantic.plan.assets[0].push.content.rights.license = "LicenseRef-Altered";
  semantic.plan_digest = `sha256:${await sha256(canonicalJSON(semantic.plan))}`;
  await assert.rejects(
    () => importMigrationWorkspace(semantic, original.origin),
    /语义/,
  );
  console.log(
    "MIGRATION_CLI_CANONICAL_ROUNDTRIP_OK hash/key/state_format preserved; missing/raw/hash/semantic/legacy negatives rejected",
  );
} else {
  throw new Error("expected freeze or verify mode");
}
