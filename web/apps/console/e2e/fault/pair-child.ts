// A second process asking for a private pair, for e2e/assert-clean.parallel.selftest.ts: `node --experimental-strip-types
// e2e/fault/pair-child.ts <claimDir> [rpc,web …]`. The listed draws are offered first, in order, then random ones. Prints
// one JSON line: the pair it got and how many draws it took. It does not release: leaving the process is what is tested.
import { privatePair, randomPair, type Pair } from "../private-pair.ts";

const [claimDir, ...forced] = process.argv.slice(2);
const queue: Pair[] = forced.map((d) => {
  const [rpc, web] = d.split(",").map(Number);
  return { rpc: rpc!, web: web! };
});
let draws = 0;
const pair = await privatePair({
  claimDir,
  draw: () => {
    draws++;
    return queue.shift() ?? randomPair();
  },
});
console.log(JSON.stringify({ rpc: pair.rpc, web: pair.web, draws }));
