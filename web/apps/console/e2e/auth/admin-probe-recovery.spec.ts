import { expect, test, type Page } from "@playwright/test";
import { ADMIN_KEY, OPS_KEY } from "../../playwright.config";
import { settle, useKey } from "./helpers";

// The admin capability probe (ListAccounts) against a server that really enforces the role.
// What the console must do with each answer, and with a credential change:
//   - a probe that could not be answered (5xx, network) says nothing about the role: it is
//     retried, and the admin UI appears once the server answers, on the same page, no reload;
//   - PERMISSION_DENIED / UNAUTHENTICATED is an answer: no admin UI, and no "could not confirm";
//   - a new credential is a new question: it re-probes, also out of an unanswered probe, and an
//     answer still in flight for the previous credential must never open the admin UI.
// Runs in the auth project: OPS_KEY has no role, ADMIN_KEY the admin role.

const PROBE = "**/identity.v1.IdentityService/ListAccounts";
const unavailable = { status: 503, json: { code: "unavailable", message: "e2e: injected" } };
const unauthenticated = { status: 401, json: { code: "unauthenticated", message: "e2e: injected" } };
const NOTICE = "Could not confirm admin access";

const seed = (page: Page, key: string) =>
  page.addInitScript((k) => localStorage.setItem("console.credential", JSON.stringify({ kind: "api_key", value: k })), key);
const setButton = (page: Page) => page.getByRole("button", { name: "Set", exact: true });
const poisonNav = (page: Page) => page.locator("header nav").getByRole("link", { name: "Poison", exact: true });

test("admin: a probe that fails once with 503 recovers on the same page, same credential, no reload", async ({ page }) => {
  await seed(page, ADMIN_KEY);
  let calls = 0;
  await page.route(PROBE, (route) => (++calls === 1 ? route.fulfill(unavailable) : route.continue()));
  await page.goto("/stock");
  await expect(setButton(page)).toBeVisible({ timeout: 15_000 });
  await expect(poisonNav(page)).toBeVisible();
  await expect(page.getByText(NOTICE)).toHaveCount(0);
  expect(calls).toBeGreaterThanOrEqual(2);
});

test("admin: a probe that cannot reach the server (network failure) recovers the same way", async ({ page }) => {
  await seed(page, ADMIN_KEY);
  let calls = 0;
  await page.route(PROBE, (route) => (++calls === 1 ? route.abort("connectionreset") : route.continue()));
  await page.goto("/stock");
  await expect(setButton(page)).toBeVisible({ timeout: 15_000 });
  expect(calls).toBeGreaterThanOrEqual(2);
});

test("unauthenticated is an answer, not a failure: no admin UI, no notice, no retry", async ({ page }) => {
  await seed(page, OPS_KEY);
  await page.clock.install(); // the retry timers are ours to advance
  let calls = 0;
  await page.route(PROBE, (route) => {
    calls++;
    return route.fulfill(unauthenticated);
  });
  const answered = page.waitForResponse((r) => r.url().endsWith("/identity.v1.IdentityService/ListAccounts"));
  await page.goto("/stock");
  await answered;
  await expect(page.locator('[data-slot="card-title"]', { hasText: "Stock" }).first()).toBeVisible();
  await page.clock.runFor(30_000); // past every retry delay a failing probe would wait (500ms … 4s, ×3)
  await settle(page);
  await expect(setButton(page)).toHaveCount(0);
  await expect(poisonNav(page)).toHaveCount(0);
  await expect(page.getByText(NOTICE)).toHaveCount(0);
  expect(calls).toBe(1);
});

test("a new credential re-probes out of an unanswered probe, without Retry", async ({ page }) => {
  await seed(page, OPS_KEY);
  let failing = true;
  const keys: string[] = [];
  await page.route(PROBE, (route) => {
    keys.push(route.request().headers()["x-api-key"] ?? "");
    return failing ? route.fulfill(unavailable) : route.continue();
  });
  await page.goto("/stock");
  await expect(page.getByText(NOTICE)).toBeVisible({ timeout: 30_000 });
  await expect(setButton(page)).toHaveCount(0);

  failing = false;
  await useKey(page, ADMIN_KEY);
  await expect(setButton(page)).toBeVisible();
  await expect(page.getByText(NOTICE)).toHaveCount(0);
  expect(keys.at(-1)).toBe(ADMIN_KEY);
});

test("a late answer for the previous credential never opens the admin UI of the new one", async ({ page }) => {
  await seed(page, ADMIN_KEY);
  let release!: () => void;
  const hold = new Promise<void>((r) => (release = r));
  let captured!: () => void;
  const adminAnswerInHand = new Promise<void>((r) => (captured = r));
  await page.route(PROBE, async (route) => {
    if (route.request().headers()["x-api-key"] !== ADMIN_KEY) return route.continue();
    const answer = await route.fetch(); // the admin key's real 200, in hand but not yet delivered
    captured();
    await hold;
    await route.fulfill({ response: answer });
  });
  const probeResponse = (status: number) =>
    page.waitForResponse((r) => r.url().endsWith("/identity.v1.IdentityService/ListAccounts") && r.status() === status);
  const opsDenied = probeResponse(403);
  const lateAdmin = probeResponse(200);
  opsDenied.catch(() => {}); // only awaited on the happy path
  lateAdmin.catch(() => {});
  try {
    await page.goto("/stock");
    await adminAnswerInHand;
    await useKey(page, OPS_KEY);
    await opsDenied; // the new credential's answer is in: denied
    await expect(page.locator('[data-slot="card-title"]', { hasText: "Stock" }).first()).toBeVisible();
    await expect(setButton(page)).toHaveCount(0);
  } finally {
    release();
  }
  await (await lateAdmin).body(); // the old 200 has reached the page
  await settle(page);
  await expect(setButton(page)).toHaveCount(0);
  await expect(poisonNav(page)).toHaveCount(0);
});
