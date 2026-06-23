import { defineConfig, devices } from "@playwright/test"

/**
 * Playwright config for the LLM Relay dashboard e2e tests.
 *
 * The dashboard is served by the Go gateway on :3300 (same origin serves the
 * SPA + the /__console API). Tests point at a running gateway; start it with
 * `docker-compose up -d` first. Auth is via a cookie, which tests seed through
 * a global setup so every test starts already logged in.
 *
 * Run:
 *   npx playwright test              # headless
 *   npx playwright test --headed     # visible browser
 */
export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false, // the tests share one DB / gateway; run sequentially
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:3300",
    trace: "on-first-retry",
    screenshot: "only-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
  // No webServer: the gateway is assumed running (docker-compose). The Go binary
  // embeds the built frontend, so there's no separate dev server to boot.
})
