import { expect, test, type Page } from "@playwright/test";
import { uid } from "./uid";

// docs/payment.md §2: the Console shows PAID and the order's payment_required snapshot. Nothing in
// slice 1 produces a PAID order (the path that reaches it lands with Payment), so the orders are
// served by the test; what is checked is that the Console renders what the contract says.

const order = (id: string, customer: string, status: string, paymentRequired: boolean) => ({
  id,
  customerId: customer,
  status,
  paymentRequired,
  items: [{ sku: "SKU-1", quantity: 2, unitPrice: { amountMinor: "250", currency: "USD" } }],
  total: { amountMinor: "500", currency: "USD" },
});

async function serve(page: Page, o: ReturnType<typeof order>) {
  const listed: unknown[] = [];
  await page.route("**/ordering.v1.OrderService/GetOrder", (route) => route.fulfill({ json: { order: o } }));
  await page.route("**/ordering.v1.OrderService/ListOrders", (route) => {
    listed.push(route.request().postDataJSON());
    return route.fulfill({ json: { orders: [o] } });
  });
  return listed;
}

for (const [paymentRequired, text] of [
  [true, "Required"],
  [false, "Not required"],
] as const) {
  test(`detail: a PAID order with payment_required=${paymentRequired} shows PAID and "${text}"`, async ({ page }) => {
    const customer = `e2e-pay-${uid()}`;
    const id = "22222222-3333-4444-5555-666666666666";
    await serve(page, order(id, customer, "ORDER_STATUS_PAID", paymentRequired));
    await page.goto(`/orders/${id}`);

    await expect(page.getByTestId("order-status")).toHaveText("PAID");
    await expect(page.getByTestId("order-payment-required")).toHaveText(text);
    // PAID cannot be cancelled (docs/payment.md §2): no Cancel button.
    await expect(page.getByRole("button", { name: "Cancel" })).toHaveCount(0);
  });
}

test("list: the PAID filter asks for PAID, and each row shows its payment_required", async ({ page }) => {
  const customer = `e2e-pay-${uid()}`;
  const id = "22222222-3333-4444-5555-666666666666";
  const listed = await serve(page, order(id, customer, "ORDER_STATUS_PAID", true));
  await page.goto(`/orders?customer=${customer}`);

  await page.getByRole("button", { name: "PAID", exact: true }).click();
  await expect.poll(() => listed.some((req) => (req as { status?: string }).status === "ORDER_STATUS_PAID")).toBe(true);

  const row = page.getByRole("row").filter({ hasText: customer });
  await expect(row.getByTestId("payment-required")).toHaveText("Required");
});
