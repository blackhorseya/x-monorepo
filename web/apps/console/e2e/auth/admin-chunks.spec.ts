import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY } from "../../playwright.config";

// The admin pages are their own chunks (lazy routes). When an admin's chunk cannot be loaded the console shows
// a visible error with the layout intact, and Retry loads it. Runs in the auth project: ADMIN_KEY has the admin role.

const seed = (page: Page, key: string) =>
  page.addInitScript((k) => localStorage.setItem("console.credential", JSON.stringify({ kind: "api_key", value: k })), key);
const POISON_CHUNK = "**/assets/poison-*.js";
const gate = (page: Page) => page.getByText("Admin role required");
const poisonPage = (page: Page) => page.getByText("Poison queue");

test("admin: a chunk that fails to load is a visible error, not a blank page, and Retry recovers", async ({ page }) => {
  await seed(page, ADMIN_KEY);
  let failing = true;
  await page.route(POISON_CHUNK, (route) => (failing ? route.abort("failed") : route.continue()));
  await page.goto("/poison");
  await expect(page.getByText("Failed to load this page")).toBeVisible();
  await expect(gate(page)).toHaveCount(0);
  await expect(page.locator("header nav")).toBeVisible(); // the layout survives

  failing = false;
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(poisonPage(page)).toBeVisible();
  await expect(page.getByText("Failed to load this page")).toHaveCount(0);
});
