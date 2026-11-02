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

export type Connect = (opts: { port: number; host: string }) => Pick<Socket, "once" | "destroy">;

/** Only ECONNREFUSED is a definite answer; every other error code, or none, is not. */
export function classifyConnectError(code: string | undefined): "closed" | "unknown" {
  return code === "ECONNREFUSED" ? "closed" : "unknown";
}

export function probe(host: string, port: number, timeoutMs = 2_000, connect: Connect = createConnection): Promise<Probe> {
  return new Promise((resolve) => {
    const socket = connect({ port, host });
    const timer = setTimeout(() => (socket.destroy(), resolve({ state: "unknown", detail: `${host}:${port} timed out after ${timeoutMs}ms` })), timeoutMs);
    socket.once("connect", () => (clearTimeout(timer), socket.destroy(), resolve({ state: "listening" })));
    socket.once("error", (err: NodeJS.ErrnoException) => {
      clearTimeout(timer);
      socket.destroy();
      resolve(classifyConnectError(err.code) === "closed" ? { state: "closed" } : { state: "unknown", detail: `${host}:${port} ${err.code ?? err.message}` });
    });
  });
}

/** Bind errors that mean this host has no such loopback address: nothing can be listening on it. */
const NO_ADDRESS = new Set(["EADDRNOTAVAIL", "EAFNOSUPPORT", "EPROTONOSUPPORT"]);

/**
 * Whether ::1 exists here. Decided by binding it (the host's own answer), not by reading a connect
 * error: a host without IPv6 loopback has nothing that could be listening there. Any other bind error
 * cannot be told apart from "IPv6 exists but is broken", so it is thrown, not guessed.
 */
export function hasIPv6Loopback(): Promise<{ available: true } | { available: false; code: string }> {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.once("error", (err: NodeJS.ErrnoException) => {
      if (err.code && NO_ADDRESS.has(err.code)) resolve({ available: false, code: err.code });
      else reject(err);
    });
    s.listen({ port: 0, host: "::1", ipv6Only: true }, () => s.close(() => resolve({ available: true })));
  });
}
