import { expect, test, type Page } from "@playwright/test";
import { seedProduct } from "./catalog";
import { uid } from "./uid";
import { createCard } from "./orders-ui";

// CreateOrder carries an idempotency key (a UUID v7) that stands for one intent to place an
// order: the same lines for the same customer re-sent (the response was lost, the user pressed
// the button again) reuse it, so the server answers with the order it already made; lines or a
// customer that changed get a new one, because the server answers a reused key with other
// content ALREADY_EXISTS. Mode-agnostic: the in-memory and the Postgres repository both
// deduplicate on (customer, key). Orders are told apart by a customer of the test's own.

const V7 = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

type Sent = { customerId: string; items: { sku: string; quantity: number }[]; idempotencyKey?: string };

// Intercepts CreateOrder. The first call (when `loseFirst`) reaches the server and commits, but
// the browser never sees the answer: the lost-response case. Every body sent is recorded;
// `answered(n)` resolves once the n-th call has been answered.
async function watchCreateOrder(page: Page, { loseFirst }: { loseFirst: boolean }) {
  const sent: Sent[] = [];
  let done = 0;
  const answered = (n: number) => expect.poll(() => done, { message: `CreateOrder call ${n} answered` }).toBeGreaterThanOrEqual(n);
  await page.route("**/ordering.v1.OrderService/CreateOrder", async (route) => {
    sent.push(route.request().postDataJSON() as Sent);
    if (loseFirst && sent.length === 1) {
      await route.fetch(); // the server committed ...
      await route.abort("connectionreset"); // ... and the answer never arrived
    } else {
      await route.continue();
    }
    done++;
  });
  return { sent, answered };
}

async function ordersOf(page: Page, customerId: string) {
  const res = await page.request.post("/api/ordering.v1.OrderService/ListOrders", { data: { customerId, pageSize: 50 } });
  expect(res.ok()).toBe(true);
  return ((await res.json()) as { orders?: { id: string }[] }).orders ?? [];
}

async function open(page: Page) {
  const customer = `e2e-idem-${uid()}`;
  const sku = `E2E-IDEM-${uid()}`;
  await seedProduct(page.request, sku);
  await page.goto("/orders");
  await createCard(page).getByLabel("Customer").fill(customer);
  await createCard(page).getByLabel("SKU").fill(sku);
  return { customer, sku };
}

const submit = (page: Page) => page.getByRole("button", { name: "Create order" }).click();

test("pressing Create again after a lost response makes one order, not two", async ({ page }) => {
  const { customer } = await open(page);
  const { sent, answered } = await watchCreateOrder(page, { loseFirst: true });

  await submit(page);
  await answered(1);
  await expect(page.getByRole("button", { name: "Create order" })).toBeEnabled();
  await submit(page);
  await answered(2);

  expect(sent).toHaveLength(2); // positive control: both presses reached the network
  // The first press committed server-side: the count is the verdict on the second.
  expect(await ordersOf(page, customer)).toHaveLength(1);
});

test("the key is a UUID v7, and a re-send of the same content carries the same one", async ({ page }) => {
  await open(page);
  const { sent, answered } = await watchCreateOrder(page, { loseFirst: true });

  await submit(page);
  await answered(1);
  await expect(page.getByRole("button", { name: "Create order" })).toBeEnabled();
  await submit(page);
  await answered(2);

  expect(sent).toHaveLength(2);
  expect(sent[0].idempotencyKey).toMatch(V7);
  expect(sent[1].idempotencyKey).toBe(sent[0].idempotencyKey);
});

test("changed lines get a new key, and the server does not answer idempotency_key_reused", async ({ page }) => {
  const { customer } = await open(page);
  const { sent, answered } = await watchCreateOrder(page, { loseFirst: true });

  await submit(page);
  await answered(1);
  await expect(page.getByRole("button", { name: "Create order" })).toBeEnabled();
  await createCard(page).getByLabel("Qty").fill("3");
  await submit(page);
  await answered(2);

  expect(sent).toHaveLength(2);
  expect(sent[0].idempotencyKey).toMatch(V7);
  expect(sent[1].idempotencyKey).toMatch(V7);
  expect(sent[1].idempotencyKey).not.toBe(sent[0].idempotencyKey);
  await expect(page).toHaveURL(/\/orders\/[^/?]+$/);
  await expect(page.getByText("idempotency_key_reused")).toHaveCount(0);
  expect(await ordersOf(page, customer)).toHaveLength(2); // the lost one and the changed one
});

test("a changed customer gets a new key (the key is scoped to the customer)", async ({ page }) => {
  const { customer } = await open(page);
  const other = `${customer}-b`;
  const { sent, answered } = await watchCreateOrder(page, { loseFirst: true });

  await submit(page);
  await answered(1);
  await expect(page.getByRole("button", { name: "Create order" })).toBeEnabled();
  await createCard(page).getByLabel("Customer").fill(other);
  await submit(page);
  await answered(2);

  expect(sent).toHaveLength(2);
  expect(sent[0].idempotencyKey).toMatch(V7);
  expect(sent[1].customerId).toBe(other);
  expect(sent[1].idempotencyKey).toMatch(V7);
  expect(sent[1].idempotencyKey).not.toBe(sent[0].idempotencyKey);
  await expect(page).toHaveURL(/\/orders\/[^/?]+$/);
  expect(await ordersOf(page, other)).toHaveLength(1);
});

test("a success opens the new order, and the next order of the same content is a new intent", async ({ page }) => {
  const { customer, sku } = await open(page);
  const { sent, answered } = await watchCreateOrder(page, { loseFirst: false });

  await submit(page);
  await answered(1);
  await expect(page).toHaveURL(/\/orders\/[^/?]+$/);
  const first = new URL(page.url()).pathname.split("/").pop()!;
  await expect(page.locator('[data-slot="card-title"]', { hasText: first })).toBeVisible();

  // The same customer and lines again, typed afresh: another order, so another key.
  await page.goto("/orders");
  await createCard(page).getByLabel("Customer").fill(customer);
  await createCard(page).getByLabel("SKU").fill(sku);
  await submit(page);
  await answered(2);
  await expect(page).toHaveURL(/\/orders\/[^/?]+$/);

  expect(sent).toHaveLength(2);
  expect(sent[0].idempotencyKey).toMatch(V7);
  expect(sent[1].idempotencyKey).toMatch(V7);
  expect(sent[1].idempotencyKey).not.toBe(sent[0].idempotencyKey);
  expect(new URL(page.url()).pathname.split("/").pop()).not.toBe(first);
  expect(await ordersOf(page, customer)).toHaveLength(2);
});
