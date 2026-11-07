import { randomInt } from "node:crypto";
import { mkdirSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { hasIPv6Loopback } from "./clean.ts";
import { derivePorts } from "./ports.ts";

// The E2E pair (E2E_RPC_PORT, E2E_WEB_PORT) the assert-clean selftest holds its listeners on (e2e/assert-clean.selftest.ts).
// It draws a pair at random from the OS's dynamic range, CLAIMS every port derived from it (e2e/ports.ts, 19 of them across two
// blocks), and keeps it only if every one of them can also be bound right now on every loopback. The OS cannot hand out the pair
// itself: it hands out one port at a time, usually the next free one, and the derived ports must be spread over two separate blocks.
//
// The claim is what keeps concurrent runs apart (#495). "Free right now" says nothing about a process that has drawn a pair and not
// yet bound it, and several selftest processes run at once (`task web-e2e-ports` starts four of them); two of them could pass the
// check on overlapping pairs, and one then found the other's listener on a port it derives. A claim is a directory per port,
// `<tmpdir>/x-monorepo-e2e-port-<port>`, made with mkdir: atomic, so of two processes claiming the same port exactly one succeeds, and
// no one has to be listening for it to hold. It is given back by `release()` and, whatever the caller did, when the process exits.
// A process that is SIGKILLed leaves its claims: those ports are then skipped by later draws, never shared.

export type Pair = { rpc: number; web: number };
/** A pair, and `release()` for whoever is done with it. */
export type ClaimedPair = Pair & { release: () => void };

const bindable = (host: string, port: number) =>
  new Promise<boolean>((resolve) => {
    const s = createServer();
    s.once("error", () => resolve(false));
    s.listen({ port, host, ipv6Only: host === "::1" }, () => s.close(() => resolve(true)));
  });

/** A random pair, with room for the largest offset (+120) below 65535; the two blocks are far apart. */
export const randomPair = (): Pair => ({ rpc: randomInt(20_000, 30_000), web: randomInt(40_000, 65_000) });

/** All of `ports`, or none: the claim of each is one atomic mkdir, and a port that is taken gives back the ones made so far. Returns the release, or undefined. */
function claimPorts(ports: number[], dir: string): (() => void) | undefined {
  const made: string[] = [];
  const release = () => {
    for (const d of made.splice(0)) rmSync(d, { recursive: true, force: true });
  };
  for (const port of ports) {
    const d = join(dir, `x-monorepo-e2e-port-${port}`);
    try {
      mkdirSync(d);
    } catch (e) {
      release();
      if ((e as NodeJS.ErrnoException).code === "EEXIST") return undefined;
      throw e;
    }
    made.push(d);
  }
  return release;
}

export async function privatePair({ draw = randomPair, attempts = 100, claimDir = tmpdir() }: { draw?: () => Pair; attempts?: number; claimDir?: string } = {}): Promise<ClaimedPair> {
  // Two projects deriving the same port is a bug in e2e/ports.ts, not bad luck in the draw below: say so, instead of
  // letting every draw be skipped and the run end as "no free pair".
  const sample = derivePorts({ E2E_RPC_PORT: "20000", E2E_WEB_PORT: "40000" }).all;
  const twice = sample.filter((port, i) => sample.indexOf(port) !== i);
  if (twice.length) throw new Error(`derivePorts() derives port(s) ${[...new Set(twice)].join(", ")} twice (offsets of two projects collide, e2e/ports.ts)`);
  const hosts = (await hasIPv6Loopback()).available ? ["127.0.0.1", "::1"] : ["127.0.0.1"];
  for (let attempt = 0; attempt < attempts; attempt++) {
    const pair = draw();
    const all = derivePorts({ E2E_RPC_PORT: String(pair.rpc), E2E_WEB_PORT: String(pair.web) }).all;
    if (Math.max(...all) > 65535 || new Set(all).size !== all.length) continue;
    const unclaim = claimPorts(all, claimDir);
    if (!unclaim) continue; // another run holds one of these ports
    const release = () => {
      process.off("exit", release);
      unclaim();
    };
    const free = await Promise.all(all.flatMap((port) => hosts.map((host) => bindable(host, port))));
    if (free.every(Boolean)) {
      process.on("exit", release); // rmSync is synchronous: it runs in an exit handler
      return { ...pair, release };
    }
    unclaim();
  }
  throw new Error(`no private E2E port pair with every derived port free after ${attempts} tries`);
}
