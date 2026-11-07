import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  globalSetup: "./global-setup.js",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: 0, // tests must be deterministic; don't paper over flakiness
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  timeout: 15_000,
  expect: { timeout: 3_000 },
  use: {
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
