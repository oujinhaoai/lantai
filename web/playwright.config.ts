import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  fullyParallel: false,
  workers: 1,
  timeout: 30000,
  retries: 0,
  reporter: [["list"], ["html", { open: "never" }]],
  use: {
    baseURL: "https://localhost:4180",
    ignoreHTTPSErrors: true,
    screenshot: "only-on-failure",
    trace: "off",
  },
  projects: [
    {
      name: "chromium",
      use: {
        ...devices["Desktop Chrome"],
        channel: "chromium",
        viewport: { width: 1440, height: 1050 },
      },
    },
    { name: "webkit-mobile", use: { ...devices["iPhone 13"] } },
    { name: "webkit-tablet", use: { ...devices["iPad Pro 11"] } },
  ],
  webServer: {
    command:
      "cd .. && LANTAI_WEB_FIXTURE=1 go test -tags=lantai_test_hooks -run ^TestWebBrowserFixture$ -count=1 -timeout=15m ./tests/integration",
    url: "https://localhost:4180/ready",
    ignoreHTTPSErrors: true,
    reuseExistingServer: process.env.LANTAI_WEB_REUSE === "1",
    timeout: 120000,
  },
});
