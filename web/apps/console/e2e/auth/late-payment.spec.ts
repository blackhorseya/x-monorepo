import { expect, test } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { uid } from "../uid";
import { mockIdp, signIn, stubConfig } from "./oidc";
import { mintJWT } from "./jwt";
import { settle, subject, useJWT, useKey } from "./helpers";

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

// What the Pay card remembers (an error, the checkout link) belongs to the credential it was shown for.
// Switching credentials starts the card over, so neither a failure that arrives late nor a link that was
// already there can show up under the new one. Controls first: for the credential in use, a failure shows
// its message and a success its link, so the absences below are not a card that never shows them.

const GET_ORDER = "**/ordering.v1.OrderService/GetOrder";
const START = "**/payment.v1.PublicPaymentService/StartPayment";
const GET_PAYMENT = "**/payment.v1.PublicPaymentService/GetPayment";

async function waitingOrder(page: import("@playwright/test").Page, id: string) {
  await page.route(GET_ORDER, (route) =>
    route.fulfill({
      json: { order: { id, customerId: "someone", status: "ORDER_STATUS_CONFIRMED", paymentRequired: true, items: [], total: { amountMinor: "100", currency: "USD" } } },
    }),
  );
  await page.route("**/inventory.v1.InventoryService/GetReservation", (route) =>
    route.fulfill({ status: 404, json: { code: "not_found", message: "none" } }),
  );
}

const notFound = { status: 404, headers: { "x-error-code": "payment.not_found" }, json: { code: "not_found", message: "none" } };
const disabled = { status: 400, headers: { "x-error-code": "payment.disabled" }, json: { code: "failed_precondition", message: "SECRET-d41d8cd9" } };

test("controls: for the credential in use a failure shows its message and a success its link", async ({ page }) => {
  const id = `e2e-pay-ctl-${uid()}`;
  await waitingOrder(page, id);
  await page.route(GET_PAYMENT, (route) => route.fulfill(notFound));
  let answer: "fail" | "ok" = "fail";
  await page.route(START, (route) =>
    answer === "fail"
      ? route.fulfill(disabled)
      : route.fulfill({ json: { payment: { id: "p", orderId: id, status: "PAYMENT_STATUS_PENDING", checkoutUrl: CHECKOUT } } }),
  );
  await page.context().route("https://pay.example.test/**", (route) => route.fulfill({ contentType: "text/html", body: "<title>c</title>" }));
  await page.goto(`/orders/${id}`);
  await useKey(page, OPS_KEY);
  await expect(page.getByTestId("payment-card")).toBeVisible();
  await page.getByRole("button", { name: "Pay", exact: true }).click();
  await expect(page.getByTestId("payment-message")).toBeVisible();
  answer = "ok";
  const popup = page.context().waitForEvent("page");
  await page.getByRole("button", { name: "Pay", exact: true }).click();
  expect((await popup).url()).toBe(CHECKOUT); // the success of the credential in use opens the page
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT);
  await expect(page.getByTestId("payment-message")).toHaveCount(0); // and the success cleared the failure
});

test("a StartPayment failure for the previous credential shows nothing under the new one", async ({ page }) => {
  const id = `e2e-pay-late-err-${uid()}`;
  await waitingOrder(page, id);
  await page.route(GET_PAYMENT, (route) => route.fulfill(notFound));
  let received!: () => void;
  const hasReceived = new Promise<void>((r) => (received = r));
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let delivered!: () => void;
  const wasDelivered = new Promise<void>((r) => (delivered = r));
  await page.route(START, async (route) => {
    received();
    await released;
    await route.fulfill(disabled);
    delivered();
  });
  await page.goto(`/orders/${id}`);
  await useKey(page, OPS_KEY);
  await expect(page.getByTestId("payment-card")).toBeVisible();
  await page.getByRole("button", { name: "Pay", exact: true }).click();
  await hasReceived; // positive control: the call is on its way
  const before = await subject(page).textContent();

  await useJWT(page, mintJWT(`e2e-b-${uid()}`));
  await expect(subject(page)).not.toHaveText(before ?? ""); // the new credential is in force
  await expect(page.getByTestId("payment-card")).toBeVisible();
  await expect(page.getByRole("button", { name: "Pay", exact: true })).toBeEnabled(); // the card is back to idle

  release();
  await wasDelivered; // positive control: the old failure has been handed to the page
  await settle(page);
  await page.waitForTimeout(300);
  expect(await page.getByTestId("payment-message").count()).toBe(0); // a snapshot: nothing later repairs a message shown
});

test("a checkout link of the previous credential is gone under the new one", async ({ page }) => {
  const id = `e2e-pay-link-${uid()}`;
  await waitingOrder(page, id);
  await page.route(GET_PAYMENT, (route) => route.fulfill(notFound)); // the new credential has no payment for this order
  await page.route(START, (route) =>
    route.fulfill({ json: { payment: { id: "p", orderId: id, status: "PAYMENT_STATUS_PENDING", checkoutUrl: CHECKOUT } } }),
  );
  await page.context().route("https://pay.example.test/**", (route) => route.fulfill({ contentType: "text/html", body: "<title>c</title>" }));
  await page.goto(`/orders/${id}`);
  await useKey(page, OPS_KEY);
  await expect(page.getByTestId("payment-card")).toBeVisible();
  await page.getByRole("button", { name: "Pay", exact: true }).click();
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT); // positive control: it was there
  const before = await subject(page).textContent();

  await useJWT(page, mintJWT(`e2e-b-${uid()}`));
  await expect(subject(page)).not.toHaveText(before ?? ""); // the new credential is in force
  await expect(page.getByTestId("payment-card")).toBeVisible();
  await settle(page);
  await page.waitForTimeout(300);
  expect(await page.getByTestId("payment-link").count()).toBe(0);
});

// A refreshed access token is the same session and the same person: the generation stays (src/auth.ts
// rotateSessionToken), so the card keeps its link. Resetting on every token would erase a payment in
// progress every few minutes. The IdP gives short-lived tokens so that the console refreshes by itself.
test("a refreshed access token of the same session does not reset the card", async ({ page }) => {
  const id = `e2e-pay-rotate-${uid()}`;
  await stubConfig(page);
  const idp = await mockIdp(page, { expiresIn: 12 });
  await waitingOrder(page, id);
  await page.route(GET_PAYMENT, (route) => route.fulfill(notFound));
  await page.route(START, (route) =>
    route.fulfill({ json: { payment: { id: "p", orderId: id, status: "PAYMENT_STATUS_PENDING", checkoutUrl: CHECKOUT } } }),
  );
  await page.context().route("https://pay.example.test/**", (route) => route.fulfill({ contentType: "text/html", body: "<title>c</title>" }));
  // The orders list offers the order (scripted), and the page is reached by following its link: within the
  // single-page app, since a reload would restore the session through a refresh of its own.
  const order = { id, customerId: "someone", status: "ORDER_STATUS_CONFIRMED", paymentRequired: true, items: [], total: { amountMinor: "100", currency: "USD" } };
  await page.route("**/ordering.v1.OrderService/ListOrders", (route) => route.fulfill({ json: { orders: [order] } }));
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", (route) => route.fulfill({ json: {} }));
  await page.goto("/login");
  await signIn(page);
  await page.getByRole("link", { name: id.slice(-8) }).click();
  await expect(page.getByTestId("payment-card")).toBeVisible();
  await page.getByRole("button", { name: "Pay", exact: true }).click();
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT);

  const grants = () => idp.tokenRequests.filter((t) => t.get("grant_type") === "refresh_token").length;
  const issued = idp.accessTokens.length;
  await expect.poll(() => idp.accessTokens.length, { timeout: 25_000 }).toBeGreaterThan(issued); // a new access token was issued
  expect(grants()).toBeGreaterThanOrEqual(1); // by a refresh
  await settle(page);
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT); // and the card is as it was
});
