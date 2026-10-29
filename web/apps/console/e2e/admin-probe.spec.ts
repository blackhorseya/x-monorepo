import { expect, test, type Page } from "@playwright/test";

// The console decides what an admin sees from one probe (ListAccounts). A probe that could not
// be answered (5xx, network) says nothing about the role: it is retried, and when it stays
// unanswered the console says so and offers a retry, instead of quietly hiding the admin UI.
// auth.mode is off here, so every caller is an admin once the probe is answered.

const PROBE = "**/identity.v1.IdentityService/ListAccounts";
const unavailable = { status: 503, json: { code: "unavailable", message: "e2e: injected" } };
const setButton = (page: Page) => page.getByRole("button", { name: "Set", exact: true });

test("a first probe that fails with 503 is retried and the admin UI appears without a reload", async ({ page }) => {
  let calls = 0;
  await page.route(PROBE, (route) => (++calls === 1 ? route.fulfill(unavailable) : route.continue()));
  await page.goto("/stock");
  await expect(setButton(page)).toBeVisible({ timeout: 15_000 });
  expect(calls).toBeGreaterThanOrEqual(2);
});

test("a probe that stays unanswered says so, and Retry brings the admin UI back", async ({ page }) => {
  let failing = true;
  await page.route(PROBE, (route) => (failing ? route.fulfill(unavailable) : route.continue()));
  await page.goto("/stock");
  const notice = page.getByText("Could not confirm admin access");
  await expect(notice).toBeVisible({ timeout: 30_000 });
  await expect(setButton(page)).toHaveCount(0);

  failing = false;
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(setButton(page)).toBeVisible();
  await expect(notice).toHaveCount(0);
});
