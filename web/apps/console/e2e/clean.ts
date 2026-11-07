import { createConnection, createServer, type Socket } from "node:net";

// What "is something still listening here?" means for the cleanup check (e2e/assert-clean.ts).
// `localhost` is ::1 on a machine that prefers IPv6 (Vite preview binds it) and 127.0.0.1 on one that
// does not, and a wildcard listener answers on both, so every loopback a fixture can bind is asked —
// asking one family reports a clean run while a server is still up (#386).
export const LOOPBACKS = ["127.0.0.1", "::1"] as const;

/**
 * listening: something accepted the connection. closed: the host answered ECONNREFUSED, a definite
 * "nobody here". unknown: anything else (timeout, unreachable, permission…) — it is not an answer, so
 * the caller must fail rather than call the port closed.
 */
export type Probe = { state: "listening" } | { state: "closed" } | { state: "unknown"; detail: string };

export type Connect = (opts: { port: number; host: string }) => Pick<Socket, "once" | "destroy"> & Partial<Pick<Socket, "localPort" | "remotePort" | "localAddress" | "remoteAddress">>;

/**
 * A socket the kernel connected to ITSELF: connecting to a port nobody holds, from the ephemeral range, can pick that
 * very port as the source port, and TCP simultaneous open then "succeeds" (connect event, local == remote). Nobody
 * answered. A real listener cannot produce this: the source port is never the port it holds on the same address.
 */
const isSelfConnect = (s: ReturnType<Connect>): boolean => s.localPort !== undefined && s.localPort === s.remotePort && s.localAddress === s.remoteAddress;

/** Attempts a probe makes when the kernel answers with a self-connect; the next one draws another source port. */
const SELF_CONNECT_ATTEMPTS = 5;

/** Only ECONNREFUSED is a definite answer; every other error code, or none, is not. */
export function classifyConnectError(code: string | undefined): "closed" | "unknown" {
  return code === "ECONNREFUSED" ? "closed" : "unknown";
}

export async function probe(host: string, port: number, timeoutMs = 2_000, connect: Connect = createConnection): Promise<Probe> {
  for (let attempt = 1; ; attempt++) {
    const r = await probeOnce(host, port, timeoutMs, connect);
    if (r !== "self-connect") return r;
    // Not an answer, and not "closed" either: ask again, and fail closed when the kernel keeps doing it.
    if (attempt >= SELF_CONNECT_ATTEMPTS) return { state: "unknown", detail: `${host}:${port} self-connect on ${attempt} attempts in a row (the kernel connected the probe to itself)` };
  }
}

function probeOnce(host: string, port: number, timeoutMs: number, connect: Connect): Promise<Probe | "self-connect"> {
  return new Promise((resolve) => {
    const socket = connect({ port, host });
    const timer = setTimeout(() => (socket.destroy(), resolve({ state: "unknown", detail: `${host}:${port} timed out after ${timeoutMs}ms` })), timeoutMs);
    socket.once("connect", () => (clearTimeout(timer), (isSelfConnect(socket) ? resolve("self-connect") : resolve({ state: "listening" })), socket.destroy()));
    socket.once("error", (err: NodeJS.ErrnoException) => {
      clearTimeout(timer);
      socket.destroy();
      resolve(classifyConnectError(err.code) === "closed" ? { state: "closed" } : { state: "unknown", detail: `${host}:${port} ${err.code ?? err.message}` });
    });
  });
}

/** Bind errors that mean this host has no such loopback address: nothing can be listening on it. */
const NO_ADDRESS = new Set(["EADDRNOTAVAIL", "EAFNOSUPPORT", "EPROTONOSUPPORT"]);
/** The one definition of "this host has no such address", for the checker and for the selftest that skips on it. */
export const isNoAddress = (code: string | undefined): boolean => code !== undefined && NO_ADDRESS.has(code);

/**
 * Whether ::1 exists here. Decided by binding it (the host's own answer), not by reading a connect
 * error: a host without IPv6 loopback has nothing that could be listening there. Any other bind error
 * cannot be told apart from "IPv6 exists but is broken", so it is thrown, not guessed.
 */
export function hasIPv6Loopback(): Promise<{ available: true } | { available: false; code: string }> {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.once("error", (err: NodeJS.ErrnoException) => {
      if (err.code && isNoAddress(err.code)) resolve({ available: false, code: err.code });
      else reject(err);
    });
    s.listen({ port: 0, host: "::1", ipv6Only: true }, () => s.close(() => resolve({ available: true })));
  });
}

/**
 * Waits until every host answers ECONNREFUSED on `port` (the same probe, and the same meaning of "closed", as the
 * cleanup check), for at most `deadlineMs`. true: all closed. false: the deadline passed first; the caller's own check
 * then reports what is left. A failed HTTP request is not an answer: a server that is shutting down still holds the
 * port and resets connections (fetch fails with ECONNRESET), and a timeout or any error other than a refusal is
 * "unknown", which keeps the wait going.
 */
export async function waitClosed(
  hosts: readonly string[],
  port: number,
  opts: { deadlineMs?: number; intervalMs?: number; probe?: typeof probe } = {},
): Promise<boolean> {
  const { deadlineMs = 10_000, intervalMs = 50, probe: ask = probe } = opts;
  const end = Date.now() + deadlineMs;
  for (;;) {
    const results = await Promise.all(hosts.map((host) => ask(host, port, Math.max(1, Math.min(2_000, end - Date.now())))));
    if (results.every((r) => r.state === "closed")) return true;
    const left = end - Date.now();
    if (left <= 0) return false;
    await new Promise((r) => setTimeout(r, Math.min(intervalMs, left)));
  }
}
