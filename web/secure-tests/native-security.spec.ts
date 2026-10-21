import { test, expect } from "./fixtures";
import { request as httpsRequest } from "node:https";
import { readFile } from "node:fs/promises";

test("native Chromium uses sandbox and validates the local CA and hostname", async ({
  page,
  baseURL,
}) => {
  if (!baseURL)
    throw new Error("The explicit synthetic HTTPS baseURL is missing.");
  const origin = new URL(baseURL);
  expect(origin.protocol).toBe("https:");
  expect(["localhost", "127.0.0.1"]).toContain(origin.hostname);
  const ca = await readFile(process.env.LANTAI_WEB_CA_FILE!);
  const connect = (extra: { ca: Buffer | never[]; servername?: string }) =>
    new Promise<number>((resolve, reject) => {
      const request = httpsRequest(
        `${origin.origin}/ready`,
        { ...extra, rejectUnauthorized: true, timeout: 5000 },
        (response) => {
          response.resume();
          resolve(response.statusCode || 0);
        },
      );
      request.on("error", reject);
      request.on("timeout", () =>
        request.destroy(new Error("synthetic TLS timeout")),
      );
      request.end();
    });
  await expect(connect({ ca: [] })).rejects.toThrow();
  await expect(
    connect({ ca, servername: "wrong-host.synthetic.invalid" }),
  ).rejects.toThrow();
  expect(await connect({ ca })).toBe(204);
  await page.goto("/");
  await expect(page.getByLabel("账户名称")).toBeVisible();
  expect(new URL(page.url()).origin).toBe(origin.origin);
});
