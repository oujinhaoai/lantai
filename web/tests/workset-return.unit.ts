import test from "node:test";
import assert from "node:assert/strict";
import { returnRights, reviseReturnedWorkset } from "../src/workset-return";
import type { PushReceipt, Rights } from "../src/managed-transfer";
import type { WorksetInput } from "../src/workset-helpers";

const entry = {
  asset_id: "01J00000000000000000000001",
  version_id: "01J00000000000000000000002",
  destination: "project",
};
const input: WorksetInput = {
  schema: "lantai.client-workset/v1",
  entries: [entry],
  entrypoints: ["project/model.obj"],
  dependencies: [
    { from: "project/model.obj", requires: ["project/materials.mtl"] },
  ],
};
const receipt = {
  asset_id: entry.asset_id,
  version_id: "01J00000000000000000000003",
} as PushReceipt;
const rights: Rights = {
  usage: "reference",
  license: "LicenseRef-Synthetic",
  redistribute_raw: false,
  noai: true,
  sensitivity: "personal",
};

test("new version rights only narrow baseline usage and preserve all other restrictions", () => {
  assert.deepEqual(returnRights(rights, "restricted"), {
    ...rights,
    usage: "restricted",
  });
  assert.deepEqual(rights, {
    usage: "reference",
    license: "LicenseRef-Synthetic",
    redistribute_raw: false,
    noai: true,
    sensitivity: "personal",
  });
  assert.throws(() => returnRights(rights, "production"), /许可范围/);
  assert.throws(
    () => returnRights({ ...rights, usage: "restricted" }, "reference"),
    /许可范围/,
  );
  assert.throws(
    () => returnRights({ ...rights, noai: undefined }, "reference"),
    /许可范围/,
  );
  assert.throws(() => returnRights(undefined, "reference"), /许可范围/);
});
test("workset revision is an explicit new fixed reference and leaves original input intact", () => {
  const revised = reviseReturnedWorkset(input, entry, receipt);
  assert.equal(input.entries[0].version_id, entry.version_id);
  assert.equal(revised.entries[0].version_id, receipt.version_id);
  assert.deepEqual(revised.dependencies, input.dependencies);
  assert.deepEqual(revised.entrypoints, input.entrypoints);
});
test("changed workset, wrong asset, same version, invalid version and pending result cannot revise references", () => {
  assert.throws(
    () =>
      reviseReturnedWorkset(
        { ...input, entries: [{ ...entry, destination: "other" }] },
        entry,
        receipt,
      ),
    /已改变/,
  );
  for (const wrong of [
    { ...receipt, asset_id: "01J00000000000000000000004" },
    { ...receipt, version_id: entry.version_id },
    { ...receipt, version_id: "latest" },
    { ...receipt, description_pending: true },
  ])
    assert.throws(() => reviseReturnedWorkset(input, entry, wrong));
});
