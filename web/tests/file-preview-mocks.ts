import type { Page } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";

const mockVersions = new WeakMap<
  Page,
  (id: string) => {
    version: object;
    manifest: { content: { files: { size: number }[] } };
  }
>();
export function previewMockVersion(page: Page, id: string) {
  const get = mockVersions.get(page);
  if (!get) throw new Error("Synthetic version fixture missing.");
  return structuredClone(get(id));
}

// Explicit opt-in frontend fixture: all identities, manifests and bytes are
// synthetic. No real account, cookie or authority is created by these responses.
export async function previewMocks(page: Page) {
  if (process.env.LANTAI_PREVIEW_MOCKS !== "1")
    throw new Error("Synthetic preview mocks are disabled.");
  const projectID = "01J0000000000000000000000C";
  const instanceID = "01J0000000000000000000000D";
  const ids: Record<string, string> = {};
  const resources = new Map<
    string,
    { kind: string; name: string; files: Map<string, Buffer> }
  >();
  const text = new Map(
    Object.entries({
      "readme.md":
        "# 安全阅读\n\n这里汇总项目说明、预览图与配置示例。\n\n## 文件说明\n\n- README.md：项目使用说明\n- preview.png：示例预览图\n- config.json：基础配置示例\n\n```text\n" +
        "long_markdown_line_".repeat(100) +
        "\n```\n",
      "config.json": '{"name":"合成项目","enabled":true}\n',
      "source.py":
        "# 合成代码，不执行\nprint('只读预览_" +
        "long_code_line_".repeat(100) +
        "')\n",
      "notes.txt": "合成笔记\n",
      "参考资料/跨团队素材与多语言环境说明-very-long-filename-for-responsive-layout.txt":
        "合成长文件名\n",
      "disguised.txt": "PK synthetic renamed archive, never shown",
      "archive.zip": "PK synthetic archive, never expanded",
    }).map(([name, value]) => [name, Buffer.from(value)]),
  );
  text.set("binary.bin", Buffer.from([0, 1, 2, 3]));
  let serial = 1;
  for (const [name, ext, kind] of [
    ["text", "", "doc"],
    ["image", "png", "image"],
    ["restricted", "png", "image"],
    ["audio", "wav", "audio"],
    ["video", "mp4", "video"],
    ["unsupported", "", "doc"],
  ]) {
    const id = `01J0000000000000000000000${serial++}`;
    ids[name] = id;
    let files: Map<string, Buffer>;
    if (name === "text") files = new Map(text);
    else if (name === "unsupported")
      files = new Map(
        [...text].filter(
          ([file]) => file === "archive.zip" || file === "binary.bin",
        ),
      );
    else {
      files = new Map([
        [
          `sample.${ext}`,
          await readFile(
            new URL(
              `../../tests/fixtures/media/sample.${ext}`,
              import.meta.url,
            ),
          ),
        ],
      ]);
      if (name === "image") {
        for (const file of ["readme.md", "config.json", "notes.txt"])
          files.set(file, text.get(file)!);
        files.set(
          "wide.png",
          await readFile(
            new URL("../../tests/fixtures/media/wide.png", import.meta.url),
          ),
        );
      }
    }
    resources.set(id, { kind, name, files });
  }
  const version = (id: string) => ({
    asset_id: id,
    project_id: projectID,
    version_id: `01J0000000000000000000001${[...resources.keys()].indexOf(id) + 1}`,
    version_number: 1,
    committed_at: "2026-01-02T03:04:05.000Z",
    uri: "",
    manifest_digest: `sha256:${"a".repeat(64)}`,
  });
  const exact = (id: string) => {
    const resource = resources.get(id)!;
    const v = version(id);
    v.uri = `lantai://${instanceID}/assets/${id}/versions/${v.version_id}`;
    return {
      version: v,
      manifest: {
        instance_id: instanceID,
        project_id: projectID,
        asset_id: id,
        version_id: v.version_id,
        version_number: 1,
        manifest_digest: v.manifest_digest,
        content: {
          asset_type: resource.kind,
          files: [...resource.files].map(([name, body]) => ({
            path: name,
            size: body.length,
            role: "primary",
            sha256: createHash("sha256").update(body).digest("hex"),
          })),
        },
      },
    };
  };
  mockVersions.set(page, exact);
  await page.addInitScript(() =>
    sessionStorage.setItem(
      "lantai.csrf.v1",
      JSON.stringify({ session: "synthetic-session", value: "synthetic-csrf" }),
    ),
  );
  const grants = new Map<string, { body: Buffer; file: string }>();
  await page.route("**/*", (route) => {
    const url = new URL(route.request().url());
    if (
      url.origin !== "http://127.0.0.1:4188" ||
      !(
        url.pathname === "/" ||
        url.pathname.startsWith("/assets/") ||
        url.pathname.startsWith("/help/")
      )
    )
      return route.abort("blockedbyclient");
    return route.continue();
  });
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    const method = route.request().method();
    if (url.pathname === "/api/v1/whoami" && method === "GET")
      return route.fulfill({
        json: {
          principal: { name: "synthetic", display_name: "示例用户" },
          session: {
            session_id: "synthetic-session",
            channel: "browser",
            session_kind: "normal",
            expires_at: new Date(Date.now() + 3600000).toISOString(),
            scopes: ["self", "read"],
          },
        },
      });
    const parts = url.pathname.split("/");
    const id = parts[4];
    const resource = resources.get(id);
    if (!resource)
      return route.fulfill({
        status: 404,
        json: { error: { code: "NOT_FOUND" } },
      });
    const v = version(id);
    v.uri = `lantai://${instanceID}/assets/${id}/versions/${v.version_id}`;
    const assetPath = `/api/v1/assets/${id}`;
    const versionPath = `${assetPath}/versions/${v.version_id}`;
    if (url.pathname === `${versionPath}/read-grants` && method === "POST") {
      const input = route.request().postDataJSON();
      if (!["archive_review", "raw_export"].includes(input.purpose))
        return route.fulfill({
          status: 400,
          json: { error: { code: "SCHEMA_INVALID" } },
        });
      const body = resource.files.get(input.path);
      if (!body)
        return route.fulfill({
          status: 404,
          json: { error: { code: "NOT_FOUND" } },
        });
      if (resource.name === "restricted" && input.purpose === "raw_export")
        return route.fulfill({
          status: 403,
          json: { error: { code: "USE_RESTRICTED" } },
        });
      const token = `SYNTHETIC${grants.size}`;
      grants.set(token, { body, file: input.path });
      return route.fulfill({
        json: {
          url: `/xfer/v1/reads/${token}`,
          path: input.path,
          size: body.length,
          sha256: createHash("sha256").update(body).digest("hex"),
          range: true,
          expires_at: new Date(Date.now() + 120000).toISOString(),
        },
      });
    }
    if (url.pathname === versionPath && method === "GET")
      return route.fulfill({ json: exact(id) });
    if (url.pathname !== assetPath || method !== "GET")
      return route.fulfill({
        status: 404,
        json: { error: { code: "NOT_FOUND" } },
      });
    return route.fulfill({
      json: {
        asset_id: id,
        project_id: projectID,
        asset_type: resource.kind,
        latest: v,
        description: {
          title: "项目参考资料",
          slug: resource.name,
          summary: "合成示例，用于界面验证。",
          tags: [],
          subjects: [],
          revision: 1,
        },
      },
    });
  });
  await page.route("**/xfer/**", async (route) => {
    const grant = grants.get(
      new URL(route.request().url()).pathname.split("/").pop()!,
    );
    if (!grant) return route.fulfill({ status: 404 });
    const range = route
      .request()
      .headers()
      .range?.match(/^bytes=(\d+)-(\d*)$/);
    const start = range ? Number(range[1]) : 0;
    const end = range?.[2]
      ? Math.min(Number(range[2]), grant.body.length - 1)
      : grant.body.length - 1;
    const contentType = grant.file.endsWith(".png")
      ? "image/png"
      : grant.file.endsWith(".wav")
        ? "audio/wav"
        : grant.file.endsWith(".mp4")
          ? "video/mp4"
          : "text/plain; charset=utf-8";
    return route.fulfill({
      status: range ? 206 : 200,
      headers: range
        ? { "Content-Range": `bytes ${start}-${end}/${grant.body.length}` }
        : {},
      contentType,
      body: grant.body.subarray(start, end + 1),
    });
  });
  return ids;
}
