import { expect, test } from "@playwright/test";
import { withDB } from "./mode";
import { seedProduct } from "./catalog";
import { uid } from "./uid";
import { cellUnder, confirm, createCard, filterCard, expectOrderStatus } from "./orders-ui";
import { setStock } from "./stock-ui";
import { expectReservationStatus } from "./reservation-ui";

// The whole event chain through the UI: CreateOrder → ordering outbox → worker →
// inventory reserves → GetReservation shows RESERVED → StockReserved → the order
// shows CONFIRMED; Cancel (still allowed) → CANCELLED + RELEASED, or Complete →
// COMPLETED + COMMITTED (on hand drops). Needs the worker e2e/worker.setup.ts
// starts, so DB mode only.
test.skip(!withDB, "needs Postgres + worker (E2E_DATABASE_URL)");

const WORKER = { timeout: 30_000 };

test("an order is reserved by the worker and confirmed, cancelling releases it", async ({
  page,
  request,
}) => {
  const sku = `E2E-W-${uid()}`;
  const customer = `e2e-worker-${uid()}`;
  await seedProduct(request, sku);

  await page.goto(`/stock?prefix=${sku}`); // paged by SKU: filter, or older E2E-* rows push ours off page 1
  await setStock(page, sku, 5);

  await page.goto("/orders");
  await createCard(page).getByLabel("Customer").fill(customer);
  await createCard(page).getByLabel("SKU").fill(sku);
  await page.getByLabel("Qty").fill("2");
  await page.getByRole("button", { name: "Create order" }).click();
  await filterCard(page).getByLabel("Customer").fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();

  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row).toHaveCount(1);
  await expectReservationStatus(row, "RESERVED", WORKER);
  // Ordering consumed inventory's StockReserved: the saga closes on the happy path too.
  await expectOrderStatus(page, row, "CONFIRMED", WORKER);

  // Stock shows the hold.
  await page.goto(`/stock?prefix=${sku}`);
  const stockRow = page.getByRole("row").filter({ hasText: sku });
  await expect(stockRow).toContainText("2"); // reserved
  await expect(stockRow).toContainText("3"); // available

  await page.goto(`/orders?customer=${customer}`);
  await expectOrderStatus(page, row, "CONFIRMED");
  await row.getByRole("button", { name: "Cancel" }).click();
  await confirm(page, "Cancel order");
  await expectOrderStatus(page, row, "CANCELLED");
  await expectReservationStatus(row, "RELEASED", WORKER);

  // The badge links to the reservation itself.
  await row.getByRole("link", { name: "RELEASED" }).click();
  await expect(page).toHaveURL(/\/reservations\?order=/);
  // The pinned card (the first card), not a search of the page: the status filter has a RELEASED button.
  await expectReservationStatus(page.locator('[data-slot="card"]').first(), "RELEASED");
});

test("completing a confirmed order commits the reservation: the stock is sold", async ({
  page,
  request,
}) => {
  const sku = `E2E-C-${uid()}`;
  const customer = `e2e-complete-${uid()}`;
  await seedProduct(request, sku);

  await page.goto(`/stock?prefix=${sku}`); // paged by SKU: filter, or older E2E-* rows push ours off page 1
  await setStock(page, sku, 5);

  await page.goto("/orders");
  await createCard(page).getByLabel("Customer").fill(customer);
  await createCard(page).getByLabel("SKU").fill(sku);
  await page.getByLabel("Qty").fill("2");
  await page.getByRole("button", { name: "Create order" }).click();
  await filterCard(page).getByLabel("Customer").fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();

  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row).toHaveCount(1);
  // Complete only shows once inventory answered (CONFIRMED). "No Complete while PENDING" is not
  // asserted here: the worker can confirm before the check runs, so it never settles to 0
  // (orders.spec.ts asserts it where no worker runs).
  await expectOrderStatus(page, row, "CONFIRMED", WORKER);
  await row.getByRole("button", { name: "Complete" }).click();
  await confirm(page, "Complete order");
  await expectOrderStatus(page, row, "COMPLETED");
  // Terminal: neither button is left.
  await expect(row.getByRole("button")).toHaveCount(0);
  await expectReservationStatus(row, "COMMITTED", WORKER);

  // The stock was sold: on hand 5 → 3, nothing reserved.
  await page.goto(`/stock?prefix=${sku}`);
  const stockRow = page.getByRole("row").filter({ hasText: sku });
  await expect(await cellUnder(page, stockRow, "On hand")).toHaveText("3");
  await expect(await cellUnder(page, stockRow, "Reserved")).toHaveText("0");

  // The COMMITTED filter lists it.
  await page.goto(`/reservations?status=COMMITTED`);
  await expect(
    page.getByRole("button", { name: "COMMITTED", exact: true })
  ).toBeVisible();
});
