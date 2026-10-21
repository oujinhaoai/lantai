import test from "node:test";
import assert from "node:assert/strict";
import {
  assetSearchQuery,
  compareFiles,
  fitImage,
  frozenValueText,
  imageSize,
  metadataPatch,
  objLimits,
  parseOBJ,
  restoreMetadataDraft,
  type ManifestFile,
} from "../src/asset-tools";

const assetID = "01J0000000000000000000000A";
const hash = "a".repeat(64);
const file = (
  path: string,
  changes: Partial<ManifestFile> = {},
): ManifestFile => ({
  path,
  role: "primary",
  size: 3,
  sha256: hash,
  ...changes,
});

test("complete search sends exact AND terms to the server without filtering one page", () => {
  const p = new URLSearchParams({
    q: "合成",
    project: assetID,
    type: "motion",
    character: "hero-a,hero-b",
    source: "studio-a",
    state: "pending_review",
    legacy: "v001",
    tags: "tag-a,tag-b",
  });
  const q = assetSearchQuery(`/assets?${p}`);
  assert.equal(q.get("project_id"), assetID);
  assert.equal(q.get("q"), "合成");
  assert.equal(q.get("asset_type"), "motion");
  assert.deepEqual(q.getAll("tag"), [
    "character:hero-a",
    "character:hero-b",
    "legacy-version:v001",
    "source:studio-a",
    "state:pending_review",
    "tag-a",
    "tag-b",
  ]);
  assert.equal(assetSearchQuery("/assets?tags=A,a").getAll("tag").length, 2);
});

test("search normalizes NFC and enforces budgets before making any API request", () => {
  assert.deepEqual(
    assetSearchQuery(`/assets?tags=${encodeURIComponent("e\u0301,é")}`).getAll(
      "tag",
    ),
    ["é"],
  );
  assert.throws(() => assetSearchQuery("/assets?project=other"), /项目标识/);
  assert.throws(
    () => assetSearchQuery(`/assets?source=${"x".repeat(64)}`),
    /64/,
  );
  assert.throws(
    () =>
      assetSearchQuery(
        `/assets?tags=${Array.from({ length: 65 }, (_, i) => `tag${i}`).join(",")}`,
      ),
    /64/,
  );
});

test("metadata patch only changes ordinary current description fields", () => {
  const patch = metadataPatch({
    title: " 合成资料 ",
    summary: " 摘要 ",
    tags: "b, a\na",
    subjects: "角色甲\n角色甲",
  });
  assert.deepEqual(patch, {
    title: "合成资料",
    summary: "摘要",
    tags: ["a", "b"],
    subjects: ["角色甲"],
  });
  assert.equal("rights" in patch, false);
  assert.equal("sensitivity" in patch, false);
  assert.equal("extra" in patch, false);
  assert.throws(
    () => metadataPatch({ title: "", summary: "", tags: "", subjects: "" }),
    /标题/,
  );
});

function snapshot() {
  const draft = { title: "合成", summary: "保留", tags: "a", subjects: "" };
  return {
    schema: "lantai.web-metadata-draft/v1",
    origin: "https://example.invalid",
    asset_id: assetID,
    draft,
    attempt: {
      key: "original-key-01",
      revision: 7,
      body: metadataPatch(draft),
    },
    uncertain: true,
  };
}

test("an unresolved metadata draft restores the original key, body and revision", () => {
  const saved = snapshot();
  const restored = restoreMetadataDraft(saved, saved.origin, assetID);
  assert.deepEqual(restored.attempt, saved.attempt);
  assert.equal(restored.uncertain, true);
  assert.equal(restored.attempt?.revision, 7);
});

test("metadata recovery refuses another origin/asset, edited content and missing original keys", () => {
  const saved = snapshot();
  assert.throws(() =>
    restoreMetadataDraft(saved, "https://different.invalid", assetID),
  );
  assert.throws(() =>
    restoreMetadataDraft(saved, saved.origin, "01J0000000000000000000000B"),
  );
  assert.throws(
    () =>
      restoreMetadataDraft(
        { ...saved, draft: { ...saved.draft, title: "Changed" } },
        saved.origin,
        assetID,
      ),
    /冻结内容/,
  );
  assert.throws(
    () =>
      restoreMetadataDraft(
        { ...saved, attempt: undefined },
        saved.origin,
        assetID,
      ),
    /原提交键/,
  );
  assert.throws(() =>
    restoreMetadataDraft(
      { ...saved, token: "not-accepted" },
      saved.origin,
      assetID,
    ),
  );
  assert.throws(() =>
    restoreMetadataDraft(
      {
        ...saved,
        attempt: {
          ...saved.attempt,
          body: { ...saved.attempt.body, rights: { usage: "production" } },
        },
      },
      saved.origin,
      assetID,
    ),
  );
});

test("version comparison uses frozen file role, bytes and digest, not current title", () => {
  const result = compareFiles(
    [file("same"), file("role"), file("removed")],
    [file("same"), file("role", { role: "source" }), file("added")],
  );
  assert.deepEqual(
    result.map(({ path, kind }) => [path, kind]),
    [
      ["added", "added"],
      ["removed", "removed"],
      ["role", "changed"],
      ["same", "unchanged"],
    ],
  );
  assert.equal(
    compareFiles([file("x")], [file("x", { sha256: "b".repeat(64) })])[0].kind,
    "changed",
  );
});

test("comparison rejects malformed or ambiguous manifests instead of showing misleading diffs", () => {
  assert.throws(() => compareFiles([file("x"), file("x")], []));
  assert.throws(() => compareFiles([file("../outside")], []));
  assert.throws(() => compareFiles([file("x", { size: Infinity })], []));
  assert.throws(() => compareFiles([file("x", { sha256: "invalid" })], []));
});

const obj = "v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n";
test("OBJ polygons render as bounded edges; negative indices work and libraries remain inert", () => {
  const value = parseOBJ(
    `mtllib https://untrusted.invalid/material.mtl\n${obj}f -3 -2 -1\n`,
  );
  assert.equal(value.vertices.length, 3);
  assert.equal(value.faces, 2);
  assert.equal(value.edges.length, 3);
  assert.deepEqual(value.materialLibraries, [
    "https://untrusted.invalid/material.mtl",
  ]);
});

test("OBJ rejects invalid indices, unbounded coordinates, controls and unsupported geometry", () => {
  for (const bad of [
    "f 1 2 3",
    obj.replace("f 1 2 3", "f 0 2 3"),
    obj.replace("f 1 2 3", "f 1 2 4"),
    obj.replace("v 1 0 0", "v Infinity 0 0"),
    obj + "\u0000",
    obj + "curv 0 1 1 2\n",
    obj.replace("v 1 0 0", "v 1e20 0 0"),
  ])
    assert.throws(() => parseOBJ(bad));
});

test("OBJ enforces limits rather than sampling or silently truncating large geometry", () => {
  assert.throws(() => parseOBJ("#".repeat(objLimits.bytes + 1)), /2 MiB/);
  assert.throws(
    () => parseOBJ("v 0 0 0\n".repeat(objLimits.vertices + 1)),
    /20,000/,
  );
  assert.throws(
    () =>
      parseOBJ(
        "v 0 0 0\n".repeat(65) +
          "f " +
          Array.from({ length: 65 }, (_, i) => i + 1).join(" "),
      ),
    /3–64/,
  );
});

const png = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/bZkAAAAASUVORK5CYII=",
  "base64",
);
test("image header budget checks happen before image decoding", () => {
  assert.deepEqual(imageSize(png), { width: 1, height: 1, mime: "image/png" });
  const bomb = new Uint8Array(png);
  new DataView(bomb.buffer).setUint32(16, 16384);
  new DataView(bomb.buffer).setUint32(20, 16384);
  assert.throws(() => imageSize(bomb), /像素/);
  assert.throws(() => imageSize(png.subarray(0, 10)), /支持/);
});

test("WebP dimensions are bounded and animated WebP is explicitly refused", () => {
  const bytes = new Uint8Array(30);
  bytes.set(new TextEncoder().encode("RIFF"));
  bytes.set(new TextEncoder().encode("WEBPVP8X"), 8);
  bytes[24] = 63;
  bytes[27] = 31;
  assert.deepEqual(imageSize(bytes), {
    width: 64,
    height: 32,
    mime: "image/webp",
  });
  bytes[20] = 2;
  assert.throws(() => imageSize(bytes), /动画/);
  assert.throws(
    () => imageSize(new TextEncoder().encode("GIF89a")),
    /静态 PNG/,
  );
});

test("proxy sizing preserves aspect, does not upscale, and refuses unbounded output options", () => {
  assert.deepEqual(fitImage(4000, 2000, 1024), { width: 1024, height: 512 });
  assert.deepEqual(fitImage(100, 50, 2048), { width: 100, height: 50 });
  assert.throws(() => fitImage(100, 50, 100000));
});

test("arbitrary frozen metadata is text-only and bounded by depth, node and display budgets", () => {
  assert.equal(
    frozenValueText({ b: 2, a: 1 }).text,
    frozenValueText({ a: 1, b: 2 }).text,
  );
  assert.equal(
    frozenValueText({ rights: { usage: "restricted" } }).truncated,
    false,
  );
  assert.equal(frozenValueText({ note: "x".repeat(100000) }).truncated, true);
  let nested: unknown = "leaf";
  for (let i = 0; i < 1000; i++) nested = { child: nested };
  const bounded = frozenValueText(nested);
  assert.equal(bounded.truncated, true);
  assert.ok(bounded.text.length < 4200);
});

test("JPEG header preflight walks sized markers and refuses truncated or oversized dimensions", () => {
  const jpeg = new Uint8Array(27);
  jpeg.set([
    255, 216, 255, 224, 0, 4, 0, 0, 255, 192, 0, 17, 8, 0, 32, 0, 64, 3,
  ]);
  assert.deepEqual(imageSize(jpeg), {
    width: 64,
    height: 32,
    mime: "image/jpeg",
  });
  const truncated = new Uint8Array(jpeg);
  truncated[5] = 255;
  assert.throws(() => imageSize(truncated), /截断/);
  const bomb = new Uint8Array(jpeg);
  new DataView(bomb.buffer).setUint16(13, 10000);
  new DataView(bomb.buffer).setUint16(15, 10000);
  assert.throws(() => imageSize(bomb), /像素/);
});
