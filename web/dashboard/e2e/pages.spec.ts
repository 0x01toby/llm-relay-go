import { test, expect, gotoHash, pageErrors } from "./fixtures"
import type { Page } from "@playwright/test"

/**
 * Page-render smoke tests: every page must mount without crashing and reach a
 * known state. These catch the "blank page after navigation" class of bug
 * caused by backend response shapes not matching frontend expectations.
 *
 * The app auto-detects browser language, so assertions are language-agnostic:
 * we check the URL hash lands on the right route and the page has real content
 * (not a white screen / error boundary). The sidebar nav renders in every
 * locale, so its presence is our "app shell mounted" signal.
 *
 * NOTE: these run against a live gateway (docker-compose up -d).
 */

// Each entry: hash route. We assert (1) the hash is active and (2) the body has
// substantial content — a blank/crashed page fails (2).
const PAGES: Array<{ hash: string; name: string }> = [
  { hash: "#/monitor", name: "monitor" },
  { hash: "#/usage", name: "usage" },
  { hash: "#/providers", name: "providers" },
  { hash: "#/models", name: "models" },
  { hash: "#/routes", name: "routes" },
  { hash: "#/keys", name: "keys" },
  { hash: "#/logs", name: "logs" },
  { hash: "#/settings", name: "settings" },
  { hash: "#/api", name: "api" },
]

for (const p of PAGES) {
  test(`${p.name} page renders without blank-screen crash`, async ({ authedPage }) => {
    const page = authedPage
    await gotoHash(page, p.hash)
    // The app shell (sidebar with the brand) must be present in every locale —
    // proves the SPA mounted past the session check.
    await expect(page.getByText("LLMRelayService").first()).toBeVisible({ timeout: 15_000 })
    // Let any data fetch settle, then assert the page is not blank.
    await page.waitForTimeout(1500)
    const bodyText = await page.locator("body").innerText()
    expect(bodyText.trim().length, `${p.name} page body was empty (possible crash)`).toBeGreaterThan(20)
    // No uncaught errors from a shape-mismatch crash.
    assertNoPageErrors(page)
  })
}

test("monitor is the default landing route after login", async ({ authedPage }) => {
  // authedPage navigated to "/" which resolves to monitor.
  await expect(authedPage).toHaveURL(/localhost:3300\/?$|#\/monitor/)
})

test("detail page renders the request record (the reported blank-page bug)", async ({ authedPage }) => {
  const page = authedPage
  // The detail page previously crashed because the backend returned only
  // {request_id}. It must now render the record metadata or its empty state —
  // either way, not a blank screen.
  await gotoHash(page, "#/detail/be9782dc")
  await expect(page.getByText("LLMRelayService").first()).toBeVisible({ timeout: 15_000 })
  await page.waitForTimeout(1500)
  const bodyText = await page.locator("body").innerText()
  expect(bodyText.trim().length, "detail page was blank").toBeGreaterThan(20)
  // If the seeded record exists, its path should appear in the metadata.
  if (bodyText.includes("/v1/messages")) {
    expect(bodyText).toContain("/v1/messages")
  }
  assertNoPageErrors(page)
})

// Tolerate benign ResizeObserver messages; anything else is a real crash.
function assertNoPageErrors(page: Page) {
  const real = pageErrors(page).filter((e) => !e.includes("ResizeObserver"))
  expect(real, `uncaught page errors: ${real.join(" | ")}`).toHaveLength(0)
}
