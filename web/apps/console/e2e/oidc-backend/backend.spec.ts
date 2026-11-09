import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { BACKEND_IDP_PORT, BACKEND_RPC_PORT, FOREIGN_IDP_PORT } from "../../playwright.config";
import { stubConfig } from "../auth/oidc";
import { subject, useKey } from "../auth/helpers";
import { uid } from "../uid";
import { TestIdp } from "./idp";

// The console against the REAL rpc with auth.oidc on (config generated from e2e/oidc-backend/rpc.yaml by rpc-server.ts) and a loopback IdP
// whose ES256 tokens the rpc verifies through its real JWKS fetch. Covers what the stubbed `auth`
// project cannot: the server accepts the token, an unbound identity really gets
// auth.unbound_identity, ClaimInvite really binds, and RevokeSessions really refuses old tokens.
// (Real Dex is #274 part 2.) Serial: one rpc, accounts and bindings are shared state.

const CLIENT_ID = "x-console-e2e";
const ADMIN_KEY = "e2e-backend-admin-key-0123456789abcdef";
const api = (method: string) => `http://127.0.0.1:${BACKEND_RPC_PORT}/api/identity.v1.IdentityService/${method}`;

test.describe.configure({ mode: "serial" });

// The IdP runs as a webServer process (idp-server.ts); the tests steer who signs in next over HTTP.
const idp = {
  issuer: `http://127.0.0.1:${BACKEND_IDP_PORT}/idp`,
  user: { sub: "", email: "", emailVerified: true } as Awaited<ReturnType<typeof setUser>>,
};
async function setUser(request: APIRequestContext, user: { sub: string; email: string; emailVerified: boolean }) {
  const res = await request.post(`http://127.0.0.1:${BACKEND_IDP_PORT}/control/user`, { data: user });
  expect(res.status()).toBe(200);
  return user;
}

// admin calls the rpc as the static admin key (setup that the console under test must not do for itself).
async function admin(request: APIRequestContext, method: string, data: object) {
  const res = await request.post(api(method), { data, headers: { "X-API-Key": ADMIN_KEY } });
  expect(res.status(), `${method}: ${await res.text()}`).toBe(200);
  return res.json();
}

async function signIn(page: Page) {
  await stubConfig(page, { issuer: idp.issuer, clientId: CLIENT_ID, apiKeyLogin: false });
  await page.goto("/login");
  await page.getByRole("button", { name: "Sign in" }).click();
}

// roles: what the bound account may do (the rpc maps a bound identity to its account's roles).
async function invitedUser(request: APIRequestContext, roles: string[] = []) {
  const id = uid();
  const email = `e2e-${id}@example.com`;
  const { account } = await admin(request, "CreateAccount", { name: `e2e-oidc-${id}`, roles });
  await admin(request, "CreateInvite", { accountId: account.id, email });
  idp.user = await setUser(request, { sub: `sub-${id}`, email, emailVerified: true });
  return { accountId: account.id as string, email };
}

// An admin page opened as the bound account. The positive signal comes first: the rpc answered ListAccounts
// 200, which only an account holding the admin role gets. Only then is "no alert" a statement about the
// bound identity rather than about a probe that has not come back yet.
async function openAccountsAsBound(page: Page) {
  const listed = page.waitForResponse((r) => r.url().endsWith("/identity.v1.IdentityService/ListAccounts"));
  await page.goto("/accounts");
  expect((await listed).status()).toBe(200);
  await expect(page.getByRole("button", { name: "Create account" })).toBeVisible();
  await expect(page.getByText("Not authenticated")).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
}

test("an OIDC token the server verified, for an identity with no account, gets the claim flow; claiming binds it and opens the console @ports", async ({ page, request }) => {
  await invitedUser(request, ["admin"]);
  const authorized: string[] = [];
  page.on("request", (r) => r.url().includes("/api/") && r.headers()["authorization"] && authorized.push(r.headers()["authorization"]));

  await signIn(page);
  // auth.unbound_identity can only come from a server that verified this ES256 token against the IdP's JWKS.
  await expect(page.getByRole("heading", { name: "Link your account" })).toBeVisible();
  expect(authorized.length).toBeGreaterThan(0);

  await page.getByRole("button", { name: "Claim invite" }).click();
  await expect(page.getByRole("heading", { name: "Link your account" })).toHaveCount(0);
  await expect(subject(page)).toHaveText(/^jwt · /);
  // The console's own calls now succeed as the bound account (the subject is the account id, not the token's sub).
  await openAccountsAsBound(page);
});

// The rpc trusts exactly one issuer, the one generated from the same port as the IdP (rpc-server.ts). A token
// from another IdP, even one that signs correctly for the same client id, must not get through: the
// console shows no claim flow for it. The sign-in through the configured IdP right after is the control
// that this test can pass only when the rpc is really talking to its own IdP.
test("a token from an IdP the rpc is not configured for is refused; the configured IdP still gets the claim flow @ports", async ({ page, request }) => {
  await invitedUser(request);
  const other = new TestIdp(FOREIGN_IDP_PORT, CLIENT_ID);
  await other.start();
  try {
    other.user = { ...idp.user };
    expect(other.issuer).not.toBe(idp.issuer);

    await stubConfig(page, { issuer: other.issuer, clientId: CLIENT_ID, apiKeyLogin: false });
    await page.goto("/login");
    // The refusal is the rpc's own answer, awaited, not inferred from something being absent: a token
    // whose iss is no configured issuer is auth.invalid_credential (there is no auth.jwt to fall back on).
    const refused = page.waitForResponse((r) => r.url().includes("/api/identity.v1.IdentityService/") && r.headers()["x-error-code"] === "auth.invalid_credential");
    await page.getByRole("button", { name: "Sign in" }).click();
    expect((await refused).status()).toBe(401);
    await expect(page.getByRole("heading", { name: "Link your account" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Claim invite" })).toHaveCount(0);
  } finally {
    await other.stop();
  }

  // Control: the configured IdP, same invite, a fresh browser context (nothing of the refused session left).
  const fresh = await page.context().browser()!.newContext({ baseURL: test.info().project.use.baseURL });
  try {
    const page2 = await fresh.newPage();
    await signIn(page2);
    await expect(page2.getByRole("heading", { name: "Link your account" })).toBeVisible();
  } finally {
    await fresh.close();
  }
});


test("a token with an unverified email cannot claim: refused with invite.email_unverified, still unbound", async ({ page, request }) => {
  await invitedUser(request);
  idp.user = await setUser(request, { ...idp.user, emailVerified: false });
  await signIn(page);
  await page.getByRole("button", { name: "Claim invite" }).click();
  await expect(page.getByRole("alert")).toContainText("invite.email_unverified");
  await expect(page.getByRole("heading", { name: "Link your account" })).toBeVisible();
});

test("RevokeSessions: the server refuses the old access token (within the 30 s resolver cache); a fresh sign-in from the IdP still works", async ({ page, request }) => {
  test.setTimeout(120_000); // waits out the resolver's 30 s principal cache
  const { accountId } = await invitedUser(request, ["admin"]);
  let bearer = "";
  page.on("request", (r) => {
    const a = r.headers()["authorization"];
    if (r.url().includes("/api/") && a) bearer = a;
  });
  await signIn(page);
  await page.getByRole("button", { name: "Claim invite" }).click();
  await expect(page.getByRole("heading", { name: "Link your account" })).toHaveCount(0);
  await expect.poll(() => bearer).not.toBe("");
  const old = bearer;
  const whoami = () => request.post(api("WhoAmI"), { data: {}, headers: { Authorization: old } });
  expect((await whoami()).status()).toBe(200);

  await page.waitForTimeout(1100); // token iat has one-second resolution: revoke strictly after it
  await admin(request, "RevokeSessions", { accountId });
  // The principal is cached for up to 30 s; after that the account's sessions_revoke_time wins.
  await expect.poll(async () => (await whoami()).status(), { timeout: 45_000, intervals: [1000] }).toBe(401);

  // Revocation is "sign out everywhere", not a ban (§11): a token issued after it is accepted, so the
  // console, whose old token the server now refuses, renews through the IdP and carries on.
  await page.waitForTimeout(1100);
  await page.reload();
  await expect(subject(page)).toHaveText(/^jwt · /);
  await openAccountsAsBound(page);
});

// Bindings card: after a real claim an admin sees the bound identity and the CLAIMED invite (no stubs on the admin side).
test("after a real claim the admin sees the bound identity and the CLAIMED invite", async ({ browser, request, baseURL }) => {
  const id = uid();
  const email = `bind-${id}@example.com`;
  const sub = `sub-bind-${id}`;
  const { account } = await admin(request, "CreateAccount", { name: `e2e-bind-${id}`, roles: [] });
  await admin(request, "CreateInvite", { accountId: account.id, email });

  // 1. the invited person signs in and claims.
  idp.user = await setUser(request, { sub, email, emailVerified: true });
  const user = await browser.newPage({ baseURL });
  await stubConfig(user, { issuer: idp.issuer, clientId: CLIENT_ID, apiKeyLogin: false });
  await user.goto("/login");
  await user.getByRole("button", { name: "Sign in" }).click();
  await user.getByRole("button", { name: "Claim invite" }).click();
  await expect(user.getByRole("heading", { name: "Link your account" })).toHaveCount(0);
  await user.close();

  // 2. an admin looks at the account.
  const adminPage = await browser.newPage({ baseURL });
  await stubConfig(adminPage, { oidc: false });
  await adminPage.goto(`/accounts/${account.id}`);
  await useKey(adminPage, ADMIN_KEY);
  const identity = adminPage.getByTestId("identity-row");
  await expect(identity).toHaveCount(1);
  await expect(identity).toContainText(idp.issuer);
  await expect(identity).toContainText(sub);
  const invite = adminPage.getByTestId("invite-row");
  await expect(invite).toHaveCount(1);
  await expect(invite).toContainText(email);
  await expect(invite).toContainText("CLAIMED");
  await adminPage.close();
});
