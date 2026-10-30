import { expect, test } from "@playwright/test";
import { CLIENT_ID, ISSUER, collectConsole, mockIdp, signIn, stubConfig, webStorage } from "./oidc";
import { subject } from "./helpers";
import { ADMIN_KEY } from "../../playwright.config";

// OIDC login of the console (docs/identity-login.md §8): Authorization Code + PKCE against the IdP
// in runtime config (/config.json). The IdP is stubbed in the browser (oidc.ts); the rpc is the
// auth project's real one, which accepts the access tokens the stub hands out.

test("signed out with OIDC on: every page goes to /login, which offers the IdP", async ({ page }) => {
  await stubConfig(page);
  await mockIdp(page);
  await page.goto("/orders");
  await expect(page).toHaveURL(/\/login(\?|$)/);
  await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
});

test("after signing in you land where you were going; a returnTo to another origin is ignored", async ({ page }) => {
  await stubConfig(page);
  await mockIdp(page);
  await page.goto("/stock");
  await signIn(page);
  await expect(page).toHaveURL(/\/stock$/);

  const other = await page.context().newPage();
  await stubConfig(other);
  await mockIdp(other);
  await other.goto("/login?returnTo=https://evil.example/phish");
  await signIn(other);
  await expect(other).toHaveURL(/\/orders$/);
});

test("sign in: authorization code + PKCE (S256), state and nonce, callback path /auth/callback", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  await page.goto("/login");
  await signIn(page);

  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await expect(page).toHaveURL(/\/orders$/);

  expect(idp.authorizeRequests).toHaveLength(1);
  const q = idp.authorizeRequests[0];
  expect(q.get("response_type")).toBe("code");
  expect(q.get("client_id")).toBe(CLIENT_ID);
  expect(q.get("redirect_uri")).toBe(new URL("/auth/callback", page.url()).toString());
  expect(q.get("scope")?.split(" ")).toEqual(expect.arrayContaining(["openid", "profile", "email", "offline_access"]));
  expect(q.get("code_challenge_method")).toBe("S256");
  expect(q.get("code_challenge")).toMatch(/^[A-Za-z0-9_-]{43}$/);
  expect(q.get("state")?.length).toBeGreaterThanOrEqual(16);
  expect(q.get("nonce")?.length).toBeGreaterThanOrEqual(16);

  // The exchange proves possession of the verifier, and is for a public client (no secret).
  expect(idp.tokenRequests).toHaveLength(1);
  const t = idp.tokenRequests[0];
  expect(t.get("grant_type")).toBe("authorization_code");
  expect(t.get("code_verifier")).toMatch(/^[A-Za-z0-9_~.-]{43,128}$/);
  expect(t.get("client_id")).toBe(CLIENT_ID);
  expect(t.get("client_secret")).toBeNull();
  expect(t.get("redirect_uri")).toBe(q.get("redirect_uri"));
});

test("the signed-in console calls the rpc with the access token", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page, { claims: { roles: ["admin"] } });
  const seen: string[] = [];
  page.on("request", (r) => {
    const a = r.headers()["authorization"];
    if (r.url().includes("/api/") && a) seen.push(a);
  });
  await page.goto("/login");
  await signIn(page);
  await expect(page.locator("header nav").getByRole("link", { name: "Accounts" })).toBeVisible(); // admin probe passed
  expect(seen.length).toBeGreaterThan(0);
  expect(new Set(seen)).toEqual(new Set([`Bearer ${idp.accessTokens[0]}`]));
});

test("tokens stay out of localStorage, the URL, the console, and the flow's secrets are spent", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  const logs = collectConsole(page);
  const urls: string[] = [];
  page.on("framenavigated", (f) => urls.push(f.url()));
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");

  const local = await page.evaluate(() => JSON.stringify({ ...localStorage }));
  const session = await page.evaluate(() => JSON.stringify({ ...sessionStorage }));
  for (const secret of [...idp.accessTokens, ...idp.idTokens]) {
    expect(local).not.toContain(secret);
    expect(session).not.toContain(secret); // access / id token: memory only
    expect(logs.join("\n")).not.toContain(secret);
    expect(urls.join("\n")).not.toContain(secret);
  }
  expect(local).not.toContain(idp.refreshTokens[0]);
  expect(logs.join("\n")).not.toContain(idp.refreshTokens[0]);
  expect(session).toContain(idp.refreshTokens[0]); // §8: the refresh token is the one thing sessionStorage keeps

  // The authorization code and state left the address bar (and history) after the exchange; the
  // PKCE verifier, state and nonce are gone from storage.
  expect(page.url()).not.toMatch(/[?&](code|state)=/);
  const state = idp.authorizeRequests[0].get("state")!;
  const verifier = idp.tokenRequests[0].get("code_verifier")!;
  const nonce = idp.authorizeRequests[0].get("nonce")!;
  const everything = await webStorage(page);
  for (const spent of [state, verifier, nonce]) expect(everything).not.toContain(spent);
});

test("a callback whose state is not the one we sent is refused: no token request, nothing signed in", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  idp.tamperState = true;
  await page.goto("/login");
  await signIn(page);

  await expect(page.getByRole("alert")).toContainText(/state/i);
  expect(idp.tokenRequests).toHaveLength(0);
  await expect(subject(page)).toHaveText("anonymous");
  expect(page.url()).not.toMatch(/[?&](code|state)=/);
  const verifier = idp.authorizeRequests[0].get("code_challenge")!;
  expect(await webStorage(page)).not.toContain(idp.authorizeRequests[0].get("state")!);
  expect(verifier).toBeTruthy();
});

test("a callback with no login in progress is refused", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  await page.goto("/auth/callback?code=stolen&state=whatever");
  await expect(page.getByRole("alert")).toBeVisible();
  expect(idp.tokenRequests).toHaveLength(0);
  expect(page.url()).not.toMatch(/[?&](code|state)=/);
});

test("an authorization error from the IdP is shown and ends the attempt", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  await page.goto("/login");
  await page.route("**/dex/auth?**", (route) => {
    const q = new URL(route.request().url()).searchParams;
    const back = new URL(q.get("redirect_uri")!);
    back.searchParams.set("error", "access_denied");
    back.searchParams.set("state", q.get("state")!);
    return route.fulfill({ status: 302, headers: { location: back.toString() } });
  });
  await signIn(page);
  await expect(page.getByRole("alert")).toContainText("access_denied");
  expect(idp.tokenRequests).toHaveLength(0);
});

test("apiKeyLogin=false: the API key entry is not in the page at all", async ({ page }) => {
  await stubConfig(page, { apiKeyLogin: false });
  await mockIdp(page);
  await page.goto("/login");
  await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
  await expect(page.getByLabel("API key")).toHaveCount(0);
  await expect(page.getByRole("button", { name: /API key/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Credential" })).toHaveCount(0);
  await expect(page.getByText(/api key/i)).toHaveCount(0);
});

test("apiKeyLogin=true: an API key is a secondary way in (break-glass), OIDC stays the main one", async ({ page }) => {
  await stubConfig(page, { apiKeyLogin: true });
  await mockIdp(page);
  await page.goto("/login");
  await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
  await page.getByLabel("API key").fill(ADMIN_KEY);
  await page.getByRole("button", { name: "Use API key" }).click();
  await expect(subject(page)).toHaveText("api key");
  await expect(page).not.toHaveURL(/\/login/);
});

test("without OIDC in the runtime config nothing changes: no login page, the credential control stays", async ({ page }) => {
  await stubConfig(page, { oidc: false });
  await page.goto("/orders");
  await expect(page).toHaveURL(/\/orders$/);
  await expect(page.getByRole("button", { name: "Credential" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Sign in" })).toHaveCount(0);
});

// The token endpoint receives the authorization code and the PKCE verifier. The CSP allows the
// console's own origin (the RPCs) and the issuer's, so the check that the endpoint belongs to the
// issuer has to be the console's own: any other origin is refused before a code is ever requested.
for (const [label, issuer, tokenEndpoint] of [
  ["https issuer, token endpoint on another origin", ISSUER, "https://tokens.e2e.example/token"],
  ["https issuer, token endpoint on the same host but another port", ISSUER, "https://idp.e2e.example:8443/token"],
  ["http issuer, token endpoint on the console's own origin", "http://idp.e2e.example/dex", "http://localhost:4174/review-token"],
  ["http issuer, token endpoint on another http origin", "http://idp.e2e.example/dex", "http://tokens.e2e.example/token"],
] as const) {
  test(`discovery with ${label} is refused: nothing is sent to it`, async ({ page }) => {
    await stubConfig(page, { issuer });
    const idp = await mockIdp(page, { issuer, tokenEndpoint });
    await page.goto("/login");
    await signIn(page);
    await expect(page.getByRole("alert")).toContainText(/origin/i);
    await page.waitForTimeout(300);
    expect(idp.authorizeRequests).toHaveLength(0); // never even sent to the IdP to sign in
    expect(idp.tokenRequests).toHaveLength(0);
    await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
    await expect(page).toHaveURL(/\/login/);
  });
}
