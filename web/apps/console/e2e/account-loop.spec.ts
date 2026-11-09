import { expect, test } from "@playwright/test";
import { uid } from "./uid";

// The write path of the console through the real server (and, in the image project, through nginx with no
// page.route in the way): /accounts creates an account and the list shows it, its page opens. With auth off
// the caller passes the admin gate. The auth project (e2e/auth/) covers who may see the pages.

test("create an account, find it in the list, open it", async ({ page }) => {
  const name = `e2e-loop-${uid()}`;
  await page.goto("/accounts");
  await page.getByLabel("Name", { exact: true }).fill(name);
  await page.getByRole("button", { name: "Create account" }).click();

  const row = page.getByRole("row").filter({ hasText: name });
  await expect(row).toHaveCount(1);
  await row.getByRole("link", { name }).click();
  await expect(page).toHaveURL(/\/accounts\/[^/]+$/);
  await expect(page.getByText("No sign-ins bound yet")).toBeVisible();
});
