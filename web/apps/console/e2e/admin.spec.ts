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
  // No bounded context consumes events yet, so no schema is hosted: the dry run covers "all" (an empty
  // field), which is nothing, and a named one would be admin.unknown_bounded_context.
  await page.goto("/prune?days=7&forget=old-group");
  await expect(page.getByLabel("Bounded context (empty = all)")).toHaveValue("");
  await expect(page.getByLabel("Older than (days)")).toHaveValue("7");
  await expect(page.getByLabel("Forget groups (comma-separated)")).toHaveValue("old-group");
  await page.getByRole("button", { name: "Dry run", exact: true }).click();
  if (!withDB) {
    await expect(page.getByRole("alert")).toContainText("admin.no_database");
    return;
  }
  // Dry run: the result card says what *would* go, per table, nothing deleted.
  await expect(page.locator('[data-slot="card-title"]', { hasText: /^would delete \d+ row\(s\)$/ })).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
});

test("/prune takes the bounded context from the URL", async ({ page }) => {
  await page.goto("/prune?bc=example");
  await expect(page.getByLabel("Bounded context (empty = all)")).toHaveValue("example");
});
