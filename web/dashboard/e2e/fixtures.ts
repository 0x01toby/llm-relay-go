import { test as base, type Page, expect } from "@playwright/test"

/**
 * Shared e2e fixtures.
 *
 * The dashboard is behind cookie auth (CONSOLE_COOKIE_NAME). The `authedPage`
 * fixture pre-seeds the cookie so each test lands inside the app, and also
 * captures uncaught page errors so tests can assert no shape-mismatch crash
 * occurred. Inspect via `pageErrors(page)`.
 */

const GATEWAY_KEY = process.env.E2E_GATEWAY_KEY ?? "deploy-test-key"

// FNV-1a 32-bit hash, matching internal/consoleauth/auth.go.
function fnvHash(secret: string): string {
  let hash = 0x811c9dc5
  for (let i = 0; i < secret.length; i++) {
    hash ^= secret.charCodeAt(i)
    hash = Math.imul(hash, 0x01000193)
  }
  return (hash >>> 0).toString(16).padStart(8, "0")
}

export function authToken(): string {
  return "v1:" + fnvHash(GATEWAY_KEY)
}

// Stash the error list on the page object so any test can read it.
type ErrorPage = Page & { __pageErrors?: string[] }

export function pageErrors(page: Page): string[] {
  return (page as ErrorPage).__pageErrors ?? []
}

// Navigate the SPA via its hash router (reliable cross-language selector).
export async function gotoHash(page: Page, hash: string): Promise<void> {
  await page.goto(`/${hash}`)
}

export const test = base.extend<{ authedPage: Page }>({
  authedPage: async ({ page }, use) => {
    const errors: string[] = []
    ;(page as ErrorPage).__pageErrors = errors
    page.on("pageerror", (err) => errors.push(err.message))

    await page.context().addCookies([
      {
        name: "CONSOLE_COOKIE_NAME",
        value: authToken(),
        domain: "localhost",
        path: "/",
      },
    ])
    await page.goto("/")
    await use(page)
  },
})

export { expect }
