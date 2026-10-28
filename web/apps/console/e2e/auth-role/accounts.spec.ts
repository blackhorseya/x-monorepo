import { expect, test } from "@playwright/test";
import { NOT_OPERATOR_KEY, OPERATOR_KEY } from "../../playwright.config";
import { useKey } from "../auth/helpers";

// The server runs with auth.admin_role=operator. The admin gate of /accounts must follow what the
// server actually requires, not a role name baked into the console.

const nav = (page: import("@playwright/test").Page) => page.locator("header nav").getByRole("link", { name: "Accounts" });

test("a custom auth.admin_role opens the accounts pages, and 'admin' no longer does", async ({ page }) => {
  await page.goto("/orders");

  await useKey(page, OPERATOR_KEY);
  await expect(nav(page)).toBeVisible();
  await page.goto("/accounts");
  await expect(page.getByRole("button", { name: "Create account" })).toBeVisible();
  await expect(page.getByText("Admin role required")).toHaveCount(0);

  // Holds the role named "admin", which this server does not treat as admin.
  await useKey(page, NOT_OPERATOR_KEY);
  await expect(page.getByText("Admin role required")).toBeVisible();
  await expect(page.getByRole("button", { name: "Create account" })).toHaveCount(0);
  await expect(nav(page)).toHaveCount(0);
});
