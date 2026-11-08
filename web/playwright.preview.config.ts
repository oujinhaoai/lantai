import { defineConfig, devices } from "@playwright/test";

// Frontend-only verification against explicit synthetic responses. This entry
// never authenticates to a server and does not replace the HTTPS integration run.
if (process.env.LANTAI_PREVIEW_MOCKS !== "1")
  throw new Error("Enable the explicit synthetic frontend preview harness.");
export default defineConfig({
  testDir: "./tests",
  testMatch: "file-preview.spec.ts",
  workers: 1,
  retries: 0,
  timeout: 30000,
  reporter: [["list"]],
  use: {
    baseURL: "http://127.0.0.1:4188",
    ignoreHTTPSErrors: false,
    screenshot: "only-on-failure",
  },
  projects: [
    {
      name: "chromium-preview",
      use: {
        ...devices["Desktop Chrome"],
        channel: "chromium",
        launchOptions: {
          chromiumSandbox: true,
          executablePath: process.env.LANTAI_WEB_CHROMIUM_EXECUTABLE,
        },
      },
    },
    { name: "webkit-mobile-preview", use: { ...devices["iPhone 13"] } },
    { name: "webkit-tablet-preview", use: { ...devices["iPad Pro 11"] } },
  ],
  webServer: {
    command:
      "npm run preview -- --config vite.preview.config.ts --port 4188 --strictPort",
    url: "http://127.0.0.1:4188",
    reuseExistingServer: false,
  },
});
