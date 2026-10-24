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
