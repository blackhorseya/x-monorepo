import { expect, test, type Page } from "@playwright/test";
import { withDB } from "./mode";
import { seedProduct } from "./catalog";
import { uid } from "./uid";
import { cellUnder, createCard, filterCard } from "./orders-ui";

// Cancel and Complete on an order ask first: Cancel gives the reservation back, Complete commits
// it (stock sold) and cannot be undone. Declining must send nothing.

const order = (id: string, customer: string) => ({
  id,
  customerId: customer,
  status: "ORDER_STATUS_CONFIRMED",
  items: [{ sku: "SKU-1", quantity: 1, unitPrice: { amountMinor: "100", currency: "USD" } }],
  total: { amountMinor: "100", currency: "USD" },
});

// A CONFIRMED order served by the console's own queries, whatever the worker does, and a counter
// for the CompleteOrder calls that reach the network.
async function scriptConfirmed(page: Page, id: string, customer: string) {
  const completes: string[] = [];
  await page.route("**/ordering.v1.OrderService/ListOrders", (route) => route.fulfill({ json: { orders: [order(id, customer)] } }));
  await page.route("**/ordering.v1.OrderService/GetOrder", (route) => route.fulfill({ json: { order: order(id, customer) } }));
  await page.route("**/ordering.v1.OrderService/CompleteOrder", (route) => {
    completes.push(id);
    return route.fulfill({ json: { order: { ...order(id, customer), status: "ORDER_STATUS_COMPLETED" } } });
  });
  return completes;
}

test("list: Complete asks first, says it cannot be undone, and declining sends nothing", async ({ page }) => {
  const customer = `e2e-confirm-${uid()}`;
  const id = "11111111-2222-3333-4444-555555555555";
  const completes = await scriptConfirmed(page, id, customer);
  await page.goto(`/orders?customer=${customer}`);
  const row = page.getByRole("row").filter({ hasText: customer });

  await row.getByRole("button", { name: "Complete" }).click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toContainText(/cannot be undone/i);
  await expect(dialog).toContainText(/commits the reservation/i);
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  expect(completes).toEqual([]);

  await row.getByRole("button", { name: "Complete" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Complete order" }).click();
  await expect.poll(() => completes.length).toBe(1);
});

test("detail: Complete asks first and declining sends nothing", async ({ page }) => {
  const customer = `e2e-confirm-${uid()}`;
  const id = "11111111-2222-3333-4444-555555555555";
  const completes = await scriptConfirmed(page, id, customer);
  await page.goto(`/orders/${id}`);

  await page.getByRole("button", { name: "Complete" }).click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toContainText(/cannot be undone/i);
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  expect(completes).toEqual([]);

  await page.getByRole("button", { name: "Complete" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Complete order" }).click();
  await expect.poll(() => completes.length).toBe(1);
});

test("list: Cancel asks first; declining leaves the order PENDING", async ({ page, request }) => {
  const customer = `e2e-confirm-${uid()}`;
  // With a worker, an order for a SKU nobody stocked is REJECTED and cancelled by inventory, so
  // its Cancel button vanishes whenever the worker beats the click. Stock a SKU of our own, as
  // orders.spec.ts does; the order then settles at CONFIRMED and stays cancellable.
  const sku = `E2E-C-${uid()}`;
  await seedProduct(request, sku);
  if (withDB) {
    await page.goto(`/stock?prefix=${sku}`);
    await page.getByLabel("SKU", { exact: true }).fill(sku);
    await page.getByLabel("On hand").fill("5");
    await page.getByRole("button", { name: "Set", exact: true }).click();
    await expect(page.getByRole("row").filter({ hasText: sku })).toHaveCount(1);
  }
  await page.goto("/orders");
  await createCard(page).getByLabel("Customer").fill(customer);
  await createCard(page).getByLabel("SKU").fill(sku);
  await page.getByRole("button", { name: "Create order" }).click();
  await filterCard(page).getByLabel("Customer").fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();
  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row).toHaveCount(1);
  const status = await cellUnder(page, row, "Order");

  await row.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByRole("alertdialog")).toBeVisible();
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByRole("alertdialog")).toHaveCount(0);
  await expect(status).toHaveText(withDB ? /^(PENDING|CONFIRMED)$/ : "PENDING");
  await expect(row.getByRole("button", { name: "Cancel", exact: true })).toBeVisible();

  await row.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByRole("alertdialog")).toBeVisible();
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel order" }).click();
  await expect(status).toHaveText("CANCELLED");
});
