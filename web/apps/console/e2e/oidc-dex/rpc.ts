import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { test as base } from "@playwright/test";

// One rpc per test, with the dev Dex as its only issuer (rpc.yaml) and in-memory repositories.
// Dex's two accounts are fixed and a binding lives as long as the rpc, so a shared rpc would make
// the second attempt of a test (--repeat-each, or CI's --retries) find its identity already bound;
// a fresh rpc makes every attempt start with "no account".
const bin = "../../../bin";

export type TestRpc = {
  origin: string;
  call: (method: string, data: object) => Promise<any>;
};

/**
 * startRpc starts bin/rpc and resolves with the address THE PROCESS says it is serving on.
 *
 * There is no port to lose: the rpc binds 127.0.0.1:0 itself (the kernel picks the port and the
 * socket is already the rpc's), and logs `server listening addr=…` on its stdout once it holds it.
 * That line comes down the pipe of the child this function spawned, so no other service can be
 * mistaken for it, and nothing between "pick a port" and "start the server" exists to race. The
 * origin is never guessed or probed for: if the process does not report an address (it exited, e.g.
 * because the port is taken) the call fails.
 *
 * `port` pins the port instead of 0. It is for the regression that holds a port and checks that
 * the fixture fails rather than adopt whatever answers there.
 */
/**
 * spawnServer starts bin/<name> on addr and resolves with the origin it logged (`server listening addr=…`),
 * once its readiness answers. Used for the catalog-rpc an rpc needs; the rpc itself reads the same line below.
 */
async function spawnServer(name: string, addr: string, env: Record<string, string>) {
  const proc = spawn(`${bin}/${name}`, [], {
    env: {
      ...process.env,
      APP_CONFIG: "/dev/null",
      APP_HTTP_ADDR: addr,
      APP_HTTP_DOCS: "false",
      APP_LOG_LEVEL: "info", // the "server listening" line is Info
      APP_LOG_FORMAT: "json",
      ...env,
    },
    stdio: ["ignore", "pipe", "inherit"],
  });
  const exited = new Promise<number | null>((resolve) => proc.once("exit", resolve));
  const stop = async () => {
    proc.kill("SIGTERM");
    await Promise.race([exited, new Promise((r) => setTimeout(r, 5_000))]);
    if (proc.exitCode === null) proc.kill("SIGKILL");
  };
  try {
    const listening = new Promise<string>((resolve, reject) => {
      let buf = "";
      let found = false;
      proc.stdout!.setEncoding("utf8");
      proc.stdout!.on("data", (chunk: string) => {
        if (found) return;
        buf += chunk;
        for (const line of buf.split("\n").slice(0, -1)) {
          try {
            const entry = JSON.parse(line);
            if (entry.msg === "server listening" && typeof entry.addr === "string") {
              found = true;
              resolve(entry.addr);
              return;
            }
          } catch {
            // not a JSON log line
          }
        }
        buf = buf.slice(buf.lastIndexOf("\n") + 1);
      });
      proc.once("exit", (code) => reject(new Error(`${name} exited with ${code} before it reported an address`)));
      setTimeout(() => reject(new Error(`${name} did not report an address`)), 15_000).unref();
    });
    const origin = `http://${await listening}`;
    for (let i = 0; ; i++) {
      if (proc.exitCode !== null) throw new Error(`${name} exited with ${proc.exitCode}`);
      if (await fetch(`${origin}/api/readyz`).then((r) => r.ok, () => false)) return { origin, stop };
      if (i > 100) throw new Error(`${name} did not become ready`);
      await new Promise((r) => setTimeout(r, 100));
    }
  } catch (e) {
    await stop();
    throw e;
  }
}

export async function startRpc(port = 0) {
  // rpc has no catalog of its own: this attempt's rpc gets a catalog-rpc of its own too (in memory), reached
  // with the service key rpc=catalogKey:admin. Its address is read from its log like the rpc's below.
  const catalogKey = `e2e-dex-catalog-${randomUUID()}`;
  const catalog = await spawnServer("catalog-rpc", "127.0.0.1:0", {
    APP_AUTH_MODE: "required",
    APP_AUTH_API_KEYS: `rpc=${catalogKey}:admin`,
  });
  const key = `e2e-dex-${randomUUID()}`; // this attempt's admin key, for the setup calls
  const proc = spawn(`${bin}/rpc`, [], {
    env: {
      ...process.env,
      APP_CONFIG: "./e2e/oidc-dex/rpc.yaml",
      APP_HTTP_ADDR: `127.0.0.1:${port}`,
      APP_HTTP_DOCS: "false",
      APP_LOG_LEVEL: "info", // the "server listening" line is Info
      APP_LOG_FORMAT: "json",
      APP_AUTH_API_KEYS: `root=${key}:admin`,
      APP_CATALOG_UPSTREAM: catalog.origin,
      APP_CATALOG_API_KEY: catalogKey,
    },
    stdio: ["ignore", "pipe", "inherit"],
  });
  const exited = new Promise<number | null>((resolve) =>
    proc.once("exit", resolve)
  );
  const stop = async () => {
    proc.kill("SIGTERM");
    await Promise.race([exited, new Promise((r) => setTimeout(r, 5_000))]);
    if (proc.exitCode === null) proc.kill("SIGKILL");
    await catalog.stop();
  };

  // The address from the rpc's own log. The pipe is read to the end so the child never blocks on it.
  const listening = new Promise<string>((resolve, reject) => {
    let buf = "";
    let found = false;
    proc.stdout!.setEncoding("utf8");
    proc.stdout!.on("data", (chunk: string) => {
      if (found) return;
      buf += chunk;
      for (const line of buf.split("\n").slice(0, -1)) {
        try {
          const entry = JSON.parse(line);
          if (
            entry.msg === "server listening" &&
            typeof entry.addr === "string"
          ) {
            found = true;
            resolve(entry.addr);
            return;
          }
        } catch {
          // not a JSON log line
        }
      }
      buf = buf.slice(buf.lastIndexOf("\n") + 1);
    });
    proc.once("exit", (code) =>
      reject(
        new Error(
          `rpc exited with ${code} before it reported an address (port ${
            port || "any"
          } taken?)`
        )
      )
    );
    setTimeout(
      () => reject(new Error("rpc did not report an address")),
      15_000
    ).unref();
  });

  let origin: string;
  try {
    origin = `http://${await listening}`;
    // Bound, but the handlers may not be wired yet: wait for the rpc's own readiness.
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
