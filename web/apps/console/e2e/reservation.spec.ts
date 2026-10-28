import { expect, test } from "@playwright/test";
import { withDB } from "./mode";
import { uid } from "./uid";

// The whole event chain through the UI: CreateOrder → ordering outbox → worker →
// inventory reserves → GetReservation shows RESERVED → StockReserved → the order
// shows CONFIRMED; Cancel (still allowed) → CANCELLED + RELEASED, or Complete →
// COMPLETED + COMMITTED (on hand drops). Needs the worker e2e/worker.setup.ts
// starts, so DB mode only.
test.skip(!withDB, "needs Postgres + worker (E2E_DATABASE_URL)");

const WORKER = { timeout: 30_000 };

test("an order is reserved by the worker and confirmed, cancelling releases it", async ({
  page,
}) => {
  const sku = `E2E-W-${uid()}`;
  const customer = `e2e-worker-${uid()}`;

  await page.goto(`/stock?prefix=${sku}`); // paged by SKU: filter, or older E2E-* rows push ours off page 1
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
  // Ordering consumed inventory's StockReserved: the saga closes on the happy path too.
  await expect(row.getByText("CONFIRMED")).toBeVisible(WORKER);

  // Stock shows the hold.
  await page.goto(`/stock?prefix=${sku}`);
  const stockRow = page.getByRole("row").filter({ hasText: sku });
  await expect(stockRow).toContainText("2"); // reserved
  await expect(stockRow).toContainText("3"); // available

  await page.goto(`/orders?customer=${customer}`);
  await expect(row.getByText("CONFIRMED")).toBeVisible();
  await row.getByRole("button", { name: "Cancel" }).click();
  await expect(row.getByText("CANCELLED")).toBeVisible();
  await expect(row.getByText("RELEASED")).toBeVisible(WORKER);

  // The badge links to the reservation itself.
  await row.getByRole("link", { name: "RELEASED" }).click();
  await expect(page).toHaveURL(/\/reservations\?order=/);
  await expect(page.getByText("RELEASED").first()).toBeVisible();
});

test("completing a confirmed order commits the reservation: the stock is sold", async ({
  page,
}) => {
  const sku = `E2E-C-${uid()}`;
  const customer = `e2e-complete-${uid()}`;

  await page.goto(`/stock?prefix=${sku}`); // paged by SKU: filter, or older E2E-* rows push ours off page 1
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
  // Complete only shows once inventory answered (CONFIRMED). "No Complete while PENDING" is not
  // asserted here: the worker can confirm before the check runs, so it never settles to 0
  // (orders.spec.ts asserts it where no worker runs).
  await expect(row.getByText("CONFIRMED")).toBeVisible(WORKER);
  await row.getByRole("button", { name: "Complete" }).click();
  await expect(row.getByText("COMPLETED")).toBeVisible();
  // Terminal: neither button is left.
  await expect(row.getByRole("button")).toHaveCount(0);
  await expect(row.getByText("COMMITTED")).toBeVisible(WORKER);

  // The stock was sold: on hand 5 → 3, nothing reserved.
  await page.goto(`/stock?prefix=${sku}`);
  const stockRow = page.getByRole("row").filter({ hasText: sku });
  await expect(stockRow.getByRole("cell").nth(1)).toHaveText("3"); // on hand
  await expect(stockRow.getByRole("cell").nth(2)).toHaveText("0"); // reserved

  // The COMMITTED filter lists it.
  await page.goto(`/reservations?status=COMMITTED`);
  await expect(
    page.getByRole("button", { name: "COMMITTED", exact: true })
  ).toBeVisible();
});
