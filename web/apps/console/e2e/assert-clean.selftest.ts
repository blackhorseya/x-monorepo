import { type ChildProcess, execFile, spawn } from "node:child_process";
import { createServer, type Server } from "node:net";
import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { derivePorts } from "./ports.ts";

// The cleanup check (e2e/assert-clean.ts) must fail while ANYTHING still listens on a port the e2e stack
// derives, on every loopback address a fixture can bind: 127.0.0.1, ::1 (what `localhost` is on a
// machine that prefers IPv6 — Vite preview binds it), and the wildcards. A checker that looks at one
// family reports a clean run while a server is still up (#386). Each case holds a listener on a derived
// port, expects exit 1, closes it, and expects exit 0 again; the real `vite preview` is one of the cases.
//
//   node --test --experimental-strip-types e2e/assert-clean.selftest.ts      (task web-e2e-ports runs it first)
const env = { ...process.env, E2E_RPC_PORT: "19711", E2E_WEB_PORT: "4711" };
const p = derivePorts(env);
const consoleDir = fileURLToPath(new URL("..", import.meta.url));
const run = promisify(execFile);

async function checker(): Promise<{ code: number; out: string }> {
  try {
    const { stdout, stderr } = await run("node", ["--experimental-strip-types", "e2e/assert-clean.ts"], { cwd: consoleDir, env });
    return { code: 0, out: stdout + stderr };
  } catch (e) {
    const err = e as { code?: number; stdout?: string; stderr?: string };
    return { code: typeof err.code === "number" ? err.code : -1, out: `${err.stdout ?? ""}${err.stderr ?? ""}` };
  }
}

function listen(host: string, port: number): Promise<Server> {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.once("error", reject);
    s.listen({ port, host, ipv6Only: host === "::1" || host === "::" }, () => resolve(s));
  });
}
const close = (s: Server) => new Promise<void>((r) => s.close(() => r()));

/** Holds `hold()` open: the checker must say 1 while it is, 0 once `release()` has returned. */
async function failsWhileHeld(name: string, hold: () => Promise<() => Promise<void>>) {
  const before = await checker();
  assert.equal(before.code, 0, `${name}: the control must start clean, got ${before.code}: ${before.out}`);
  const release = await hold();
  let during: { code: number; out: string };
  try {
    during = await checker();
  } finally {
    await release();
  }
  assert.equal(during.code, 1, `${name}: a live listener on a derived port must fail the check, got exit ${during.code}: ${during.out}`);
  const after = await checker();
  assert.equal(after.code, 0, `${name}: closed again, the check must pass, got ${after.code}: ${after.out}`);
}

const viteChildren: ChildProcess[] = [];
after(() => viteChildren.forEach((c) => c.kill("SIGKILL")));

describe("assert-clean sees live listeners", () => {
  it("passes when nothing listens (positive control)", async () => {
    assert.equal((await checker()).code, 0);
  });

  it("IPv4 listener on 127.0.0.1", async () => {
    await failsWhileHeld("127.0.0.1", async () => {
      const s = await listen("127.0.0.1", p.authRpcPort);
      return () => close(s);
    });
  });

  it("IPv6-only listener on ::1", async (t) => {
    let s: Server;
    try {
      s = await listen("::1", p.authRpcPort);
    } catch (e) {
      t.skip(`no IPv6 loopback on this host (${(e as NodeJS.ErrnoException).code}): nothing can listen on ::1`);
      return;
    }
    await close(s);
    await failsWhileHeld("::1", async () => {
      const held = await listen("::1", p.authRpcPort);
      return () => close(held);
    });
  });

  it("the real `vite preview` on localhost", async () => {
    await failsWhileHeld("vite preview", async () => {
      const vite = spawn("pnpm", ["exec", "vite", "preview", "--port", String(p.webPort), "--strictPort"], { cwd: consoleDir, stdio: "ignore" });
      viteChildren.push(vite);
      const exited = new Promise<void>((r) => vite.once("exit", () => r()));
      for (let i = 0; ; i++) {
        if (vite.exitCode !== null) throw new Error(`vite preview exited ${vite.exitCode} (is ${p.webPort} taken, is dist built?)`);
        if (await fetch(`http://localhost:${p.webPort}/`).then((r) => r.ok, () => false)) break;
        if (i > 100) throw new Error("vite preview did not come up in 10s");
        await new Promise((r) => setTimeout(r, 100));
      }
      return async () => {
        vite.kill("SIGTERM");
        await Promise.race([exited, new Promise((r) => setTimeout(r, 5_000))]);
        if (vite.exitCode === null) vite.kill("SIGKILL");
        await exited;
      };
    });
  });
});
