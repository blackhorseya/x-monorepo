import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";
import { routeProbe } from "../probe";

// The admin capability probe (ListPoison, limit 1) against a server that really enforces the role. The admin key's answer
// is FAILED_PRECONDITION admin.no_database (HTTP 400): this server has no database, and the console reads it as "admin".
// A probe that could not be answered (5xx) says nothing about the role:
//   - failing once, it is retried and the admin UI appears once the server answers, on the same page, no reload;
//   - staying unanswered, the console says so ("Could not confirm admin access") and Retry brings the admin UI back.
// Runs in the auth project with ADMIN_KEY (the admin role). Only the probe is intercepted (e2e/probe.ts).

const unavailable = { status: 503, json: { code: "unavailable", message: "e2e: injected" } };
const NOTICE = "Could not confirm admin access";

const seed = (page: Page, key: string) =>
  page.addInitScript((k) => localStorage.setItem("console.credential", JSON.stringify({ kind: "api_key", value: k })), key);
const setButton = (page: Page) => page.getByRole("button", { name: "Dry run", exact: true });
const pruneLink = (page: Page) => page.getByRole("link", { name: "Prune" });
const poisonNav = (page: Page) => page.locator("header nav").getByRole("link", { name: "Poison", exact: true });

test("admin: a probe that fails once with 503 recovers on the same page, same credential, no reload", async ({ page }) => {
  await seed(page, ADMIN_KEY);
  let calls = 0;
  await routeProbe(page, (route) => (++calls === 1 ? route.fulfill(unavailable) : route.continue()));
  await page.goto("/prune");
  await expect(setButton(page)).toBeVisible({ timeout: 15_000 });
  await expect(poisonNav(page)).toBeVisible();
  await expect(page.getByText(NOTICE)).toHaveCount(0);
  expect(calls).toBeGreaterThanOrEqual(2);
});

test("a probe that stays unanswered says so, and Retry brings the admin UI back", async ({ page }) => {
  // The admin key: this server enforces the role, so the real answer after Retry is the admin one (admin.no_database).
  await seed(page, ADMIN_KEY);
  let failing = true;
  await routeProbe(page, (route) => (failing ? route.fulfill(unavailable) : route.continue()));
  await page.goto("/poison");
  const notice = page.getByText(NOTICE);
  await expect(notice).toBeVisible({ timeout: 30_000 });
  await expect(pruneLink(page)).toHaveCount(0);

  failing = false;
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(pruneLink(page)).toBeVisible();
  await expect(notice).toHaveCount(0);
});
