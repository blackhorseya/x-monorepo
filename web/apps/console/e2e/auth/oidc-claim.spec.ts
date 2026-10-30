import { expect, test, type Page } from "@playwright/test";
import { mockIdp, signIn, stubConfig } from "./oidc";
import { subject } from "./helpers";

// A signed-in identity the server has no account for (docs/identity-login.md §5, §6): every RPC but
// ClaimInvite answers PERMISSION_DENIED with x-error-code auth.unbound_identity, and the console
// shows the ClaimInvite flow instead of pages that can only fail. The server side is stubbed here
// (it is #272's); what is tested is what the console does with those answers.

const connectError = (code: string, status: number, xCode: string, message = "refused") => ({
  status,
  headers: { "content-type": "application/json", "x-error-code": xCode },
  body: JSON.stringify({ code, message }),
});

async function unboundServer(page: Page, claim: (n: number) => { status: number; headers?: Record<string, string>; body: string }) {
  const state = { bound: false, claimCalls: 0, claimBodies: [] as string[], claimAuth: [] as (string | undefined)[], others: 0 };
  await page.route("**/api/*.v1.*/*", (route) => {
    const url = route.request().url();
    if (url.endsWith("/ClaimInvite")) {
      state.claimCalls++;
      state.claimBodies.push(route.request().postData() ?? "");
      state.claimAuth.push(route.request().headers()["authorization"]);
      const res = claim(state.claimCalls);
      if (res.status === 200) state.bound = true;
      return route.fulfill({ ...res, headers: { "content-type": "application/json", ...res.headers } });
    }
    if (state.bound) return route.fallback();
    state.others++;
    return route.fulfill(connectError("permission_denied", 403, "auth.unbound_identity"));
  });
  return state;
}

const account = JSON.stringify({ account: { id: "acc-claimed", name: "Pat Claimed", roles: [], status: "ACCOUNT_STATUS_ACTIVE" } });

test("an unbound identity gets the ClaimInvite flow, and claiming opens the console", async ({ page }) => {
  await stubConfig(page);
  await mockIdp(page);
  const server = await unboundServer(page, () => ({ status: 200, body: account }));
  await page.goto("/login");
  await signIn(page);

  await expect(page.getByRole("heading", { name: "Link your account" })).toBeVisible();
  await expect(page.locator("header nav").getByRole("link", { name: "Orders" })).toHaveCount(0);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");

  await page.getByRole("button", { name: "Claim invite" }).click();
  await expect(page.getByRole("heading", { name: "Link your account" })).toHaveCount(0);
  await expect(page.locator("header nav").getByRole("link", { name: "Orders" })).toBeVisible();

  expect(server.claimCalls).toBe(1);
  expect(JSON.parse(server.claimBodies[0] || "{}")).toEqual({}); // issuer / subject / email come from the token, never from us
  expect(server.claimAuth[0]).toMatch(/^Bearer /);
});

for (const [xCode, hint] of [
  ["invite.not_found", /no invite/i],
  ["invite.expired", /expired/i],
  ["invite.already_claimed", /already/i],
  ["invite.email_unverified", /verif/i],
  ["invite.email_mismatch", /email/i],
  ["identity.already_bound", /already/i],
] as const) {
  test(`ClaimInvite refused with ${xCode}: the code and a plain explanation are shown, the user can try again`, async ({ page }) => {
    await stubConfig(page);
    await mockIdp(page);
    const server = await unboundServer(page, (n) =>
      n === 1 ? connectError("failed_precondition", 400, xCode) : { status: 200, body: account },
    );
    await page.goto("/login");
    await signIn(page);
    await page.getByRole("button", { name: "Claim invite" }).click();

    const alert = page.getByRole("alert");
    await expect(alert).toContainText(xCode);
    await expect(alert).toContainText(hint);
    await expect(page.getByRole("heading", { name: "Link your account" })).toBeVisible();

    await page.getByRole("button", { name: "Claim invite" }).click();
    await expect(page.getByRole("heading", { name: "Link your account" })).toHaveCount(0);
    expect(server.claimCalls).toBe(2);
  });
}

test("an unbound identity can sign out from the claim view", async ({ page }) => {
  await stubConfig(page);
  await mockIdp(page);
  await unboundServer(page, () => ({ status: 200, body: account }));
  await page.goto("/login");
  await signIn(page);
  await expect(page.getByRole("heading", { name: "Link your account" })).toBeVisible();
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login(\?|$)/);
  await expect(page.getByRole("heading", { name: "Link your account" })).toHaveCount(0);
});

test("PERMISSION_DENIED for another reason is not the claim flow", async ({ page }) => {
  await stubConfig(page);
  await mockIdp(page);
  await page.route("**/api/*.v1.*/*", (route) => route.fulfill(connectError("permission_denied", 403, "auth.permission_denied")));
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await expect(page.getByRole("heading", { name: "Link your account" })).toHaveCount(0);
});
