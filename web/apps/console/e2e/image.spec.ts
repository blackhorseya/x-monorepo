import { expect, test } from "@playwright/test";
import { mockIdp } from "./auth/oidc";

// What only the real image can show: nginx's own behaviour from deploy/default.conf.template.
// vite preview also serves the SPA and proxies /api, but with its own headers, so these run
// only when the chromium project points at the container (E2E_CONSOLE_URL, e2e/image.sh).
test.skip(
  !process.env.E2E_CONSOLE_URL,
  "needs the console image (task web-e2e-image)"
);

test("deep link falls back to index.html, never cached", async ({
  request,
}) => {
  const res = await request.get("/stock?prefix=E2E");
  expect(res.status()).toBe(200);
  expect(res.headers()["content-type"]).toContain("text/html");
  expect(res.headers()["cache-control"]).toBe("no-cache");
  expect(await res.text()).toContain("<title>x-monorepo console</title>");
});

test("hashed assets are immutable, unknown ones are 404 (not index.html)", async ({
  request,
}) => {
  const html = await (await request.get("/")).text();
  const asset = html.match(/\/assets\/[^"']+\.js/)?.[0];
  expect(asset, "index.html references a hashed script").toBeTruthy();
  const ok = await request.get(asset!);
  expect(ok.status()).toBe(200);
  expect(ok.headers()["cache-control"]).toContain("immutable");
  expect((await request.get("/assets/does-not-exist.js")).status()).toBe(404);
});

test("security headers on what nginx serves, not on the proxied /api", async ({
  request,
}) => {
  const page = (await request.get("/orders")).headers();
  expect(page["content-security-policy"]).toContain("frame-ancestors 'none'");
  expect(page["content-security-policy"]).toContain("script-src 'self'");
  expect(page["x-content-type-options"]).toBe("nosniff");
  expect(page["x-frame-options"]).toBe("DENY");
  expect(page["referrer-policy"]).toBe("strict-origin-when-cross-origin");
  // The rpc owns /api responses (and /api/docs loads Scalar from a CDN): no CSP from nginx there.
  const api = (await request.get("/api/version")).headers();
  expect(api["content-security-policy"]).toBeUndefined();
});

test("/api is the rpc behind nginx; /healthz is nginx itself", async ({
  request,
}) => {
  const version = await request.get("/api/version");
  expect(version.status()).toBe(200);
  expect(await version.json()).toMatchObject({
    version: expect.any(String),
    go: expect.stringMatching(/^go\d/),
  });
  const health = await request.get("/healthz");
  expect(health.status()).toBe(200);
  expect(await health.text()).toBe("ok\n");
});

// Runtime configuration (docs/identity-login.md §8): CONSOLE_* env → /config.json for the SPA, and
// the issuer's origin (exactly that, nothing wider) → the CSP's connect-src. image.sh starts two
// containers (the default, one configured for OIDC) and checks that misconfigured ones refuse to start.
const connectSrc = (csp: string) => csp.split(";").map((d) => d.trim()).find((d) => d.startsWith("connect-src"));

test("no CONSOLE_* env: /config.json has an empty issuer (OIDC off), API key login on, never cached; connect-src stays 'self'", async ({
  request,
}) => {
  const res = await request.get("/config.json");
  expect(res.status()).toBe(200);
  expect(res.headers()["content-type"]).toContain("application/json");
  expect(res.headers()["cache-control"]).toBe("no-store");
  expect(await res.json()).toEqual({ oidcIssuer: "", oidcClientId: "", apiKeyLogin: true });
  const page = (await request.get("/orders")).headers();
  expect(connectSrc(page["content-security-policy"])).toBe("connect-src 'self'");
});

test.describe("configured for OIDC", () => {
  test.skip(!process.env.E2E_CONSOLE_OIDC_URL, "needs the OIDC-configured container (e2e/image.sh)");
  const base = () => process.env.E2E_CONSOLE_OIDC_URL!;

  test("/config.json carries only the three public values; connect-src gains the issuer's exact origin", async ({
    request,
  }) => {
    const res = await request.get(`${base()}/config.json`);
    expect(await res.json()).toEqual({
      oidcIssuer: "https://idp.e2e.example/dex",
      oidcClientId: "x-console-e2e",
      apiKeyLogin: false,
    });
    const text = await res.text();
    expect(text).not.toContain("s3cr3t-must-not-leak"); // another env var of the container: not on the whitelist
    const csp = (await request.get(`${base()}/orders`)).headers()["content-security-policy"];
    expect(connectSrc(csp)).toBe("connect-src 'self' https://idp.e2e.example");
    expect(csp).not.toContain("*");
    expect(csp).not.toContain("s3cr3t-must-not-leak");
    expect(csp).toContain("script-src 'self'"); // the rest of the policy is untouched
  });

  // The browser enforces the CSP, so this is the check that connect-src really lets the SPA reach
  // the issuer (discovery, token) while page.route() stands in for the IdP's network side.
  test("a browser signs in against the configured issuer under the image's CSP; no API key entry", async ({ page }) => {
    await mockIdp(page);
    const violations: string[] = [];
    page.on("console", (m) => /content security policy/i.test(m.text()) && violations.push(m.text()));
    await page.goto(`${base()}/orders`);
    await expect(page).toHaveURL(/\/login/);
    await expect(page.getByLabel("API key")).toHaveCount(0);
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.locator("header").getByText(/^jwt · /)).toBeVisible();
    expect(violations).toEqual([]);
  });

  test("/config.json also carries the security headers' sibling cache rule, and /api has no CSP", async ({ request }) => {
    const res = await request.get(`${base()}/config.json`);
    expect(res.headers()["cache-control"]).toBe("no-store");
    expect((await request.get(`${base()}/api/version`)).headers()["content-security-policy"]).toBeUndefined();
  });
});
