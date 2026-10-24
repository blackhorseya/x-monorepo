import { expect, test } from "@playwright/test";
import { withDB } from "./mode";

// The whole event chain through the UI: CreateOrder → ordering outbox → worker →
// inventory reserves → GetReservation shows RESERVED; Cancel → RELEASED. Needs
// the worker e2e/worker.setup.ts starts, so DB mode only.
test.skip(!withDB, "needs Postgres + worker (E2E_DATABASE_URL)");

const WORKER = { timeout: 30_000 };

test("an order is reserved by the worker, cancelling releases it", async ({ page }) => {
  const sku = `E2E-W-${Date.now()}`;
  const customer = `e2e-worker-${Date.now()}`;

  await page.goto("/stock");
  await page.getByLabel("SKU", { exact: true }).fill(sku);
  await page.getByLabel("On hand").fill("5");
  await page.getByRole("button", { name: "Set", exact: true }).click();
  await expect(page.getByRole("row").filter({ hasText: sku })).toHaveCount(1);

  await page.goto("/orders");
  await page.getByLabel("Customer").first().fill(customer);
  await page.getByLabel("SKU").first().fill(sku);
  await page.getByLabel("Qty").fill("2");
  await page.getByRole("button", { name: "Create order" }).click();
  await page.getByLabel("Customer").nth(1).fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();

  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row).toHaveCount(1);
  await expect(row.getByText("RESERVED")).toBeVisible(WORKER);

  // Stock shows the hold.
  await page.goto(`/stock?prefix=${sku}`);
  const stockRow = page.getByRole("row").filter({ hasText: sku });
  await expect(stockRow).toContainText("2"); // reserved
  await expect(stockRow).toContainText("3"); // available

  await page.goto(`/orders?customer=${customer}`);
  await row.getByRole("button", { name: "Cancel" }).click();
  await expect(row.getByText("CANCELLED")).toBeVisible();
  await expect(row.getByText("RELEASED")).toBeVisible(WORKER);

  // The badge links to the reservation itself.
  await row.getByRole("link", { name: "RELEASED" }).click();
  await expect(page).toHaveURL(/\/reservations\?order=/);
  await expect(page.getByText("RELEASED").first()).toBeVisible();
});
