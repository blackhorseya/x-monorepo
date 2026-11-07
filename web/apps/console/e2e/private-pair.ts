import { randomInt } from "node:crypto";
import { createServer } from "node:net";
import { hasIPv6Loopback } from "./clean.ts";
import { derivePorts } from "./ports.ts";

// The E2E pair (E2E_RPC_PORT, E2E_WEB_PORT) the assert-clean selftest holds its listeners on (e2e/assert-clean.selftest.ts).
// It picks a pair at random from the OS's dynamic range and keeps it only if EVERY port derived from it (e2e/ports.ts, 19 of
// them across two blocks) can be bound right now on every loopback. The OS cannot hand out the pair itself: it hands out one
// port at a time, usually the next free one, and the derived ports must be spread over two separate blocks.

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

export async function privatePair({ draw = randomPair, attempts = 100 }: { draw?: () => Pair; attempts?: number; claimDir?: string } = {}): Promise<ClaimedPair> {
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
    const free = await Promise.all(all.flatMap((port) => hosts.map((host) => bindable(host, port))));
    if (free.every(Boolean)) return { ...pair, release: () => {} };
  }
  throw new Error(`no private E2E port pair with every derived port free after ${attempts} tries`);
}
