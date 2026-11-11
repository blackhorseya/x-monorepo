import { expect, test, type Page } from "@playwright/test";
import { routeProbe } from "./probe";

// The console decides what an admin sees from one probe (ListPoison, limit 1). A probe that could not
// be answered (5xx, network) says nothing about the role: it is retried, and when it stays
// unanswered the console says so and offers a retry, instead of quietly hiding the admin UI.
// auth.mode is off here, so every caller is an admin once the probe is answered: with a database the answer is
// 200, without one FAILED_PRECONDITION admin.no_database, which the console also reads as "admin".
// Only the probe is intercepted (e2e/probe.ts): the Poison page asks the same RPC for its own list.

const unavailable = { status: 503, json: { code: "unavailable", message: "e2e: injected" } };
const pruneLink = (page: Page) => page.getByRole("link", { name: "Prune" });

test("a first probe that fails with 503 is retried and the admin UI appears without a reload", async ({ page }) => {
  let calls = 0;
  await routeProbe(page, (route) => (++calls === 1 ? route.fulfill(unavailable) : route.continue()));
  await page.goto("/poison");
  await expect(pruneLink(page)).toBeVisible({ timeout: 15_000 });
  expect(calls).toBeGreaterThanOrEqual(2);
});

test("a probe that stays unanswered says so, and Retry brings the admin UI back", async ({ page }) => {
  let failing = true;
  await routeProbe(page, (route) => (failing ? route.fulfill(unavailable) : route.continue()));
  await page.goto("/poison");
  const notice = page.getByText("Could not confirm admin access");
  await expect(notice).toBeVisible({ timeout: 30_000 });
  await expect(pruneLink(page)).toHaveCount(0);

  failing = false;
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(pruneLink(page)).toBeVisible();
  await expect(notice).toHaveCount(0);
});
