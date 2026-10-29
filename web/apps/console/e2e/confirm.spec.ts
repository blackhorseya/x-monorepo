import { expect, test, type Page } from "@playwright/test";
import { uid } from "./uid";

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
  await expect(dialog).toContainText(/committed/i);
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

test("list: Cancel asks first; declining leaves the order PENDING", async ({ page }) => {
  const customer = `e2e-confirm-${uid()}`;
  await page.goto("/orders");
  await page.getByLabel("Customer").first().fill(customer);
  await page.getByRole("button", { name: "Create order" }).click();
  await page.getByLabel("Customer").nth(1).fill(customer);
  await page.getByRole("button", { name: "Filter" }).click();
  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row).toHaveCount(1);

  await row.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByRole("alertdialog")).toHaveCount(0);
  await expect(row.getByRole("button", { name: "Cancel", exact: true })).toBeVisible();

  await row.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel order" }).click();
  await expect(row.getByText("CANCELLED")).toBeVisible();
});
