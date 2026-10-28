import { expect, test } from "@playwright/test";
import { withDB } from "./mode";

// The server keeps orders in memory for the whole run and the tests run in
// parallel, so each one works under its own customer id and filters by it.
const customer = (t: string) => `e2e-${t}-${Date.now()}`;

test("/ lands on /orders", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/orders$/);
  await expect(page.locator('[data-slot="card-title"]', { hasText: "Orders" })).toBeVisible();
});

test("create, see, cancel an order", async ({ page }) => {
  const id = customer("cancel");
  // With a worker, an order for a SKU nobody stocked is REJECTED and cancelled by inventory
  // (a racing PENDING / Cancel button), so seed stock for a SKU of our own, as order.spec.ts does.
  const sku = withDB ? `E2E-O-${Date.now()}` : "SKU-1";
  if (withDB) {
    await page.goto(`/stock?prefix=${sku}`); // paged by SKU: filter, or older E2E-* rows push ours off page 1
    await page.getByLabel("SKU", { exact: true }).fill(sku);
    await page.getByLabel("On hand").fill("5");
    await page.getByRole("button", { name: "Set", exact: true }).click();
    await expect(page.getByRole("row").filter({ hasText: sku })).toHaveCount(1);
  }
  await page.goto("/orders");
  await page.getByLabel("Customer").first().fill(id);
  await page.getByLabel("SKU").first().fill(sku);
  await page.getByRole("button", { name: "Create order" }).click();

  await page.getByLabel("Customer").nth(1).fill(id);
  await page.getByRole("button", { name: "Filter" }).click();
  await expect(page).toHaveURL(new RegExp(`customer=${id}`));

  const row = page.getByRole("row").filter({ hasText: id });
  await expect(row).toHaveCount(1);
  // The order's own status cell (4th): the reservation cell can show a CANCELLED badge too.
  const status = row.getByRole("cell").nth(3);
  // With a worker the order may already be CONFIRMED (StockReserved consumed) by now.
  await expect(status).toHaveText(withDB ? /^(PENDING|CONFIRMED)$/ : "PENDING");
  await expect(row.getByText("25.98 USD")).toBeVisible(); // 2 × 1299 minor USD
  if (!withDB) {
    // Without a worker nothing ever reserves, so the order stays PENDING: nothing to complete yet.
    await expect(row.getByText("none yet")).toBeVisible();
    await expect(row.getByRole("button", { name: "Complete" })).toHaveCount(0);
  }
  // With one (DB mode) the reservation cell flips to RESERVED at its own pace, which
  // e2e/reservation.spec.ts covers.

  await row.getByRole("button", { name: "Cancel" }).click();
  await expect(status).toHaveText("CANCELLED");
  await expect(row.getByRole("button", { name: "Cancel" })).toHaveCount(0);

  // The status filter is a search param and the list follows it.
  await page.getByRole("button", { name: "PENDING", exact: true }).click();
  await expect(page).toHaveURL(/status=PENDING/);
  await expect(page.getByText(`No orders with status PENDING for ${id} yet.`)).toBeVisible();
});

test("validation failures surface the field", async ({ page }) => {
  await page.goto("/orders");
  await page.getByLabel("Customer").first().fill(customer("invalid"));
  await page.getByLabel("Qty").fill("0");
  await page.getByRole("button", { name: "Create order" }).click();
  const alert = page.getByRole("alert");
  await expect(alert).toContainText("Request failed");
  await expect(alert).toContainText("request.validation_failed");
  await expect(alert).toContainText("items[0].quantity");
});
