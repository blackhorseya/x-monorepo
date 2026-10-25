import { expect, test } from "@playwright/test";
import { withDB } from "./mode";

// Without a database AdminService answers FAILED_PRECONDITION with x-error-code
// admin.no_database on every call, and what is checked is that the pages reach
// the server and show that code. With one (E2E_DATABASE_URL) the poison list
// and a prune dry run run for real against freshly migrated tables.

test("/poison lists the poison tables (or says there is no database)", async ({ page }) => {
  await page.goto("/poison");
  if (!withDB) {
    const alert = page.getByRole("alert");
    await expect(alert).toContainText("Request failed");
    await expect(alert).toContainText("admin.no_database");
    return;
  }
  // Rows may exist from other runs against the same database (k6, task run);
  // what must hold is that the table rendered without an error.
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(page.getByRole("row").filter({ hasText: /Nothing parked\.|^\d+/ }).first()).toBeVisible();
});

test("/prune is prefilled from the URL and a dry run reaches the server", async ({ page }) => {
  await page.goto("/prune?bc=ordering&days=7&forget=old.ordering");
  await expect(page.getByLabel("Bounded context (empty = all)")).toHaveValue("ordering");
  await expect(page.getByLabel("Older than (days)")).toHaveValue("7");
  await expect(page.getByLabel("Forget groups (comma-separated)")).toHaveValue("old.ordering");
  await page.getByRole("button", { name: "Dry run", exact: true }).click();
  if (!withDB) {
    await expect(page.getByRole("alert")).toContainText("admin.no_database");
    return;
  }
  // Dry run: the result card says what *would* go, per table, nothing deleted.
  await expect(page.locator('[data-slot="card-title"]', { hasText: /^would delete \d+ row\(s\)$/ })).toBeVisible();
  await expect(page.getByRole("row").filter({ hasText: "ordering." }).first()).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  // A group badge in the note adds that group to forget (URL too), deduplicated. Which groups
  // are behind depends on what the worker is doing right now, so take whatever is there.
  const badges = page.getByRole("button", { name: /^forget / });
  if ((await badges.count()) > 0) {
    const group = (await badges.first().getAttribute("aria-label"))!.replace(/^forget /, "");
    await badges.first().click();
    await expect(page.getByLabel("Forget groups (comma-separated)")).toHaveValue(`old.ordering,${group}`);
    await expect(page).toHaveURL(new RegExp(`forget=old\\.ordering%2C${group.replace(/\./g, "\\.")}`));
    await badges.first().click();
    await expect(page.getByLabel("Forget groups (comma-separated)")).toHaveValue(`old.ordering,${group}`);
  }
});

test("/reservations renders", async ({ page }) => {
  await page.goto("/reservations");
  await expect(page.locator('[data-slot="card-title"]', { hasText: "Reservations" })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
});
