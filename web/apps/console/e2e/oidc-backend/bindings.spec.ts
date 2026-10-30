import { expect, test, type APIRequestContext } from "@playwright/test";
import { BACKEND_IDP_PORT, BACKEND_RPC_PORT } from "../../playwright.config";
import { stubConfig } from "../auth/oidc";
import { useKey } from "../auth/helpers";
import { uid } from "../uid";

// The bindings card against the REAL rpc: someone claims an invite through the loopback IdP, then an
// admin (the static key) opens the account and sees the identity that is now bound and the invite
// as CLAIMED. Nothing is stubbed on the admin side.

const CLIENT_ID = "x-console-e2e";
const ADMIN_KEY = "e2e-backend-admin-key-0123456789abcdef";
const issuer = `http://127.0.0.1:${BACKEND_IDP_PORT}/idp`;
const api = (m: string) => `http://127.0.0.1:${BACKEND_RPC_PORT}/api/identity.v1.IdentityService/${m}`;

async function admin(request: APIRequestContext, method: string, data: object) {
  const res = await request.post(api(method), { data, headers: { "X-API-Key": ADMIN_KEY } });
  expect(res.status(), `${method}: ${await res.text()}`).toBe(200);
  return res.json();
}

test("after a real claim the admin sees the bound identity and the CLAIMED invite", async ({ browser, request }) => {
  const id = uid();
  const email = `bind-${id}@example.com`;
  const sub = `sub-bind-${id}`;
  const { account } = await admin(request, "CreateAccount", { name: `e2e-bind-${id}`, roles: [] });
  await admin(request, "CreateInvite", { accountId: account.id, email });

  // 1. the invited person signs in and claims.
  await request.post(`http://127.0.0.1:${BACKEND_IDP_PORT}/control/user`, { data: { sub, email, emailVerified: true } });
  const user = await browser.newPage({ baseURL: "http://localhost:4193" });
  await stubConfig(user, { issuer, clientId: CLIENT_ID, apiKeyLogin: false });
  await user.goto("/login");
  await user.getByRole("button", { name: "Sign in" }).click();
  await user.getByRole("button", { name: "Claim invite" }).click();
  await expect(user.getByRole("heading", { name: "Link your account" })).toHaveCount(0);
  await user.close();

  // 2. an admin looks at the account.
  const adminPage = await browser.newPage({ baseURL: "http://localhost:4193" });
  await stubConfig(adminPage, { oidc: false });
  await adminPage.goto(`/accounts/${account.id}`);
  await useKey(adminPage, ADMIN_KEY);
  const identity = adminPage.getByTestId("identity-row");
  await expect(identity).toHaveCount(1);
  await expect(identity).toContainText(issuer);
  await expect(identity).toContainText(sub);
  const invite = adminPage.getByTestId("invite-row");
  await expect(invite).toHaveCount(1);
  await expect(invite).toContainText(email);
  await expect(invite).toContainText("CLAIMED");
  await adminPage.close();
});
