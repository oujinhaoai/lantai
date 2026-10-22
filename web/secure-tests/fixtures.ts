import { test as base, expect } from "@playwright/test";
import { mkdir, stat } from "node:fs/promises";
import { resolve } from "node:path";
import { trustLocalTestCA } from "../../scripts/browser-profile-trust.cjs";

export const test = base.extend({
  context: async ({ playwright, browserName }, provideContext, info) => {
    if (browserName !== "chromium")
      throw new Error(
        "This acceptance entry requires Chromium's sandbox and profile certificate manager.",
      );
    const caFile = process.env.LANTAI_WEB_CA_FILE;
    if (!caFile || process.env.NODE_EXTRA_CA_CERTS !== caFile)
      throw new Error(
        "Set the explicit test CA for both LANTAI_WEB_CA_FILE and NODE_EXTRA_CA_CERTS before starting Playwright.",
      );
    const profile = resolve(info.outputPath("private-browser-profile"));
    await mkdir(profile, { mode: 0o700 });
    const mode = (await stat(profile)).mode & 0o777;
    if (mode !== 0o700)
      throw new Error("The private browser profile must have mode 0700.");
    const context = await playwright.chromium.launchPersistentContext(profile, {
      executablePath: process.env.LANTAI_WEB_CHROMIUM_EXECUTABLE,
      headless: true,
      chromiumSandbox: true,
      ignoreHTTPSErrors: false,
      locale: "en-US",
      args: ["--lang=en-US"],
    });
    try {
      await trustLocalTestCA(context, resolve(caFile));
      await provideContext(context);
    } finally {
      await context.close();
    }
  },
});
export { expect };
