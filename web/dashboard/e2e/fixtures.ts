import { test as base, type Page, expect } from "@playwright/test"

/**
 * Shared e2e fixtures.
 *
 * The dashboard is behind cookie auth (CONSOLE_COOKIE_NAME). Rather than drive
 * the login form in every test, the `authedPage` fixture pre-seeds the cookie so
 * each test lands inside the app. The cookie value is the FNV-1a hash of the
 * gateway password ("v1:" + hash); the default matches the docker-compose
 * GATEWAY_API_KEY. Override via E2E_GATEWAY_KEY for other setups.
 */

const GATEWAY_KEY = process.env.E2E_GATEWAY_KEY ?? "deploy-test-key"

// FNV-1a 32-bit hash, matching internal/consoleauth/auth.go. Rendered as 8-char
// lowercase hex (the auth token is "v1:" + this).
function fnvHash(secret: string): string {
  let hash = 0x811c9dc5
  for (let i = 0; i < secret.length; i++) {
    hash ^= secret.charCodeAt(i)
    // Math.imul gives 32-bit multiply semantics.
    hash = Math.imul(hash, 0x01000193)
  }
  // >>> 0 forces unsigned 32-bit, then pad to 8 hex chars.
  return (hash >>> 0).toString(16).padStart(8, "0")
}

export function authToken(): string {
  return "v1:" + fnvHash(GATEWAY_KEY)
}

// Navigate the SPA via its hash router (the most reliable cross-language
// selector — nav button labels are i18n'd). Returns nothing; await the URL.
export async function gotoHash(page: Page, hash: string): Promise<void> {
  await page.goto(`/${hash}`)
}

// A page that is already authenticated. Use as `({ authedPage })` in a test.
export const test = base.extend<{ authedPage: Page }>({
  authedPage: async ({ page }, use) => {
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
