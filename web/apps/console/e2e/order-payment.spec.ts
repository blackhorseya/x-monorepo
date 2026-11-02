import { expect, test, type Page } from "@playwright/test";
import { seedProduct } from "./catalog";
import { uid } from "./uid";

// The payment entry of /orders/$id (docs/payment.md §6, slice 4). The console does not know the
// ordering.require_payment switch: the Pay card shows for exactly one thing, an order that is CONFIRMED
// and whose own payment_required snapshot is true. With the default configuration every snapshot is
// false, so the page is what it was. The server side is scripted (page.route) over a real order: the
// real stack cannot create a payment before the saga exists (slice 3), and what is measured here is what
// the console does with the answers. Every window that asserts "nothing happened" starts with a positive
// control that the thing was possible.

const GET_ORDER = "ordering.v1.OrderService/GetOrder";
const START = "payment.v1.PublicPaymentService/StartPayment";
const GET_PAYMENT = "payment.v1.PublicPaymentService/GetPayment";
const CHECKOUT = "https://pay.example.test/checkout/pay-1";

type State = { status: string; paymentRequired: boolean };

async function realOrder(page: Page) {
  const sku = `E2E-PAY-${uid()}`;
  await seedProduct(page.request, sku);
  const res = await page.request.post("/api/ordering.v1.OrderService/CreateOrder", {
    data: { customerId: `e2e-pay-${uid()}`, items: [{ sku, quantity: 1 }] },
  });
  return ((await res.json()) as { order: { id: string } }).order.id;
}

// GetOrder answers the real order with the status and snapshot `state` says; the payment service is
// scripted by the test, defaulting to "no payment yet".
async function script(page: Page, state: State) {
  const calls = { order: 0, start: [] as unknown[], payment: 0 };
  await page.route(`**/${GET_ORDER}`, async (route) => {
    calls.order++;
    const body = (await (await route.fetch()).json()) as { order: Record<string, unknown> };
    body.order.status = `ORDER_STATUS_${state.status}`;
    body.order.paymentRequired = state.paymentRequired;
    await route.fulfill({ json: body });
  });
  await page.route(`**/${GET_PAYMENT}`, (route) => {
    calls.payment++;
    return route.fulfill({ status: 404, headers: { "x-error-code": "payment.not_found" }, json: { code: "not_found", message: "no payment" } });
  });
  return calls;
}

const startAnswers = (page: Page, calls: { start: unknown[] }, reply: { status?: number; xcode?: string; json: unknown }) =>
  page.route(`**/${START}`, (route) => {
    calls.start.push(route.request().postDataJSON());
    return route.fulfill({ status: reply.status ?? 200, headers: reply.xcode ? { "x-error-code": reply.xcode } : {}, json: reply.json });
  });

const ok = (checkoutUrl: string) => ({ json: { payment: { id: "pay-1", orderId: "x", status: "PAYMENT_STATUS_PENDING", checkoutUrl } } });

const card = (page: Page) => page.getByTestId("payment-card");
const pay = (page: Page) => page.getByRole("button", { name: "Pay", exact: true });

async function open(page: Page, state: State) {
  const id = await realOrder(page);
  const calls = await script(page, state);
  await page.goto(`/orders/${id}`);
  await expect(page.getByTestId("order-status")).toHaveText(state.status);
  expect(calls.order).toBeGreaterThanOrEqual(1); // positive control: the page asked
  return { id, calls };
}

test("the Pay card is for a CONFIRMED order whose snapshot says payment is required, and only that", async ({ page }) => {
  // The default configuration's world: CONFIRMED, snapshot false. The page is what it was.
  await open(page, { status: "CONFIRMED", paymentRequired: false });
  await expect(page.getByRole("button", { name: "Complete" })).toBeVisible(); // positive control: a CONFIRMED page rendered
  await expect(card(page)).toHaveCount(0);
  await expect(pay(page)).toHaveCount(0);
});

for (const status of ["PENDING", "PAID", "COMPLETED", "CANCELLED"]) {
  test(`no Pay card for a ${status} order, even with the snapshot set`, async ({ page }) => {
    await open(page, { status, paymentRequired: true });
    await expect(card(page)).toHaveCount(0);
  });
}

test("Pay asks for the order's payment and opens the checkout page in a new tab, without an opener", async ({ page }) => {
  const { id, calls } = await open(page, { status: "CONFIRMED", paymentRequired: true });
  await startAnswers(page, calls, ok(CHECKOUT));
  await page.context().route("https://pay.example.test/**", (route) => route.fulfill({ contentType: "text/html", body: "<title>checkout</title>" }));
  await expect(card(page)).toBeVisible();

  const popup = page.context().waitForEvent("page");
  await pay(page).click();
  const tab = await popup;
  await tab.waitForLoadState();
  expect(tab.url()).toBe(CHECKOUT);
  expect(await tab.evaluate(() => window.opener)).toBeNull(); // noopener: the checkout page cannot reach back
  expect(calls.start).toEqual([{ orderId: id }]); // the order id and nothing else: who pays is the credential's business

  // The console page stays, and offers the link too (a blocked pop-up is not a dead end).
  await expect(page).toHaveURL(new RegExp(`/orders/${id}$`));
  const link = page.getByTestId("payment-link");
  await expect(link).toHaveAttribute("href", CHECKOUT);
  await expect(link).toHaveAttribute("target", "_blank");
  await expect(link).toHaveAttribute("rel", /noopener/);
});

test("with the pop-up blocked the link is there to click", async ({ page }) => {
  await page.addInitScript(() => {
    window.open = () => null;
  });
  const { calls } = await open(page, { status: "CONFIRMED", paymentRequired: true });
  await startAnswers(page, calls, ok(CHECKOUT));
  await expect(card(page)).toBeVisible(); // the card is what the click needs; its absence is the failure, not a timeout on the button
  await pay(page).click();
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT);
  expect(calls.start).toHaveLength(1); // positive control: the call was made
});

// The checkout URL is data from outside: only http(s) is opened or linked.
for (const url of ["javascript:window.__pwned=1", "data:text/html,<script>window.__pwned=1</script>", "ftp://pay.example.test/x", "//pay.example.test/x", ""]) {
  test(`a checkout URL that is not http(s) is neither opened nor linked: ${JSON.stringify(url).slice(0, 40)}`, async ({ page }) => {
    const { calls } = await open(page, { status: "CONFIRMED", paymentRequired: true });
    await startAnswers(page, calls, ok(url));
    let opened = 0;
    page.context().on("page", () => opened++);
    await expect(card(page)).toBeVisible(); // the card is what the click needs; its absence is the failure, not a timeout on the button
  await pay(page).click();
    await expect.poll(() => calls.start.length).toBe(1); // positive control: the call was made and answered
    await expect(page.getByTestId("payment-message")).toBeVisible();
    await page.waitForTimeout(300);
    expect(opened).toBe(0);
    await expect(page.getByTestId("payment-link")).toHaveCount(0);
    expect(await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned)).toBeUndefined();
  });
}

test("payment.disabled is a fixed message, not a page and not a crash", async ({ page }) => {
  const { calls } = await open(page, { status: "CONFIRMED", paymentRequired: true });
  await startAnswers(page, calls, {
    status: 400,
    xcode: "payment.disabled",
    json: { code: "failed_precondition", message: "SECRET-SERVER-TEXT-d41d8cd9" },
  });
  let opened = 0;
  page.context().on("page", () => opened++);
  await expect(card(page)).toBeVisible(); // the card is what the click needs; its absence is the failure, not a timeout on the button
  await pay(page).click();
  await expect(page.getByTestId("payment-message")).toContainText("not available");
  expect(calls.start).toHaveLength(1);
  expect(opened).toBe(0);
  await expect(page.getByTestId("payment-link")).toHaveCount(0);
  await expect(page.locator("body")).not.toContainText("SECRET-SERVER-TEXT"); // what the server wrote is not echoed
  await expect(card(page)).toBeVisible(); // the page keeps working
});

test("payment-rpc being down is its own fixed message", async ({ page }) => {
  const { calls } = await open(page, { status: "CONFIRMED", paymentRequired: true });
  await startAnswers(page, calls, {
    status: 503,
    xcode: "payment.unavailable",
    json: { code: "unavailable", message: "SECRET-SERVER-TEXT-d41d8cd9" },
  });
  await expect(card(page)).toBeVisible(); // the card is what the click needs; its absence is the failure, not a timeout on the button
  await pay(page).click();
  await expect(page.getByTestId("payment-message")).toContainText("try again");
  await expect(page.locator("body")).not.toContainText("SECRET-SERVER-TEXT");
  await expect(pay(page)).toBeEnabled(); // and can be pressed again
});

// While the card is there the page looks every 2s, so a customer who comes back from the checkout tab
// sees the result; the 15s of an ordinary CONFIRMED order would leave them staring at a Pay button.
// When the order moves on the card goes and so does the polling that came with it.
test("with the card shown the order is polled every 2s, and the card and the polling end with the order", async ({ page }) => {
  await page.clock.install();
  const state: State = { status: "CONFIRMED", paymentRequired: true };
  const id = await realOrder(page);
  const calls = await script(page, state);
  await page.goto(`/orders/${id}`);
  await expect(card(page)).toBeVisible();
  await page.waitForTimeout(300);

  const before = calls.order;
  await page.clock.fastForward(2_100);
  await page.waitForTimeout(400);
  expect(calls.order).toBeGreaterThan(before); // not the 15s of a CONFIRMED order without payment

  state.status = "COMPLETED"; // the saga captured the payment and completed the order
  await page.clock.fastForward(2_100);
  await expect(page.getByTestId("order-status")).toHaveText("COMPLETED");
  await expect(card(page)).toHaveCount(0);
  await page.waitForTimeout(300);

  const settled = calls.order;
  for (let i = 0; i < 3; i++) {
    await page.clock.fastForward(20_000);
    await page.waitForTimeout(400);
  }
  expect(calls.order).toBe(settled); // COMPLETED is final: nothing keeps asking
});

test("an ordinary CONFIRMED order (no payment) keeps its slow poll", async ({ page }) => {
  await page.clock.install();
  const id = await realOrder(page);
  const calls = await script(page, { status: "CONFIRMED", paymentRequired: false });
  await page.goto(`/orders/${id}`);
  await expect(page.getByTestId("order-status")).toHaveText("CONFIRMED");
  await page.waitForTimeout(300);
  const before = calls.order;
  await page.clock.fastForward(5_000);
  await page.waitForTimeout(400);
  expect(calls.order).toBe(before); // 15s, not 2s
});
