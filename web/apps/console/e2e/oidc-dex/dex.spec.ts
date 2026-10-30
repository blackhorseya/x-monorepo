import {
  expect,
  test,
  type APIRequestContext,
  type Page,
} from "@playwright/test";
import { DEX_RPC_PORT } from "../../playwright.config";
import { stubConfig } from "../auth/oidc";
import { subject } from "../auth/helpers";
import { uid } from "../uid";

// The console signing in through the REAL dev Dex (deployments/docker/dex/config.yaml, issuer
// http://dex.localtest.me:5556/dex), against a real rpc whose only OIDC issuer is that Dex
// (e2e/oidc-dex/rpc.yaml). The loopback IdP of the oidc-backend project proves the server's side
// with tokens we mint; this proves the dev IdP we ship, its login form, the client / redirect
// registration and the issuer string a browser and the rpc must agree on (#274).
//
// Needs Dex running and dex.localtest.me resolvable (public DNS, or /etc/hosts offline:
// docs/operations.md "Dex dev IdP"): `task web-e2e-dex` starts it, CI's console-e2e job does too.
// Binding goes through the real CreateInvite + ClaimInvite, no pre-bound accounts.
// Serial: one rpc, accounts and bindings are shared state.

const ISSUER = "http://dex.localtest.me:5556/dex";
const CLIENT_ID = "x-console";
const ADMIN_KEY = "e2e-dex-admin-key-0123456789abcdef";
const PASSWORD = "password"; // both static accounts
const api = (method: string) =>
  `http://127.0.0.1:${DEX_RPC_PORT}/api/identity.v1.IdentityService/${method}`;

test.describe.configure({ mode: "serial" });

// setup calls the rpc as the static admin key (what the console under test must not do for itself).
async function setup(request: APIRequestContext, method: string, data: object) {
  const res = await request.post(api(method), {
    data,
    headers: { "X-API-Key": ADMIN_KEY },
  });
  expect(res.status(), `${method}: ${await res.text()}`).toBe(200);
  return res.json();
}

// invite makes an account (no roles) and an invite for email, the way an admin would.
async function invite(request: APIRequestContext, email: string) {
  const { account } = await setup(request, "CreateAccount", {
    name: `e2e-dex-${uid()}`,
    roles: [],
  });
  await setup(request, "CreateInvite", { accountId: account.id, email });
  return account.id as string;
}

async function signInAtDex(page: Page, email: string, password = PASSWORD) {
  await stubConfig(page, {
    issuer: ISSUER,
    clientId: CLIENT_ID,
    apiKeyLogin: false,
  });
  await page.goto("/login");
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.waitForURL(`${ISSUER}/**`);
  await page.locator('input[name="login"]').fill(email);
  await page.locator('input[name="password"]').fill(password);
  await page.locator('button[type="submit"]').click();
}

test("admin@example.com signs in at Dex, is unbound until the invite is claimed, then opens the console", async ({
  page,
  request,
}) => {
  await invite(request, "admin@example.com");
  const bearers: string[] = [];
  page.on("request", (r) => {
    const a = r.headers()["authorization"];
    if (r.url().includes("/api/") && a) bearers.push(a);
  });

  await signInAtDex(page, "admin@example.com");
  // auth.unbound_identity can only come from an rpc that verified Dex's token against Dex's JWKS.
  await expect(
    page.getByRole("heading", { name: "Link your account" })
  ).toBeVisible();
  expect(bearers.length).toBeGreaterThan(0);

  await page.getByRole("button", { name: "Claim invite" }).click();
  await expect(
    page.getByRole("heading", { name: "Link your account" })
  ).toHaveCount(0);
  await expect(subject(page)).toHaveText(/^jwt · /);
  await page.getByRole("link", { name: "Orders" }).click();
  await expect(page.getByText("Not authenticated")).toHaveCount(0);
  await expect(page.getByRole("alert")).toHaveCount(0);
});

test("the bound identity signs in again at Dex and goes straight in, no claim page", async ({
  page,
}) => {
  await signInAtDex(page, "admin@example.com");
  await expect(subject(page)).toHaveText(/^jwt · /);
  await expect(
    page.getByRole("heading", { name: "Link your account" })
  ).toHaveCount(0);
});

test("user@example.com is a different Dex identity: unbound again, claims its own invite", async ({
  page,
  request,
}) => {
  await invite(request, "user@example.com");
  await signInAtDex(page, "user@example.com");
  await expect(
    page.getByRole("heading", { name: "Link your account" })
  ).toBeVisible();
  await page.getByRole("button", { name: "Claim invite" }).click();
  await expect(subject(page)).toHaveText(/^jwt · /);
});

test("a wrong password stays at Dex and never reaches the console", async ({
  page,
}) => {
  await signInAtDex(page, "admin@example.com", "not-the-password");
  await expect(page.getByText(/invalid/i)).toBeVisible();
  expect(page.url()).toContain(ISSUER);
});
