import { spawn } from "node:child_process";
import { createServer } from "node:net";
import { test as base } from "@playwright/test";

// One rpc per test, on a free port, with the dev Dex as its only issuer (rpc.yaml) and in-memory
// repositories. Dex's two accounts are fixed and a binding lives as long as the rpc, so a shared
// rpc would make the second attempt of a test (--repeat-each, or CI's --retries) find its identity
// already bound; a fresh rpc makes every attempt start with "no account".
const bin = "../../../bin";
export const ADMIN_KEY = "e2e-dex-admin-key-0123456789abcdef"; // keep in sync with rpc.yaml

async function freePort() {
  const srv = createServer();
  await new Promise<void>((resolve) => srv.listen(0, "127.0.0.1", resolve));
  const { port } = srv.address() as { port: number };
  await new Promise((resolve) => srv.close(resolve));
  return port;
}

export type TestRpc = {
  origin: string;
  call: (method: string, data: object) => Promise<any>;
};

export const test = base.extend<{ rpc: TestRpc }>({
  rpc: [
    async ({ page }, use) => {
      const port = await freePort();
      const origin = `http://127.0.0.1:${port}`;
      const proc = spawn(`${bin}/rpc`, [], {
        env: {
          ...process.env,
          APP_CONFIG: "./e2e/oidc-dex/rpc.yaml",
          APP_HTTP_ADDR: `:${port}`,
          APP_HTTP_DOCS: "false",
          APP_LOG_LEVEL: "warn",
        },
        stdio: ["ignore", "ignore", "inherit"],
      });
      const exited = new Promise<number | null>((resolve) =>
        proc.once("exit", resolve)
      );
      try {
        for (let i = 0; ; i++) {
          if (proc.exitCode !== null)
            throw new Error(`rpc exited with ${proc.exitCode}`);
          if (
            await fetch(`${origin}/api/readyz`).then(
              (r) => r.ok,
              () => false
            )
          )
            break;
          if (i > 100) throw new Error("rpc did not become ready");
          await new Promise((r) => setTimeout(r, 100));
        }
        // The preview server proxies /api to a dead address: send the console's calls to this test's rpc.
        await page.route("**/api/**", (route) => {
          const u = new URL(route.request().url());
          return route.continue({ url: `${origin}${u.pathname}${u.search}` });
        });
        const call = async (method: string, data: object) => {
          const res = await fetch(
            `${origin}/api/identity.v1.IdentityService/${method}`,
            {
              method: "POST",
              headers: {
                "content-type": "application/json",
                "X-API-Key": ADMIN_KEY,
              },
              body: JSON.stringify(data),
            }
          );
          if (!res.ok)
            throw new Error(`${method}: ${res.status} ${await res.text()}`);
          return res.json();
        };
        await use({ origin, call });
      } finally {
        proc.kill("SIGTERM");
        await Promise.race([exited, new Promise((r) => setTimeout(r, 5_000))]);
        if (proc.exitCode === null) proc.kill("SIGKILL");
      }
    },
    { auto: true }, // every test of the project has one, whether or not it names it
  ],
});
