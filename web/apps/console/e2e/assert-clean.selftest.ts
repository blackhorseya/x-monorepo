import { type ChildProcess, execFile, spawn } from "node:child_process";
import { randomInt } from "node:crypto";
import { createConnection, createServer, type Server } from "node:net";
import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { EventEmitter } from "node:events";
import { type Connect, LOOPBACKS, classifyConnectError, hasIPv6Loopback, isNoAddress, probe, waitClosed } from "./clean.ts";
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
const ipv6Loopback = await hasIPv6Loopback();
const pair = await privatePair();
const env = { ...process.env, E2E_RPC_PORT: String(pair.rpc), E2E_WEB_PORT: String(pair.web) };
const p = derivePorts(env);
const consoleDir = fileURLToPath(new URL("..", import.meta.url));
const run = promisify(execFile);

async function checker(): Promise<{ code: number; out: string }> {
  try {
    const { stdout, stderr } = await run("node", ["--experimental-strip-types", "e2e/assert-clean.ts"], { cwd: consoleDir, env, timeout: 60_000, killSignal: "SIGKILL" });
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

const hosts = LOOPBACKS.filter((h) => h !== "::1" || ipv6Loopback.available);
// ---- child-wait helper: the ONLY place that spawns a child or waits for one to end (scanned by "no code waits for a child ..." at the end of this file) ----
/** How a child ended, or that it did not within the wait. A spawn error (no such executable) is an end: no `exit` event follows it. */
type ChildEnd = { kind: "exit"; code: number | null; signal: NodeJS.Signals | null } | { kind: "error"; error: Error } | { kind: "timeout" };
type Tracked = { child: ChildProcess; ended?: ChildEnd; whenEnded: Promise<ChildEnd> };
/** Hooks `exit` and `error` NOW (an `error` with no listener is an uncaught exception), so that no wait has to come first. */
function track(child: ChildProcess): Tracked {
  const t: Tracked = { child, whenEnded: undefined as unknown as Promise<ChildEnd> };
  t.whenEnded = new Promise<ChildEnd>((resolve) => {
    const end = (e: ChildEnd) => resolve((t.ended ??= e));
    child.once("exit", (code, signal) => end({ kind: "exit", code, signal }));
    child.once("error", (error) => end({ kind: "error", error }));
  });
  return t;
}
/** Resolves as soon as the child exits, fails to spawn, or `ms` have passed (`timeout`). Never longer than `ms`. */
function waitChild(t: Tracked, ms: number): Promise<ChildEnd> {
  if (t.ended) return Promise.resolve(t.ended);
  let timer: ReturnType<typeof setTimeout> | undefined;
  return Promise.race([t.whenEnded, new Promise<ChildEnd>((r) => (timer = setTimeout(() => r({ kind: "timeout" }), ms)))]).finally(() => clearTimeout(timer));
}
const viteChildren: Tracked[] = [];
/** Every child of this file starts here: tracked from the first tick, and reaped by `after()` whatever the test did. */
function spawnTracked(command: readonly string[], options: { cwd?: string } = {}): Tracked {
  const t = track(spawn(command[0]!, command.slice(1), { cwd: options.cwd, stdio: "ignore", detached: true }));
  viteChildren.push(t);
  return t;
}
// ---- end of the child-wait helper ----

// `pnpm exec` starts vite as ITS child: signal the group, or vite outlives the test and holds the port.
const killGroup = (c: ChildProcess, signal: NodeJS.Signals) => {
  try {
    if (c.pid) process.kill(-c.pid, signal);
  } catch (e) {
    // ESRCH: the group is gone. macOS answers EPERM for a group whose members have all ended but are not yet
    // reaped, so EPERM counts as gone only once the child we started has ended; anything else is a real failure.
    const code = (e as NodeJS.ErrnoException).code;
    const gone = c.exitCode !== null || c.signalCode !== null;
    if (code !== "ESRCH" && !(code === "EPERM" && gone)) throw e;
  }
};
// Whatever a test left behind: SIGKILL every group, then wait (at most 5 s each) until each child is reaped.
after(async () => {
  viteChildren.forEach((t) => killGroup(t.child, "SIGKILL"));
  await Promise.all(viteChildren.map((t) => waitChild(t, 5_000)));
});

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
      const vite = await startVite(p.webPort);
      return () => stopVite(vite, p.webPort);
    });
  });
});

type Vite = Tracked;
/**
 * Starts `command` (default: the real `vite preview` on `port`) and waits `readyMs` for it to answer HTTP. A child that
 * never gets there, or never starts (spawn error), is killed and reaped, for at most `reapMs`, before this throws.
 */
async function startVite(port: number, { command = ["pnpm", "exec", "vite", "preview", "--port", String(port), "--strictPort"], readyMs = 10_000, reapMs = 5_000 } = {}): Promise<Vite> {
  const vite = spawnTracked(command, { cwd: consoleDir });
  try {
    const end = Date.now() + readyMs;
    for (;;) {
      if (vite.ended?.kind === "error") throw new Error(`spawn failed: ${vite.ended.error.message}`);
      if (vite.ended?.kind === "exit") throw new Error(`vite preview exited ${vite.ended.code ?? vite.ended.signal} (is ${port} taken, is dist built?)`);
      // A listener that accepts and never answers must not hang the wait: every request is bounded.
      if (await fetch(`http://localhost:${port}/`, { signal: AbortSignal.timeout(1_000) }).then((r) => r.ok, () => false)) return vite;
      if (Date.now() >= end) throw new Error(`vite preview did not come up in ${readyMs}ms`);
      await new Promise((r) => setTimeout(r, 100));
    }
  } catch (e) {
    killGroup(vite.child, "SIGKILL");
    await waitChild(vite, reapMs);
    throw e;
  }
}
/** SIGTERM the group, SIGKILL after `graceMs`, reap for at most `reapMs`, then wait (at most `closeMs`) until every loopback refuses `port`. Never longer than their sum: what is still bound is for the cleanup check to report. */
async function stopVite(vite: Vite, port: number, { closeMs = 10_000, graceMs = 5_000, reapMs = 5_000 } = {}): Promise<void> {
  killGroup(vite.child, "SIGTERM");
  if ((await waitChild(vite, graceMs)).kind === "timeout") killGroup(vite.child, "SIGKILL");
  await waitChild(vite, reapMs);
  // The group is signalled, but the port is only free once vite really let go of it: a vite that is shutting down
  // still holds it and resets connections, so a failed fetch says nothing. Ask every loopback the way the checker does.
  await waitClosed(hosts, port, { deadlineMs: closeMs });
}

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

// Two ways the checker said "listening" on a port nobody holds (the #477 run, console-e2e (2): the real vite preview
// case failed with `listening [[::1]:48929]` after vite was told to stop). Real sockets, except where a case says otherwise.
const holdHost = (await hasIPv6Loopback()).available ? "::1" : "127.0.0.1";
describe("a port nobody holds is not reported as listening", () => {
  const host = holdHost;

  /** The kernel can connect a socket to ITSELF when it picks the source port that equals the destination port (TCP simultaneous open); `localPort` forces it. */
  const selfConnectOnce = (): Connect => {
    let n = 0;
    return ({ port, host: h }) => (n++ === 0 ? createConnection({ port, host: h, localPort: port, localAddress: h }) : createConnection({ port, host: h }));
  };
  // A destroyed self-connected socket keeps its port in TIME_WAIT, so every real self-connect needs a port of its own.
  const [controlPort, oncePort] = [p.all[5]!, p.all[6]!];
  // The kernel's answer, as the probe sees it, when it connects the socket to itself: connect, from the port to the same port.
  const selfConnectFake: Connect = ({ port, host: h }) => {
    const s = Object.assign(new EventEmitter(), { destroy: () => s, localPort: port, remotePort: port, localAddress: h, remoteAddress: h });
    queueMicrotask(() => s.emit("connect"));
    return s as unknown as ReturnType<Connect>;
  };

  it("control: the forced self-connect really is a connect from the port to itself", async () => {
    const s = createConnection({ port: controlPort, host, localPort: controlPort, localAddress: host });
    const seen = await new Promise<string>((resolve) => {
      s.once("connect", () => resolve(s.localPort === s.remotePort ? "self-connect" : "connect"));
      s.once("error", (e: NodeJS.ErrnoException) => resolve(String(e.code)));
    });
    s.destroy();
    assert.equal(seen, "self-connect", "this kernel does not self-connect, so the cases below would test nothing");
  });

  it("probe: a self-connect is no answer; the next attempt decides (refused → closed)", async () => {
    assert.deepEqual(await probe(host, oncePort, 2_000, selfConnectOnce()), { state: "closed" });
  });

  it("probe: only self-connects → unknown (fail closed), never listening", async () => {
    const r = await probe(host, p.authRpcPort, 2_000, selfConnectFake);
    assert.equal(r.state, "unknown");
    assert.match(r.state === "unknown" ? r.detail : "", /self-connect/);
  });

  // What vite does while it shuts down: the listener is still bound and resets every connection, so fetch fails
  // (ECONNRESET) although the port is not free yet.
  const holdResetting = async () => {
    const s = createServer((c) => c.destroy());
    await new Promise<void>((resolve) => s.listen({ port: p.authRpcPort, host, ipv6Only: host === "::1" }, resolve));
    return s;
  };
  it("waitClosed: a listener that resets connections is not closed", async () => {
    const s = await holdResetting();
    try {
      const t0 = Date.now();
      assert.equal(await waitClosed(LOOPBACKS, p.authRpcPort, { deadlineMs: 600, intervalMs: 20 }), false);
      assert.ok(Date.now() - t0 >= 500, "it gave up before its deadline");
    } finally {
      await close(s);
    }
  });
  it("waitClosed: returns true only after the listener is gone (and not before)", async () => {
    const s = await holdResetting();
    const t0 = Date.now();
    setTimeout(() => void close(s), 300);
    assert.equal(await waitClosed(LOOPBACKS, p.authRpcPort, { deadlineMs: 5_000, intervalMs: 20 }), true);
    assert.ok(Date.now() - t0 >= 250, `it returned after ${Date.now() - t0}ms, before the listener was closed`);
  });
  it("waitClosed: unknown is not closed (a timeout or any error but a refusal keeps it waiting)", async () => {
    const unknown = async () => ({ state: "unknown", detail: "x" }) as const;
    assert.equal(await waitClosed(["127.0.0.1"], p.authRpcPort, { deadlineMs: 150, intervalMs: 10, probe: unknown }), false);
  });
  it("waitClosed: one host closed does not make the port closed", async () => {
    const mixed = async (h: string) => (h === "127.0.0.1" ? ({ state: "closed" } as const) : ({ state: "listening" } as const));
    assert.equal(await waitClosed(["127.0.0.1", "::1"], p.authRpcPort, { deadlineMs: 150, intervalMs: 10, probe: mixed }), false);
  });
  it("waitClosed: nothing listening → true at once (positive control)", async () => {
    const t0 = Date.now();
    assert.equal(await waitClosed(LOOPBACKS, p.authRpcPort, { deadlineMs: 5_000, intervalMs: 20 }), true);
    assert.ok(Date.now() - t0 < 1_000);
  });
});

// The release path must stay bounded and must leave no child behind, whatever the port or the child does (reviews of
// #479: a listener that accepts TCP and never answers HTTP held the old fetch poll, and with it `failsWhileHeld`'s
// finally, for good; a command that does not exist emits `error` and never `exit`).
describe("starting and stopping vite preview is bounded", () => {
  const idle = [process.execPath, "-e", "setInterval(() => {}, 1000)"];
  const ignoresSigterm = [process.execPath, "-e", "process.on('SIGTERM', () => {}); setInterval(() => {}, 1000)"];
  const groupAlive = (c: ChildProcess) => {
    try {
      process.kill(-c.pid!, 0);
      return true;
    } catch (e) {
      return (e as NodeJS.ErrnoException).code === "EPERM" && c.exitCode === null;
    }
  };
  const within = <T>(ms: number, what: string, work: Promise<T>) => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    return Promise.race([work, new Promise<never>((_, reject) => (timer = setTimeout(() => reject(new Error(`${what} did not finish in ${ms}ms`)), ms)))]).finally(() => clearTimeout(timer));
  };
  const hangingListener = async (port: number) => {
    const held: import("node:net").Socket[] = [];
    const s = createServer((c) => held.push(c)); // accepts, says nothing, keeps the socket
    await new Promise<void>((resolve) => s.listen({ port, host: holdHost, ipv6Only: holdHost === "::1" }, resolve));
    return async () => (held.forEach((c) => c.destroy()), close(s));
  };

  it("stopVite: a port that accepts TCP and never answers does not hang the release; the checker still sees it", async () => {
    const port = p.all[7]!;
    const stop = await hangingListener(port);
    try {
      const vite = spawnTracked(idle);
      const t0 = Date.now();
      await within(6_000, "stopVite", stopVite(vite, port, { closeMs: 500 }));
      assert.ok(Date.now() - t0 < 6_000);
      assert.ok(!groupAlive(vite.child), "the child group is still alive after stopVite");
      assert.equal((await probe(holdHost, port)).state, "listening", "control: the listener is still bound, so the release gave up on the deadline, not because the port was free");
    } finally {
      await stop();
    }
  });

  it("startVite: a child that never answers is killed and reaped before the error is thrown", async () => {
    await within(6_000, "startVite", assert.rejects(startVite(p.all[8]!, { command: idle, readyMs: 400 }), /did not come up/));
    const vite = viteChildren[viteChildren.length - 1]!;
    assert.equal(vite.ended?.kind, "exit", "the child was not reaped");
    assert.ok(!groupAlive(vite.child), "the child group is still alive");
  });

  it("startVite: a port that accepts TCP and never answers HTTP does not hang the start", async () => {
    const stop = await hangingListener(p.all[9]!);
    try {
      await within(6_000, "startVite", assert.rejects(startVite(p.all[9]!, { command: idle, readyMs: 400 }), /did not come up/));
      assert.ok(!groupAlive(viteChildren[viteChildren.length - 1]!.child), "the child group is still alive");
    } finally {
      await stop();
    }
  });

  it("startVite: a command that does not exist rejects with the spawn error inside readyMs, leaving no child and no port", async () => {
    const port = p.all[10]!;
    const t0 = Date.now();
    await within(3_000, "startVite", assert.rejects(startVite(port, { command: ["/nonexistent/x-monorepo-no-such-command"], readyMs: 400, reapMs: 200 }), /spawn failed: .*ENOENT/));
    assert.ok(Date.now() - t0 < 400 + 1_000, `took ${Date.now() - t0}ms`);
    const vite = viteChildren[viteChildren.length - 1]!;
    assert.equal(vite.ended?.kind, "error");
    assert.equal(vite.child.pid, undefined, "a child exists although nothing was spawned");
    assert.deepEqual(await probe(holdHost, port), { state: "closed" });
  });

  it("stopVite: a child that ignores SIGTERM is SIGKILLed after the grace period, reaped, and the release returns in bounds", async () => {
    const vite = spawnTracked(ignoresSigterm);
    await new Promise((r) => setTimeout(r, 300)); // the handler must be installed before the signal
    const t0 = Date.now();
    await within(4_000, "stopVite", stopVite(vite, p.all[11]!, { graceMs: 400, reapMs: 2_000, closeMs: 200 }));
    assert.ok(Date.now() - t0 >= 400, "SIGKILL came before the grace period ended: the child was not ignoring SIGTERM");
    assert.deepEqual(vite.ended, { kind: "exit", code: null, signal: "SIGKILL" });
    assert.ok(!groupAlive(vite.child));
  });

  it("stopVite: a child that never reports its end (not even after SIGKILL) does not hang the release", async () => {
    const real = spawnTracked(idle);
    const silent: Tracked = { child: real.child, whenEnded: new Promise<ChildEnd>(() => {}) }; // the end that does not come
    const t0 = Date.now();
    await within(3_000, "stopVite", stopVite(silent, p.all[12]!, { graceMs: 200, reapMs: 200, closeMs: 200 }));
    assert.ok(Date.now() - t0 >= 400, "the two waits were skipped, not bounded");
  });

  it("failsWhileHeld goes red when the release gives up on a port that still holds a listener", async () => {
    let stop: (() => Promise<void>) | undefined;
    try {
      await within(
        20_000,
        "failsWhileHeld",
        assert.rejects(
          failsWhileHeld("leftover", async () => {
            stop = await hangingListener(p.authRpcPort);
            // the bounded release: gives up at its deadline with the listener still bound
            return () => waitClosed(hosts, p.authRpcPort, { deadlineMs: 500 }).then(() => undefined);
          }),
          /closed again, the check must pass, got 1/,
        ),
      );
    } finally {
      await stop?.();
    }
  });
});

// The wait helper itself, on a fake child: every branch, without a process.
describe("waitChild: exit, error and timeout end a wait", () => {
  const fake = () => Object.assign(new EventEmitter(), { pid: undefined, exitCode: null, signalCode: null }) as unknown as ChildProcess;
  it("exit → { exit, code, signal }", async () => {
    const c = fake();
    const t = track(c);
    queueMicrotask(() => c.emit("exit", 3, null));
    assert.deepEqual(await waitChild(t, 1_000), { kind: "exit", code: 3, signal: null });
  });
  it("error → { error }, and an `error` before anyone waits is not an uncaught exception (the listener is hooked at track())", async () => {
    const c = fake();
    const t = track(c);
    const failure = Object.assign(new Error("spawn x ENOENT"), { code: "ENOENT" });
    assert.doesNotThrow(() => c.emit("error", failure));
    assert.deepEqual(await waitChild(t, 1_000), { kind: "error", error: failure });
  });
  it("neither → { timeout } after ms, and not before", async () => {
    const t = track(fake());
    const t0 = Date.now();
    assert.deepEqual(await waitChild(t, 150), { kind: "timeout" });
    assert.ok(Date.now() - t0 >= 140);
  });
  it("the first end wins, and a later wait answers at once with it", async () => {
    const c = fake();
    const t = track(c);
    c.emit("exit", 0, null);
    c.emit("error", new Error("late"));
    const t0 = Date.now();
    assert.deepEqual(await waitChild(t, 5_000), { kind: "exit", code: 0, signal: null });
    assert.ok(Date.now() - t0 < 500);
  });
});

// ---- guard: excluded from its own scan ----
// Every wait for a child goes through waitChild(), whose helper block is the only place that listens for a child's
// end or spawns one. A later `await new Promise((r) => child.once("exit", r))`, an awaited `exited`, or a second
// spawn() is what made #479 unbounded twice; this fails the file instead of a reviewer having to find it.
describe("no code waits for a child except through waitChild", () => {
  const BEGIN = "// ---- child-wait helper";
  const END = "// ---- end of the child-wait helper ----";
  const GUARD = "// ---- guard: excluded from its own scan ----";
  const BYPASS = /\b(?:once|on|addListener|prependListener|off|removeListener)\(\s*["'`](?:exit|close|disconnect)["'`]|\bspawn\(|\bawait\s+\w*\.?exited\b|\bimport\s*\{[^}]*\bonce\b[^}]*\}\s*from\s*["']node:events["']/;
  const source = readFileSync(fileURLToPath(import.meta.url), "utf8");
  const begin = source.indexOf("\n" + BEGIN) + 1;
  const end = source.indexOf("\n" + END) + 1;
  const guard = source.indexOf("\n" + GUARD) + 1;
  const code = (text: string) => text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");

  it("the markers are there, once each, in order", () => {
    // as a whole line each: the constants above, which name the markers, are not markers
    const lines = source.split("\n");
    const at = (m: string) => lines.flatMap((l, i) => (l.startsWith(m) ? [i] : []));
    for (const m of [BEGIN, END, GUARD]) assert.equal(at(m).length, 1, `${m} must start exactly one line`);
    assert.ok(at(BEGIN)[0]! < at(END)[0]! && at(END)[0]! < at(GUARD)[0]!);
  });
  it("nothing outside the helper block (and this guard) listens for a child's end or spawns one", () => {
    const scanned = code(source.slice(0, begin) + source.slice(end, guard));
    assert.doesNotMatch(scanned, BYPASS);
  });
  it("controls: the pattern does see the helper block, and each bypass shape", () => {
    assert.match(code(source.slice(begin, end)), BYPASS, "the helper block must trip the pattern, or the pattern matches nothing");
    for (const shape of [`await new Promise<void>((r) => child.once("exit", () => r()));`, `c.on("close", f)`, `const x = spawn("a", []);`, `await exited;`, `await vite.exited`, `import { once } from "node:events";`]) {
      assert.match(code(shape), BYPASS, shape);
    }
  });
});
