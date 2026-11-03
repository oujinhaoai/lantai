import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { compareFiles, type ManifestFile } from "../src/asset-tools";
const cases = JSON.parse(
  readFileSync(
    new URL(
      "../../tests/fixtures/asset-derivations/comparison-cases.json",
      import.meta.url,
    ),
    "utf8",
  ),
) as {
  name: string;
  before: ManifestFile[];
  after: ManifestFile[];
  expected: string[];
}[];
for (const fixture of cases) {
  test(`shared comparison ${fixture.name}`, () => {
    assert.deepEqual(
      compareFiles(fixture.before, fixture.after).map((row) => row.kind),
      fixture.expected,
    );
  });
}
