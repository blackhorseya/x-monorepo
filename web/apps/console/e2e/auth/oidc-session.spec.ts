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
  const idp = await mockIdp(page, { claims: { roles: ["admin"] } });
  // The console thinks this token is fine (exp is ahead), the server does not (wrong audience).
  idp.claimsQueue.push({ aud: "someone-else", roles: ["admin"] });
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

// expires_in is the IdP's word for when to refresh next; anything but a positive whole number of
// seconds is not a lifetime. It must fail closed, never become 0 / NaN and a refresh loop.
for (const bad of ["soon", 0, -5, 1.5, "300", null, 9, 86_401, 1e12]) {
  test(`a refresh answered with expires_in ${JSON.stringify(bad)} ends the session and is not retried in a loop`, async ({ page }) => {
    await stubConfig(page);
    const idp = await mockIdp(page);
    await page.goto("/login");
    await signIn(page);
    await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
    idp.expiresInRaw = bad;

    await page.reload();
    await expect(page).toHaveURL(/\/login(\?|$)/);
    await page.waitForTimeout(1500);
    expect(refreshGrants(idp)).toHaveLength(1);
    await expect(subject(page)).toHaveText("anonymous");
    expect(await page.evaluate(() => sessionStorage.length)).toBe(0);
  });

  test(`a sign-in whose token response has expires_in ${JSON.stringify(bad)} is refused`, async ({ page }) => {
    await stubConfig(page);
    const idp = await mockIdp(page);
    idp.expiresInRaw = bad;
    await page.goto("/login");
    await signIn(page);
    await expect(page.getByRole("alert")).toContainText(/expires_in/);
    await page.waitForTimeout(500);
    expect(idp.tokenRequests).toHaveLength(1);
    await expect(subject(page)).toHaveText("anonymous");
  });
}

test("many calls refused at once ask the IdP once; while it is unreachable the backoff holds for later ones too", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page, { claims: { roles: ["admin"] } });
  idp.claimsQueue.push({ aud: "someone-else", roles: ["admin"] }); // the first token is one the server refuses
  idp.failRefreshWith = 503;
  await page.goto("/login");
  await signIn(page);
  await expect.poll(() => refreshGrants(idp).length).toBeGreaterThanOrEqual(1);
  await page.waitForTimeout(2500); // well inside the first 5 s backoff
  expect(refreshGrants(idp)).toHaveLength(1);

  // The user's own Retry is not held back by the backoff (the reload's own restore is refused by the IdP, too).
  await page.goto("/login");
  await expect(page.getByRole("status").filter({ hasText: /identity provider/i })).toBeVisible();
  idp.failRefreshWith = null;
  const before = refreshGrants(idp).length;
  await page.getByRole("button", { name: "Retry" }).click();
  await expect.poll(() => refreshGrants(idp).length).toBeGreaterThan(before);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
});

test("an access token that has expired while the IdP is unreachable is not shown as signed in: recovery notice with Retry, refresh token kept", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page, { expiresIn: 10 });
  idp.failRefreshWith = 503;
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  const kept = idp.refreshTokens[0];

  // 10 s lifetime, the IdP refuses to renew: once it is over the console must say so.
  await expect(page).toHaveURL(/\/login(\?|$)/, { timeout: 15_000 });
  await expect(subject(page)).toHaveText("anonymous");
  await expect(page.getByRole("status").filter({ hasText: /identity provider/i })).toBeVisible();
  expect(await page.evaluate(() => JSON.stringify({ ...sessionStorage }))).toContain(kept);

  idp.failRefreshWith = null;
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await expect(page).not.toHaveURL(/\/login/);
});

test("the access token's expiry is acted on at the moment it passes, even with a refresh still pending", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page, { expiresIn: 10 });
  let release!: () => void;
  idp.gate = new Promise<void>((r) => (release = r));
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await expect.poll(() => refreshGrants(idp).length, { timeout: 12_000 }).toBe(1); // asked at ~5 s, not answered

  // 10 s are over and the refresh is still out: that token is no credential any more.
  await expect(page).toHaveURL(/\/login(\?|$)/, { timeout: 12_000 });
  await expect(subject(page)).toHaveText("anonymous");

  // The answer that arrives late is still good (it is the same session), and signs the user back in.
  release();
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  expect(await page.evaluate(() => JSON.stringify({ ...sessionStorage }))).toContain(idp.refreshTokens[1]);
});

// Without expires_in the console used to assume 300 s, longer than the token's own exp.
const soon = () => Math.floor(Date.now() / 1000) + 3;
for (const [label, raw] of [["no expires_in", undefined], ["expires_in 300", 300]] as const) {
  test(`a token whose own exp is 3 s away is refused (${label}): the earlier of the two counts`, async ({ page }) => {
    await stubConfig(page);
    const idp = await mockIdp(page);
    idp.claims = { exp: soon() };
    idp.expiresInRaw = raw;
    await page.goto("/login");
    await signIn(page);
    await expect(page.getByRole("alert")).toContainText(/expire/i);
    await expect(subject(page)).toHaveText("anonymous");
    expect(idp.tokenRequests).toHaveLength(1);
  });
}

test("no expires_in and an exp 120 s away: accepted, and the session restores after a reload", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  idp.claims = { exp: Math.floor(Date.now() / 1000) + 120 };
  idp.expiresInRaw = undefined;
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  await page.reload();
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
});

test("an IdP that never answers a refresh is given up on: the page still comes up, with the recovery notice", async ({ page }) => {
  await stubConfig(page);
  const idp = await mockIdp(page);
  await page.goto("/login");
  await signIn(page);
  await expect(subject(page)).toHaveText("jwt · e2e-oidc-user");
  let release!: () => void;
  idp.gate = new Promise<void>((r) => (release = r)); // restore on reload is asked, never answered
  try {
    await page.goto("/orders", { waitUntil: "commit" });
    await expect(page.getByRole("status").filter({ hasText: /identity provider/i })).toBeVisible({ timeout: 20_000 });
    expect(await page.evaluate(() => JSON.stringify({ ...sessionStorage }))).toContain(idp.refreshTokens[0]); // kept
  } finally {
    release();
  }
});
