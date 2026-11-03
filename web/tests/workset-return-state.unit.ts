import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { validatePushState } from "../src/managed-transfer";
import type { WorksetReturn } from "../src/workset-return";
const cases = JSON.parse(
  readFileSync(
    new URL("../../tests/fixtures/workset-return/cases.json", import.meta.url),
    "utf8",
  ),
) as { name: string; return: WorksetReturn }[];
for (const fixture of cases) {
  test(`workset return preserves ${fixture.name} original state`, async () => {
    const value = fixture.return;
    await validatePushState(value.input, value.state, value.state.origin);
    await assert.rejects(
      validatePushState(
        value.input,
        { ...value.state, commit_key: value.state.create_key },
        value.state.origin,
      ),
    );
    await assert.rejects(
      validatePushState(
        {
          ...value.input,
          content: { ...value.input.content, version_note: "changed" },
        },
        value.state,
        value.state.origin,
      ),
    );
  });
}
