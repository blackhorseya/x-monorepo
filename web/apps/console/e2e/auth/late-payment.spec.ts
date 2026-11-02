import { expect, test } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { uid } from "../uid";
import { settle, useKey } from "./helpers";

// Pay opens the provider's checkout page when StartPayment answers. An answer that arrives after the
// console switched to another credential belongs to the old one: the payment was started for the old
// customer's order, so it must not open a page (or show a link) for the new credential. The page keeps
// the Pay card of the order it shows, whoever the user now is. The server is scripted (page.route);
// what is controlled is the order of the answer.

const CHECKOUT = "https://pay.example.test/checkout/pay-late";

test("a StartPayment answer for the previous credential opens nothing", async ({ page }) => {
  const id = `e2e-late-pay-${uid()}`;
  await page.route("**/ordering.v1.OrderService/GetOrder", (route) =>
    route.fulfill({
      json: {
        order: {
          id,
          customerId: "someone",
          status: "ORDER_STATUS_CONFIRMED",
          paymentRequired: true,
          items: [],
          total: { amountMinor: "100", currency: "USD" },
        },
      },
    }),
  );
  await page.route("**/inventory.v1.InventoryService/GetReservation", (route) =>
    route.fulfill({ status: 404, json: { code: "not_found", message: "none" } }),
  );
  await page.route("**/payment.v1.PublicPaymentService/GetPayment", (route) =>
    route.fulfill({ status: 404, headers: { "x-error-code": "payment.not_found" }, json: { code: "not_found", message: "none" } }),
  );

  let received!: () => void;
  const hasReceived = new Promise<void>((r) => (received = r));
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let delivered!: () => void;
  const wasDelivered = new Promise<void>((r) => (delivered = r));
  await page.route("**/payment.v1.PublicPaymentService/StartPayment", async (route) => {
    received();
    await released;
    await route.fulfill({ json: { payment: { id: "pay-late", orderId: id, status: "PAYMENT_STATUS_PENDING", checkoutUrl: CHECKOUT } } });
    delivered();
  });

  let opened = 0;
  page.context().on("page", () => opened++);
  await page.goto(`/orders/${id}`);
  await useKey(page, OPS_KEY);
  await expect(page.getByTestId("payment-card")).toBeVisible();
  await page.getByRole("button", { name: "Pay", exact: true }).click();
  await hasReceived; // positive control: the call is on its way

  await useKey(page, ADMIN_KEY); // another credential takes over while the answer is held
  await expect(page.getByTestId("payment-card")).toBeVisible();

  release();
  await wasDelivered; // positive control: the old answer has been handed to the page
  await settle(page);
  await page.waitForTimeout(300);

  // A snapshot, not a retrying expect: nothing later repairs a window that was opened.
  expect(opened).toBe(0);
  expect(await page.getByTestId("payment-link").count()).toBe(0);
});
