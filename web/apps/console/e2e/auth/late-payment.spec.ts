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
  // The order changes a little when a new token has been issued (its customer line), so that the page
  // renders again with the new token: a card that is reset by anything but a new credential would show it.
  let customer = "someone";
  const order = () => ({ id, customerId: customer, status: "ORDER_STATUS_CONFIRMED", paymentRequired: true, items: [], total: { amountMinor: "100", currency: "USD" } });
  await page.route(GET_ORDER, (route) => route.fulfill({ json: { order: order() } }));
  await page.route("**/inventory.v1.InventoryService/GetReservation", (route) =>
    route.fulfill({ status: 404, json: { code: "not_found", message: "none" } }),
  );
  await page.route(GET_PAYMENT, (route) => route.fulfill(notFound));
  await page.route(START, (route) =>
    route.fulfill({ json: { payment: { id: "p", orderId: id, status: "PAYMENT_STATUS_PENDING", checkoutUrl: CHECKOUT } } }),
  );
  await page.context().route("https://pay.example.test/**", (route) => route.fulfill({ contentType: "text/html", body: "<title>c</title>" }));
  // The orders list offers the order (scripted), and the page is reached by following its link: within the
  // single-page app, since a reload would restore the session through a refresh of its own.
  await page.route("**/ordering.v1.OrderService/ListOrders", (route) => route.fulfill({ json: { orders: [order()] } }));
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
  customer = "someone-else"; // the next poll of the order changes what the page renders
  await expect(page.getByText("someone-else")).toBeVisible({ timeout: 10_000 }); // positive control: the page rendered again after the refresh
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT); // and the card is as it was
});

// What the card reads from the query cache belongs to the credential too (#391, the fourth cell: a result
// already cached, then the credential changes). The server answers per credential here: ops and the JWT see
// the order and, for ops, a payment with a checkout link; nobody (anonymous) is refused with 401. Anchors
// first: the new credential is in force and its refused or empty requests have really been answered, so an
// absent link is not just a request that has not come back yet.

type Who = "ops" | "jwt" | "admin" | "none";
const whoIs = (headers: Record<string, string>): Who => {
  if (headers["x-api-key"] === OPS_KEY) return "ops";
  if (headers["x-api-key"] === ADMIN_KEY) return "admin";
  if (headers["authorization"]?.startsWith("Bearer ")) return "jwt";
  return "none";
};
const refused = { status: 401, headers: { "x-error-code": "auth.unauthenticated" }, json: { code: "unauthenticated", message: "no credential" } };

async function perCredential(page: import("@playwright/test").Page, id: string, linkFor: Who[]) {
  const answered = { none: 0, getPaymentFor: [] as Who[] };
  await page.route(GET_ORDER, (route) => {
    const who = whoIs(route.request().headers());
    if (who === "none") {
      answered.none++;
      return route.fulfill(refused);
    }
    return route.fulfill({
      json: { order: { id, customerId: "someone", status: "ORDER_STATUS_CONFIRMED", paymentRequired: true, items: [], total: { amountMinor: "100", currency: "USD" } } },
    });
  });
  await page.route("**/inventory.v1.InventoryService/GetReservation", (route) =>
    route.fulfill({ status: 404, json: { code: "not_found", message: "none" } }),
  );
  await page.route(GET_PAYMENT, (route) => {
    const who = whoIs(route.request().headers());
    answered.getPaymentFor.push(who);
    if (who === "none") {
      answered.none++;
      return route.fulfill(refused);
    }
    if (!linkFor.includes(who)) return route.fulfill(notFound);
    return route.fulfill({ json: { payment: { id: "p", orderId: id, status: "PAYMENT_STATUS_PENDING", checkoutUrl: CHECKOUT } } });
  });
  return answered;
}

test("a payment result cached for a credential that signed out is not shown", async ({ page }) => {
  const id = `e2e-pay-cache-out-${uid()}`;
  const answered = await perCredential(page, id, ["ops"]);
  await page.goto(`/orders/${id}`);
  await useKey(page, OPS_KEY);
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT); // positive control: the result was cached and shown

  await page.locator("header").getByRole("button", { name: "Clear" }).click();
  await expect(subject(page)).toHaveText("anonymous"); // the credential is gone
  await expect.poll(() => answered.none, { timeout: 10_000 }).toBeGreaterThanOrEqual(2); // and the refusals (order and payment) have been answered
  await settle(page);
  await page.waitForTimeout(300);
  expect(await page.getByTestId("payment-link").count()).toBe(0);
  expect(await page.getByTestId("payment-card").count()).toBe(0); // the card is not kept up by the order data of the credential that left
});

test("a payment result cached for one credential is not shown to another; going back finds it again", async ({ page }) => {
  const id = `e2e-pay-cache-switch-${uid()}`;
  const answered = await perCredential(page, id, ["ops"]); // the JWT has an order but no payment
  await page.goto(`/orders/${id}`);
  await useKey(page, OPS_KEY);
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT); // positive control

  await useJWT(page, mintJWT(`e2e-b-${uid()}`));
  await expect(subject(page)).toHaveText(/^jwt/);
  await expect.poll(() => answered.getPaymentFor.includes("jwt"), { timeout: 10_000 }).toBe(true); // the JWT's own GetPayment has been answered (404)
  await expect(page.getByTestId("payment-card")).toBeVisible();
  await settle(page);
  expect(await page.getByTestId("payment-link").count()).toBe(0);

  // The normal path still works: the first credential again asks again and gets its result.
  await useKey(page, OPS_KEY);
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT);
});

test("a GetPayment answer that arrives late for the previous credential is not shown to the new one", async ({ page }) => {
  const id = `e2e-pay-cache-late-${uid()}`;
  const answered = await perCredential(page, id, []); // nobody has a link by default
  // ops' payment answer is held back; it carries a link
  let received!: () => void;
  const hasReceived = new Promise<void>((r) => (received = r));
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let delivered!: () => void;
  const wasDelivered = new Promise<void>((r) => (delivered = r));
  let held = false;
  await page.route(GET_PAYMENT, async (route) => {
    if (whoIs(route.request().headers()) !== "ops" || held) return route.fallback();
    held = true;
    received();
    await released;
    await route.fulfill({ json: { payment: { id: "p", orderId: id, status: "PAYMENT_STATUS_PENDING", checkoutUrl: CHECKOUT } } });
    delivered();
  });
  await page.goto(`/orders/${id}`);
  await useKey(page, OPS_KEY);
  await hasReceived; // positive control: ops' payment request is on its way
  await expect(page.getByTestId("payment-card")).toBeVisible();

  await useJWT(page, mintJWT(`e2e-b-${uid()}`));
  await expect(subject(page)).toHaveText(/^jwt/); // the new credential is in force
  await expect(page.getByTestId("payment-card")).toBeVisible();
  release();
  await wasDelivered; // the old answer has been handed to the page
  await settle(page);
  await page.waitForTimeout(500);
  expect(await page.getByTestId("payment-link").count()).toBe(0);
  // After the assertion that matters (a request the old query joined would never show up here): the new
  // credential did ask for its own payment, and was answered.
  expect(answered.getPaymentFor).toContain("jwt");
});

// Same session, refreshed token: the payment result in the cache is the same person's and stays.
test("a payment result from the query is kept through a token refresh of the same session", async ({ page }) => {
  const id = `e2e-pay-cache-rotate-${uid()}`;
  await stubConfig(page);
  const idp = await mockIdp(page, { expiresIn: 12 });
  let customer = "someone";
  const order = () => ({ id, customerId: customer, status: "ORDER_STATUS_CONFIRMED", paymentRequired: true, items: [], total: { amountMinor: "100", currency: "USD" } });
  await page.route(GET_ORDER, (route) => route.fulfill({ json: { order: order() } }));
  await page.route("**/inventory.v1.InventoryService/GetReservation", (route) =>
    route.fulfill({ status: 404, json: { code: "not_found", message: "none" } }),
  );
  await page.route(GET_PAYMENT, (route) =>
    route.fulfill({ json: { payment: { id: "p", orderId: id, status: "PAYMENT_STATUS_PENDING", checkoutUrl: CHECKOUT } } }),
  );
  await page.route("**/ordering.v1.OrderService/ListOrders", (route) => route.fulfill({ json: { orders: [order()] } }));
  await page.route("**/inventory.v1.InventoryService/BatchGetReservations", (route) => route.fulfill({ json: {} }));
  await page.goto("/login");
  await signIn(page);
  await page.getByRole("link", { name: id.slice(-8) }).click();
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT); // from GetPayment: Pay was never pressed

  const issued = idp.accessTokens.length;
  await expect.poll(() => idp.accessTokens.length, { timeout: 25_000 }).toBeGreaterThan(issued); // a new access token was issued
  customer = "someone-else"; // the next poll of the order changes what the page renders
  await expect(page.getByText("someone-else")).toBeVisible({ timeout: 10_000 }); // positive control: rendered again after the refresh
  await expect(page.getByTestId("payment-link")).toHaveAttribute("href", CHECKOUT);
});
