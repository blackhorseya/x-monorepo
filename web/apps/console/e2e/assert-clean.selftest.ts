import { type ChildProcess, execFile, spawn } from "node:child_process";
import { randomInt } from "node:crypto";
import { createServer, type Server } from "node:net";
import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { EventEmitter } from "node:events";
import { type Connect, classifyConnectError, hasIPv6Loopback, isNoAddress, probe } from "./clean.ts";
import { derivePorts } from "./ports.ts";

// The cleanup check (e2e/assert-clean.ts) must fail while ANYTHING still listens on a port the e2e stack
// derives, on every loopback address a fixture can bind: 127.0.0.1, ::1 (what `localhost` is on a
// machine that prefers IPv6 — Vite preview binds it), and the wildcards. A checker that looks at one
// family reports a clean run while a server is still up (#386). Each case holds a listener on a derived
// port, expects exit 1, closes it, and expects exit 0 again; the real `vite preview` is one of the cases.
//
//   node --test --experimental-strip-types e2e/assert-clean.selftest.ts      (task web-e2e-ports runs it first)
// This test is internal to the task: it does not read the caller's E2E pair (that belongs to the smoke
// run, and to whatever else the caller runs beside it) and writes no port of its own. It picks a pair at
// random from the OS's dynamic range and keeps it only if EVERY port derived from it (e2e/ports.ts, 19 of
// them across two blocks) can be bound right now on every loopback, so any number of runs — under any
// callers' pairs — can go side by side (#386). The OS cannot hand out the pair itself: it hands out one port at a
// time, usually the next free one, and the derived ports must be spread over two separate blocks.
const bindable = (host: string, port: number) =>
  new Promise<boolean>((resolve) => {
    const s = createServer();
    s.once("error", () => resolve(false));
    s.listen({ port, host, ipv6Only: host === "::1" }, () => s.close(() => resolve(true)));
  });
async function privatePair(): Promise<{ rpc: number; web: number }> {
  // Two projects deriving the same port is a bug in e2e/ports.ts, not bad luck in the draw below: say so, instead of
  // letting every draw be skipped and the run end as "no free pair".
  const sample = derivePorts({ E2E_RPC_PORT: "20000", E2E_WEB_PORT: "40000" }).all;
  const twice = sample.filter((port, i) => sample.indexOf(port) !== i);
  if (twice.length) throw new Error(`derivePorts() derives port(s) ${[...new Set(twice)].join(", ")} twice (offsets of two projects collide, e2e/ports.ts)`);
  const hosts = (await hasIPv6Loopback()).available ? ["127.0.0.1", "::1"] : ["127.0.0.1"];
  for (let attempt = 0; attempt < 100; attempt++) {
    // Dynamic range, with room for the largest offset (+120) below 65535; the two blocks are far apart.
    const rpc = randomInt(20_000, 30_000);
    const web = randomInt(40_000, 65_000);
    const all = derivePorts({ E2E_RPC_PORT: String(rpc), E2E_WEB_PORT: String(web) }).all;
    if (Math.max(...all) > 65535 || new Set(all).size !== all.length) continue;
    const free = await Promise.all(all.flatMap((port) => hosts.map((host) => bindable(host, port))));
    if (free.every(Boolean)) return { rpc, web };
  }
  throw new Error("no private E2E port pair with every derived port free after 100 tries");
}
const pair = await privatePair();
const env = { ...process.env, E2E_RPC_PORT: String(pair.rpc), E2E_WEB_PORT: String(pair.web) };
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
    // The code stays on the error: the caller decides what it means (taken vs. no such address).
    s.once("error", (err: NodeJS.ErrnoException) => reject(Object.assign(new Error(`${host}:${port} of the private pair could not be bound after it was chosen (${err.code}); rerun`, { cause: err }), { code: err.code })));
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
// `pnpm exec` starts vite as ITS child: signal the group, or vite outlives the test and holds the port.
const killGroup = (c: ChildProcess, signal: NodeJS.Signals) => {
  try {
    if (c.pid) process.kill(-c.pid, signal);
  } catch (e) {
    // ESRCH: the group is gone. macOS answers EPERM for a group whose members have all exited but are not yet
    // reaped, so EPERM counts as gone only once the child we started has exited; anything else is a real failure.
    const code = (e as NodeJS.ErrnoException).code;
    const exited = c.exitCode !== null || c.signalCode !== null;
    if (code !== "ESRCH" && !(code === "EPERM" && exited)) throw e;
  }
};
after(() => viteChildren.forEach((c) => killGroup(c, "SIGKILL")));

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
      // Only a host that has no ::1 may skip (the checker's own definition); a port taken after it was
      // chosen, or any other error, is a failure with its reason.
      const code = (e as NodeJS.ErrnoException).code;
      if (!isNoAddress(code)) throw e;
      t.skip(`no IPv6 loopback on this host (${code}): nothing can listen on ::1`);
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
      const vite = spawn("pnpm", ["exec", "vite", "preview", "--port", String(p.webPort), "--strictPort"], { cwd: consoleDir, stdio: "ignore", detached: true });
      viteChildren.push(vite);
      const exited = new Promise<void>((r) => vite.once("exit", () => r()));
      for (let i = 0; ; i++) {
        if (vite.exitCode !== null) throw new Error(`vite preview exited ${vite.exitCode} (is ${p.webPort} taken, is dist built?)`);
        if (await fetch(`http://localhost:${p.webPort}/`).then((r) => r.ok, () => false)) break;
        if (i > 100) throw new Error("vite preview did not come up in 10s");
        await new Promise((r) => setTimeout(r, 100));
      }
      return async () => {
        killGroup(vite, "SIGTERM");
        await Promise.race([exited, new Promise((r) => setTimeout(r, 5_000))]);
        if (vite.exitCode === null) killGroup(vite, "SIGKILL");
        await exited;
        // The group is signalled, but the port is only free once vite really let go of it.
        for (let i = 0; i < 50 && (await fetch(`http://localhost:${p.webPort}/`).then(() => true, () => false)); i++) await new Promise((r) => setTimeout(r, 100));
      };
    });
  });
});

// Only a refused connection says "nobody here". A timeout or any other error is not an answer, so the
// check must fail on it rather than call the port closed.
describe("what counts as closed", () => {
  it("classifyConnectError: only ECONNREFUSED", () => {
    assert.equal(classifyConnectError("ECONNREFUSED"), "closed");
    for (const code of ["ETIMEDOUT", "EHOSTUNREACH", "ENETUNREACH", "EADDRNOTAVAIL", "EACCES", "ECONNRESET", undefined]) assert.equal(classifyConnectError(code), "unknown", String(code));
  });

  const fake = (script: (s: EventEmitter) => void): Connect => () => {
    const s = Object.assign(new EventEmitter(), { destroy: () => s }) as unknown as ReturnType<Connect> & EventEmitter;
    queueMicrotask(() => script(s));
    return s;
  };
  it("probe: connect → listening, refused → closed, other errors and silence → unknown", async () => {
    assert.deepEqual(await probe("::1", 1, 50, fake((s) => s.emit("connect"))), { state: "listening" });
    assert.deepEqual(await probe("::1", 1, 50, fake((s) => s.emit("error", Object.assign(new Error("x"), { code: "ECONNREFUSED" })))), { state: "closed" });
    for (const code of ["ETIMEDOUT", "EHOSTUNREACH"]) {
      const r = await probe("::1", 1, 50, fake((s) => s.emit("error", Object.assign(new Error("x"), { code }))));
      assert.equal(r.state, "unknown", code);
    }
    const silent = await probe("127.0.0.1", 1, 50, fake(() => {}));
    assert.equal(silent.state, "unknown");
    assert.match(silent.state === "unknown" ? silent.detail : "", /timed out/);
  });
});
