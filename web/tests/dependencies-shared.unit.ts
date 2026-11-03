import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { analyzeSelectedDependencies } from "../src/dependency-parser";

test("browser shares exact dependency byte/semantic fixtures with CLI/MCP parser", async () => {
  const cases = JSON.parse(
    await readFile(
      new URL(
        "../../tests/fixtures/dependency-analysis/cases.json",
        import.meta.url,
      ),
      "utf8",
    ),
  ) as {
    name: string;
    sources: { path: string; text: string }[];
    comfy_input_root?: string;
    complete: boolean;
    expected: {
      format: string;
      references: string[];
      notice_count: number;
      complete: boolean;
      sha256: string;
    }[];
  }[];
  for (const fixture of cases) {
    const out = await analyzeSelectedDependencies(
      fixture.sources.map(({ path, text }) => ({
        path,
        bytes: new TextEncoder().encode(text),
      })),
      { comfyInputRoot: fixture.comfy_input_root },
    );
    assert.equal(out.complete, fixture.complete, fixture.name);
    assert.deepEqual(
      out.files.map((file) => ({
        format: file.format,
        references: file.references,
        notice_count: file.notices.length,
        complete: file.complete,
        sha256: file.sha256,
      })),
      fixture.expected,
      fixture.name,
    );
  }
});
