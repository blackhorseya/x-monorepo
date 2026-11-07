// A second process asking for a private pair, for e2e/assert-clean.parallel.selftest.ts:
//   node --experimental-strip-types e2e/fault/pair-child.ts <readyFile> <exit|throw|hold> <lockBase> <lockSize> [rpc,web …]
// The listed draws are offered first, in order, then calm random ones. Once it holds a pair it writes one JSON line (the pair, how many
// draws it took) to <readyFile>, then ends the way the mode says: `exit` reaches the end of the script, `throw` throws, `hold` stays alive
// until it is signalled. It never releases: how the process ends is what is tested.
import { randomInt } from "node:crypto";
import { renameSync, writeFileSync } from "node:fs";
import { privatePair, type Pair } from "../private-pair.ts";

const [readyFile, mode, base, size, ...forced] = process.argv.slice(2);
const queue: Pair[] = forced.map((d) => {
  const [rpc, web] = d.split(",").map(Number);
  return { rpc: rpc!, web: web! };
});
// Below the OS's ephemeral range (macOS 49152): a port in it can be busy for a moment, and the draw would be refused for that.
const calm = (): Pair => ({ rpc: randomInt(20_000, 30_000), web: randomInt(40_000, 48_500) });
let draws = 0;
const pair = await privatePair({
  locks: { base: Number(base), size: Number(size) },
  draw: () => {
    draws++;
    return queue.shift() ?? calm();
  },
});
writeFileSync(`${readyFile}.tmp`, JSON.stringify({ rpc: pair.rpc, web: pair.web, draws }));
renameSync(`${readyFile}.tmp`, readyFile); // the parent reads it only whole
if (mode === "throw") throw new Error("uncaught on purpose");
if (mode === "hold") setInterval(() => {}, 1_000);
