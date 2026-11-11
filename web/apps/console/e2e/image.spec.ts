import { expect, test } from "@playwright/test";

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
  const res = await request.get("/poison?x=E2E");
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
  const page = (await request.get("/poison")).headers();
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

// The CSP is rendered from deploy/security-headers.inc.template by the entrypoint's envsubst (RPC_UPSTREAM is its only
// whitelisted variable): the header has to come out whole. connect-src is exactly 'self' (the app only calls its own
// origin, /api), and no `${...}` of a template variable may be left in it.
const connectSrc = (csp: string) => csp.split(";").map((d) => d.trim()).find((d) => d.startsWith("connect-src"));

test("the CSP is fully rendered: connect-src is 'self' and no template variable is left in it", async ({ request }) => {
  const csp = (await request.get("/poison")).headers()["content-security-policy"];
  expect(csp, "the page has a CSP").toBeTruthy();
  expect(connectSrc(csp)).toBe("connect-src 'self'");
  expect(csp).not.toContain("${");
  expect(csp).not.toContain("*");
});
