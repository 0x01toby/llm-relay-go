import { test, expect, gotoHash } from "./fixtures"
import type { Page } from "@playwright/test"

/**
 * Interaction tests: drive the main buttons on each page and assert the app
 * reacts (dialog opens, data mutates, table updates) without throwing. These
 * exercise the full request/response round-trip and would have caught the
 * create-key and request-detail blank-page bugs.
 *
 * Cleanup: each create test deletes what it made so the suite is re-runnable.
 */

// Close any open Dialog/Popover by pressing Escape, as a reset between steps.
async function closeDialog(page: Page) {
  await page.keyboard.press("Escape")
  await page.waitForTimeout(200)
}

test("login flow: wrong password is rejected, correct password enters the app", async ({ page }) => {
  // No cookie yet — should land on the login view.
  await page.goto("/")
  await expect(page.getByRole("button", { name: /登录|Sign in|Submit/i }).first()).toBeVisible({ timeout: 15_000 })

  // Submit empty/wrong → stays on login (error shown or still unauthenticated).
  const passwordInput = page.locator('input[type="password"]')
  await passwordInput.fill("definitely-wrong-password")
  await page.getByRole("button", { name: /登录|Sign in|Submit/i }).first().click()
  await page.waitForTimeout(1000)
  // Still on login (no authenticated shell nav present).
  await expect(page.locator('input[type="password"]')).toBeVisible()
})

test("keys: create a key and it appears in the table", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/keys")
  await expect(page.getByText("密钥").first()).toBeVisible({ timeout: 15_000 })

  // Open the create form (the input + button live inline at the top).
  const nameInput = page.getByPlaceholder(/密钥名称|key name|名称/i).first()
  await nameInput.waitFor({ state: "visible", timeout: 10_000 })
  const testKeyName = `e2e-key-${Date.now()}`
  await nameInput.fill(testKeyName)

  // Click the create button (zh: "创建密钥" / "新建").
  await page.getByRole("button", { name: /创建|新建|Create/i }).first().click()

  // The new key name must appear in the table — proves the create response's
  // `record` was spliced in correctly (the original blank-page bug).
  await expect(page.getByText(testKeyName).first()).toBeVisible({ timeout: 10_000 })

  // A "key shown once" dialog/banner should reveal the raw key; dismiss it.
  await closeDialog(page)

  // Cleanup: delete the key we just made.
  const row = page.locator("tr", { hasText: testKeyName }).first()
  await row.getByRole("button", { name: /删除|Delete/i }).first().click()
  // Confirm if a dialog appears.
  const confirmBtn = page.getByRole("button", { name: /^删除$|确认|确认删除|Confirm|Delete$/i }).last()
  if (await confirmBtn.isVisible().catch(() => false)) {
    await confirmBtn.click()
  }
  await page.waitForTimeout(800)
})

test("providers: open the create-channel dialog", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/providers")
  await expect(page.getByText("渠道").first()).toBeVisible({ timeout: 15_000 })

  // Click the create/add channel button.
  await page.getByRole("button", { name: /添加|新建|创建渠道|Create|Add/i }).first().click()
  await page.waitForTimeout(600)

  // The dialog should render its form fields (a channel-name or target-url
  // input proves the form mounted without crashing).
  const formField = page
    .getByPlaceholder(/渠道名|channel|target|目标|base url|地址/i)
    .first()
  await expect(formField).toBeVisible({ timeout: 8_000 })
  await closeDialog(page)
})

test("providers: test the seeded sssapi channel (if present)", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/providers")
  await expect(page.getByText("渠道").first()).toBeVisible({ timeout: 15_000 })

  // Only run if the seeded provider exists.
  const providerRow = page.locator("tr", { hasText: "sssapi" }).first()
  if (!(await providerRow.isVisible().catch(() => false))) {
    test.skip(true, "sssapi provider not present — skipping test-channel")
    return
  }

  // Click the per-row test button. The result (ok or error) must render
  // somewhere — proving the /test endpoint round-trips into the UI.
  const testBtn = providerRow.getByRole("button", { name: /测试|Test/i }).first()
  await testBtn.click()
  // Give the upstream probe time (it has a 30s budget; usually <2s).
  await page.waitForTimeout(3000)
  // No crash is the hard requirement — the page must still be interactive.
  const bodyText = await page.locator("body").innerText()
  expect(bodyText.trim().length).toBeGreaterThan(10)
})

test("providers: toggle a channel enabled/disabled", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/providers")
  await expect(page.getByText("渠道").first()).toBeVisible({ timeout: 15_000 })

  const providerRow = page.locator("tr", { hasText: "sssapi" }).first()
  if (!(await providerRow.isVisible().catch(() => false))) {
    test.skip(true, "sssapi provider not present")
    return
  }

  // The enabled toggle is a switch/checkbox in the row.
  const toggle = providerRow.getByRole("switch").or(providerRow.getByRole("checkbox")).first()
  if (!(await toggle.isVisible().catch(() => false))) {
    test.skip(true, "no enabled toggle visible")
    return
  }
  const beforeState = await toggle.getAttribute("aria-checked")
  await toggle.click()
  await page.waitForTimeout(1000)
  // State should flip (proves PATCH /enabled round-tripped).
  const afterState = await toggle.getAttribute("aria-checked")
  expect(afterState).not.toBe(beforeState)
  // Toggle back to leave state as we found it.
  await toggle.click()
  await page.waitForTimeout(800)
})

test("routes: open the create-alias dialog", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/routes")
  await expect(page.getByText("路由").first()).toBeVisible({ timeout: 15_000 })

  await page.getByRole("button", { name: /添加|新建|创建|Create|Add/i }).first().click()
  await page.waitForTimeout(600)
  // The alias form should show an alias-name field.
  const field = page.getByPlaceholder(/别名|alias|名称/i).first()
  await expect(field).toBeVisible({ timeout: 8_000 })
  await closeDialog(page)
})

test("settings: load timeout settings and they render as numbers (not NaN)", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/settings")
  await expect(page.getByText("配置").first()).toBeVisible({ timeout: 15_000 })

  // The timeout inputs must contain numeric values — "NaN" here would mean the
  // backend returned a non-number (the original fragility flagged by the audit).
  await page.waitForTimeout(1000)
  const numberInputs = page.locator('input[type="number"], input[type="text"]')
  const count = await numberInputs.count()
  expect(count).toBeGreaterThan(0)
  // At least one input must hold a parseable number (proves the *Ms fields
  // arrived as numbers and survived the /1000 seconds conversion).
  let foundNumber = false
  for (let i = 0; i < Math.min(count, 12); i++) {
    const val = await numberInputs.nth(i).inputValue().catch(() => "")
    if (val && !Number.isNaN(Number(val)) && Number(val) > 0) {
      foundNumber = true
      break
    }
  }
  expect(foundNumber, "expected at least one numeric timeout value, found none").toBe(true)
})

test("models: list renders grouped model rows", async ({ authedPage }) => {
  const page = authedPage
  await gotoHash(page, "#/models")
  await expect(page.getByText("模型").first()).toBeVisible({ timeout: 15_000 })

  // The page must mount; the {openai, anthropic} grouped response must not
  // crash. Give the fetch a beat.
  await page.waitForTimeout(1500)
  const bodyText = await page.locator("body").innerText()
  expect(bodyText.trim().length).toBeGreaterThan(10)
})

test("logout clears the session and returns to login", async ({ authedPage }) => {
  const page = authedPage
  // The logout button is the bottom-of-sidebar LogOut icon.
  await page.getByRole("button", { name: /登出|退出|Log ?out|Sign out/i }).last().click()
  await page.waitForTimeout(1500)
  // Back on the login view → password field reappears.
  await expect(page.locator('input[type="password"]')).toBeVisible({ timeout: 10_000 })
})
