import { expect, test } from "@playwright/test";
import { CLIENT_ID, END_SESSION, collectConsole, mockIdp, signIn, stubConfig, webStorage } from "./oidc";
import { subject } from "./helpers";
import { ADMIN_KEY } from "../../playwright.config";

// The life of an OIDC session (docs/identity-login.md §8): reload restores it from the refresh
// token (rotation), the access token is renewed before it expires, a dead or unreachable IdP is
// told apart, and signing out leaves nothing behind.

const refreshGrants = (idp: Awaited<ReturnType<typeof mockIdp>>) =>
  idp.tokenRequests.filter((t) => t.get("grant_type") === "refresh_token");

test("reload restores the session silently from the refresh token, which rotates", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  const first = idp.refreshTokens[0];

  await page.reload();
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await expect(page).toHaveURL(/\/orders$/); // never bounced through /login

  const grants = refreshGrants(idp);
  expect(grants).toHaveLength(1);
  expect(grants[0].get("refresh_token")).toBe(first);
  expect(grants[0].get("client_id")).toBe(CLIENT_ID);
  const stored = await page.evaluate(() => JSON.stringify({ ...sessionStorage }));
  expect(stored).toContain(idp.refreshTokens[1]); // the rotated one
  expect(stored).not.toContain(first); // the spent one is gone
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage }))).not.toContain(idp.refreshTokens[1]);
});

test("a refresh token the IdP no longer accepts ends the session: back to /login, nothing kept", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  idp.validRefreshTokens.clear(); // revoked at the IdP

  await page.reload();
  await expect(page).toHaveURL(/\/login(\?|$)/);
  await expect(subject(page)).toHaveText("anonymous");
  expect(await webStorage(page)).not.toContain(idp.refreshTokens[0]);
  expect(await page.evaluate(() => sessionStorage.length)).toBe(0);
});

test("an unreachable IdP (5xx) is not a sign-out: the refresh token is kept and Retry recovers", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  const kept = idp.refreshTokens[0];
  idp.failRefreshWith = 503;

  await page.reload();
  await expect(page.getByRole("status").filter({ hasText: /identity provider/i })).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
  expect(await page.evaluate(() => JSON.stringify({ ...sessionStorage }))).toContain(kept);

  idp.failRefreshWith = null;
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await expect(page).not.toHaveURL(/\/login/);
});

test("the access token is renewed before it expires, without a page reload", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page, { expiresIn: 65 }); // renewed 60 s early, never sooner than 5 s
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await expect.poll(() => refreshGrants(idp).length, { timeout: 20_000 }).toBeGreaterThanOrEqual(1);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");

  const used: string[] = [];
  page.on("request", (r) => {
    const a = r.headers()["authorization"];
    if (r.url().includes("/api/") && a) used.push(a);
  });
  await page.getByRole("link", { name: "Stock" }).click();
  await expect.poll(() => used.length).toBeGreaterThan(0);
  expect(used.at(-1)).toBe(`Bearer ${idp.accessTokens.at(-1)}`);
});

test("an access token the server refuses (UNAUTHENTICATED) is renewed once and the call retried", async ({ page }) => {
  await stubConfig(page);
  const expired = Math.floor(Date.now() / 1000) - 600;
  const idp = await mockIdp(page, { claims: { roles: ["admin"] } });
  idp.claimsQueue.push({ exp: expired, roles: ["admin"] });
  await page.goto("/login");
  await signIn(page);
  await expect(page.locator("header nav").getByRole("link", { name: "Accounts" })).toBeVisible();
  expect(refreshGrants(idp)).toHaveLength(1);
  await expect(page.getByText("Not authenticated")).toHaveCount(0);
});

test("a refresh answered after sign-out is discarded", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page, { expiresIn: 65 });
  let release!: () => void;
  idp.gate = new Promise<void>((r) => (release = r));
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await expect.poll(() => refreshGrants(idp).length, { timeout: 20_000 }).toBe(1); // asked, not answered

  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/\/login(\?|$)/);
  release();
  await expect.poll(() => idp.refreshTokens.length).toBe(2); // the IdP did answer
  await page.waitForTimeout(300);
  await expect(subject(page)).toHaveText("anonymous");
  expect(await page.evaluate(() => sessionStorage.length)).toBe(0);
});

test("sign out: local state cleared, no token left anywhere, later calls carry none, reload stays signed out", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  const logs = collectConsole(page);
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");

  const after: (string | undefined)[] = [];
  await page.getByRole("button", { name: "Sign out" }).click();
  page.on("request", (r) => r.url().includes("/api/identity") && after.push(r.headers()["authorization"]));
  await expect(page).toHaveURL(/\/login(\?|$)/);
  await expect(subject(page)).toHaveText("anonymous");

  const stored = await webStorage(page);
  for (const secret of [...idp.accessTokens, ...idp.refreshTokens, ...idp.idTokens]) {
    expect(stored).not.toContain(secret);
    expect(logs.join("\n")).not.toContain(secret);
  }
  expect(after.filter(Boolean)).toEqual([]);

  await page.reload();
  await expect(page).toHaveURL(/\/login(\?|$)/);
  expect(refreshGrants(idp)).toHaveLength(0);
});

test("sign out goes to the IdP's end_session_endpoint when discovery has one, with no token in the URL", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page, { endSession: true });
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");

  await page.getByRole("button", { name: "Sign out" }).click();
  await page.waitForURL(`${END_SESSION}**`);
  const url = new URL(page.url());
  expect(url.searchParams.get("client_id")).toBe(CLIENT_ID);
  expect(url.searchParams.get("post_logout_redirect_uri")).toBe(new URL("/login", "http://localhost:4174").toString());
  expect(url.searchParams.has("id_token_hint")).toBe(false);
  for (const secret of [...idp.accessTokens, ...idp.idTokens, ...idp.refreshTokens]) expect(page.url()).not.toContain(secret);
});

test("sign out without an end_session_endpoint (Dex) stays in the console", async ({ page }) => {
  await stubConfig(page);
  await mockIdp(page, { endSession: false });
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page).toHaveURL(/localhost:4174\/login/);
});

test("an API key chosen on top of an OIDC session replaces it: refresh token dropped, the key survives a reload", async ({ page }) => {
  await stubConfig(page, { apiKeyLogin: true });
  const idp = await mockIdp(page);
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");

  await page.goto("/login"); // reachable while signed in (restored from the refresh token)
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await page.getByLabel("API key").fill(ADMIN_KEY);
  await page.getByRole("button", { name: "Use API key" }).click();
  await expect(subject(page)).toHaveText("api key");
  for (const r of idp.refreshTokens) expect(await webStorage(page)).not.toContain(r);

  const grants = refreshGrants(idp).length;
  await page.reload();
  await expect(subject(page)).toHaveText("api key");
  expect(refreshGrants(idp)).toHaveLength(grants); // no silent OIDC refresh behind the key's back
});
