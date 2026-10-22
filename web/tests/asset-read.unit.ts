import test, { afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readAssetFile } from "../src/asset-read";
import { APIError, rememberCSRF } from "../src/api";

const fetchBefore = globalThis.fetch;
const locationBefore = Object.getOwnPropertyDescriptor(globalThis, "location");
afterEach(() => {
  globalThis.fetch = fetchBefore;
  if (locationBefore)
    Object.defineProperty(globalThis, "location", locationBefore);
  else Reflect.deleteProperty(globalThis, "location");
});
const assetID = "01J0000000000000000000000A",
  versionID = "01J0000000000000000000000B",
  grantID = "01J0000000000000000000000C";
const payload = new TextEncoder().encode("abc");
const file = {
  path: "synthetic.obj",
  role: "primary",
  size: 3,
  sha256: createHash("sha256").update(payload).digest("hex"),
};
const options = () => ({
  assetID,
  versionID,
  file,
  purpose: "archive_review" as const,
  byteLimit: 1024,
  signal: new AbortController().signal,
});
function setup(grant: object = {}, transfer: Uint8Array = payload) {
  Object.defineProperty(globalThis, "location", {
    configurable: true,
    value: { origin: "https://app.invalid" },
  });
  rememberCSRF("synthetic-session", "synthetic-csrf");
  const calls: { url: string; method?: string; body?: unknown }[] = [];
  globalThis.fetch = async (input, init) => {
    const url = String(input);
    calls.push({
      url,
      method: init?.method,
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
    });
    if (url.startsWith("/api/v1/"))
      return Response.json({
        ...file,
        grant_id: grantID,
        asset_id: assetID,
        version_id: versionID,
        purpose: "archive_review",
        method: "GET",
        url: `/xfer/v1/reads/${grantID}?sig=${"a".repeat(43)}`,
        expires_at: new Date(Date.now() + 60000).toISOString(),
        ...grant,
      });
    return new Response(new Uint8Array(transfer).buffer);
  };
  return calls;
}

test("bounded asset tools obtain the explicit purpose, validate the fixed grant and hash all bytes", async () => {
  const calls = setup();
  const result = await readAssetFile(options());
  assert.deepEqual(result.bytes, payload);
  assert.ok(result.expires > Date.now());
  assert.deepEqual(calls[0].body, {
    path: file.path,
    purpose: "archive_review",
  });
  assert.equal(
    calls[1].url,
    `https://app.invalid/xfer/v1/reads/${grantID}?sig=${"a".repeat(43)}`,
  );
});

test("a proxy export denial never falls back to archive_review or requests transfer bytes", async () => {
  const calls = setup();
  globalThis.fetch = async (input, init) => {
    calls.push({ url: String(input), body: JSON.parse(String(init?.body)) });
    return Response.json(
      { error: { code: "USE_RESTRICTED" } },
      { status: 403 },
    );
  };
  await assert.rejects(
    () => readAssetFile({ ...options(), purpose: "raw_export" }),
    (error: unknown) =>
      error instanceof APIError && error.code === "USE_RESTRICTED",
  );
  assert.equal(calls.length, 1);
  assert.deepEqual(calls[0].body, { path: file.path, purpose: "raw_export" });
});

test("invalid identities, digest grants, expired grants and off-origin URLs stop before content fetch", async () => {
  for (const grant of [
    { asset_id: versionID },
    { version_id: assetID },
    { purpose: "raw_export" },
    { method: "POST" },
    { grant_id: assetID },
    { path: "different.obj" },
    { sha256: "b".repeat(64) },
    { size: 4 },
    { expires_at: "2000-01-01T00:00:00Z" },
    { url: "https://untrusted.invalid/xfer/v1/reads/" + grantID },
    { url: `/xfer/v1/reads/${grantID}?secret=1` },
    { url: `/xfer/v1/reads/${grantID}` },
    {
      url: `/xfer/v1/reads/${grantID}?sig=${"a".repeat(43)}&sig=${"b".repeat(43)}`,
    },
    { url: `/xfer/v1/reads/${grantID}?sig=${"a".repeat(43)}&redirect=1` },
  ]) {
    const calls = setup(grant);
    await assert.rejects(() => readAssetFile(options()));
    assert.equal(calls.length, 1);
  }
});

test("overlong, short or wrong-hash transfer bytes cannot produce derived content", async () => {
  for (const transfer of [
    new TextEncoder().encode("abcd"),
    new TextEncoder().encode("ab"),
    new TextEncoder().encode("abd"),
  ]) {
    setup({}, transfer);
    await assert.rejects(() => readAssetFile(options()));
  }
});

test("tool byte budgets are checked before even creating an authorization request", async () => {
  const calls = setup();
  await assert.rejects(() => readAssetFile({ ...options(), byteLimit: 2 }));
  assert.equal(calls.length, 0);
});
