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
    browserName,
  }) => {
    const certPath = process.env[certVariable];
    const keyPath = process.env[keyVariable];
    if (!certPath || !keyPath)
      throw new Error(
        "Run the strict browser harness with its negative TLS fixtures.",
      );
    await page.goto(baseURL!);
    await expect(page.getByLabel("账户名称")).toBeVisible();
    let receivedHTTPRequests = 0;
    const server = createServer(
      { cert: await readFile(certPath), key: await readFile(keyPath) },
      (_request, response) => {
        receivedHTTPRequests++;
        response.end("TLS negative fixture");
      },
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
      const failedRequests: string[] = [];
      let receivedResponses = 0;
      rejectedPage.on("requestfailed", (request) =>
        failedRequests.push(request.url()),
      );
      rejectedPage.on("response", (response) => {
        if (response.url() === target) receivedResponses++;
      });
      try {
        let rejection: unknown;
        try {
          await rejectedPage.goto(target);
        } catch (error) {
          rejection = error;
        }
        expect(rejection).toBeInstanceOf(Error);
        const message = (rejection as Error).message;
        // WebKit can surface a rejected provisional TLS navigation as this
        // cancellation. Require the failed request and no HTTP on either side,
        // rather than treating every navigation error as a certificate failure.
        expect(
          /certificate|SSL|TLS|CERT/i.test(message) ||
            (browserName === "webkit" &&
              /Operation was cancelled; maybe frame was detached/u.test(
                message,
              )),
        ).toBe(true);
        await expect.poll(() => failedRequests).toContain(target);
        expect(receivedResponses).toBe(0);
        expect(receivedHTTPRequests).toBe(0);
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
