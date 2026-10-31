import { expect, test, type Page } from "@playwright/test";
import { seedProduct } from "./catalog";
import { expectOrderStatus } from "./orders-ui";
import { uid } from "./uid";

// The orders list against the real server, with the Reservation column's answer (and, where stated,
// the order list's) given by the test: a row can then show Order=PENDING next to Reservation=CANCELLED
// (a tombstone: the order was cancelled before inventory saw it created), which a search of the whole
// row for "CANCELLED" cannot tell from an order that is cancelled.

const SHORT = { timeout: 1_000 };

async function reservationsAre(page: Page, status: string) {
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", (route) => {
    const orderIds: string[] = route.request().postDataJSON().orderIds ?? [];
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ reservations: orderIds.map((orderId) => ({ orderId, status, lines: [], shortfallSkus: [] })) }),
    });
  });
}

async function ordersAre(page: Page, customerId: string, status: string) {
  await page.route("**/ordering.v1.OrderService/ListOrders", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ orders: [{ id: crypto.randomUUID(), customerId, status, items: [], total: { amountMinor: "1299", currency: "USD" } }] }),
    }),
  );
}

test("Order PENDING next to Reservation CANCELLED is not a cancelled order", async ({ page }) => {
  const customer = `e2e-os-${uid()}`;
  await ordersAre(page, customer, "ORDER_STATUS_PENDING");
  await reservationsAre(page, "RESERVATION_STATUS_CANCELLED");
  await page.goto(`/orders?customer=${customer}`);
  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row.getByTestId("reservation-status")).toHaveText("CANCELLED");
  await expectOrderStatus(page, row, "PENDING");
  await expect(expectOrderStatus(page, row, "CANCELLED", SHORT), "CANCELLED for an order that is PENDING").rejects.toThrow();
});

test("an order that is cancelled is CANCELLED, whatever its reservation says", async ({ page, request }) => {
  const customer = `e2e-os-${uid()}`;
  const sku = `E2E-OS-${uid()}`;
  await seedProduct(request, sku);
  const created = await request.post("/api/ordering.v1.OrderService/CreateOrder", { data: { customerId: customer, items: [{ sku, quantity: 1 }] } });
  expect(created.ok(), `CreateOrder: ${created.status()} ${await created.text()}`).toBe(true);
  const id = (await created.json()).order.id as string;
  const cancelled = await request.post("/api/ordering.v1.OrderService/CancelOrder", { data: { id } });
  expect(cancelled.ok(), `CancelOrder: ${cancelled.status()} ${await cancelled.text()}`).toBe(true);

  await reservationsAre(page, "RESERVATION_STATUS_RESERVED");
  await page.goto(`/orders?customer=${customer}`);
  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row.getByTestId("reservation-status")).toHaveText("RESERVED");
  await expectOrderStatus(page, row, "CANCELLED");
  await expect(expectOrderStatus(page, row, "PENDING", SHORT), "PENDING for an order that is CANCELLED").rejects.toThrow();
});
