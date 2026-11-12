import { expect, test, type Page } from "@playwright/test";
import { failRequests } from "./fail-requests";
import { withDB } from "./mode";
import { PAGE_BODY } from "./probe";

// A read that fails ONCE for a reason that passes (a connection reset) must not leave the console showing an
// error forever. The rpc is the real bin/rpc: failRequests (e2e/fail-requests.ts) only breaks the calls a test
// names, everything else goes on to it. Real time, no fake clock: the backoff is the thing under test.
// The Poison list is polled every 5 s (src/queries.ts poisonQuery), so "the list recovered by itself" only
// means the retry did it, not the next poll, if it is on screen sooner than that.
const BEFORE_NEXT_POLL_MS = 4_000;

const PAGE_LIST = { method: "ListPoison", bodyIncludes: PAGE_BODY } as const;

// What the Poison page shows once its list was answered: with a database a table (rows from other runs
// may exist), without one the server's own answer, admin.no_database. Never the injected failure.
async function expectListAnswered(page: Page, timeout: number) {
  if (withDB) {
    await expect(page.getByRole("row").filter({ hasText: /Nothing parked\.|^\d+/ }).first()).toBeVisible({ timeout });
    await expect(page.getByRole("alert")).toHaveCount(0);
    return;
  }
  await expect(page.getByRole("alert")).toContainText("admin.no_database", { timeout });
  await expect(page.getByRole("alert")).not.toContainText(/unavailable/i);
}

test("(a2) the first Poison list resets the connection: the page recovers by itself", async ({ page }) => {
  const fault = await failRequests(page, { ...PAGE_LIST, mode: "reset" });

  await page.goto("/poison");
  await expectListAnswered(page, BEFORE_NEXT_POLL_MS);
  expect(fault.failed()).toBe(1);
  expect(fault.matched()).toBeGreaterThanOrEqual(2);
});
