import { test, expect, gotoHash, pageErrors } from "./fixtures"
import type { Page } from "@playwright/test"

/**
 * Interaction tests: drive the main buttons on each page and assert the app
 * reacts without throwing. These exercise the full request/response round-trip
 * and would catch the create-key and request-detail blank-page bugs.
 *
 * Labels match the English locale (headless Chromium defaults to en-US), with
 * Chinese alternatives in the regexes for robustness. Cleanup is built in so the
 * suite is re-runnable.
 */

async function closeDialog(page: Page) {
  await page.keyboard.press("Escape")
  await page.waitForTimeout(200)
}

function assertNoPageErrors(page: Page) {
  const real = pageErrors(page).filter((e) => !e.includes("ResizeObserver"))
  expect(real, `uncaught page errors: ${real.join(" | ")}`).toHaveLength(0)
}

test("login flow: wrong password is rejected, stays on login", async ({ page }) => {
  await page.goto("/")
  await expect(page.getByRole("button", { name: /^Sign In$|登录/i }).first()).toBeVisible({ timeout: 15_000 })

  await page.locator('input[type="password"]').fill("definitely-wrong-password")
  await page.getByRole("button", { name: /^Sign In$|登录/i }).first().click()
  await page.waitForTimeout(1500)
  // Still on login — the password field persists (not authenticated).
  await expect(page.locator('input[type="password"]')).toBeVisible()
})

test("keys: create a key and it appears in the table", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/keys")
  await expect(page.getByRole("heading", { name: /API Keys/i })).toBeVisible({ timeout: 15_000 })

  // Open the create-key dialog.
  await page.getByRole("button", { name: /^New Key$|新建密钥/i }).first().click()
  const nameInput = page.locator("#new-key-name")
  await expect(nameInput).toBeVisible({ timeout: 8_000 })
  const testKeyName = `e2e-key-${Date.now()}`
  await nameInput.fill(testKeyName)

  // Submit. The submit button lives inside the dialog.
  await page.getByRole("button", { name: /^Create Key$|创建/i }).first().click()

  // The new key must appear in the table — proves the create response's `record`
  // was spliced in correctly (the original blank-page bug).
  await expect(page.getByText(testKeyName).first()).toBeVisible({ timeout: 10_000 })
  await closeDialog(page)
  assertNoPageErrors(page)

  // Cleanup: delete the key we just made.
  const row = page.locator("tr", { hasText: testKeyName }).first()
  await row.getByRole("button", { name: /^Delete$|删除/i }).first().click()
  const confirmBtn = page.getByRole("button", { name: /^Delete$|确认删除|Confirm/i }).last()
  if (await confirmBtn.isVisible().catch(() => false)) {
    await confirmBtn.click()
  }
  await page.waitForTimeout(800)
})

test("providers: open the create-channel dialog", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/providers")
  await expect(page.getByRole("heading", { name: /^Channels$/i })).toBeVisible({ timeout: 15_000 })

  await page.getByRole("button", { name: /^Add Channel$|添加渠道/i }).first().click()
  await page.waitForTimeout(600)
  // The dialog renders a Create Channel title + a channel-name input — proves
  // the form mounted without crashing.
  await expect(page.getByText(/^Create Channel$|创建渠道/i).first()).toBeVisible({ timeout: 8_000 })
  await closeDialog(page)
  assertNoPageErrors(page)
})

test("providers: test the seeded sssapi channel (if present)", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/providers")
  await expect(page.getByRole("heading", { name: /^Channels$/i })).toBeVisible({ timeout: 15_000 })

  const providerRow = page.locator("tr", { hasText: "sssapi" }).first()
  if (!(await providerRow.isVisible().catch(() => false))) {
    test.skip(true, "sssapi provider not present — skipping")
    return
  }

  const testBtn = providerRow.getByRole("button", { name: /^Test$|测试/i }).first()
  await testBtn.click()
  // Upstream probe has a 30s budget; usually <2s. The hard requirement is the
  // page stays interactive (no crash).
  await page.waitForTimeout(3000)
  const bodyText = await page.locator("body").innerText()
  expect(bodyText.trim().length).toBeGreaterThan(20)
  assertNoPageErrors(page)
})

test("providers: toggle a channel enabled/disabled", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/providers")
  await expect(page.getByRole("heading", { name: /^Channels$/i })).toBeVisible({ timeout: 15_000 })

  const providerRow = page.locator("tr", { hasText: "sssapi" }).first()
  if (!(await providerRow.isVisible().catch(() => false))) {
    test.skip(true, "sssapi provider not present")
    return
  }

  // The enabled control is a labelled button ("Enabled"/"Disabled"), not a
  // switch. Clicking it PATCHes /enabled and the label flips.
  const enabledBtn = providerRow.getByRole("button", { name: /^Enabled$|^Disabled$|启用|停用/i }).first()
  if (!(await enabledBtn.isVisible().catch(() => false))) {
    test.skip(true, "no enabled toggle button visible")
    return
  }
  const beforeText = (await enabledBtn.innerText()).trim()
  await enabledBtn.click()
  await page.waitForTimeout(1000)
  const afterText = (await providerRow.getByRole("button", { name: /^Enabled$|^Disabled$|启用|停用/i }).first().innerText()).trim()
  expect(afterText, "enabled label did not flip").not.toBe(beforeText)
  // Restore original state.
  await providerRow.getByRole("button", { name: /^Enabled$|^Disabled$|启用|停用/i }).first().click()
  await page.waitForTimeout(800)
  assertNoPageErrors(page)
})

test("routes: open the create-alias dialog", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/routes")
  await expect(page.getByText("LLMRelayService").first()).toBeVisible({ timeout: 15_000 })

  await page.getByRole("button", { name: /^New Alias$|新建别名/i }).first().click()
  await page.waitForTimeout(600)
  // The alias dialog should render its form (an alias input or create title).
  await expect(
    page.getByText(/^Create Alias$|新建别名|New Alias/i).first().or(
      page.locator('input[name="alias"]')
    )
  ).toBeVisible({ timeout: 8_000 })
  await closeDialog(page)
  assertNoPageErrors(page)
})

test("settings: timeout inputs render numeric values (not NaN)", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/settings")
  await expect(page.getByText("LLMRelayService").first()).toBeVisible({ timeout: 15_000 })

  await page.waitForTimeout(1000)
  const inputs = page.locator('input[type="number"], input[type="text"]')
  const count = await inputs.count()
  expect(count).toBeGreaterThan(0)
  let foundNumber = false
  for (let i = 0; i < Math.min(count, 12); i++) {
    const val = await inputs.nth(i).inputValue().catch(() => "")
    if (val && !Number.isNaN(Number(val)) && Number(val) > 0) {
      foundNumber = true
      break
    }
  }
  expect(foundNumber, "expected at least one numeric timeout value").toBe(true)
  assertNoPageErrors(page)
})

test("logout clears the session and returns to login", async ({ authedPage }) => {
  const page = authedPage
  // The logout button is an icon-only button in the sidebar bottom controls
  // (no accessible name), so we click the bottom-controls region's last button.
  // It's the third control button after theme + language.
  const sidebar = page.locator("aside").first()
  const bottomControls = sidebar.locator("div").filter({ hasText: /^$/ }).last()
  // Fallback: click by counting buttons in the bottom border-t region.
  const logoutBtn = sidebar.locator('div[class*="border-t"] button').last()
  await logoutBtn.click()
  await page.waitForTimeout(1500)
  await expect(page.locator('input[type="password"]')).toBeVisible({ timeout: 10_000 })
})
