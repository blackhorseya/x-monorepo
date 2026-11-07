import { randomInt } from "node:crypto";
import { createConnection, createServer, type Server } from "node:net";
import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { EventEmitter } from "node:events";
import { type Connect, LOOPBACKS, classifyConnectError, hasIPv6Loopback, isNoAddress, probe, waitClosed } from "./clean.ts";
import { derivePorts } from "./ports.ts";
import * as childModule from "./child.ts";
import { MAX_WAIT_MS, type Tracked, __testing, groupAlive, lastSpawned, reapAll, runBounded, signalGroup, spawnTracked, waitChild } from "./child.ts";

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

const checker = () => runBounded("node", ["--experimental-strip-types", "e2e/assert-clean.ts"], { cwd: consoleDir, env, timeoutMs: 60_000 });

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
// Whatever a test left behind: SIGKILL every group, then wait (at most 5 s each) until each child is reaped (e2e/child.ts).
after(() => reapAll(5_000));

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
    signalGroup(vite, "SIGKILL");
    await waitChild(vite, reapMs);
    throw e;
  }
}
/** SIGTERM the group, SIGKILL after `graceMs`, reap for at most `reapMs`, then wait (at most `closeMs`) until every loopback refuses `port`. Never longer than their sum: what is still bound is for the cleanup check to report. */
async function stopVite(vite: Vite, port: number, { closeMs = 10_000, graceMs = 5_000, reapMs = 5_000 } = {}): Promise<void> {
  signalGroup(vite, "SIGTERM");
  if ((await waitChild(vite, graceMs)).kind === "timeout") signalGroup(vite, "SIGKILL");
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
      assert.ok(!groupAlive(vite), "the child group is still alive after stopVite");
      assert.equal((await probe(holdHost, port)).state, "listening", "control: the listener is still bound, so the release gave up on the deadline, not because the port was free");
    } finally {
      await stop();
    }
  });

  it("startVite: a child that never answers is killed and reaped before the error is thrown", async () => {
    await within(6_000, "startVite", assert.rejects(startVite(p.all[8]!, { command: idle, readyMs: 400 }), /did not come up/));
    const vite = lastSpawned()!;
    assert.equal(vite.ended?.kind, "exit", "the child was not reaped");
    assert.ok(!groupAlive(vite), "the child group is still alive");
  });

  it("startVite: a port that accepts TCP and never answers HTTP does not hang the start", async () => {
    const stop = await hangingListener(p.all[9]!);
    try {
      await within(6_000, "startVite", assert.rejects(startVite(p.all[9]!, { command: idle, readyMs: 400 }), /did not come up/));
      assert.ok(!groupAlive(lastSpawned()!), "the child group is still alive");
    } finally {
      await stop();
    }
  });

  it("startVite: a command that does not exist rejects with the spawn error inside readyMs, leaving no child and no port", async () => {
    const port = p.all[10]!;
    const t0 = Date.now();
    await within(3_000, "startVite", assert.rejects(startVite(port, { command: ["/nonexistent/x-monorepo-no-such-command"], readyMs: 400, reapMs: 200 }), /spawn failed: .*ENOENT/));
    assert.ok(Date.now() - t0 < 400 + 1_000, `took ${Date.now() - t0}ms`);
    const vite = lastSpawned()!;
    assert.equal(vite.ended?.kind, "error");
    assert.equal(vite.pid, undefined, "a child exists although nothing was spawned");
    assert.deepEqual(await probe(holdHost, port), { state: "closed" });
  });

  it("stopVite: a child that ignores SIGTERM is SIGKILLed after the grace period, reaped, and the release returns in bounds", async () => {
    const vite = spawnTracked(ignoresSigterm);
    await new Promise((r) => setTimeout(r, 300)); // the handler must be installed before the signal
    const t0 = Date.now();
    await within(4_000, "stopVite", stopVite(vite, p.all[11]!, { graceMs: 400, reapMs: 2_000, closeMs: 200 }));
    assert.ok(Date.now() - t0 >= 400, "SIGKILL came before the grace period ended: the child was not ignoring SIGTERM");
    assert.deepEqual(vite.ended, { kind: "exit", code: null, signal: "SIGKILL" });
    assert.ok(!groupAlive(vite));
  });

  it("stopVite: a child that never reports its end (not even after SIGKILL) does not hang the release", async () => {
    // the same group, but its end is never reported (a real child always reports it after SIGKILL)
    const silent = __testing.neverEnds(spawnTracked(idle));
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
  const fake = () => Object.assign(new EventEmitter(), { pid: undefined, exitCode: null, signalCode: null });
  it("exit → { exit, code, signal }", async () => {
    const c = fake();
    const t = __testing.fake(c);
    queueMicrotask(() => c.emit("exit", 3, null));
    assert.deepEqual(await waitChild(t, 1_000), { kind: "exit", code: 3, signal: null });
  });
  it("error → { error }, and an `error` before anyone waits is not an uncaught exception (the listener is hooked at __testing.fake())", async () => {
    const c = fake();
    const t = __testing.fake(c);
    const failure = Object.assign(new Error("spawn x ENOENT"), { code: "ENOENT" });
    assert.doesNotThrow(() => c.emit("error", failure));
    assert.deepEqual(await waitChild(t, 1_000), { kind: "error", error: failure });
  });
  it("neither → { timeout } after ms, and not before", async () => {
    const t = __testing.fake(fake());
    const t0 = Date.now();
    assert.deepEqual(await waitChild(t, 150), { kind: "timeout" });
    assert.ok(Date.now() - t0 >= 140);
  });
  it("signalGroup and groupAlive refuse a Tracked that spawnTracked did not make: no signal to a pid through a fake", () => {
    const real = spawnTracked([process.execPath, "-e", "setInterval(() => {}, 1000)"]);
    const forged = __testing.fake(Object.assign(new EventEmitter(), { pid: real.pid, exitCode: null, signalCode: null }));
    assert.throws(() => signalGroup(forged, "SIGKILL"), TypeError);
    assert.throws(() => groupAlive(forged), TypeError);
    assert.throws(() => signalGroup({ pid: real.pid, ended: undefined } as unknown as Tracked, "SIGKILL"), TypeError);
    assert.equal(groupAlive(real), true, "control: the real group was not touched, and the real Tracked still answers");
  });
  it("a wait is finite: NaN, negative, Infinity and past MAX_WAIT_MS are refused, not turned into a 1 ms timer or a forever", () => {
    const t = __testing.fake(fake());
    for (const ms of [Number.NaN, -1, Number.POSITIVE_INFINITY, MAX_WAIT_MS + 1, 2 ** 31]) assert.throws(() => waitChild(t, ms), RangeError, String(ms));
    return assert.rejects(runBounded(process.execPath, ["-e", ""], { timeoutMs: Number.POSITIVE_INFINITY }), RangeError);
  });
  it("the first end wins, and a later wait answers at once with it", async () => {
    const c = fake();
    const t = __testing.fake(c);
    c.emit("exit", 0, null);
    c.emit("error", new Error("late"));
    const t0 = Date.now();
    assert.deepEqual(await waitChild(t, 5_000), { kind: "exit", code: 0, signal: null });
    assert.ok(Date.now() - t0 < 500);
  });
});

// The selftest has no way to start a process or hook a child's `exit`: that is all in e2e/child.ts, whose Tracked
// hides the ChildProcess, and this file does not name the module that provides it. A new `import`, `require` or
// `import()` of it has to say its name, so adding a wait of one's own shows here before a reviewer has to find it.
// This is a tripwire for the accident, not a barrier against someone who builds the name from pieces.
describe("the selftest has no handle on a child process", () => {
  const MODULE = ["child", "process"].join("_");
  const read = (rel: string) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), "utf8");
  it("control: child.ts names the module, so this check can see it", () => {
    assert.ok(read("./child.ts").includes(MODULE));
  });
  it("assert-clean.selftest.ts does not", () => {
    assert.ok(!read("./assert-clean.selftest.ts").includes(MODULE), "this file names the child-process module: wait for a child through child.ts");
  });
});

// What a caller can reach on a Tracked is checked by the compiler (`task web-typecheck`, CI's web job): nothing to await
// without a limit, no handle to listen on. If one of these ever becomes reachable, its @ts-expect-error turns into an error.
export const noUnboundedWaitOnTracked = (t: Tracked) => {
  // @ts-expect-error the promise is private: the only wait is waitChild(t, ms)
  void t.whenEnded;
  // @ts-expect-error no ChildProcess handle either
  void t.child;
  // @ts-expect-error nor a way to hook `exit`
  void t.once;
  // @ts-expect-error a Tracked cannot be made outside child.ts: track() is not exported ...
  void childModule.track;
  // @ts-expect-error ... and no constructor (Tracked is exported as a type only)
  void new childModule.Tracked();
};
