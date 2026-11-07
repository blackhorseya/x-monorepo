import { randomInt } from "node:crypto";
import { createServer, type Server } from "node:net";
import { hasIPv6Loopback } from "./clean.ts";
import { derivePorts } from "./ports.ts";

// The E2E pair (E2E_RPC_PORT, E2E_WEB_PORT) the assert-clean selftest holds its listeners on (e2e/assert-clean.selftest.ts).
// It draws a pair at random from the OS's dynamic range, LOCKS every port derived from it (e2e/ports.ts, 19 of them across two
// blocks), and keeps it only if every one of them can also be bound right now on every loopback. The OS cannot hand out the pair
// itself: it hands out one port at a time, usually the next free one, and the derived ports must be spread over two separate blocks.
//
// The lock is what keeps concurrent runs apart (#495). "Free right now" says nothing about a process that has drawn a pair and not
// yet bound it, and several selftest processes run at once (`task web-e2e-ports` starts four of them, and another worktree may run
// its own beside them); two of them could pass the check on overlapping pairs, and one then found the other's listener on a port it
// derives. A lock is a socket the process keeps bound on 127.0.0.1 in the LOCK WINDOW below, one for each derived port (the port's
// number modulo the window's size picks the slot, so two draws that share a derived port share a slot). A second bind of a slot fails
// with EADDRINUSE: atomic, nobody has to be listening on the derived port itself, and no file is involved. The kernel closes the socket
// when the process ends, however it ends (exit, uncaught exception, SIGTERM, SIGINT, SIGKILL): nothing is left behind, so there is
// nothing to clean up and no handler to install. Two draws that share only a slot, not a port, refuse each other as well; the second
// one just draws again.
//
// The window is above every port the selftest binds or the checker probes (the derived rpc block ends at 30119, the web block starts at
// 40000) and below the ephemeral ranges (Linux 32768, macOS 49152); the default e2e ports (18091, 4173, 19511, 4511) are outside it too.
// Another program that listens inside it takes that slot out of play: the pairs that map onto it are skipped.

export type Pair = { rpc: number; web: number };
/** Where the lock sockets are: ports `base` .. `base + size - 1` on 127.0.0.1. */
export type LockWindow = { base: number; size: number };
export const LOCKS: LockWindow = { base: 30_200, size: 1_300 };
/** A pair, and `release()` to give its locks back before the process ends. Idempotent. */
export type ClaimedPair = Pair & { release: () => Promise<void> };

const bindable = (host: string, port: number) =>
  new Promise<boolean>((resolve) => {
    const s = createServer();
    s.once("error", () => resolve(false));
    s.listen({ port, host, ipv6Only: host === "::1" }, () => s.close(() => resolve(true)));
  });

/** A random pair, with room for the largest offset (+120) below 65535; the two blocks are far apart. */
export const randomPair = (): Pair => ({ rpc: randomInt(20_000, 30_000), web: randomInt(40_000, 65_000) });

const closeServer = (s: Server) => new Promise<void>((resolve) => s.close(() => resolve()));

/** All of `ports`' slots, or none: a slot that is taken gives back the ones bound so far. `held.release` closes them (and may be called again). */
async function lockSlots(ports: number[], { base, size }: LockWindow): Promise<{ release: () => Promise<void> } | { refused: string }> {
  const held: Server[] = [];
  const release = async () => {
    await Promise.all(held.splice(0).map(closeServer));
  };
  for (const slot of new Set(ports.map((port) => base + (port % size)))) {
    const s = createServer();
    const failure = await new Promise<NodeJS.ErrnoException | undefined>((resolve) => {
      s.once("error", resolve);
      s.listen({ port: slot, host: "127.0.0.1", exclusive: true }, () => resolve(undefined));
    });
    if (failure) {
      await release();
      return { refused: failure.code ?? String(failure) };
    }
    s.removeAllListeners("error");
    s.unref(); // a lock must not keep the process alive
    held.push(s);
  }
  return { release };
}

export async function privatePair({ draw = randomPair, attempts = 100, locks = LOCKS }: { draw?: () => Pair; attempts?: number; locks?: LockWindow } = {}): Promise<ClaimedPair> {
  // Two projects deriving the same port is a bug in e2e/ports.ts, not bad luck in the draw below: say so, instead of
  // letting every draw be skipped and the run end as "no free pair".
  const sample = derivePorts({ E2E_RPC_PORT: "20000", E2E_WEB_PORT: "40000" }).all;
  const twice = sample.filter((port, i) => sample.indexOf(port) !== i);
  if (twice.length) throw new Error(`derivePorts() derives port(s) ${[...new Set(twice)].join(", ")} twice (offsets of two projects collide, e2e/ports.ts)`);
  const hosts = (await hasIPv6Loopback()).available ? ["127.0.0.1", "::1"] : ["127.0.0.1"];
  let lastRefusal = "none";
  for (let attempt = 0; attempt < attempts; attempt++) {
    const pair = draw();
    const all = derivePorts({ E2E_RPC_PORT: String(pair.rpc), E2E_WEB_PORT: String(pair.web) }).all;
    if (Math.max(...all) > 65535 || new Set(all).size !== all.length) continue;
    const locked = await lockSlots(all, locks);
    if ("refused" in locked) {
      lastRefusal = locked.refused;
      continue; // another run holds one of these ports (or a program listens on the slot)
    }
    const free = await Promise.all(all.flatMap((port) => hosts.map((host) => bindable(host, port))));
    if (free.every(Boolean)) return { ...pair, release: locked.release };
    await locked.release();
  }
  throw new Error(`no private E2E port pair with every derived port free and unlocked after ${attempts} tries (locks: 127.0.0.1:${locks.base}-${locks.base + locks.size - 1}, last refusal: ${lastRefusal})`);
}
