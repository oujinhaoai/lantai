import { defineConfig, devices } from "@playwright/test";
import base from "./playwright.config";

// Separate acceptance entry. The fixture provisions its CA only through
// Chromium's native certificate manager in a private persistent test profile.
export default defineConfig({
  ...base,
  testDir: "./secure-tests",
  projects: [
    {
      name: "chromium-sandbox-strict-tls",
      use: {
        ...devices["Desktop Chrome"],
        ignoreHTTPSErrors: false,
        launchOptions: { chromiumSandbox: true },
      },
    },
  ],
});
