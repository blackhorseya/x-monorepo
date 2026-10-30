import { expect, test } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { settle, useKey } from "./helpers";
import { uid } from "../uid";

// An admin on an account page swaps the credential for one without the admin role. The bindings
// list is admin-only, so nothing may ask for it with the new credential: not even the refetch the
// credential control triggers for every active query before the page's admin gate has closed.

test("admin → role-less key on the account page: ListAccountIdentities is never sent with the new key", async ({ page }) => {
  const name = `e2e-bindcred-${uid()}`;
  await page.goto("/accounts");
  await useKey(page, ADMIN_KEY);
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByRole("button", { name: "Create account" }).click();
  await page.getByRole("row").filter({ hasText: name }).getByRole("link", { name }).click();
  await expect(page.getByText("No sign-ins bound yet")).toBeVisible();

  const withOps: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("ListAccountIdentities") && r.headers()["x-api-key"] === OPS_KEY) withOps.push(r.url());
  });
  await useKey(page, OPS_KEY);
  await expect(page.getByText("Admin role required")).toBeVisible();
  await settle(page);
  await page.waitForTimeout(500);
  expect(withOps).toEqual([]);
  await expect(page.getByTestId("identity-row")).toHaveCount(0);
});
