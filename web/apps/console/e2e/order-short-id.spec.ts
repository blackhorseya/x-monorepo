import { expect, test } from "@playwright/test";
import { uid } from "./uid";

// UUID v7 starts with a millisecond timestamp, so orders created within ~65s share their first 8
// hex digits. The list's short ID must still tell them apart. Scripted rows with such IDs make the
// test independent of when it runs (two real orders could straddle a prefix boundary).

const order = (id: string, customer: string) => ({
  id,
  customerId: customer,
  status: "ORDER_STATUS_CANCELLED",
  items: [{ sku: "SKU-1", quantity: 1, unitPrice: { amountMinor: "100", currency: "USD" } }],
  total: { amountMinor: "100", currency: "USD" },
});

test("orders created in the same minute get different short IDs in the list", async ({ page }) => {
  const customer = `e2e-shortid-${uid()}`;
  const first = "01932f4a-7c10-7a3b-8e21-4d5f60718293";
  const second = "01932f4a-7c11-7b44-9a05-c1d2e3f40516"; // same first 8 hex, 1 ms later
  expect(first.slice(0, 8)).toBe(second.slice(0, 8));

  await page.route("**/ordering.v1.OrderService/ListOrders", (route) =>
    route.fulfill({ json: { orders: [order(first, customer), order(second, customer)] } }),
  );
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", (route) => route.fulfill({ json: {} }));

  await page.goto(`/orders?customer=${customer}`);
  const links = page.getByRole("row").filter({ hasText: customer }).getByRole("link");
  await expect(links).toHaveCount(2);
  const shown = await links.allTextContents();

  expect(new Set(shown).size).toBe(2);
  expect(shown).toEqual([first.slice(-8), second.slice(-8)]);
});
