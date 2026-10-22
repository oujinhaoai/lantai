import { test, expect } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { createServer } from "node:https";
import { request as httpsRequest } from "node:https";

for (const [name, certVariable, keyVariable] of [
  ["untrusted CA", "LANTAI_WEB_UNTRUSTED_CERT", "LANTAI_WEB_UNTRUSTED_KEY"],
  ["wrong hostname", "LANTAI_WEB_WRONG_HOST_CERT", "LANTAI_WEB_WRONG_HOST_KEY"],
]) {
  test(`browser rejects ${name} with TLS verification enabled`, async ({
    page,
    context,
    baseURL,
  }) => {
    const certPath = process.env[certVariable];
    const keyPath = process.env[keyVariable];
    if (!certPath || !keyPath)
      throw new Error(
        "Run the strict browser harness with its negative TLS fixtures.",
      );
    await page.goto(baseURL!);
    await expect(page.getByLabel("账户名称")).toBeVisible();
    const server = createServer(
      { cert: await readFile(certPath), key: await readFile(keyPath) },
      (_request, response) => response.end("TLS negative fixture"),
    );
    await new Promise<void>((resolve) =>
      server.listen(0, "127.0.0.1", resolve),
    );
    try {
      const address = server.address();
      if (!address || typeof address === "string")
        throw new Error("No fixture port");
      const target = `https://127.0.0.1:${address.port}/`;
      // Prove the server is reachable independently of the expected TLS rejection.
      await expect(
        new Promise((resolve, reject) => {
          const request = httpsRequest(
            target,
            { rejectUnauthorized: true },
            resolve,
          );
          request.on("error", reject);
          request.end();
        }),
      ).rejects.toThrow(/certificate|hostname|altname|self.signed/i);
      // Chromium can commit its error page after goto has rejected. Keep that
      // navigation on its own tab so it cannot interrupt the positive probe.
      const rejectedPage = await context.newPage();
      try {
        await expect(rejectedPage.goto(target)).rejects.toThrow(
          /certificate|SSL|TLS|CERT/i,
        );
      } finally {
        await rejectedPage.close();
      }
      await page.reload();
      await expect(page.getByLabel("账户名称")).toBeVisible();
    } finally {
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) =>
        server.close((error) => (error ? reject(error) : resolve())),
      );
    }
  });
}
