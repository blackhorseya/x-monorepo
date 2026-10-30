import { expect, type Browser, type Page } from "@playwright/test";
import { stubConfig } from "../auth/oidc";
import { subject } from "../auth/helpers";
import { uid } from "../uid";
import { createServer } from "node:http";
import { startRpc, test, type TestRpc } from "./rpc";

// The console signing in through the REAL dev Dex (deployments/docker/dex/config.yaml, issuer
// http://dex.localtest.me:5556/dex), against a real rpc whose only OIDC issuer is that Dex
// (e2e/oidc-dex/rpc.yaml). The loopback IdP of the oidc-backend project proves the server's side
// with tokens we mint; this proves the dev IdP we ship, its login form, the client / redirect
// registration, the CORS origins and the issuer string a browser and the rpc must agree on (#274).
//
// Needs Dex running and dex.localtest.me resolvable (public DNS, or /etc/hosts offline:
// docs/operations.md "Dex dev IdP"): `task web-e2e-dex` starts it, CI's console-e2e job does too.
// Binding goes through the real CreateInvite + ClaimInvite, no pre-bound accounts. Every test gets
// its own rpc (rpc.ts), so "unbound on the first sign-in" holds on every attempt and retry.

const ISSUER = "http://dex.localtest.me:5556/dex";
const CLIENT_ID = "x-console";
const PASSWORD = "password"; // both static accounts

// invite makes an account (no roles) and an invite for email, the way an admin would.
async function invite(rpc: TestRpc, email: string) {
  const { account } = await rpc.call("CreateAccount", {
    name: `e2e-dex-${uid()}`,
    roles: [],
  });
  await rpc.call("CreateInvite", { accountId: account.id, email });
}

// newConsole is a browser with no cookies (no Dex session either) whose /api calls go to rpc.
async function newConsole(
  browser: Browser,
  baseURL: string | undefined,
  rpc: TestRpc
) {
  const context = await browser.newContext({ baseURL });
  const page = await context.newPage();
  await page.route("**/api/**", (route) => {
    const u = new URL(route.request().url());
    return route.continue({ url: `${rpc.origin}${u.pathname}${u.search}` });
  });
  return page;
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

test("admin@example.com signs in at Dex, is unbound until the invite is claimed, then opens the console; signing in again goes straight in", async ({
  page,
  rpc,
  browser,
  baseURL,
}) => {
  await invite(rpc, "admin@example.com");
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

  // A new browser (no console session, no Dex cookie) signs in again: the identity is bound now.
  const again = await newConsole(browser, baseURL, rpc);
  await signInAtDex(again, "admin@example.com");
  await expect(subject(again)).toHaveText(/^jwt · /);
  await expect(
    again.getByRole("heading", { name: "Link your account" })
  ).toHaveCount(0);
  await again.context().close();
});

test("user@example.com is a different Dex identity: unbound, claims its own invite", async ({
  page,
  rpc,
}) => {
  await invite(rpc, "user@example.com");
  await signInAtDex(page, "user@example.com");
  await expect(
    page.getByRole("heading", { name: "Link your account" })
  ).toBeVisible();
  await page.getByRole("button", { name: "Claim invite" }).click();
  await expect(subject(page)).toHaveText(/^jwt · /);
});

test("an identity nobody invited is unbound and cannot claim", async ({
  page,
}) => {
  await signInAtDex(page, "admin@example.com");
  await expect(
    page.getByRole("heading", { name: "Link your account" })
  ).toBeVisible();
  await page.getByRole("button", { name: "Claim invite" }).click();
  await expect(page.getByRole("alert")).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Link your account" })
  ).toBeVisible();
});

test("a wrong password stays at Dex and never reaches the console", async ({
  page,
}) => {
  await signInAtDex(page, "admin@example.com", "not-the-password");
  await expect(page.getByText(/invalid/i)).toBeVisible();
  expect(page.url()).toContain(ISSUER);
});

// The fixture's rpc is only used once it proves it is this attempt's process (rpc.ts): a listener that
// took the port after it was picked, whatever it answers, is not it. Each variant is a listener that
// answers the admin key one way and any other key another way; startRpc must refuse every one.
const json = (status: number, body: unknown) => ({
  status,
  body: JSON.stringify(body),
});
const root = json(200, {
  subject: "root",
  roles: ["admin"],
  method: "api_key",
});
const refused = json(401, {
  code: "unauthenticated",
  message: "unknown api key",
});
const foreigners: [string, ReturnType<typeof json>, ReturnType<typeof json>][] =
  [
    ["answers 200 to every key", root, root],
    ["answers 201 to a stranger", root, json(201, root)],
    ["answers 202 to a stranger", root, json(202, {})],
    ["answers 204 with no body to a stranger", root, { status: 204, body: "" }],
    [
      "refuses a stranger with 403",
      root,
      json(403, { code: "permission_denied" }),
    ],
    ["fails a stranger with 500", root, json(500, { code: "internal" })],
    [
      "refuses a stranger with 401 but another code",
      root,
      json(401, { code: "internal" }),
    ],
    [
      "refuses a stranger with 401 and no JSON",
      root,
      { status: 401, body: "nope" },
    ],
    [
      "knows the key but is not an admin",
      json(200, { subject: "root", roles: [], method: "api_key" }),
      refused,
    ],
    [
      "knows the key but is another subject",
      json(200, { subject: "someone", roles: ["admin"], method: "api_key" }),
      refused,
    ],
    ["returns the admin as an empty 200", { status: 200, body: "" }, refused],
  ];
for (const [name, rootAnswer, strangerAnswer] of foreigners) {
  test(`a foreign listener on the rpc's port that ${name} is not mistaken for the rpc`, async () => {
    const foreign = createServer((req, res) => {
      const a =
        req.headers["x-api-key"] === undefined ||
        String(req.headers["x-api-key"]).startsWith("not-")
          ? strangerAnswer
          : rootAnswer;
      res
        .writeHead(a.status, { "content-type": "application/json" })
        .end(a.body);
    });
    await new Promise<void>((resolve) =>
      foreign.listen(0, "127.0.0.1", resolve)
    );
    try {
      const { port } = foreign.address() as { port: number };
      await expect(startRpc(port)).rejects.toThrow(/not this rpc|exited with/);
    } finally {
      await new Promise((resolve) => foreign.close(resolve));
    }
  });
}
