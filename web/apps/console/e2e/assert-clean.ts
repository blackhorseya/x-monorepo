import { readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { LOOPBACKS, hasIPv6Loopback, probe } from "./clean.ts";
import { derivePorts, oidcTmpPrefix } from "./ports.ts";

// After a run: nothing the e2e stack started is still listening on a port it derived — on any
// loopback address (e2e/clean.ts) — and the per-run rpc config (e2e/oidc-backend/rpc-server.ts) is
// gone. Exit 1 lists what is left, or what could not be checked: only a refused connection counts as
// closed, so a timeout or an unexpected error fails the check instead of passing it (#383, #386).
const p = derivePorts();
const ipv6 = await hasIPv6Loopback();
const hosts = LOOPBACKS.filter((h) => h !== "::1" || ipv6.available);
if (!ipv6.available) console.log(`no IPv6 loopback on this host (${ipv6.code}): ::1 cannot hold a listener, not checked`);

const results = await Promise.all(p.all.flatMap((port) => hosts.map(async (host) => ({ host, port, result: await probe(host, port) }))));
const listening = results.filter((r) => r.result.state === "listening").map((r) => (r.host.includes(":") ? `[${r.host}]:${r.port}` : `${r.host}:${r.port}`));
const unknown = results.flatMap((r) => (r.result.state === "unknown" ? [r.result.detail] : []));

const prefix = oidcTmpPrefix(p.backendIdpPort);
const leftovers = readdirSync(tmpdir()).filter((name) => name.startsWith(prefix));
if (listening.length || unknown.length || leftovers.length) {
  console.error(
    `e2e left behind or could not be verified: listening [${listening.join(", ")}], unverifiable [${unknown.join("; ")}], temp config dirs [${leftovers.join(", ")}] in ${tmpdir()}`,
  );
  process.exit(1);
}
console.log(`e2e cleanup ok: ${p.all.length} derived ports closed on ${hosts.join(" and ")}, no temp config under ${tmpdir()}`);
