import { expect, test } from "@playwright/test";

// The smoke server has no database, so AdminService answers FAILED_PRECONDITION
// with x-error-code admin.no_database on every call: what is checked here is
// that the pages reach the server and show that code, not poison / prune itself.

test("/poison shows the server's error code", async ({ page }) => {
  await page.goto("/poison");
  const alert = page.getByRole("alert");
  await expect(alert).toContainText("Request failed");
  await expect(alert).toContainText("admin.no_database");
});

test("/prune is prefilled from the URL and a dry run reaches the server", async ({ page }) => {
  await page.goto("/prune?bc=ordering&days=7&forget=old.ordering");
  await expect(page.getByLabel("Bounded context (empty = all)")).toHaveValue("ordering");
  await expect(page.getByLabel("Older than (days)")).toHaveValue("7");
  await expect(page.getByLabel("Forget groups (comma-separated)")).toHaveValue("old.ordering");
  await page.getByRole("button", { name: "Dry run", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("admin.no_database");
});

test("/reservations renders its empty state", async ({ page }) => {
  await page.goto("/reservations");
  await expect(page.locator('[data-slot="card-title"]', { hasText: "Reservations" })).toBeVisible();
});
