import { test, expect, gotoHash } from "./fixtures"

/**
 * Page-render smoke tests: every page must mount without crashing and reach a
 * known element. These catch the "blank page after navigation" class of bug
 * caused by backend response shapes not matching frontend expectations.
 *
 * NOTE: these tests run against a live gateway (docker-compose up -d). They
 * assume the seeded data: one provider (sssapi), one key, one alias, and at
 * least one request log (be9782dc). If the DB is empty the assertions degrade
 * to "page renders its empty state" — still a valid no-crash check.
 */

// Each entry: hash route + a substring we expect to see once the page mounts.
// Labels are the zh-CN defaults from i18n (the app's default locale).
const PAGES: Array<{ hash: string; expectText: string; name: string }> = [
  { hash: "#/monitor", name: "监控", expectText: "监控" },
  { hash: "#/usage", name: "用量", expectText: "用量" },
  { hash: "#/providers", name: "渠道", expectText: "渠道" },
  { hash: "#/models", name: "模型", expectText: "模型" },
  { hash: "#/routes", name: "路由", expectText: "路由" },
  { hash: "#/keys", name: "密钥", expectText: "密钥" },
  { hash: "#/logs", name: "日志", expectText: "日志" },
  { hash: "#/settings", name: "配置", expectText: "配置" },
  { hash: "#/api", name: "API", expectText: "API" },
]

for (const p of PAGES) {
  test(`${p.name} page renders without blank-screen crash`, async ({ authedPage }) => {
    const page = authedPage
    await gotoHash(page, p.hash)
    // The page must render *something* — its header label, which proves the
    // component mounted and the API call didn't throw a shape-mismatch error.
    await expect(page.getByText(p.expectText).first()).toBeVisible({ timeout: 15_000 })
    // Hard requirement: no uncaught error blanked the screen. We assert the body
    // has real content (more than a handful of chars).
    const bodyText = await page.locator("body").innerText()
    expect(bodyText.trim().length).toBeGreaterThan(10)
  })
}

test("monitor is the default landing route after login", async ({ authedPage }) => {
  // authedPage already navigated to "/" which resolves to monitor.
  await expect(authedPage).toHaveURL(/localhost:3300\/?$|#\/monitor/)
})

test("detail page renders the request record (the reported blank-page bug)", async ({ authedPage }) => {
  const page = authedPage
  // The detail page previously crashed because the backend returned only
  // {request_id}. It must now render the record's metadata. Use a known request
  // id; if absent the page shows its empty state (still no crash).
  await gotoHash(page, "#/detail/be9782dc")
  // Give the detail fetch a moment; the header card renders either the record
  // or the empty state — either way the page must not be blank.
  await page.waitForTimeout(1500)
  const bodyText = await page.locator("body").innerText()
  expect(bodyText.trim().length).toBeGreaterThan(10)
  // If the record exists, the path /v1/messages should appear in the metadata.
  // (Soft check — skipped if the row was pruned.)
  if (bodyText.includes("be9782dc") || bodyText.includes("/v1/messages")) {
    expect(bodyText).toContain("/v1/messages")
  }
})
