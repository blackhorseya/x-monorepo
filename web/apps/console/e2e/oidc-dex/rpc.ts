import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { createServer } from "node:net";
import { test as base } from "@playwright/test";

// One rpc per test, on a free port, with the dev Dex as its only issuer (rpc.yaml) and in-memory
// repositories. Dex's two accounts are fixed and a binding lives as long as the rpc, so a shared
// rpc would make the second attempt of a test (--repeat-each, or CI's --retries) find its identity
// already bound; a fresh rpc makes every attempt start with "no account".
const bin = "../../../bin";

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

const whoAmI = (origin: string, key: string) =>
  fetch(`${origin}/api/identity.v1.IdentityService/WhoAmI`, {
    method: "POST",
    headers: { "content-type": "application/json", "X-API-Key": key },
    body: "{}",
  });

/**
 * startRpc starts bin/rpc on port (a free one by default) and resolves once THIS process answers.
 * The port is free only until it is handed to the rpc, so another service can take it in between:
 * "readyz answered 200" would then be that service. Instead the rpc gets an admin key nobody else
 * knows (this attempt's own), and it counts as ours only when that key is accepted as the admin and
 * a different key is refused. A listener that answers everything the same way fails the second check.
 */
export async function startRpc(port?: number) {
  port ??= await freePort();
  const origin = `http://127.0.0.1:${port}`;
  const key = `e2e-dex-${randomUUID()}`;
  const proc = spawn(`${bin}/rpc`, [], {
    env: {
      ...process.env,
      APP_CONFIG: "./e2e/oidc-dex/rpc.yaml",
      APP_HTTP_ADDR: `:${port}`,
      APP_HTTP_DOCS: "false",
      APP_LOG_LEVEL: "warn",
      APP_AUTH_API_KEYS: `root=${key}:admin`,
    },
    stdio: ["ignore", "ignore", "inherit"],
  });
  const exited = new Promise<number | null>((resolve) =>
    proc.once("exit", resolve)
  );
  const stop = async () => {
    proc.kill("SIGTERM");
    await Promise.race([exited, new Promise((r) => setTimeout(r, 5_000))]);
    if (proc.exitCode === null) proc.kill("SIGKILL");
  };
  try {
    for (let i = 0; ; i++) {
      if (proc.exitCode !== null)
        throw new Error(
          `rpc exited with ${proc.exitCode} (port ${port} taken?)`
        );
      const mine = await whoAmI(origin, key).then(
        async (r) => r.ok && (await r.json()).subject === "root",
        () => false
      );
      if (mine) {
        const stranger = await whoAmI(origin, `not-${key}`).then(
          (r) => r.status,
          () => 0
        );
        if (stranger === 200)
          throw new Error(`port ${port} answers any credential: not this rpc`);
        if (proc.exitCode !== null)
          throw new Error(
            `rpc exited with ${proc.exitCode} (port ${port} taken?)`
          );
        break;
      }
      if (i > 100) throw new Error("rpc did not become ready");
      await new Promise((r) => setTimeout(r, 100));
    }
  } catch (e) {
    await stop();
    throw e;
  }
  const call = async (method: string, data: object) => {
    const res = await fetch(
      `${origin}/api/identity.v1.IdentityService/${method}`,
      {
        method: "POST",
        headers: { "content-type": "application/json", "X-API-Key": key },
        body: JSON.stringify(data),
      }
    );
    if (!res.ok)
      throw new Error(`${method}: ${res.status} ${await res.text()}`);
    return res.json();
  };
  return { origin, call, stop };
}

export const test = base.extend<{ rpc: TestRpc }>({
  rpc: [
    async ({ page }, use) => {
      const { origin, call, stop } = await startRpc();
      try {
        // The preview server proxies /api to a dead address: send the console's calls to this test's rpc.
        await page.route("**/api/**", (route) => {
          const u = new URL(route.request().url());
          return route.continue({ url: `${origin}${u.pathname}${u.search}` });
        });
        await use({ origin, call });
      } finally {
        await stop();
      }
    },
    { auto: true }, // every test of the project has one, whether or not it names it
  ],
});
