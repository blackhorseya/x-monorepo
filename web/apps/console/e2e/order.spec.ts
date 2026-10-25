import { expect, test } from "@playwright/test";
import { withDB } from "./mode";

// /orders/$id: the id cell of the list links here; the page shows every line,
// the total, the reservation, and cancels. A deep link to an unknown id says so.

test("the list links to the order; the page shows lines, cancels, and polls the status", async ({
  page,
}) => {
  const customer = `e2e-detail-${Date.now()}`;
  // With a worker, an order for a SKU nobody stocked is REJECTED and cancelled
  // by inventory before this test reaches its Cancel button — so seed stock for
  // a SKU of our own and the Reservation card gets to show RESERVED.
  const sku = withDB ? `E2E-D-${Date.now()}` : "SKU-1";
  if (withDB) {
    await page.goto("/stock");
    await page.getByLabel("SKU", { exact: true }).fill(sku);
    await page.getByLabel("On hand").fill("5");
    await page.getByRole("button", { name: "Set", exact: true }).click();
    await expect(page.getByRole("row").filter({ hasText: sku })).toHaveCount(1);
  }
  await page.goto("/orders");
  await page.getByLabel("Customer").first().fill(customer);
  await page.getByLabel("SKU").first().fill(sku);
  await page.getByRole("button", { name: "Create order" }).click();
  await page.getByLabel("Customer").nth(1).fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();

  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row).toHaveCount(1);
  await row.getByRole("link", { name: /^[0-9a-f]{8}$/ }).click();
  await expect(page).toHaveURL(/\/orders\/[^/?]+$/);
  const id = new URL(page.url()).pathname.split("/").pop()!;
  await expect(
    page.locator('[data-slot="card-title"]', { hasText: id })
  ).toBeVisible();
  await expect(page.getByText(`customer ${customer}`)).toBeVisible();

  const lines = page.getByRole("row").filter({ hasText: sku });
  await expect(lines).toHaveCount(1);
  await expect(lines).toContainText("12.99 USD"); // unit
  await expect(lines).toContainText("25.98 USD"); // 2 × 1299 minor
  await expect(page.getByTestId("order-total")).toContainText("25.98 USD");
  await expect(page.getByTestId("order-status")).toHaveText("PENDING");
  // Without a worker nothing reserves; with one the card flips to RESERVED at
  // the worker's pace and shows the reserved line.
  const reservation = page
    .locator('[data-slot="card"]', { hasText: "Reservation" })
    .last();
  if (withDB) {
    await expect(reservation.getByText("RESERVED")).toBeVisible({
      timeout: 30_000,
    });
    await expect(reservation).toContainText(`${sku} × 2`);
  } else {
    await expect(reservation.getByText("none yet")).toBeVisible();
  }
  await expect(
    page.getByRole("link", { name: "Open in Reservations" })
  ).toHaveAttribute("href", `/reservations?order=${id}`);

  await page.getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByTestId("order-status")).toHaveText("CANCELLED");
  await expect(page.getByRole("button", { name: "Cancel" })).toHaveCount(0);
  if (withDB)
    await expect(reservation.getByText("RELEASED")).toBeVisible({
      timeout: 30_000,
    });

  // Back on the list the row agrees (same cache key prefix, invalidated).
  await page.getByRole("link", { name: "All orders" }).click();
  await expect(page).toHaveURL(/\/orders$/);
  await page.getByLabel("Customer").nth(1).fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();
  await expect(row.getByText("CANCELLED")).toBeVisible();
});

test("an unknown id is a not-found page, not an error", async ({ page }) => {
  await page.goto("/orders/order-that-never-existed");
  await expect(
    page.locator('[data-slot="card-title"]', {
      hasText: "order-that-never-existed",
    })
  ).toBeVisible();
  await expect(page.getByText("No such order")).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByRole("link", { name: "All orders" }).click();
  await expect(page).toHaveURL(/\/orders$/);
});

test("the Order id box on /orders opens the page for a pasted id", async ({
  page,
}) => {
  const customer = `e2e-jump-${Date.now()}`;
  await page.goto("/orders");
  await page.getByLabel("Customer").first().fill(customer);
  await page.getByRole("button", { name: "Create order" }).click();
  await page.getByLabel("Customer").nth(1).fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();
  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row).toHaveCount(1);
  // The cell shows 8 characters; the link's title carries the whole id.
  const id = await row
    .getByRole("link", { name: /^[0-9a-f]{8}$/ })
    .getAttribute("title");
  expect(id).toMatch(/^[0-9a-f-]{36}$/);

  const open = page.getByRole("button", { name: "Open", exact: true });
  await expect(open).toBeDisabled(); // nothing to open yet
  await page.getByLabel("Order id", { exact: true }).fill(`  ${id}  `); // pasted with whitespace
  await open.click();
  await expect(page).toHaveURL(`/orders/${id}`);
  await expect(
    page.locator('[data-slot="card-title"]', { hasText: id! })
  ).toBeVisible();
  await expect(page.getByText(`customer ${customer}`)).toBeVisible();

  // An id nobody has lands on the not-found page, still no alert.
  await page.getByRole("link", { name: "All orders" }).click();
  await page.getByLabel("Order id", { exact: true }).fill("no-such-order");
  await page.getByRole("button", { name: "Open", exact: true }).click();
  await expect(page).toHaveURL("/orders/no-such-order");
  await expect(page.getByText("No such order")).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
});
