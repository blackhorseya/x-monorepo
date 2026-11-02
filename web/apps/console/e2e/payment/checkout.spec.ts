import { spawnSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";
import { seedProduct } from "../catalog";
import { createOrder } from "../orders-ui";
import { useKey } from "../auth/helpers";
import { uid } from "../uid";
import { ADMIN_KEY, BIN, SHOPPER_KEY, databaseUrls, mockEnv, paymentPorts } from "./env";
import { derivePorts } from "../ports";

// The Console's payment flow on the real stack (docs/payment.md §9 S6, §6「入口」): nothing in it is scripted with
// page.route. An order is created in the console, reserved by the worker, confirmed under the payment_required
// snapshot the worker's switch says, paid through payment-rpc's own mock control, and completed by ordering
// consuming PaymentCaptured. The stack is e2e/payment/stack-server.ts (real catalog-rpc, payment-rpc serve and
// consume, rpc and worker on a database of their own).
//
// One thing is stubbed and it is not the application: the checkout page. The mock provider's checkout URL is
// https://mock-pay.invalid/checkout/<ref> — a host that does not exist and a page nobody built (a real provider
// hosts it). The browser context answers that host with a placeholder page so the new tab has an address to
// settle on; what the spec asserts is the address the console opened, and that it is the checkout the payment
// really has (the ref the mock control then pays).

const P = derivePorts();
const ports = paymentPorts();
const db = databaseUrls(process.env.E2E_DATABASE_URL ?? "postgres://unset", P.rpcPort);
const CHECKOUT = /^https:\/\/mock-pay\.invalid\/checkout\/([0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12})$/;
const POLL_MS = 2000; // src/queries.ts

const status = (page: Page) => page.getByTestId("order-status");
const card = (page: Page) => page.getByTestId("payment-card");

test.describe.configure({ mode: "serial" });
let paidOrder = ""; // the order of the first test, looked at again by the second
test.setTimeout(120_000);

// A product with stock, through the admin credential (SetStock refuses a SKU the catalog does not have).
async function stocked(request: import("@playwright/test").APIRequestContext) {
  const sku = `E2E-PAY-${uid()}`;
  const headers = { "X-API-Key": ADMIN_KEY };
  await seedProduct(request, sku, { headers });
  const res = await request.post("/api/inventory.v1.InventoryService/SetStock", { headers, data: { sku, onHand: 5 } });
  expect(res.ok(), `SetStock ${sku}: ${res.status()} ${await res.text()}`).toBe(true);

  return sku;
}

// The customer orders in the console under the shopper credential; returns the new order's id.
async function placeOrder(page: Page, sku: string): Promise<string> {
  await page.goto("/orders");
  await useKey(page, SHOPPER_KEY);
  await page.getByLabel("SKU").fill(sku);

  return createOrder(page);
}

// The wrapper's control port: the worker is the process that takes the payment_required snapshot, so
// "the switch" is a worker started with ordering.require_payment on or off.
async function switchWorker(requirePayment: boolean) {
  const res = await fetch(`http://127.0.0.1:${ports.control}/worker?require_payment=${requirePayment}`, { method: "POST" });
  expect(res.status, "the stack did not restart its worker").toBe(200);
}

test("a CONFIRMED order that needs payment: Pay opens the checkout, the mock pays it, the order completes and the page stops watching", async ({ page, request }) => {
  const sku = await stocked(request);
  await page.context().route("https://mock-pay.invalid/**", (route) => route.fulfill({ contentType: "text/html", body: "<title>checkout</title>" }));

  const id = await placeOrder(page, sku);
  paidOrder = id;
  await page.goto(`/orders/${id}`);
  // the worker reserved the stock and confirmed the order under snapshot true: the Pay card is the proof
  await expect(status(page)).toHaveText("CONFIRMED", { timeout: 45_000 });
  await expect(card(page)).toBeVisible();
  // The payment is created by payment-rpc consume from the OrderConfirmed event, a hop after the order is
  // CONFIRMED: a Pay pressed in that gap is answered "no payment for this order" (the customer presses it again).
  // The card shows the payment's status once there is one; wait for it, so the press is the one under test.
  await expect(page.getByTestId("payment-status")).toHaveText("PENDING", { timeout: 30_000 });

  const popup = page.context().waitForEvent("page");
  await page.getByRole("button", { name: "Pay", exact: true }).click();
  const tab = await popup;
  await tab.waitForLoadState();
  const match = CHECKOUT.exec(tab.url());
  expect(match, `the tab opened ${tab.url()}, want a mock checkout address`).not.toBeNull();
  const ref = match![1];
  expect(await tab.evaluate(() => window.opener)).toBeNull(); // noopener
  await tab.close();
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", tab.url());
  await expect(page.getByTestId("payment-status")).toHaveText("PENDING");

  // Count the page's own GetOrder polls; the positive control is that it IS polling while it waits.
  let polls = 0;
  page.on("request", (req) => {
    if (req.url().includes("ordering.v1.OrderService/GetOrder")) polls++;
  });
  await expect.poll(() => polls, { timeout: 15_000, message: "the page does not poll a CONFIRMED order that waits for payment" }).toBeGreaterThanOrEqual(2);

  // The customer pays at the provider: the mock control pays the order's checkout and delivers the signed
  // webhook to payment-rpc's own route, as the provider would.
  const pay = spawnSync(BIN.payment, ["mock", "pay", "--order", id, "--deliver", `http://127.0.0.1:${ports.serve}/api/payment/webhook`], {
    env: mockEnv(db.stack),
    encoding: "utf8",
    timeout: 30_000,
  });
  expect(pay.status, `mock pay: ${pay.stderr}`).toBe(0);
  const delivery = JSON.parse(pay.stdout) as { ref: string; delivered_status: number };
  expect(delivery.delivered_status).toBe(200);
  expect(delivery.ref, "the control paid another checkout than the one the console opened").toBe(ref);

  // webhook -> payment consume (PaymentCaptured) -> ordering (PAID -> COMPLETED) -> the page's next poll
  await expect(status(page)).toHaveText("COMPLETED", { timeout: 45_000 });
  await expect(card(page)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Pay", exact: true })).toHaveCount(0);

  // ...and the page stops asking: no GetOrder for a window of more than two polling periods.
  const settled = polls;
  await page.waitForTimeout(2 * POLL_MS + 1_000);
  expect(polls, "the page kept polling an order that is COMPLETED").toBe(settled);
});

test("with the switch turned back off, a new order is CONFIRMED without a Pay card — and the paid order is untouched", async ({ page, request }) => {
  await switchWorker(false);
  const sku = await stocked(request);
  const id = await placeOrder(page, sku);
  await page.goto(`/orders/${id}`);
  await expect(status(page)).toHaveText("CONFIRMED", { timeout: 45_000 });
  // positive control: a CONFIRMED page rendered with its actions, then the card's absence means something
  await expect(page.getByRole("button", { name: "Complete" })).toBeVisible();
  await expect(card(page)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Pay", exact: true })).toHaveCount(0);

  // the order that was paid while the switch was on is what it was: the switch never touches it
  expect(paidOrder, "the first test did not run").not.toBe("");
  await page.goto(`/orders/${paidOrder}`);
  await expect(status(page)).toHaveText("COMPLETED", { timeout: 15_000 });
  await expect(card(page)).toHaveCount(0);
});
